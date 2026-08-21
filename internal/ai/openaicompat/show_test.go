package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// The /api/show introspection behind doctor's context-floor check (issue
// #71): a native ollama endpoint beside the /v1 surface, read best-effort.

func showServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := sseServer(t, handler)
	return New("ollama", srv.URL+"/v1", "")
}

func TestOllamaServedContextReadsNumCtxAndArchitectureLimit(t *testing.T) {
	client := showServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			t.Errorf("path = %q, want /api/show beside the /v1 surface", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "jarvix-qwen7b" {
			t.Errorf("request body = %v (%v), want the model name", body, err)
		}
		// The shape of a num_ctx variant: the modelfile parameter block plus
		// the architecture's own limit.
		_, _ = fmt.Fprint(w, `{
			"parameters": "num_ctx                        16384\nstop \"<|im_end|>\"",
			"model_info": {"qwen2.context_length": 32768, "qwen2.embedding_length": 3584}
		}`)
	})
	served, err := client.OllamaServedContext(context.Background(), "jarvix-qwen7b")
	if err != nil {
		t.Fatal(err)
	}
	if served.NumCtx != 16384 || served.MaxCtx != 32768 {
		t.Errorf("served = %+v, want NumCtx 16384 MaxCtx 32768", served)
	}
}

func TestOllamaServedContextWithoutNumCtxReportsZero(t *testing.T) {
	// The live incident's shape: a stock model, no num_ctx anywhere — the
	// caller must learn "nothing stated" (0), not a guess, so the default
	// assumption stays in one documented place.
	client := showServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{
			"parameters": "stop \"<|im_end|>\"",
			"model_info": {"qwen2.context_length": 32768}
		}`)
	})
	served, err := client.OllamaServedContext(context.Background(), "qwen2.5:7b")
	if err != nil {
		t.Fatal(err)
	}
	if served.NumCtx != 0 || served.MaxCtx != 32768 {
		t.Errorf("served = %+v, want NumCtx 0 MaxCtx 32768", served)
	}
}

func TestOllamaServedContextDegradesOnErrors(t *testing.T) {
	// A dead endpoint fails fast (the timeout is the reliability contract);
	// an HTTP error is an error, not a fabricated reading.
	if _, err := New("ollama", "http://127.0.0.1:1/v1", "").
		OllamaServedContext(context.Background(), "m"); err == nil {
		t.Error("a dead endpoint must error, not invent a context length")
	}
	client := showServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := client.OllamaServedContext(context.Background(), "missing"); err == nil {
		t.Error("an HTTP error must surface as one")
	}
}

func TestParseNumCtx(t *testing.T) {
	tests := []struct {
		parameters string
		want       int
	}{
		{"num_ctx 16384", 16384},
		{"stop \"x\"\nnum_ctx    4096\ntemperature 0.7", 4096},
		{"", 0},
		{"num_ctx nonsense", 0},
		{"num_ctx -1", 0},
	}
	for _, tt := range tests {
		if got := parseNumCtx(tt.parameters); got != tt.want {
			t.Errorf("parseNumCtx(%q) = %d, want %d", tt.parameters, got, tt.want)
		}
	}
}
