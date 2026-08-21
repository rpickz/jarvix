package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goldenArtifact runs the tool with the given renderer and source and
// returns the bytes it wrote, so golden tests can assert verbatim saves.
func goldenArtifact(t *testing.T, r Renderer, title, source string) []byte {
	t.Helper()
	a, _ := newArtifact(t, r)
	runFormat(t, a, r.Format(), title, source)
	path := filepath.Join(a.Dir, time.Now().Format("2006-01-02")+"-"+slugify(title)+r.OutputExt())
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	return got
}

// A document is a draft the user keeps editing, so it must land exactly as
// the model wrote it: YAML front matter, spacing, indentation — the golden
// file must come back byte-for-byte, not "equivalent Markdown".
func TestDocumentGoldenFrontMatterUntouched(t *testing.T) {
	golden, err := os.ReadFile("testdata/document_frontmatter.md")
	if err != nil {
		t.Fatal(err)
	}
	got := goldenArtifact(t, &DocumentRenderer{}, "quarterly brief", string(golden))
	if !bytes.Equal(got, golden) {
		t.Errorf("document altered on save:\ngot:  %q\nwant: %q", got, golden)
	}
}

func TestDocumentRendererShape(t *testing.T) {
	r := &DocumentRenderer{}
	if r.Format() != "document" || r.SourceExt() != ".md" || r.OutputExt() != ".md" {
		t.Errorf("shape = %s %s %s", r.Format(), r.SourceExt(), r.OutputExt())
	}
	if err := r.Available(); err != nil {
		t.Errorf("Available: %v (a passthrough has no dependencies)", err)
	}
	if _, ok := any(r).(SourceValidator); ok {
		t.Error("document must not validate: Markdown has no invalid form, and any check risks rejecting a legitimate draft")
	}
}
