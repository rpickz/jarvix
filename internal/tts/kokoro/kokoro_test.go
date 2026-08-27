package kokoro

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/tts"
)

func TestReadySurfacesMissingFiles(t *testing.T) {
	s := &Synthesizer{
		Python:     "/nonexistent/python",
		Script:     "/nonexistent/script.py",
		ModelPath:  "/nonexistent/model.onnx",
		VoicesPath: "/nonexistent/voices.bin",
	}
	err := s.Ready()
	if err == nil || !strings.Contains(err.Error(), "setup-kokoro") {
		t.Errorf("err = %v, want setup hint", err)
	}
}

func TestReadSampleRate(t *testing.T) {
	rate, err := readSampleRate(strings.NewReader("some log line\nSAMPLE_RATE=24000\nmore\n"), tts.NewTail(256))
	if err != nil || rate != 24000 {
		t.Errorf("rate=%d err=%v", rate, err)
	}
	// A helper that dies without the marker is quoted in the error: its last
	// stderr lines are the diagnosis (issue #113).
	tail := tts.NewTail(256)
	if _, err := readSampleRate(strings.NewReader("ImportError: no module named kokoro_onnx\n"), tail); err == nil ||
		!strings.Contains(err.Error(), "ImportError: no module named kokoro_onnx") {
		t.Errorf("err = %v, want the helper's stderr quoted", err)
	}
}

func TestName(t *testing.T) {
	if (&Synthesizer{}).Name() != "kokoro" {
		t.Error("wrong name")
	}
}
