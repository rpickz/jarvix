package wake

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// The detector protocol is tested over in-memory pipes rather than a real
// helper. That is not a shortcut around spawning a process — it is what makes
// every branch of the wire format reachable: a helper that answers with a
// protocol version from the future, or a truncated line, or an ERROR halfway
// through, is three lines of fixture here and an unwritable test otherwise.

// pipe is the helper's stdin as the detector sees it, keeping what was
// written so the frame encoding can be checked byte for byte.
type pipe struct {
	bytes.Buffer
	closed bool
}

func (p *pipe) Close() error { p.closed = true; return nil }

// dial wires a detector to a scripted helper.
func dial(t *testing.T, script string) (*ProcessDetector, *pipe, error) {
	t.Helper()
	in := &pipe{}
	out := bufio.NewReader(strings.NewReader(script))
	d, err := newProcessDetector("test-helper", in, out, 4242, func() {}, nil)
	return d, in, err
}

// The happy path: the helper announces itself, Jarvix sends a frame, the
// helper answers with a score.
func TestDetectorHandshakeAndScoring(t *testing.T) {
	d, in, err := dial(t, "READY 1 1280 hey_jarvis\nSCORE 0.9312\nSCORE 0.0100\n")
	if err != nil {
		t.Fatal(err)
	}
	// The model name travels with the handshake because it is not necessarily
	// the wake word that was asked for — `jarvix status` must report what is
	// really listening.
	if !strings.Contains(d.Name(), "hey_jarvis") {
		t.Errorf("Name() is %q; it must carry the model the helper actually loaded", d.Name())
	}
	if d.PID() != 4242 {
		t.Errorf("PID() is %d, want 4242", d.PID())
	}

	score, err := d.Score(make([]int16, FrameSamples))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "first score", score, 0.9312, 0.0001)
	if got := in.Len(); got != FrameBytes {
		t.Errorf("wrote %d bytes for one frame, want %d", got, FrameBytes)
	}
	if score, err = d.Score(make([]int16, FrameSamples)); err != nil || score > 0.02 {
		t.Errorf("second score: %v, %v", score, err)
	}
}

// Samples are little-endian two's complement. A silent regression here would
// hand the model noise that happens to have the right length, and the only
// symptom would be a wake word that never fires.
func TestDetectorEncodesFramesAsLittleEndianS16(t *testing.T) {
	d, in, err := dial(t, "READY 1 1280 m\nSCORE 0\n")
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]int16, FrameSamples)
	frame[0], frame[1], frame[2], frame[3] = 0, -1, 32767, -32768
	if _, err := d.Score(frame); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x00, 0xff, 0xff, 0xff, 0x7f, 0x00, 0x80}
	if got := in.Bytes()[:len(want)]; !bytes.Equal(got, want) {
		t.Errorf("frame bytes % x, want % x", got, want)
	}
}

// A helper that wants a different window size must be refused at the
// handshake. Accepting it would mean scoring windows that are not the ones
// Jarvix believes it sent — wrong answers, no error, nothing in the log.
func TestDetectorRefusesAFrameSizeMismatch(t *testing.T) {
	_, _, err := dial(t, "READY 1 512 m\n")
	if err == nil {
		t.Fatal("a helper wanting 512-sample frames was accepted")
	}
	if !strings.Contains(err.Error(), "512") || !strings.Contains(err.Error(), "1280") {
		t.Errorf("the error should name both sizes; got %q", err)
	}
}

// Every other way the handshake can fail. Each is a real installation
// mistake — an old helper, a wrapper script printing a banner, a venv that
// died on import — and each must produce a message naming the helper rather
// than a bare EOF from deep inside a read.
func TestDetectorRefusesABadHandshake(t *testing.T) {
	for _, c := range []struct{ name, script string }{
		{"protocol from the future", "READY 9 1280 m\n"},
		{"not a READY line", "Traceback (most recent call last):\n"},
		{"truncated", "READY 1\n"},
		{"nothing at all", ""},
	} {
		if _, _, err := dial(t, c.script); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if !strings.Contains(err.Error(), "test-helper") {
			t.Errorf("%s: the error does not name the helper: %q", c.name, err)
		}
	}
}

// A helper reporting a fatal problem must surface as an error so the
// supervisor replaces it, not as a score of zero that quietly means the wake
// word stops working.
func TestDetectorReportsAHelperError(t *testing.T) {
	d, _, err := dial(t, "READY 1 1280 m\nERROR model file is missing\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Score(make([]int16, FrameSamples))
	if err == nil {
		t.Fatal("an ERROR line was treated as a score")
	}
	if !strings.Contains(err.Error(), "model file is missing") {
		t.Errorf("the helper's own words are missing from %q", err)
	}
}

// Anything unparseable is an error too. Scoring on a value we could not read
// would be worse than failing: the number would be zero, and zero is a
// perfectly plausible score.
func TestDetectorRefusesAnUnreadableScore(t *testing.T) {
	for _, c := range []struct{ name, script string }{
		{"not a number", "READY 1 1280 m\nSCORE probably\n"},
		{"unknown verb", "READY 1 1280 m\nHELLO\n"},
		{"empty line", "READY 1 1280 m\n\n"},
		{"helper exited", "READY 1 1280 m\n"},
	} {
		d, _, err := dial(t, c.script)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if _, err := d.Score(make([]int16, FrameSamples)); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

// The frame the listener sends is always FrameSamples long; a mismatch is a
// programming error, and catching it here beats sending a short frame the
// helper will block waiting to complete.
func TestDetectorRefusesAWrongSizedFrame(t *testing.T) {
	d, _, err := dial(t, "READY 1 1280 m\nSCORE 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Score(make([]int16, 10)); err == nil {
		t.Error("a 10-sample frame was sent to the helper")
	}
}

// Close must be safe to call twice: the supervisor closes retired children,
// and shutdown closes whatever is live, and the two can be the same child.
func TestDetectorCloseIsIdempotent(t *testing.T) {
	stops := 0
	in := &pipe{}
	d, err := newProcessDetector("test-helper", in,
		bufio.NewReader(strings.NewReader("READY 1 1280 m\n")), 1, func() { stops++ }, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	d.Close()
	if stops != 1 {
		t.Errorf("the helper was stopped %d times, want 1", stops)
	}
}

// A helper that names no model still gets a usable name: the configured
// command. An empty label in `jarvix status` would read as "no detector".
func TestDetectorNameFallsBackToTheCommand(t *testing.T) {
	d, _, err := dial(t, "READY 1 1280\n")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "test-helper" {
		t.Errorf("Name() is %q, want the command name", d.Name())
	}
}

// The pre-flight check the daemon and doctor share. It has to fail on an
// empty command as well as a missing binary, because an empty
// activation.wake_command would otherwise spawn nothing forever.
func TestDetectorReadyRejectsWhatCannotRun(t *testing.T) {
	if err := DetectorReady(nil); err == nil {
		t.Error("an empty wake_command was reported as installed")
	}
	if err := DetectorReady([]string{"jarvix-wake-that-is-not-installed"}); err == nil {
		t.Error("a missing helper was reported as installed")
	}
	// Something that certainly exists, to prove the check is not simply
	// always failing.
	if err := DetectorReady([]string{"sh", "-c", "true"}); err != nil {
		t.Errorf("a helper that exists was rejected: %v", err)
	}
}
