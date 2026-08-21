package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		openFn: func(_ []string, path string) error {
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
	if !strings.Contains(out, "fake rendering unavailable") || !strings.Contains(out, "prose") {
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
		openFn:    func([]string, string) error { return fmt.Errorf("no display") },
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

// fakePassthrough is a validate-and-save format like the real document /
// spreadsheet / excalidraw renderers: same extension both sides, optional
// validation, no render step. renderCalled proves the tool really skips
// Render for passthrough formats.
type fakePassthrough struct {
	passthrough
	validateErr  error
	renderCalled bool
}

func (f *fakePassthrough) Format() string    { return "fakedoc" }
func (f *fakePassthrough) SourceExt() string { return ".txt" }
func (f *fakePassthrough) OutputExt() string { return ".txt" }

func (f *fakePassthrough) ValidateSource(string) error { return f.validateErr }

func (f *fakePassthrough) Render(context.Context, string, string) error {
	f.renderCalled = true
	return nil
}

func runFormat(t *testing.T, a *Artifact, format, title, source string) string {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"format": format, "title": title, "source": source})
	out, err := a.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func TestArtifactPassthroughSavesSourceVerbatimWithoutRendering(t *testing.T) {
	fake := &fakePassthrough{}
	a, opened := newArtifact(t, fake)
	source := "line one\nline two\n"

	out := runFormat(t, a, "fakedoc", "notes", source)

	path := filepath.Join(a.Dir, time.Now().Format("2006-01-02")+"-notes.txt")
	if got, err := os.ReadFile(path); err != nil || string(got) != source {
		t.Errorf("artifact file: %v, %q", err, got)
	}
	if fake.renderCalled {
		t.Error("Render must not be called for a passthrough format")
	}
	if entries, _ := os.ReadDir(a.Dir); len(entries) != 1 {
		t.Errorf("dir entries = %v, want exactly the artifact (no separate source)", entries)
	}
	if len(*opened) != 1 || (*opened)[0] != path {
		t.Errorf("opened = %v, want [%s]", *opened, path)
	}
	if !strings.Contains(out, "open on the user's screen") {
		t.Errorf("result = %q", out)
	}
}

// Validation failures must reach the model as a retry-able result before
// anything is written: an invalid structured artifact must never exist on
// disk, even briefly.
func TestArtifactValidationFailureWritesNothing(t *testing.T) {
	fake := &fakePassthrough{validateErr: fmt.Errorf("record on line 3: wrong number of fields")}
	a, opened := newArtifact(t, fake)
	out := runFormat(t, a, "fakedoc", "bad table", "x")
	if !strings.Contains(out, "invalid fakedoc source") ||
		!strings.Contains(out, "record on line 3") ||
		!strings.Contains(out, "call the tool again") {
		t.Errorf("result = %q", out)
	}
	if entries, _ := os.ReadDir(a.Dir); len(entries) != 0 {
		t.Errorf("validation failure left files behind: %v", entries)
	}
	if len(*opened) != 0 {
		t.Error("nothing must be opened")
	}
}

// Oversized content is refused, never truncated: a cut-off CSV or scene
// JSON is a silently corrupt file.
func TestArtifactOversizedSourceRefusedNotTruncated(t *testing.T) {
	a, opened := newArtifact(t, &fakePassthrough{})
	big := strings.Repeat("x", MaxArtifactSourceBytes+1)
	out := runFormat(t, a, "fakedoc", "huge", big)
	if !strings.Contains(out, "over the") || !strings.Contains(out, "Nothing was saved") {
		t.Errorf("result = %q", out)
	}
	if entries, _ := os.ReadDir(a.Dir); len(entries) != 0 {
		t.Errorf("oversized source left files behind: %v", entries)
	}
	if len(*opened) != 0 {
		t.Error("nothing must be opened")
	}
}

func TestArtifactPerFormatOpenCommandOverride(t *testing.T) {
	var commands [][]string
	a := &Artifact{
		Dir:          filepath.Join(t.TempDir(), "artifacts"),
		OpenCommand:  []string{"xdg-open"},
		OpenCommands: map[string][]string{"fakedoc": {"obsidian"}},
		Renderers:    []Renderer{&fakePassthrough{}, &fakeRenderer{}},
		openFn: func(argv []string, _ string) error {
			commands = append(commands, argv)
			return nil
		},
	}
	runFormat(t, a, "fakedoc", "with override", "x")
	runFormat(t, a, "fake", "without override", "x")
	if len(commands) != 2 || !slices.Equal(commands[0], []string{"obsidian"}) ||
		!slices.Equal(commands[1], []string{"xdg-open"}) {
		t.Errorf("open commands = %v, want [[obsidian] [xdg-open]]", commands)
	}
}

// An override of "" or "none" means the format has no viewer: the artifact
// is still saved and the result names it — by base name only, never a
// directory path, because the name is the user's only handle on the file.
func TestArtifactNoViewerConfiguredSavesAndNamesTheFile(t *testing.T) {
	for _, override := range [][]string{{}, {""}, {"none"}, nil} {
		var opened []string
		a := &Artifact{
			Dir:          filepath.Join(t.TempDir(), "artifacts"),
			OpenCommands: map[string][]string{"fakedoc": override},
			Renderers:    []Renderer{&fakePassthrough{}},
			openFn: func(_ []string, path string) error {
				opened = append(opened, path)
				return nil
			},
		}
		out := runFormat(t, a, "fakedoc", "orphan", "x")
		base := time.Now().Format("2006-01-02") + "-orphan.txt"
		if _, err := os.Stat(filepath.Join(a.Dir, base)); err != nil {
			t.Errorf("override %v: artifact not saved: %v", override, err)
		}
		if len(opened) != 0 {
			t.Errorf("override %v: nothing must be opened, got %v", override, opened)
		}
		if !strings.Contains(out, base) || !strings.Contains(out, "no viewer is configured") {
			t.Errorf("override %v: result = %q, want the base name and an explanation", override, out)
		}
		if strings.Contains(out, a.Dir) {
			t.Errorf("override %v: result leaks the directory: %q", override, out)
		}
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

// One seam, no special cases: every registered format — the three real
// passthrough formats and a renderer invented inside this test — goes
// through the identical path (same directory, same <date>-<slug> naming,
// same artifact.created event, same schema enum) with registration as the
// only per-format act. If adding a format ever needs an engine or daemon
// change, this test is where that regression surfaces.
func TestArtifactFormatsShareOneSeam(t *testing.T) {
	renderers := []Renderer{
		&DocumentRenderer{},
		&SpreadsheetRenderer{},
		&ExcalidrawRenderer{},
		&fakePassthrough{}, // stands in for "the next format someone adds"
	}
	sources := map[string]string{
		"document":    "# Title\n\nBody.\n",
		"spreadsheet": "a,b\n1,2\n",
		"excalidraw":  `{"type":"excalidraw","version":2,"elements":[]}`,
		"fakedoc":     "anything\n",
	}
	var opened []string
	created := make(map[string]string)
	a := &Artifact{
		Dir:       filepath.Join(t.TempDir(), "artifacts"),
		Renderers: renderers,
		OnCreated: func(format, path string) { created[format] = path },
		openFn: func(_ []string, path string) error {
			opened = append(opened, path)
			return nil
		},
	}

	var schema struct {
		Properties struct {
			Format struct {
				Enum []string `json:"enum"`
			} `json:"format"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(a.Schema(), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	for _, r := range renderers {
		format := r.Format()
		found := false
		for _, f := range schema.Properties.Format.Enum {
			found = found || f == format
		}
		if !found {
			t.Errorf("format %q missing from schema enum %v", format, schema.Properties.Format.Enum)
		}

		runFormat(t, a, format, "seam check "+format, sources[format])

		wantPath := filepath.Join(a.Dir,
			time.Now().Format("2006-01-02")+"-seam-check-"+format+r.OutputExt())
		if got, err := os.ReadFile(wantPath); err != nil || string(got) != sources[format] {
			t.Errorf("format %q: artifact at %s: %v, %q", format, wantPath, err, got)
		}
		if created[format] != wantPath {
			t.Errorf("format %q: artifact.created path = %q, want %q", format, created[format], wantPath)
		}
	}
	if len(opened) != len(renderers) {
		t.Errorf("opened %d artifacts, want %d", len(opened), len(renderers))
	}
}

// A stat of the output path that fails for any reason other than "not there"
// says nothing about whether the name is free. Reusing it would let the
// render write straight through whatever holds the path — here a symlink,
// but a permission or IO error reads the same way. Refusing is the only safe
// answer (raised in review of #17).
func TestArtifactUnreadableExistingOutputIsRefusedNotOverwritten(t *testing.T) {
	a, opened := newArtifact(t, &fakeRenderer{})
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A self-referential symlink: os.Stat fails with ELOOP, which is neither
	// nil nor fs.ErrNotExist — the exact ambiguity the fix is about.
	outPath := filepath.Join(a.Dir, time.Now().Format("2006-01-02")+"-collide.out")
	if err := os.Symlink(outPath, outPath); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(map[string]string{"format": "fake", "title": "collide", "source": "x"})
	out, err := a.Execute(context.Background(), input)
	if err == nil {
		t.Fatalf("an unreadable output path must be an error, got result %q", out)
	}
	if !strings.Contains(err.Error(), "collide.out") {
		t.Errorf("error must name the file it refused to touch: %v", err)
	}
	srcPath := filepath.Join(a.Dir, time.Now().Format("2006-01-02")+"-collide.src")
	if _, statErr := os.Stat(srcPath); statErr == nil {
		t.Error("refusing the name must not leave a claimed source file behind")
	}
	if len(*opened) != 0 {
		t.Error("nothing must be opened")
	}
}

// A failed write must not leave the O_EXCL placeholder behind: for a
// passthrough format that file IS the artifact, so `jarvix artifacts` would
// list a truncated or empty file as a finished document
// (raised in review of #19).
func TestArtifactWriteFailureLeavesNoPlaceholder(t *testing.T) {
	a, opened := newArtifact(t, &fakePassthrough{})
	a.writeFn = func(path string, _ []byte, _ os.FileMode) error {
		// Stand in for ENOSPC: the placeholder exists, the content does not.
		return fmt.Errorf("write %s: no space left on device", path)
	}
	input, _ := json.Marshal(map[string]string{"format": "fakedoc", "title": "doomed", "source": "x"})
	if _, err := a.Execute(context.Background(), input); err == nil {
		t.Fatal("a failed write must be an error")
	}
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed write left %v behind; a partial artifact must not be listable", entries)
	}
	if len(*opened) != 0 {
		t.Error("nothing must be opened")
	}
}

// Passthrough formats are saved verbatim; nothing renders them. The tool
// result is the model's only account of what happened and may be spoken, so
// it must not claim a render that never ran (raised in review of #19).
func TestArtifactViewerFailureSaysSavedNotRenderedForPassthrough(t *testing.T) {
	for format, want := range map[string]string{
		"fakedoc": "The artifact was saved, but",
		"fake":    "The artifact was rendered and saved, but",
	} {
		a := &Artifact{
			Dir:       filepath.Join(t.TempDir(), "artifacts"),
			Renderers: []Renderer{&fakePassthrough{}, &fakeRenderer{}},
			openFn:    func([]string, string) error { return fmt.Errorf("no display") },
		}
		out := runFormat(t, a, format, "headless", "x")
		if !strings.Contains(out, want) {
			t.Errorf("format %q: result = %q, want it to start %q", format, out, want)
		}
	}
}

// A viewer under a path with spaces has to reach exec as one argv element;
// the old whitespace-split command string could not express it
// (raised in review of #19).
func TestArtifactOpenCommandArgvIsNotResplit(t *testing.T) {
	var got []string
	a := &Artifact{
		Dir:       filepath.Join(t.TempDir(), "artifacts"),
		Renderers: []Renderer{&fakePassthrough{}},
		OpenCommands: map[string][]string{
			"fakedoc": {"/opt/my viewer/bin/view", "--new window"},
		},
		openFn: func(argv []string, _ string) error { got = argv; return nil },
	}
	runFormat(t, a, "fakedoc", "spacey", "x")
	want := []string{"/opt/my viewer/bin/view", "--new window"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}
