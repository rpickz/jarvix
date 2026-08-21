package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DefaultOllamaURL is where a local Ollama listens.
const DefaultOllamaURL = "http://127.0.0.1:11434"

// HTTPOllama detects a running Ollama over its native HTTP API. Detection is
// deliberately not routed through the AI provider abstraction: it must be a
// fast, silent local probe with no configuration required.
type HTTPOllama struct {
	// BaseURL overrides DefaultOllamaURL (tests point it at a local server).
	BaseURL string
	// Client overrides http.DefaultClient.
	Client *http.Client
}

// Models implements OllamaDetector by listing installed models via
// GET /api/tags. An error means Ollama is not reachable.
func (o *HTTPOllama) Models(ctx context.Context) ([]string, error) {
	base := o.BaseURL
	if base == "" {
		base = DefaultOllamaURL
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama responded with HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("ollama response: %w", err)
	}
	names := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		names = append(names, m.Name)
	}
	return names, nil
}
