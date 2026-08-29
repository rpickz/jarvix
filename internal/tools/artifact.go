package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/undo"
)

// Renderer turns artifact source of one format into a viewable file. The
// artifact tool is generic — diagrams today, documents and spreadsheets
// later — and everything format-specific lives behind this seam (ADR 0011).
type Renderer interface {
	// Format is the source format the model names in the tool call,
	// e.g. "mermaid".
	Format() string
	// SourceExt and OutputExt are the file extensions (dot included) for
	// the saved source and the rendered output, e.g. ".mmd" and ".png".
	SourceExt() string
	OutputExt() string
	// Available reports whether the renderer can run on this machine; the
	// error names what is missing. Checked per call, not at registration,
	// so installing the dependency works without a daemon restart.
	Available() error
	// Render converts srcPath into outPath. The returned error carries the
	// renderer's own diagnostics (stderr) so the model can fix its source
	// and retry within the tool loop. Not called for passthrough formats
	// (SourceExt == OutputExt), where the saved source is the artifact.
	Render(ctx context.Context, srcPath, outPath string) error
}

// SourceValidator is optionally implemented by renderers of structured
// formats (CSV, scene JSON). Validation runs before anything is written:
// an invalid artifact must never exist on disk even transiently, and the
// specific error goes back to the model for its retry round — the same
// contract render failures already have. Formats without failure modes
// (Markdown) simply do not implement it.
type SourceValidator interface {
	// ValidateSource checks the complete artifact source, returning an
	// error precise enough for the model to fix (line numbers, field
	// names), never a bare "invalid".
	ValidateSource(source string) error
}

// passthrough is the Renderer half shared by formats whose saved source is
// the finished artifact (Markdown documents, CSV spreadsheets, Excalidraw
// scenes): always available, because nothing external runs, and never
// rendered, because SourceExt == OutputExt makes the tool skip the render
// step entirely.
type passthrough struct{}

// Available implements Renderer. Passthrough formats need no external
// binary, so they can never be missing.
func (passthrough) Available() error { return nil }

// Render implements Renderer but is never called: the artifact tool skips
// rendering when source and output share a path.
func (passthrough) Render(context.Context, string, string) error { return nil }

// Artifact is the generic artifact tool: the model hands it source in a
// supported format, and it saves the source, renders it, opens the result in
// the user's default viewer, and tells the model to answer with a short
// spoken summary. Speech is the wrong medium for structure — this tool is how
// "diagram my publish pipeline" ends as a picture on screen instead of a
// paragraph of syntax read aloud.
type Artifact struct {
	// Dir is the directory artifacts land in, created 0700 on first use.
	// The model never controls the directory — only a slugified filename.
	Dir string
	// OpenCommand is the viewer argv (program then arguments) the rendered
	// file is appended to. Empty means DefaultOpenCommand. It is argv rather
	// than a command line because the viewer is exec'd directly, never
	// through a shell: a path or argument containing spaces has to arrive as
	// its own element, which a split string cannot express.
	OpenCommand []string
	// OpenCommands overrides OpenCommand per format (keyed by
	// Renderer.Format()), so documents can open in an editor while
	// spreadsheets open in a spreadsheet app. An entry explicitly set to
	// empty, or to the single word "none", declares the format has no
	// viewer: the artifact is saved and announced by base name, and nothing
	// is launched. Formats without an entry fall back to OpenCommand.
	OpenCommands map[string][]string
	// Timeout bounds one render. Zero means DefaultRenderTimeout.
	Timeout time.Duration
	// Renderers are the enabled per-format renderers.
	Renderers []Renderer
	// OnCreated, when set, is told about each rendered artifact (format,
	// output path). The daemon publishes artifact.created from it so the
	// overlay/notifications can link to the file.
	OnCreated func(format, path string)
	// Log records renders (format and duration, never content). Nil uses
	// slog.Default().
	Log *slog.Logger

	// openFn overrides viewer launch in tests.
	openFn func(argv []string, path string) error
	// writeFn overrides the artifact write in tests, so the failure path
	// below (ENOSPC, short write) can be exercised without filling a disk.
	// Nil uses os.WriteFile.
	writeFn func(path string, data []byte, perm os.FileMode) error
}

// Artifact tool defaults.
const (
	DefaultRenderTimeout = 10 * time.Second
	DefaultOpenCommand   = "xdg-open"
)

// MaxArtifactSourceBytes caps one artifact's source at 1 MB. Oversized
// content is refused outright rather than truncated: a truncated CSV or
// scene JSON is silently corrupt, which is worse than no file at all, and
// 1 MB is far beyond any spoken-request artifact (a 500-row table is tens
// of KB).
const MaxArtifactSourceBytes = 1 << 20

