package tools

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
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

// The argv is the contract with mmdc, pinned as a golden table: PNG must ask
// for 2× scale (a 1× raster goes blurry the first time the user zooms) and
// SVG must carry the htmlLabels config (without it the SVG has no <text> any
// image viewer can see — the #56 failure). The output extension is part of
// the same contract because it is how mmdc chooses the format.
func TestMermaidRenderArgvGolden(t *testing.T) {
	cases := []struct {
		name         string
		outputFormat string
		wantExt      string
		want         []string
	}{
		{
			name:         "png is the default (zero value)",
			outputFormat: "",
			wantExt:      ".png",
			want:         []string{"-i", "in.mmd", "-o", "out.png", "-s", "2", "--quiet"},
		},
		{
			name:         "png named explicitly",
			outputFormat: "png",
			wantExt:      ".png",
			want:         []string{"-i", "in.mmd", "-o", "out.png", "-s", "2", "--quiet"},
		},
		{
			name:         "svg opt-in renders under the htmlLabels config",
			outputFormat: "svg",
			wantExt:      ".svg",
			want:         []string{"-i", "in.mmd", "-o", "out.svg", "-c", "cfg.json", "--quiet"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &MermaidRenderer{OutputFormat: tc.outputFormat}
			if got := r.OutputExt(); got != tc.wantExt {
				t.Errorf("OutputExt() = %q, want %q", got, tc.wantExt)
			}
			got := r.renderArgs("in.mmd", "out"+tc.wantExt, "cfg.json")
			if !slices.Equal(got, tc.want) {
				t.Errorf("renderArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeMmdc writes a shell script that stands in for mmdc: it records its
// argv, copies any -c config file (Render deletes the real one before the
// test can look), and creates the -o output so Render's callers see the file
// a real render would leave. This keeps the full Render path — temp config
// included — testable without Node or a browser.
func fakeMmdc(t *testing.T, dir string) (binary, argvFile, configCopy string) {
	t.Helper()
	binary = filepath.Join(dir, "fake-mmdc")
	argvFile = filepath.Join(dir, "argv")
	configCopy = filepath.Join(dir, "config-copy")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
while [ $# -gt 0 ]; do
  case "$1" in
    -c) cp -- "$2" %q ;;
    -o) : > "$2" ;;
  esac
  shift
done
`, argvFile, configCopy)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, argvFile, configCopy
}

// The PNG path end to end, hermetically: Render must hand mmdc the golden
// argv and no config file — the raster needs none.
func TestMermaidRenderInvokesMmdcForPNG(t *testing.T) {
	dir := t.TempDir()
	binary, argvFile, _ := fakeMmdc(t, dir)
	r := &MermaidRenderer{Binary: binary}
	src := filepath.Join(dir, "d.mmd")
	out := filepath.Join(dir, "d.png")
	if err := r.Render(context.Background(), src, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-i", src, "-o", out, "-s", "2", "--quiet"}
	if got := strings.Split(strings.TrimSpace(string(argv)), "\n"); !slices.Equal(got, want) {
		t.Errorf("mmdc argv = %q, want %q", got, want)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output missing: %v", err)
	}
}

// The SVG opt-in end to end, hermetically: the config file mmdc receives
// must actually contain htmlLabels:false at the moment mmdc runs — an argv
// pointing at an empty or missing file would render foreignObject-only SVG
// while every argv assertion still passed.
func TestMermaidRenderPassesHTMLLabelsConfigForSVG(t *testing.T) {
	dir := t.TempDir()
	binary, argvFile, configCopy := fakeMmdc(t, dir)
	r := &MermaidRenderer{Binary: binary, OutputFormat: "svg"}
	src := filepath.Join(dir, "d.mmd")
	out := filepath.Join(dir, "d.svg")
	if err := r.Render(context.Background(), src, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(got) != 7 || got[0] != "-i" || got[1] != src || got[2] != "-o" || got[3] != out ||
		got[4] != "-c" || got[6] != "--quiet" {
		t.Fatalf("mmdc argv = %q, want -i %s -o %s -c <config> --quiet", got, src, out)
	}
	cfg, err := os.ReadFile(configCopy)
	if err != nil {
		t.Fatalf("mmdc saw no config file: %v", err)
	}
	if !strings.Contains(string(cfg), `"htmlLabels": false`) {
		t.Errorf("config = %q, want htmlLabels: false", cfg)
	}
	if _, err := os.Stat(got[5]); err == nil {
		t.Errorf("temp config %q survived the render; it must be cleaned up", got[5])
	}
}

// The real render, verified the way #56 was diagnosed: decode the output,
// never trust exit 0. mmdc exits 0 while producing an artifact no image
// viewer can show (the foreignObject-only SVG); a PNG that parses with
// non-trivial dimensions is evidence the browser actually drew something.
func TestMermaidRendersRealPNG(t *testing.T) {
	r := requireMmdc(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "d.mmd")
	if err := os.WriteFile(src, []byte("graph TD\n  A[Start] --> B[End]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "d.png")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.Render(ctx, src, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	img, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatalf("output does not decode as PNG: %v", err)
	}
	// A two-node graph at 2× is comfortably past 50px each way; a header
	// that parses but frames a sliver would be the exit-0 lie again.
	if img.Width < 50 || img.Height < 50 {
		t.Errorf("PNG is %dx%d, too small to be a rendered diagram", img.Width, img.Height)
	}
}

// The SVG opt-in against the real mmdc: the htmlLabels config must yield
// actual <text> elements. Without it this source renders zero (#56), so the
// presence of any is proof the config reached mermaid.
func TestMermaidRealSVGCarriesTextElements(t *testing.T) {
	r := requireMmdc(t)
	r.OutputFormat = "svg"
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
	svg, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(svg), "<text") {
		t.Error("SVG has no <text> elements; the htmlLabels config did not take effect")
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
	err := r.Render(ctx, src, filepath.Join(dir, "bad.png"))
	if err == nil {
		t.Fatal("invalid source must fail")
	}
	// The point of the error is that the model can act on it: mmdc's own
	// diagnostics must be inside, not just an exit status.
	if !strings.Contains(err.Error(), "mmdc failed") || len(err.Error()) < len("mmdc failed: x") {
		t.Errorf("err = %q", err)
	}
}
