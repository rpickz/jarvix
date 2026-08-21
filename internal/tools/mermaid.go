package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// MermaidRenderer renders Mermaid diagram source via mermaid-cli (`mmdc`) as
// a short-lived subprocess — the same pattern as the speech engines (ADR
// 0002) and chosen for the same reasons: crash isolation, reliable
// cancellation by kill, and zero cgo. See ADR 0012 for the trade-offs
// against a pure-Go renderer, and its addendum for why the opened artifact
// is a PNG.
type MermaidRenderer struct {
	// Binary is the mermaid-cli executable; empty means "mmdc" on PATH.
	Binary string
	// OutputFormat is "png" or "svg"; empty means PNG. PNG is the default
	// because mmdc's SVG output puts every label in HTML inside
	// <foreignObject>, which only a browser engine renders — an image
	// viewer opens the file as boxes with no text at all (#56). A PNG is
	// the pixels the embedded browser already drew, so the diagram that
	// opens shows its text in whatever viewer the desktop picks.
	OutputFormat string
}

// MermaidInstallHint is how to get mmdc; doctor shows it when the renderer
// is missing.
const MermaidInstallHint = "npm install -g @mermaid-js/mermaid-cli\n(or from the AUR: mermaid-cli)"

// mermaidPNGScale is the raster scale factor (mmdc -s). 2× keeps the PNG
// crisp on hidpi screens and under the casual zoom a diagram invites; the
// cost is only file size, and diagrams are small.
const mermaidPNGScale = "2"

// mermaidSVGConfig is the mermaid config the SVG path renders under.
// htmlLabels:false makes mermaid emit real SVG <text> elements for labels
// instead of HTML wrapped in <foreignObject>, so the file keeps its words
// in renderers that are not browser engines. The key is set at the top
// level and per diagram type because the per-type keys are what current
// mermaid reads while the top-level key covers older releases. This is a
// mitigation, not a cure: some shapes still emit foreignObject regardless,
// which is exactly why SVG is the opt-in and PNG the default (#56).
const mermaidSVGConfig = `{"htmlLabels": false, "flowchart": {"htmlLabels": false}, "class": {"htmlLabels": false}}`

// Format implements Renderer.
func (m *MermaidRenderer) Format() string { return "mermaid" }

// SourceExt implements Renderer.
func (m *MermaidRenderer) SourceExt() string { return ".mmd" }

// OutputExt implements Renderer. The extension is also how mmdc picks its
// output format, so this and renderArgs must agree — both key off svg().
func (m *MermaidRenderer) OutputExt() string {
	if m.svg() {
		return ".svg"
	}
	return ".png"
}

// Available implements Renderer.
func (m *MermaidRenderer) Available() error {
	if _, err := exec.LookPath(m.binary()); err != nil {
		return fmt.Errorf("%s (mermaid-cli) is not installed", m.binary())
	}
	return nil
}

// mermaidStderrCap bounds how much renderer output is fed back to the model:
// enough to diagnose a syntax error, not a transcript of a browser crash.
const mermaidStderrCap = 4 * 1024

// renderArgs is the argv handed to mmdc (after the binary), factored out so
// tests can pin it as a golden table without launching a browser. configPath
// is only consumed on the SVG path; PNG needs no config because the raster
// carries its text as pixels, not markup.
func (m *MermaidRenderer) renderArgs(srcPath, outPath, configPath string) []string {
	args := []string{"-i", srcPath, "-o", outPath}
	if m.svg() {
		args = append(args, "-c", configPath)
	} else {
		args = append(args, "-s", mermaidPNGScale)
	}
	return append(args, "--quiet")
}

// Render implements Renderer. mmdc spawns a headless browser to do the actual
// drawing, so the subprocess is put in its own process group and the whole
// group is killed on cancellation — killing only mmdc would leak the browser.
func (m *MermaidRenderer) Render(ctx context.Context, srcPath, outPath string) error {
	var configPath string
	if m.svg() {
		// The htmlLabels config travels as a real file because that is the
		// only way mmdc accepts one (-c). It lives in the system temp dir,
		// never the artifact directory, so `jarvix artifacts` cannot list
		// renderer plumbing as if the user made it.
		f, err := os.CreateTemp("", "jarvix-mermaid-*.json")
		if err != nil {
			return fmt.Errorf("mmdc config: %w", err)
		}
		configPath = f.Name()
		defer func() { _ = os.Remove(configPath) }()
		if _, err := f.WriteString(mermaidSVGConfig); err != nil {
			_ = f.Close()
			return fmt.Errorf("mmdc config: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("mmdc config: %w", err)
		}
	}

	cmd := exec.CommandContext(ctx, m.binary(), m.renderArgs(srcPath, outPath, configPath)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// If the group somehow survives the kill, give up waiting rather than
	// wedging the tool call forever.
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > mermaidStderrCap {
			msg = msg[:mermaidStderrCap] + "\n[truncated]"
		}
		if msg == "" {
			return fmt.Errorf("mmdc failed: %w", err)
		}
		return fmt.Errorf("mmdc failed: %s", msg)
	}
	return nil
}

// svg reports whether the SVG opt-in is active. Anything else — including
// the zero value, which doctor and tests construct — means the PNG default:
// the renderer must never silently produce a third thing because a config
// value was misspelled (config.Validate rejects those before it gets here).
func (m *MermaidRenderer) svg() bool { return m.OutputFormat == "svg" }

func (m *MermaidRenderer) binary() string {
	if m.Binary != "" {
		return m.Binary
	}
	return "mmdc"
}
