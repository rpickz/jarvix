package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// MermaidRenderer renders Mermaid diagram source to SVG via mermaid-cli
// (`mmdc`) as a short-lived subprocess — the same pattern as the speech
// engines (ADR 0002) and chosen for the same reasons: crash isolation,
// reliable cancellation by kill, and zero cgo. See ADR 0011 for the
// trade-offs against a pure-Go renderer.
type MermaidRenderer struct {
	// Binary is the mermaid-cli executable; empty means "mmdc" on PATH.
	Binary string
}

// MermaidInstallHint is how to get mmdc; doctor shows it when the renderer
// is missing.
const MermaidInstallHint = "npm install -g @mermaid-js/mermaid-cli\n(or from the AUR: mermaid-cli)"

// Format implements Renderer.
func (m *MermaidRenderer) Format() string { return "mermaid" }

// SourceExt implements Renderer.
func (m *MermaidRenderer) SourceExt() string { return ".mmd" }

// OutputExt implements Renderer. SVG: crisp at any zoom, and mmdc's default.
func (m *MermaidRenderer) OutputExt() string { return ".svg" }

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

// Render implements Renderer. mmdc spawns a headless browser to do the actual
// drawing, so the subprocess is put in its own process group and the whole
// group is killed on cancellation — killing only mmdc would leak the browser.
func (m *MermaidRenderer) Render(ctx context.Context, srcPath, outPath string) error {
	cmd := exec.CommandContext(ctx, m.binary(), "-i", srcPath, "-o", outPath, "--quiet")
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

func (m *MermaidRenderer) binary() string {
	if m.Binary != "" {
		return m.Binary
	}
	return "mmdc"
}
