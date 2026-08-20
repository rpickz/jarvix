package kokoro

import (
	"strings"
	"testing"
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
	rate, err := readSampleRate(strings.NewReader("some log line\nSAMPLE_RATE=24000\nmore\n"))
	if err != nil || rate != 24000 {
		t.Errorf("rate=%d err=%v", rate, err)
	}
	if _, err := readSampleRate(strings.NewReader("no marker here\n")); err == nil {
		t.Error("missing marker should error")
	}
}

func TestName(t *testing.T) {
	if (&Synthesizer{}).Name() != "kokoro" {
		t.Error("wrong name")
	}
}
