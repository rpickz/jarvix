package whispercpp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/stt"
)

func TestResolveModelPath(t *testing.T) {
	dir := "/data/models"
	cases := map[string]string{
		"base.en":              filepath.Join(dir, "ggml-base.en.bin"),
		"large-v3-turbo":       filepath.Join(dir, "ggml-large-v3-turbo.bin"),
		"custom-name":          filepath.Join(dir, "ggml-custom-name.bin"),
		"/abs/path/ggml-x.bin": "/abs/path/ggml-x.bin",
	}
	for in, want := range cases {
		if got := ResolveModelPath(in, dir); got != want {
			t.Errorf("ResolveModelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelURL(t *testing.T) {
	url, ok := ModelURL("base.en")
	if !ok || !strings.HasSuffix(url, "ggml-base.en.bin") {
		t.Errorf("url = %q ok = %v", url, ok)
	}
	if _, ok := ModelURL("nonsense"); ok {
		t.Error("unknown model must not resolve")
	}
}

func TestTranscribeMissingModelIsActionable(t *testing.T) {
	tr := &Transcriber{Binary: "whisper-cli", ModelPath: "/nonexistent/model.bin"}
	_, err := tr.Transcribe(t.Context(), stt.AudioInput{WAVPath: "x.wav"})
	if err == nil || !strings.Contains(err.Error(), "jarvix setup whisper") {
		t.Errorf("err = %v, want setup hint", err)
	}
}
