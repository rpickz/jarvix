package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

func sseServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func collect(t *testing.T, ch <-chan ai.Event) (text string, done bool, err error) {
	t.Helper()
	for ev := range ch {
		switch ev.Type {
		case ai.EventDelta:
			text += ev.Content
		case ai.EventDone:
			done = true
		case ai.EventError:
			err = ev.Err
		}
	}
	return text, done, err
}

func TestChatStreamsDeltas(t *testing.T) {
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range []string{"Hello", " ", "world"} {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})

	c := New("test", srv.URL+"/v1", "test-key")
	ch, err := c.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: ai.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, done, streamErr := collect(t, ch)
	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}
	if !done {
		t.Error("no done event")
	}
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
}

func TestChatNoAuthHeaderWithoutKey(t *testing.T) {
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("Authorization header sent for keyless endpoint")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	c := New("ollama", srv.URL+"/v1", "")
	ch, err := c.Chat(context.Background(), ai.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _, _ = collect(t, ch)
}

func TestChatHTTPErrorSurfacesMessage(t *testing.T) {
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Incorrect API key provided"}}`)
	})
	c := New("openai", srv.URL+"/v1", "bad")
	_, err := c.Chat(context.Background(), ai.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Incorrect API key") || !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v", err)
	}
}

func TestChatMidStreamCancellation(t *testing.T) {
	streaming := make(chan struct{})
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		fl.Flush()
		close(streaming)
		// Stall; the client should give up when its context is cancelled.
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	c := New("test", srv.URL+"/v1", "")
	ch, err := c.Chat(ctx, ai.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	<-streaming
	cancel()

	deadline := time.After(5 * time.Second)
	var final error
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				if !errors.Is(final, context.Canceled) {
					t.Errorf("final error = %v, want context.Canceled", final)
				}
				return
			}
			if ev.Type == ai.EventError {
				final = ev.Err
			}
		case <-deadline:
			t.Fatal("stream did not terminate after cancellation")
		}
	}
}

func TestChatEOFWithoutDoneIsClean(t *testing.T) {
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		// Connection closes without [DONE].
	})
	c := New("test", srv.URL+"/v1", "")
	ch, err := c.Chat(context.Background(), ai.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, done, streamErr := collect(t, ch)
	if streamErr != nil || !done || text != "partial" {
		t.Errorf("text=%q done=%v err=%v", text, done, streamErr)
	}
}

func TestProbe(t *testing.T) {
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = fmt.Fprint(w, `{"data":[]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	if err := New("test", srv.URL+"/v1", "").Probe(context.Background()); err != nil {
		t.Errorf("Probe: %v", err)
	}
	if err := New("down", "http://127.0.0.1:1/v1", "").Probe(context.Background()); err == nil {
		t.Error("Probe against dead endpoint should fail")
	}
}

// The engine layers system-role content through a turn (prompt, remembered
// facts, desktop context); Qwen3-family chat templates hard-error unless a
// request carries exactly one leading system message. These pin the fold.
func TestCoalesceSystemFoldsLayeredSystemMessages(t *testing.T) {
	in := []ai.Message{
		{Role: ai.RoleSystem, Content: "prompt"},
		{Role: ai.RoleSystem, Content: "remembered facts"},
		{Role: ai.RoleUser, Content: "earlier question"},
		{Role: ai.RoleAssistant, Content: "earlier answer"},
		{Role: ai.RoleSystem, Content: "desktop context"},
		{Role: ai.RoleUser, Content: "the question"},
	}
	out := coalesceSystem(in)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(out), out)
	}
	if out[0].Role != ai.RoleSystem || out[0].Content != "prompt\n\nremembered facts\n\ndesktop context" {
		t.Errorf("system = %+v", out[0])
	}
	rest := []string{string(out[1].Role), out[1].Content, string(out[2].Role), out[2].Content, string(out[3].Role), out[3].Content}
	want := []string{"user", "earlier question", "assistant", "earlier answer", "user", "the question"}
	for i := range want {
		if rest[i] != want[i] {
			t.Errorf("non-system order broken at %d: got %q want %q", i, rest[i], want[i])
		}
	}
}

func TestCoalesceSystemPassesThroughTheCommonShape(t *testing.T) {
	in := []ai.Message{
		{Role: ai.RoleSystem, Content: "prompt"},
		{Role: ai.RoleUser, Content: "hi"},
	}
	out := coalesceSystem(in)
	if len(out) != 2 || out[0].Content != "prompt" || out[1].Content != "hi" {
		t.Errorf("passthrough changed the request: %+v", out)
	}
	if none := coalesceSystem([]ai.Message{{Role: ai.RoleUser, Content: "hi"}}); len(none) != 1 {
		t.Errorf("no-system request changed: %+v", none)
	}
	if empty := coalesceSystem(nil); len(empty) != 0 {
		t.Errorf("nil request changed: %+v", empty)
	}
}

// A lone system message that is not first (context injected with no system
// prompt configured) still moves to the front — the template rule is about
// position, not count.
func TestCoalesceSystemHoistsAStraySystemMessage(t *testing.T) {
	in := []ai.Message{
		{Role: ai.RoleUser, Content: "hi"},
		{Role: ai.RoleSystem, Content: "desktop context"},
	}
	out := coalesceSystem(in)
	if len(out) != 2 || out[0].Role != ai.RoleSystem || out[0].Content != "desktop context" || out[1].Content != "hi" {
		t.Errorf("stray system not hoisted: %+v", out)
	}
}
