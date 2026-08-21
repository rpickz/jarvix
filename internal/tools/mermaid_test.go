package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireMmdc keeps CI hermetic: the real-renderer tests only run where
// mermaid-cli happens to be installed (a dev machine), and skip elsewhere.
func requireMmdc(t *testing.T) *MermaidRenderer {
	t.Helper()
	r := &MermaidRenderer{}
	if err := r.Available(); err != nil {
		t.Skipf("skipping real-renderer test: %v", err)
	}
	return r
}

func TestMermaidRendersRealDiagram(t *testing.T) {
	r := requireMmdc(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "d.mmd")
	if err := os.WriteFile(src, []byte("graph TD\n  A[Start] --> B[End]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "d.svg")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.Render(ctx, src, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Errorf("output = %v, size %d", err, info.Size())
	}
}

func TestMermaidReturnsDiagnosticsForInvalidSource(t *testing.T) {
	r := requireMmdc(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.mmd")
	if err := os.WriteFile(src, []byte("this is not mermaid at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := r.Render(ctx, src, filepath.Join(dir, "bad.svg"))
	if err == nil {
		t.Fatal("invalid source must fail")
	}
	// The point of the error is that the model can act on it: mmdc's own
	// diagnostics must be inside, not just an exit status.
	if !strings.Contains(err.Error(), "mmdc failed") || len(err.Error()) < len("mmdc failed: x") {
		t.Errorf("err = %q", err)
	}
}
