package doctor

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// Both halves of issue #83 are invisible when they work, so doctor is where a
// user learns they are on: the line must say the bias is active and how many
// mishearing aliases the transcript strip accepts.
func TestNameRecognitionReportsBiasAndAliasCount(t *testing.T) {
	cfg := config.Default()
	cfg.STT.Vocabulary = []string{"Hyprland", "kubectl"}
	r := checkNameRecognition(cfg, config.Paths{})
	if r.Status != OK {
		t.Fatalf("status = %v: %s", r.Status, r.Detail)
	}
	for _, want := range []string{"bias prompt active", `"jarvix"`, "2 vocabulary terms", "5 wake aliases"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q missing %q", r.Detail, want)
		}
	}
}

// A cleared wake word with no vocabulary is a choice, not a fault: the line
// reports the bias off without failing the doctor run.
func TestNameRecognitionReportsAnUnbiasedSetup(t *testing.T) {
	cfg := config.Default()
	cfg.Activation.WakeWord = ""
	cfg.STT.Vocabulary = nil
	r := checkNameRecognition(cfg, config.Paths{})
	if r.Status != OK || !strings.Contains(r.Detail, "bias off") {
		t.Errorf("result = %+v, want OK with the bias reported off", r)
	}
}
