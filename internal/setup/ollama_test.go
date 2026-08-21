package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPOllamaModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2:3b"},{"name":"qwen2.5:7b"}]}`))
	}))
	defer srv.Close()

	o := &HTTPOllama{BaseURL: srv.URL}
	models, err := o.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "llama3.2:3b" || models[1] != "qwen2.5:7b" {
		t.Fatalf("got %v", models)
	}
}

func TestHTTPOllamaUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more

	o := &HTTPOllama{BaseURL: url}
	if _, err := o.Models(context.Background()); err == nil {
		t.Fatal("a closed port must report unreachable")
	}
}

func TestHTTPOllamaBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	o := &HTTPOllama{BaseURL: srv.URL}
	if _, err := o.Models(context.Background()); err == nil {
		t.Fatal("a non-200 must report an error")
	}
}