// ArtifactToolName is the registry name of the artifact tool, exported so the
// policy's tiers, the status surfaces and the account can name it without a
// literal.
const ArtifactToolName = "artifact.create"

// Name implements Tool.
func (a *Artifact) Name() string { return ArtifactToolName }

// Description implements Tool.
func (a *Artifact) Description() string {
	return "Create an artifact from source you write and open it on the user's screen. " +
		"Use this whenever the user asks for a diagram or chart of a flow (Mermaid source), " +
		"a drafted document or brief (Markdown), a table of data (CSV), or a free-form sketch " +
		"(an Excalidraw scene): write the complete source and call this tool instead of " +
		"describing the content in speech. The file opens automatically and is saved for " +
		"the user."
}

// Schema implements Tool. The format enum is built from the registered
// renderers so new formats appear to the model without schema edits.
func (a *Artifact) Schema() json.RawMessage {
	formats := make([]string, 0, len(a.Renderers))
	for _, r := range a.Renderers {
		formats = append(formats, fmt.Sprintf("%q", r.Format()))
	}
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"format": {
				"type": "string",
				"enum": [%s],
				"description": "The source format of the artifact"
			},
			"title": {
				"type": "string",
				"description": "A short human title for the artifact, e.g. 'publish pipeline'. Used to name the file. Not a path."
			},
			"source": {
				"type": "string",
				"description": "The complete artifact source, e.g. the Mermaid diagram text"
			}
		},
		"required": ["format", "title", "source"]
	}`, strings.Join(formats, ", ")))
}

// Execute implements Tool. Render failures come back as text, not err: the
// tool loop feeds them to the model, which gets a retry round to fix its
// source. Only malformed input is an error.
func (a *Artifact) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Format string `json:"format"`
		Title  string `json:"title"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid artifact.create arguments: %w", err)
	}
	if strings.TrimSpace(args.Source) == "" {
		return "", fmt.Errorf("artifact.create: empty source")
	}
	// Refuse, never truncate: cutting a structured format mid-record would
	// write a silently corrupt file. The message goes back as a tool result
	// so the model can produce something smaller on its retry round.
	if len(args.Source) > MaxArtifactSourceBytes {
		return fmt.Sprintf("artifact source is %d bytes, over the %d byte (1 MB) limit. "+
			"Nothing was saved, and the source was not truncated because a truncated artifact "+
			"would be corrupt. Produce a smaller artifact or answer in prose.",
			len(args.Source), MaxArtifactSourceBytes), nil
	}
	renderer, err := a.renderer(args.Format)
	if err != nil {
		return "", err
	}
	// The model supplies a title, never a path. Anything path-like is
	// rejected outright rather than sanitised, so a model trying to steer
	// the write location gets an unambiguous refusal.
	if strings.Contains(args.Title, "..") || strings.ContainsAny(args.Title, `/\`) {
		return "", fmt.Errorf("artifact.create: title must be a short name, not a path")
	}

	if err := renderer.Available(); err != nil {
		return fmt.Sprintf("%s rendering unavailable: %v. Answer the user's request in prose instead.",
			renderer.Format(), err), nil
	}

	// Structured formats are checked before anything touches disk, so an
	// invalid artifact never exists even transiently — and the specific
	// error (line numbers, field names) is the model's retry material.
	if v, ok := renderer.(SourceValidator); ok {
		if validateErr := v.ValidateSource(args.Source); validateErr != nil {
			return fmt.Sprintf("invalid %s source: %v\nFix the source and call the tool again.",
				renderer.Format(), validateErr), nil
		}
	}

	logger := a.Log
	if logger == nil {
		logger = slog.Default()
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = DefaultRenderTimeout
	}

	// 0700: artifacts are the user's private work product.
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return "", fmt.Errorf("artifact.create: create artifact dir: %w", err)
	}
	srcPath, outPath, err := a.claimPaths(slugify(args.Title), renderer)
	if err != nil {
		return "", err
	}
	write := a.writeFn
	if write == nil {
		write = os.WriteFile
	}
	if err := write(srcPath, []byte(args.Source), 0o600); err != nil {
		// claimPaths already created srcPath (O_EXCL) to reserve the name, so
		// a failed write leaves a truncated or empty file behind. For a
		// passthrough format that file IS the artifact, and `jarvix artifacts`
		// would list the wreckage as a finished document. Unclaim the name so
		// nothing half-written survives and a retry starts clean.
		_ = os.Remove(srcPath)
		return "", fmt.Errorf("artifact.create: write source: %w", err)
	}

	start := time.Now()
	// srcPath == outPath marks a passthrough format: the validated source
	// file IS the artifact (documents, spreadsheets, scenes), so there is
	// no render step and no second file.
	if srcPath != outPath {
		logger.Info("rendering artifact", "component", "tools", "tool", a.Name(),
			"format", renderer.Format(), "name", filepath.Base(outPath))
		renderCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		renderErr := renderer.Render(renderCtx, srcPath, outPath)
		if renderErr == nil {
			if _, statErr := os.Stat(outPath); statErr != nil {
				renderErr = fmt.Errorf("renderer reported success but produced no output")
			}
		}
		if renderErr != nil {
			// Leave no half-made files behind; a retry claims fresh names.
			_ = os.Remove(srcPath)
			_ = os.Remove(outPath)
			if errors.Is(renderCtx.Err(), context.DeadlineExceeded) {
				return fmt.Sprintf("rendering timed out after %s. Simplify the source and try once more, or answer in prose.", timeout), nil
			}
			return fmt.Sprintf("rendering failed:\n%v\nFix the source and call the tool again.", renderErr), nil
		}
	}
	logger.Info("artifact created", "component", "tools", "tool", a.Name(),
		"format", renderer.Format(), "name", filepath.Base(outPath),
		"duration_ms", time.Since(start).Milliseconds())

	if a.OnCreated != nil {
		a.OnCreated(renderer.Format(), outPath)
	}

	// The account (#201, ADR 0064). Creating a file is the file restore's
	// other half: "what would put this back" is that the file was not there,
	// so undoing it removes what was made — guarded by the same digest as
	// every other file reversal, so an artifact the user has since edited is
	// refused rather than deleted out from under them.
	//
	// Only outPath is recorded, deliberately, even though a rendered format
	// also leaves a source file beside it. The account is of things the user
	// would miss, and the rendered document is the artifact; a row per
	// intermediate would make the review pane read like a build log.
	undo.Note(ctx, undo.Action{
		Tool:    ArtifactToolName,
		Summary: fmt.Sprintf("made the %s artifact %q", renderer.Format(), filepath.Base(outPath)),
		Target:  outPath,
		Restore: undo.Restore{Kind: undo.KindFile, File: &undo.FileRestore{
			Path: outPath, Existed: false, AfterDigest: undo.DigestOf(outPath),
		}},
		Provenance: []string{provenance.KindArtifact + ":" + outPath},
	})

	// What went into the answer (issue #168). The path is reported to the
	// turn, not to the model: this is the one thing about the call the
	// arguments cannot say — the title the model asked for is not the file
	// that was written, because claimPaths de-duplicates it — and it is what
	// "open the artifact" needs. It travels on the context sink, so a call
	// made outside a turn (the CLI, a test) reports to nobody.
	provenance.Note(ctx, provenance.Reference{
		Kind: provenance.KindArtifact, Ref: outPath, Subject: renderer.Format(),
	})

	// The result deliberately contains no path and no source: whatever is in
	// it may end up spoken aloud, and paths read verbatim are the failure
	// mode this tool exists to avoid. The artifact.created event carries the
	// path for machines; `jarvix artifacts` shows it to humans. The one
	// deliberate exception is the no-viewer case below, which names the file
	// by base name only — with nothing opening on screen, the name is the
	// user's only handle on their new file.
	// What actually happened to the file, in the words the model may repeat
	// aloud. Passthrough formats are saved verbatim and never rendered, so
	// claiming a render would have Jarvix describe a step that did not run
	// (raised in review of #19).
	produced := "saved"
	if srcPath != outPath {
		produced = "rendered and saved"
	}

	argv, hasViewer := a.openCommandFor(renderer.Format())
	if !hasViewer {
		return fmt.Sprintf("The artifact was saved as %q in the user's Jarvix artifacts folder, "+
			"and no viewer is configured for %s artifacts, so it was not opened. Tell the user it "+
			"is saved under that name, in a summary of at most two sentences. Do not recite the "+
			"artifact source or any directory paths.", filepath.Base(outPath), renderer.Format()), nil
	}
	if err := a.open(argv, outPath); err != nil {
		logger.Warn("artifact viewer failed to open", "component", "tools",
			"tool", a.Name(), "error", err.Error())
		return fmt.Sprintf("The artifact was %s, but the viewer could not be opened automatically. "+
			"Tell the user it is in their Jarvix artifacts folder (the `jarvix artifacts` command lists it), "+
			"in a summary of at most two sentences. Do not recite the artifact source, file names, or paths.",
			produced), nil
	}
	return "The artifact is now open on the user's screen and saved in their Jarvix artifacts " +
		"folder. Answer with a summary of at most two sentences describing what it shows. " +
		"Do not recite the artifact source, file names, or paths.", nil
}

// renderer resolves the renderer for a model-named format.
func (a *Artifact) renderer(format string) (Renderer, error) {
	names := make([]string, 0, len(a.Renderers))
	for _, r := range a.Renderers {
		if r.Format() == format {
			return r, nil
		}
		names = append(names, r.Format())
	}
	return nil, fmt.Errorf("artifact.create: unknown format %q; available: %s",
		format, strings.Join(names, ", "))
}

// claimPaths picks a unique <date>-<slug> base name, creating the source file
// with O_EXCL to claim it atomically — two artifacts in quick succession must
// never overwrite each other.
func (a *Artifact) claimPaths(slug string, r Renderer) (srcPath, outPath string, err error) {
	base := time.Now().Format("2006-01-02") + "-" + slug
	for i := 1; i <= 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		srcPath = filepath.Join(a.Dir, name+r.SourceExt())
		outPath = filepath.Join(a.Dir, name+r.OutputExt())
		if _, statErr := os.Stat(outPath); statErr == nil {
			continue // stale output from an earlier run holds the name
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			// Anything other than "it is not there" — a permission error, an
			// IO error, a symlink loop — tells us nothing about whether the
			// name is free. Treating it as free is the dangerous reading: the
			// O_EXCL claim on the *source* can still succeed (different
			// extension), and the render would then write straight through
			// whatever holds outPath. Refuse instead of overwriting
			// (raised in review of #17).
			return "", "", fmt.Errorf("artifact.create: check existing artifact %q: %w",
				filepath.Base(outPath), statErr)
		}
		f, openErr := os.OpenFile(srcPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(openErr, os.ErrExist) {
			continue
		}
		if openErr != nil {
			return "", "", fmt.Errorf("artifact.create: claim filename: %w", openErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return "", "", fmt.Errorf("artifact.create: claim filename: %w", closeErr)
		}
		return srcPath, outPath, nil
	}
	return "", "", fmt.Errorf("artifact.create: could not find a free filename for %q", base)
}

// openCommandFor resolves the viewer argv for a format: a per-format override
// wins, the shared OpenCommand is the fallback, and an override that is empty
// or the single word "none" means the format has no viewer at all — the
// caller announces the saved file instead of launching anything.
func (a *Artifact) openCommandFor(format string) (argv []string, hasViewer bool) {
	return ViewerFor(a.OpenCommand, a.OpenCommands, format)
}

// ViewerFor is openCommandFor's body as a package-level function, so anything
// that has to reopen an artifact later resolves the *same* viewer this tool
// used when it created one — the provenance panel's "open the file" among
// them (issue #168). Shared rather than reimplemented, because two answers to
// "which viewer opens a mermaid diagram" is one answer too many.
func ViewerFor(shared []string, perFormat map[string][]string, format string) (argv []string, hasViewer bool) {
	if override, ok := perFormat[format]; ok {
		if noViewer(override) {
			return nil, false
		}
		return override, true
	}
	if len(shared) > 0 && !noViewer(shared) {
		return shared, true
	}
	return []string{DefaultOpenCommand}, true
}

// noViewer reports whether an argv declares "this format has no viewer":
// nothing at all, or the single word "none" (the spelling the docs use, kept
// because it reads better in a config file than an empty array).
func noViewer(argv []string) bool {
	if len(argv) == 0 {
		return true
	}
	return len(argv) == 1 && (strings.TrimSpace(argv[0]) == "" || strings.TrimSpace(argv[0]) == "none")
}

// open launches the viewer and does not wait for it: xdg-open hands off and
// exits, but a direct viewer (eog, imv) would block until closed — and it
// must outlive the session, so it runs outside the session context. argv is
// used verbatim, never re-split, so a viewer living under a path with spaces
// launches correctly.
func (a *Artifact) open(argv []string, path string) error {
	if a.openFn != nil {
		return a.openFn(argv, path)
	}
	return Launch(argv, path)
}

// Launch runs argv with one trailing argument and does not wait for it —
// open's body, shared for the same reason ViewerFor is. argv is used
// verbatim, never re-split, so a viewer living under a path with spaces
// launches correctly, and the target is always a separate argument, so
// nothing in it can become a flag or a second word.
func Launch(argv []string, target string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command to run")
	}
	args := make([]string, 0, len(argv))
	args = append(args, argv[1:]...)
	cmd := exec.Command(argv[0], append(args, target)...) //nolint:gosec // argv comes from validated config or a fixed literal
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap; the viewer's lifetime is the user's business
	return nil
}

// slugify reduces a human title to a safe filename fragment: lowercase ASCII
// letters and digits, runs of anything else collapsed to single dashes. The
// output can never contain a path separator or dot, so a filename built from
// it stays inside the artifact directory by construction.
func slugify(title string) string {
	var b strings.Builder
	dash := true // suppress a leading dash
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "artifact"
	}
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	return slug
}
