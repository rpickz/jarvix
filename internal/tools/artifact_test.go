package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRenderer stands in for mermaid-cli so the tool tests stay hermetic:
// they assert file layout, naming, and error paths without a browser render.
type fakeRenderer struct {
	availableErr error
	renderErr    error
	// block makes Render wait for context cancellation, simulating a hung
	// renderer without any sleeping in the test.
	block    bool
	rendered []string
}

func (f *fakeRenderer) Format() string    { return "fake" }
func (f *fakeRenderer) SourceExt() string { return ".src" }
func (f *fakeRenderer) OutputExt() string { return ".out" }
func (f *fakeRenderer) Available() error  { return f.availableErr }

func (f *fakeRenderer) Render(ctx context.Context, srcPath, outPath string) error {
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.renderErr != nil {
		return f.renderErr
	}
	f.rendered = append(f.rendered, outPath)
	return os.WriteFile(outPath, []byte("rendered"), 0o600)
}

// newArtifact builds a tool over a temp dir with the viewer stubbed out,
// returning the captured open calls.
func newArtifact(t *testing.T, r Renderer) (*Artifact, *[]string) {
	t.Helper()
	var opened []string
	a := &Artifact{
		Dir:       filepath.Join(t.TempDir(), "artifacts"),
		Renderers: []Renderer{r},
		openFn: func(path string) error {
			opened = append(opened, path)
			return nil
		},
	}
	return a, &opened
}

func runArtifact(t *testing.T, a *Artifact, title, source string) string {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"format": "fake", "title": title, "source": source})
	out, err := a.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func TestArtifactCreatesRendersAndOpens(t *testing.T) {
	fake := &fakeRenderer{}
	a, opened := newArtifact(t, fake)
	created := make(map[string]string)
	a.OnCreated = func(format, path string) { created[format] = path }

	out := runArtifact(t, a, "My Publish Pipeline!", "graph TD\nA-->B")

	base := time.Now().Format("2006-01-02") + "-my-publish-pipeline"
	srcPath := filepath.Join(a.Dir, base+".src")
	outPath := filepath.Join(a.Dir, base+".out")
	if src, err := os.ReadFile(srcPath); err != nil || string(src) != "graph TD\nA-->B" {
		t.Errorf("source file: %v, %q", err, src)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("output file: %v", err)
	}
	info, err := os.Stat(a.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("artifact dir mode = %v, err = %v, want 0700", info.Mode(), err)
	}
	if len(*opened) != 1 || (*opened)[0] != outPath {
		t.Errorf("opened = %v, want [%s]", *opened, outPath)
	}
	if created["fake"] != outPath {
		t.Errorf("OnCreated = %v", created)
	}
	if !strings.Contains(out, "open on the user's screen") || !strings.Contains(out, "two sentences") {
		t.Errorf("result = %q", out)
	}
}

// The tool result may be echoed into the spoken answer, so it must never
// contain the artifact path, directory, or source (the AC's no-leak rule).
func TestArtifactResultLeaksNoPathOrSource(t *testing.T) {
	a, _ := newArtifact(t, &fakeRenderer{})
	source := "graph TD\nSECRETNODE-->B"
	out := runArtifact(t, a, "leak check", source)
	for _, forbidden := range []string{a.Dir, "leak-check", ".out", ".src", "SECRETNODE"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("result leaks %q: %q", forbidden, out)
		}
	}
}

func TestArtifactDistinctFilenamesOnCollision(t *testing.T) {
	fake := &fakeRenderer{}
	a, _ := newArtifact(t, fake)
	runArtifact(t, a, "same title", "one")
	runArtifact(t, a, "same title", "two")
	if len(fake.rendered) != 2 || fake.rendered[0] == fake.rendered[1] {
		t.Fatalf("rendered = %v, want two distinct paths", fake.rendered)
	}
	entries, err := os.ReadDir(a.Dir)
	if err != nil || len(entries) != 4 {
		t.Errorf("dir entries = %d (%v), want 4 (2 sources + 2 outputs)", len(entries), err)
	}
}

func TestArtifactRejectsPathTraversal(t *testing.T) {
	for _, title := range []string{"../evil", "/etc/passwd", `..\evil`, "a/b", "sneaky/../../x"} {
		a, opened := newArtifact(t, &fakeRenderer{})
		input, _ := json.Marshal(map[string]string{"format": "fake", "title": title, "source": "x"})
		if _, err := a.Execute(context.Background(), input); err == nil {
			t.Errorf("title %q must be rejected", title)
		}
		if entries, _ := os.ReadDir(a.Dir); len(entries) != 0 {
			t.Errorf("title %q left files behind: %v", title, entries)
		}
		if len(*opened) != 0 {
			t.Errorf("title %q opened a viewer", title)
		}
	}
}

