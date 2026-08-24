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
	for _, want := range []string{"bias prompt active", `"Jarvix"`, "2 vocabulary terms", "5 name aliases"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q missing %q", r.Detail, want)
		}
	}
}

// A cleared name with no vocabulary reports the bias off without failing the
// run: doctor states what is, and validation (which refuses a blank name) has
// its own line.
func TestNameRecognitionReportsAnUnbiasedSetup(t *testing.T) {
	cfg := config.Default()
	cfg.Assistant.Name = ""
	cfg.STT.Vocabulary = nil
	r := checkNameRecognition(cfg, config.Paths{})
	if r.Status != OK || !strings.Contains(r.Detail, "bias off") {
		t.Errorf("result = %+v, want OK with the bias reported off", r)
	}
}

// A custom name with zero aliases WARNs (issue #103): the mishearing variants
// are what make name-matching work in practice — the default name shipped
// with a tuned list precisely because whisper kept writing "Jarvis" and
// "JavaX" — so the warning must say why aliases matter and show the config
// shape that fixes it.
func TestNameRecognitionWarnsOnACustomNameWithoutAliases(t *testing.T) {
	cfg := config.Default()
	cfg.Assistant.Name = "Hal"
	r := checkNameRecognition(cfg, config.Paths{})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn: %s", r.Status, r.Detail)
	}
	// The explanation cites the history that motivates aliases, and the fix
	// shows the [assistant] shape with the user's own name in it.
	for _, want := range []string{`"Hal"`, "Jarvis", "JavaX"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q missing %q", r.Detail, want)
		}
	}
	for _, want := range []string{"[assistant]", `name = "Hal"`, "aliases = ["} {
		if !strings.Contains(r.Fix, want) {
			t.Errorf("fix %q missing %q", r.Fix, want)
		}
	}

	// Adding aliases clears the warning; so does keeping the default name.
	cfg.Assistant.Aliases = []string{"howl", "hull"}
	if r := checkNameRecognition(cfg, config.Paths{}); r.Status != OK {
		t.Errorf("with aliases configured: status = %v, want OK: %s", r.Status, r.Detail)
	}
	if r := checkNameRecognition(config.Default(), config.Paths{}); r.Status != OK {
		t.Errorf("default identity: status = %v, want OK: %s", r.Status, r.Detail)
	}
}
