package audio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wpctlInspectStub answers `wpctl inspect @DEFAULT_AUDIO_SINK@` and
// `... @DEFAULT_AUDIO_SOURCE@` with the real tool's shape: an id line, then
// indented properties, some carrying the inherited-mark asterisk.
const wpctlInspectStub = `#!/bin/sh
case "$2" in
*SINK*)
	cat <<'EOF'
id 34, type PipeWire:Interface:Node
    alsa.card = "0"
  * client.id = "47"
    node.description = "Fake Speakers Analog Stereo"
    node.name = "alsa_output.fake-speakers.analog-stereo"
    node.nick = "Fake Speakers"
EOF
	;;
*SOURCE*)
	cat <<'EOF'
id 46, type PipeWire:Interface:Node
    node.description = "Fake Microphone Mono"
    node.name = "alsa_input.fake-mic.mono-fallback"
EOF
	;;
*)
	exit 1
	;;
esac
`

func installWpctlStub(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wpctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDefaultSinkAndSourceReportNodeNames(t *testing.T) {
	installWpctlStub(t, wpctlInspectStub)

	sink, err := DefaultSink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// node.name, not node.nick or description: the name is the exact string
	// an audio.output_device pin uses, which is what makes doctor's line a
	// way to check a pin's spelling.
	if sink.Name != "alsa_output.fake-speakers.analog-stereo" {
		t.Errorf("sink name = %q", sink.Name)
	}
	if sink.Description != "Fake Speakers Analog Stereo" {
		t.Errorf("sink description = %q", sink.Description)
	}

	source, err := DefaultSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if source.Name != "alsa_input.fake-mic.mono-fallback" {
		t.Errorf("source name = %q", source.Name)
	}
}

func TestDefaultSinkFailsWithoutWpctl(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := DefaultSink(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wpctl") {
		t.Fatalf("err = %v, want a wpctl failure", err)
	}
}

func TestDefaultSinkRejectsNamelessNode(t *testing.T) {
	installWpctlStub(t, "#!/bin/sh\necho 'id 34, type PipeWire:Interface:Node'\n")
	_, err := DefaultSink(context.Background())
	if err == nil || !strings.Contains(err.Error(), "node.name") {
		t.Fatalf("err = %v, want a missing node.name failure", err)
	}
}