func TestArtifactMissingRendererDegradesToProse(t *testing.T) {
	a, opened := newArtifact(t, &fakeRenderer{availableErr: fmt.Errorf("mmdc is not installed")})
	out := runArtifact(t, a, "diagram", "graph TD")
	if !strings.Contains(out, "diagram rendering unavailable") || !strings.Contains(out, "prose") {
		t.Errorf("result = %q", out)
	}
	if entries, _ := os.ReadDir(a.Dir); len(entries) != 0 {
		t.Errorf("no files must be written, got %v", entries)
	}
	if len(*opened) != 0 {
		t.Error("nothing must be opened")
	}
}

func TestArtifactRenderFailureReturnsRendererError(t *testing.T) {
	a, opened := newArtifact(t, &fakeRenderer{renderErr: fmt.Errorf("parse error on line 2: unknown shape")})
	out := runArtifact(t, a, "bad diagram", "graph TD\nnope")
	if !strings.Contains(out, "rendering failed") || !strings.Contains(out, "parse error on line 2") {
		t.Errorf("result = %q", out)
	}
	if !strings.Contains(out, "call the tool again") {
		t.Errorf("result must invite a retry: %q", out)
	}
	// Failed attempts leave no half-made files behind.
	if entries, _ := os.ReadDir(a.Dir); len(entries) != 0 {
		t.Errorf("dir not cleaned up: %v", entries)
	}
	if len(*opened) != 0 {
		t.Error("nothing must be opened on failure")
	}
}

func TestArtifactRenderTimeout(t *testing.T) {
	a, opened := newArtifact(t, &fakeRenderer{block: true})
	a.Timeout = 50 * time.Millisecond
	start := time.Now()
	out := runArtifact(t, a, "slow", "graph TD")
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("result = %q", out)
	}
	if len(*opened) != 0 {
		t.Error("nothing must be opened on timeout")
	}
}

func TestArtifactSessionCancellationStopsRender(t *testing.T) {
	a, _ := newArtifact(t, &fakeRenderer{block: true})
	ctx, cancel := context.WithCancel(context.Background())
	go cancel() // cancel races the render start; block waits on ctx either way
	input, _ := json.Marshal(map[string]string{"format": "fake", "title": "t", "source": "x"})
	start := time.Now()
	_, _ = a.Execute(ctx, input)
	if time.Since(start) > 3*time.Second {
		t.Fatal("session cancellation did not stop the render")
	}
}

func TestArtifactRejectsBadInput(t *testing.T) {
	a, _ := newArtifact(t, &fakeRenderer{})
	for name, input := range map[string]string{
		"malformed json": `not json`,
		"empty source":   `{"format":"fake","title":"t","source":"  "}`,
		"unknown format": `{"format":"png","title":"t","source":"x"}`,
	} {
		if _, err := a.Execute(context.Background(), json.RawMessage(input)); err == nil {
			t.Errorf("%s must error", name)
		}
	}
}

func TestArtifactViewerFailureStillSaves(t *testing.T) {
	fake := &fakeRenderer{}
	a := &Artifact{
		Dir:       filepath.Join(t.TempDir(), "artifacts"),
		Renderers: []Renderer{fake},
		openFn:    func(string) error { return fmt.Errorf("no display") },
	}
	out := runArtifact(t, a, "headless", "graph TD")
	if !strings.Contains(out, "could not be opened") || !strings.Contains(out, "jarvix artifacts") {
		t.Errorf("result = %q", out)
	}
	if len(fake.rendered) != 1 {
		t.Errorf("rendered = %v, artifact must still be saved", fake.rendered)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"My Publish Pipeline!":  "my-publish-pipeline",
		"  spaces   galore  ":   "spaces-galore",
		"CAPS&symbols#2026":     "caps-symbols-2026",
		"___":                   "artifact",
		"":                      "artifact",
		"already-fine-123":      "already-fine-123",
		"unicode → arrows … ok": "unicode-arrows-ok",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	if got := slugify(strings.Repeat("long", 40)); len(got) > 60 {
		t.Errorf("slugify long title = %d chars, want <= 60", len(got))
	}
}

func TestArtifactSchemaListsRendererFormats(t *testing.T) {
	a, _ := newArtifact(t, &fakeRenderer{})
	var schema struct {
		Properties struct {
			Format struct {
				Enum []string `json:"enum"`
			} `json:"format"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(a.Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if len(schema.Properties.Format.Enum) != 1 || schema.Properties.Format.Enum[0] != "fake" {
		t.Errorf("format enum = %v", schema.Properties.Format.Enum)
	}
}
