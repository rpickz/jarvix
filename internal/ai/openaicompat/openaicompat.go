// Package openaicompat implements ai.Provider against any OpenAI-compatible
// chat completions endpoint: OpenAI itself, OpenRouter, Ollama, LM Studio,
// llama.cpp server, and most self-hosted gateways.
//
// One implementation, many providers: the endpoint (base URL + API key) is
// configuration, not code.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

// Client streams chat completions over SSE.
type Client struct {
	name    string
	baseURL string
	apiKey  string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client (used by tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New creates a client for one OpenAI-compatible endpoint. name is the
// configured provider name and is used only for logging and errors. apiKey
// may be empty for local endpoints such as Ollama.
func New(name, baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// No overall timeout: streams are long-lived. Cancellation comes from
		// the request context; dial/TLS have their own default timeouts.
		http: &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements ai.Provider.
func (c *Client) Name() string { return c.name }

type wireToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatBody struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Tools       []wireTool    `json:"tools,omitempty"`
}

// chunkToolCall is a streamed tool_calls fragment: fields arrive across
// several chunks and are stitched together by index.
type chunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			ToolCalls []chunkToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Chat implements ai.Provider. It opens an SSE stream and forwards deltas as
// events. The returned channel is closed after a final done or error event.
func (c *Client) Chat(ctx context.Context, req ai.ChatRequest) (<-chan ai.Event, error) {
	body := chatBody{
		Model:  req.Model,
		Stream: true,
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	if req.Temperature != 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			var wtc wireToolCall
			wtc.ID = tc.ID
			wtc.Type = "function"
			wtc.Function.Name = tc.Name
			wtc.Function.Arguments = tc.Arguments
			wm.ToolCalls = append(wm.ToolCalls, wtc)
		}
		body.Messages = append(body.Messages, wm)
	}
	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Schema
		body.Tools = append(body.Tools, wt)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", c.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", c.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", c.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, c.statusError(resp)
	}

	ch := make(chan ai.Event)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		calls, err := c.readStream(ctx, resp.Body, ch)
		if err != nil {
			// Cancellation surfaces as ctx.Err() so callers can tell an
			// interrupt apart from a provider failure.
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			ch <- ai.Event{Type: ai.EventError, Err: err}
			return
		}
		// Tool calls are emitted once the stream ends: their fragments are
		// only complete then, and callers act on them after the text anyway.
		for _, call := range calls {
			select {
			case ch <- ai.Event{Type: ai.EventToolCall, Call: call}:
			case <-ctx.Done():
				ch <- ai.Event{Type: ai.EventError, Err: ctx.Err()}
				return
			}
		}
		ch <- ai.Event{Type: ai.EventDone}
	}()
	return ch, nil
}

// readStream parses SSE "data:" lines until [DONE] or EOF, forwarding text
// deltas as they arrive and returning assembled tool calls at the end.
func (c *Client) readStream(ctx context.Context, r io.Reader, ch chan<- ai.Event) ([]ai.ToolCall, error) {
	// Streaming tool calls arrive as fragments keyed by index: the first
	// fragment carries id and name, later ones append argument text.
	var calls []ai.ToolCall
	byIndex := map[int]int{} // fragment index → position in calls

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return calls, nil
		}
		var ck chunk
		if err := json.Unmarshal([]byte(data), &ck); err != nil {
			return nil, fmt.Errorf("%s: malformed stream chunk: %w", c.name, err)
		}
		if ck.Error != nil {
			return nil, fmt.Errorf("%s: %s", c.name, ck.Error.Message)
		}
		for _, choice := range ck.Choices {
			for _, frag := range choice.Delta.ToolCalls {
				pos, ok := byIndex[frag.Index]
				if !ok {
					pos = len(calls)
					byIndex[frag.Index] = pos
					calls = append(calls, ai.ToolCall{})
				}
				if frag.ID != "" {
					calls[pos].ID = frag.ID
				}
				if frag.Function.Name != "" {
					calls[pos].Name = frag.Function.Name
				}
				calls[pos].Arguments += frag.Function.Arguments
			}
			if choice.Delta.Content == "" {
				continue
			}
			select {
			case ch <- ai.Event{Type: ai.EventDelta, Content: choice.Delta.Content}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: stream read: %w", c.name, err)
	}
	// EOF without [DONE]: some servers just close the stream. Treat as done.
	return calls, nil
}

// statusError turns a non-200 response into an actionable error without
// leaking credentials.
func (c *Client) statusError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var envelope struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		return fmt.Errorf("%s: HTTP %d: %s", c.name, resp.StatusCode, envelope.Error.Message)
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d: %s", c.name, resp.StatusCode, msg)
}

// Probe checks the endpoint is reachable and authenticated by listing models.
// Used by jarvix doctor; not part of the ai.Provider interface.
func (c *Client) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable at %s: %w", c.name, c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return c.statusError(resp)
	}
	return nil
}
