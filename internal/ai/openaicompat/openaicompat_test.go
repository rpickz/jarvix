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
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
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
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	c := New("ollama", srv.URL+"/v1", "")
	ch, err := c.Chat(context.Background(), ai.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collect(t, ch)
}

func TestChatHTTPErrorSurfacesMessage(t *testing.T) {
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key provided"}}`)
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
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
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
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
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
			fmt.Fprint(w, `{"data":[]}`)
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
