package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Renderer turns artifact source of one format into a viewable file. The
// artifact tool is generic — diagrams today, documents and spreadsheets
// later — and everything format-specific lives behind this seam (ADR 0011).
type Renderer interface {
	// Format is the source format the model names in the tool call,
	// e.g. "mermaid".
	Format() string
	// SourceExt and OutputExt are the file extensions (dot included) for
	// the saved source and the rendered output, e.g. ".mmd" and ".svg".
	SourceExt() string
	OutputExt() string
	// Available reports whether the renderer can run on this machine; the
	// error names what is missing. Checked per call, not at registration,
	// so installing the dependency works without a daemon restart.
	Available() error
	// Render converts srcPath into outPath. The returned error carries the
	// renderer's own diagnostics (stderr) so the model can fix its source
	// and retry within the tool loop.
	Render(ctx context.Context, srcPath, outPath string) error
}

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
	// OpenCommand launches the rendered file in a viewer. Empty means
	// DefaultOpenCommand.
	OpenCommand string
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
	openFn func(path string) error
}

// Artifact tool defaults.
const (
	DefaultRenderTimeout = 10 * time.Second
	DefaultOpenCommand   = "xdg-open"
)

// Name implements Tool.
func (a *Artifact) Name() string { return "artifact.create" }

// Description implements Tool.
func (a *Artifact) Description() string {
	return "Create a visual artifact from source you write and open it on the user's screen. " +
		"Use this whenever the user asks for a diagram, chart of a flow, or a sketch of how " +
		"something works or connects: write the source (for example Mermaid for diagrams) and " +
		"call this tool instead of describing the structure in speech. The rendered file opens " +
		"automatically and is saved for the user."
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
		return fmt.Sprintf("diagram rendering unavailable: %v. Answer the user's request in prose instead.", err), nil
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
	if err := os.WriteFile(srcPath, []byte(args.Source), 0o600); err != nil {
		return "", fmt.Errorf("artifact.create: write source: %w", err)
	}

	logger.Info("rendering artifact", "component", "tools", "tool", a.Name(),
		"format", renderer.Format(), "name", filepath.Base(outPath))
	start := time.Now()
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
	logger.Info("artifact rendered", "component", "tools", "tool", a.Name(),
		"format", renderer.Format(), "name", filepath.Base(outPath),
		"duration_ms", time.Since(start).Milliseconds())

	if a.OnCreated != nil {
		a.OnCreated(renderer.Format(), outPath)
	}

	// The result deliberately contains no path and no source: whatever is in
	// it may end up spoken aloud, and paths read verbatim are the failure
	// mode this tool exists to avoid. The artifact.created event carries the
	// path for machines; `jarvix artifacts` shows it to humans.
	if err := a.open(outPath); err != nil {
		logger.Warn("artifact viewer failed to open", "component", "tools",
			"tool", a.Name(), "error", err.Error())
		return "The artifact was rendered and saved, but the viewer could not be opened automatically. " +
			"Tell the user it is in their Jarvix artifacts folder (the `jarvix artifacts` command lists it), " +
			"in a summary of at most two sentences. Do not recite the artifact source, file names, or paths.", nil
	}
	return "The rendered artifact is now open on the user's screen, and the source is saved in " +
		"their Jarvix artifacts folder. Answer with a summary of at most two sentences describing " +
		"what it shows. Do not recite the artifact source, file names, or paths.", nil
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

// open launches the viewer and does not wait for it: xdg-open hands off and
// exits, but a direct viewer (eog, imv) would block until closed — and it
// must outlive the session, so it runs outside the session context.
func (a *Artifact) open(path string) error {
	if a.openFn != nil {
		return a.openFn(path)
	}
	command := a.OpenCommand
	if command == "" {
		command = DefaultOpenCommand
	}
	parts := strings.Fields(command)
	cmd := exec.Command(parts[0], append(parts[1:], path)...) //nolint:gosec // command comes from validated config, path from claimPaths
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
