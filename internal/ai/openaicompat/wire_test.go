package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

func TestClientName(t *testing.T) {
	c := New("openrouter", "https://x.test/v1/", "")
	if c.Name() != "openrouter" {
		t.Errorf("name = %q", c.Name())
	}
	// Trailing slashes must not produce double-slash URLs later.
	if c.baseURL != "https://x.test/v1" {
		t.Errorf("baseURL = %q", c.baseURL)
	}
}

func TestWithHTTPClientOverrides(t *testing.T) {
	h := &http.Client{}
	c := New("x", "https://x.test", "", WithHTTPClient(h))
	if c.http != h {
		t.Error("WithHTTPClient did not take effect")
	}
}

// TestChatSerializesRequestWire pins the exact wire shape: history with tool
// calls and results, tool definitions, sampling parameters, and auth header.
func TestChatSerializesRequestWire(t *testing.T) {
	var gotBody chatBody
	var gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &gotBody); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New("test", srv.URL, "sk-secret")
	temp := 0.4
	req := ai.ChatRequest{
		Model:       "m1",
		MaxTokens:   256,
		Temperature: temp,
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: "be terse"},
			{Role: ai.RoleUser, Content: "docker status?"},
			{Role: ai.RoleAssistant, Content: "", ToolCalls: []ai.ToolCall{
				{ID: "c1", Name: "run", Arguments: `{"command":"docker ps"}`}}},
			{Role: ai.RoleTool, ToolCallID: "c1", Content: "3 containers"},
		},
		Tools: []ai.ToolDef{{Name: "run", Description: "run a command",
			Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	ch, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if !gotBody.Stream || gotBody.Model != "m1" || gotBody.MaxTokens != 256 {
		t.Errorf("body = %+v", gotBody)
	}
	if gotBody.Temperature == nil || *gotBody.Temperature != temp {
		t.Errorf("temperature = %v", gotBody.Temperature)
	}
	if len(gotBody.Messages) != 4 {
		t.Fatalf("messages = %+v", gotBody.Messages)
	}
	asst := gotBody.Messages[2]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "c1" ||
		asst.ToolCalls[0].Type != "function" || asst.ToolCalls[0].Function.Name != "run" {
		t.Errorf("assistant tool calls = %+v", asst.ToolCalls)
	}
	if gotBody.Messages[3].ToolCallID != "c1" {
		t.Errorf("tool result = %+v", gotBody.Messages[3])
	}
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Function.Name != "run" || gotBody.Tools[0].Type != "function" {
		t.Errorf("tools = %+v", gotBody.Tools)
	}
}

// TestChatAssemblesStreamedToolCallFragments proves fragments keyed by index
// are stitched into complete calls and emitted after the text.
func TestChatAssemblesStreamedToolCallFragments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`data: {"choices":[{"delta":{"content":"On it. "}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c9","function":{"name":"run","arguments":"{\"comm"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"uptime\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c10","function":{"name":"run","arguments":"{}"}}]}}]}`,
			`data: [DONE]`,
		}
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n\n")
		}
	}))
	defer srv.Close()

	ch, err := New("test", srv.URL, "").Chat(context.Background(), ai.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var calls []ai.ToolCall
	for ev := range ch {
		switch ev.Type {
		case ai.EventDelta:
			text += ev.Content
		case ai.EventToolCall:
			calls = append(calls, ev.Call)
		case ai.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if text != "On it. " {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ID != "c9" || calls[0].Name != "run" || calls[0].Arguments != `{"command":"uptime"}` {
		t.Errorf("call 0 = %+v", calls[0])
	}
	if calls[1].ID != "c10" || calls[1].Arguments != "{}" {
		t.Errorf("call 1 = %+v", calls[1])
	}
}

func TestStatusErrorShapes(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"json error envelope": {
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"bad api key","type":"auth"}}`,
			want:   "HTTP 401: bad api key",
		},
		"plain text body": {
			status: http.StatusBadGateway,
			body:   "upstream fell over",
			want:   "HTTP 502: upstream fell over",
		},
		"empty body falls back to status text": {
			status: http.StatusServiceUnavailable,
			body:   "",
			want:   "HTTP 503: Service Unavailable",
		},
		"long body is truncated": {
			status: http.StatusInternalServerError,
			body:   strings.Repeat("x", 400),
			want:   "…",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			_, err := New("prov", srv.URL, "").Chat(context.Background(), ai.ChatRequest{Model: "m"})
			if err == nil {
				t.Fatal("non-200 must fail before streaming")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "prov") {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestProbeChecksModelsEndpoint(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	if err := New("test", srv.URL, "sk-1").Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/models" || gotAuth != "Bearer sk-1" {
		t.Errorf("probe hit %q with auth %q", gotPath, gotAuth)
	}
}

func TestProbeFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if err := New("test", srv.URL, "").Probe(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v", err)
	}

	unreachable := New("test", "http://127.0.0.1:1", "")
	if err := unreachable.Probe(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "unreachable") {
		t.Errorf("err = %v", err)
	}
}
