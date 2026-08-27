package tts

import (
	"strings"
	"testing"
)

func TestTailKeepsOnlyTheEnd(t *testing.T) {
	tail := NewTail(16)
	if _, err := tail.Write([]byte(strings.Repeat("x", 100) + "the actual error")); err != nil {
		t.Fatal(err)
	}
	got := tail.String()
	if got != "the actual error" {
		t.Errorf("tail = %q", got)
	}
}

func TestTailCollapsesToOneLine(t *testing.T) {
	tail := NewTail(1024)
	tail.Add("Traceback (most recent call last):")
	tail.Add("  boom")
	tail.Add("ImportError: no module named kokoro_onnx")
	got := tail.String()
	if strings.Contains(got, "\n") {
		t.Errorf("tail carries newlines: %q", got)
	}
	for _, want := range []string{"Traceback", "ImportError", " | "} {
		if !strings.Contains(got, want) {
			t.Errorf("tail %q missing %q", got, want)
		}
	}
}

func TestTailEmptyStaysEmpty(t *testing.T) {
	if got := NewTail(64).String(); got != "" {
		t.Errorf("empty tail = %q", got)
	}
}
