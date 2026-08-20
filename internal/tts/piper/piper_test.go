package piper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// voiceFixture creates a fake voice tree: dir/en/en_US/testy/medium/<name>.onnx(.json)
func voiceFixture(t *testing.T, name string, sampleRate string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "en", "en_US", "testy", "medium")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, name+".onnx")
	if err := os.WriteFile(model, []byte("onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"audio":{"sample_rate":` + sampleRate + `}}`
	if err := os.WriteFile(model+".json", []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolveVoiceByName(t *testing.T) {
	root := voiceFixture(t, "en_US-testy-medium", "22050")
	s := &Synthesizer{Binary: "piper-tts", Voice: "en_US-testy-medium", VoiceDirs: []string{root}}
	if err := s.ResolveVoice(); err != nil {
		t.Fatalf("ResolveVoice: %v", err)
	}
	if s.sampleRate != 22050 {
		t.Errorf("sampleRate = %d", s.sampleRate)
	}
	if !strings.HasSuffix(s.modelPath, "en_US-testy-medium.onnx") {
		t.Errorf("modelPath = %q", s.modelPath)
	}
}

func TestResolveVoiceByAbsolutePath(t *testing.T) {
	root := voiceFixture(t, "en_US-testy-medium", "16000")
	path := filepath.Join(root, "en", "en_US", "testy", "medium", "en_US-testy-medium.onnx")
	s := &Synthesizer{Binary: "piper-tts", Voice: path}
	if err := s.ResolveVoice(); err != nil {
		t.Fatalf("ResolveVoice: %v", err)
	}
	if s.sampleRate != 16000 {
		t.Errorf("sampleRate = %d", s.sampleRate)
	}
}

func TestResolveVoiceMissingIsActionable(t *testing.T) {
	s := &Synthesizer{Binary: "piper-tts", Voice: "no-such-voice", VoiceDirs: []string{t.TempDir()}}
	err := s.ResolveVoice()
	if err == nil || !strings.Contains(err.Error(), "no-such-voice") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveVoiceBadSidecar(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "v.onnx")
	os.WriteFile(model, []byte("onnx"), 0o644)
	os.WriteFile(model+".json", []byte("not json"), 0o644)
	s := &Synthesizer{Binary: "piper-tts", Voice: model}
	if err := s.ResolveVoice(); err == nil {
		t.Error("expected error for unreadable voice config")
	}
}
