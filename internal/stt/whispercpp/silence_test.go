package whispercpp

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
)

// Issue #191 lands on both engines, and the two carry the bias differently —
// argv for whisper-cli, a multipart form field for whisper-server — so each
// rule is pinned separately on each path. whisper.cpp is never required: the
// cold path is the shell stub from transcribe_test.go and the warm path is the
// in-process HTTP server from server_test.go.

// The bias sentence exactly as config.STTBiasPromptWith composes it, and the
// transcript whisper-cli really returns for two seconds of digital silence
// under it — leading space, terminal full stop, reproduced on the machine.
const (
	biasSentence = "The assistant is called Jarvix."
	silenceEcho  = " The assistant is called Jarvix."
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// silentWAV writes two seconds of digital silence: what a muted source or the
// wrong capture device delivers. Generated rather than committed — it is a
// hundred bytes of code against a hundred kilobytes of zeros, and the length
// and rate that make it silence are then written down beside the assertion.
func silentWAV(t *testing.T) string {
	t.Helper()
	return clipWAV(t, make([]int16, 2*16000))
}

// spokenWAV writes audio loud enough that nothing may gate it out — the
// carrier for every test about what happens *after* whisper answers.
func spokenWAV(t *testing.T) string {
	t.Helper()
	rng := rand.New(rand.NewSource(191)) //nolint:gosec // fixture, not crypto
	pcm := make([]int16, 16000)
	for i := range pcm {
		pcm[i] = int16(rng.Intn(8001) - 4000)
	}
	return clipWAV(t, pcm)
}

func clipWAV(t *testing.T, pcm []int16) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.wav")
	if err := audio.WriteWAV(path, pcm, 16000, 1); err != nil {
		t.Fatal(err)
	}
	return path
}

// drain runs a transcription to completion and returns the final event.
func drain(t *testing.T, events <-chan stt.TranscriptEvent) stt.TranscriptEvent {
	t.Helper()
	var last stt.TranscriptEvent
	for ev := range events {
		last = ev
	}
	return last
}

// --- the cold path: whisper-cli --------------------------------------------

// The primary fix: silence is never put to the engine at all, so the whole
// family of silence hallucinations — the prompt echo, " you", "Thank you." —
// cannot be produced. Proved by the stub never running: it writes its argv on
// every invocation, so the absence of that file is the absence of the process.
func TestColdPathDoesNotRunWhisperOnASilentCapture(t *testing.T) {
	tr, dir := installWhisperStub(t)
	tr.Log = discardLogger()
	tr.Prompt = biasSentence

	ev := drain(t, mustTranscribe(t, tr, silentWAV(t)))
	if ev.Type != stt.EventFinal || ev.Text != "" {
		t.Fatalf("event = %+v, want an empty final transcript", ev)
	}
	if !strings.Contains(ev.Reason, "no voiced audio") {
		t.Errorf("reason = %q, want it to say the capture had no voiced audio", ev.Reason)
	}
	if _, err := os.Stat(filepath.Join(dir, "whisper.args")); !os.IsNotExist(err) {
		t.Errorf("whisper-cli ran (%v); a silent capture must not reach the engine", err)
	}
}

// The second line of defence, for a capture that does carry signal — a
// microphone picking up a quiet room passes the energy gate and whisper will
// still echo the prompt at it.
func TestColdPathDiscardsATranscriptThatIsOnlyTheBiasPrompt(t *testing.T) {
	tr, _ := installWhisperStub(t)
	tr.Log = discardLogger()
	tr.Prompt = biasSentence
	t.Setenv("WHISPER_STUB_TEXT", silenceEcho)

	ev := drain(t, mustTranscribe(t, tr, spokenWAV(t)))
	if ev.Type != stt.EventFinal || ev.Text != "" {
		t.Fatalf("event = %+v, want the echo discarded", ev)
	}
	if !strings.Contains(ev.Reason, "bias prompt") {
		t.Errorf("reason = %q, want it to name the bias prompt", ev.Reason)
	}
}

// The comparison is against what this transcriber actually sent, so a user who
// renamed the assistant is covered without anything here knowing the name.
func TestColdPathDiscardsTheEchoOfARenamedAssistant(t *testing.T) {
	tr, _ := installWhisperStub(t)
	tr.Log = discardLogger()
	tr.PromptFunc = func() string { return "The assistant is called Friday." }
	t.Setenv("WHISPER_STUB_TEXT", " The assistant is called Friday.")

	if ev := drain(t, mustTranscribe(t, tr, spokenWAV(t))); ev.Text != "" {
		t.Errorf("text = %q, want the echo of the configured name discarded", ev.Text)
	}
}

// The pin the acceptance criteria name. A question that contains the name and
// most of the bias sentence's words is an ordinary thing to say.
func TestColdPathKeepsARealUtteranceContainingTheName(t *testing.T) {
	tr, _ := installWhisperStub(t)
	tr.Log = discardLogger()
	tr.Prompt = biasSentence
	t.Setenv("WHISPER_STUB_TEXT", " Jarvix, what is the assistant called?")

	ev := drain(t, mustTranscribe(t, tr, spokenWAV(t)))
	if ev.Text != "Jarvix, what is the assistant called?" {
		t.Errorf("text = %q, want the question through unchanged", ev.Text)
	}
	if ev.Reason != "" {
		t.Errorf("reason = %q, want none on a kept transcript", ev.Reason)
	}
}

// Every uncertainty resolves towards transcribing: a recording this package
// cannot read or parse is a recording it has no opinion about.
func TestColdPathTranscribesWhenTheCaptureCannotBeMeasured(t *testing.T) {
	tr, dir := installWhisperStub(t)
	tr.Log = discardLogger()
	unparseable := filepath.Join(t.TempDir(), "junk.wav")
	if err := os.WriteFile(unparseable, []byte("not a wav"), 0o600); err != nil {
		t.Fatal(err)
	}

	if ev := drain(t, mustTranscribe(t, tr, unparseable)); ev.Text != "scripted transcript" {
		t.Errorf("event = %+v, want the transcript: an unmeasurable clip is transcribed", ev)
	}
	if _, err := os.Stat(filepath.Join(dir, "whisper.args")); err != nil {
		t.Errorf("whisper-cli did not run (%v); an unmeasurable clip must still be asked about", err)
	}
}

func mustTranscribe(t *testing.T, tr *Transcriber, wav string) <-chan stt.TranscriptEvent {
	t.Helper()
	events, err := tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: wav})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// --- the warm path: whisper-server ------------------------------------------

// The warm worker must not even be started for a capture with nothing in it:
// a dead microphone spending thirty seconds loading a model is the same waste
// the gate exists to avoid, one layer up.
func TestWarmPathDoesNotStartAWorkerForASilentCapture(t *testing.T) {
	fx := newWarmFixture(t, okTranscript(silenceEcho))
	fx.tr.Prompt = biasSentence

	events, err := fx.tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: silentWAV(t)})
	if err != nil {
		t.Fatal(err)
	}
	ev := drain(t, events)
	if ev.Type != stt.EventFinal || ev.Text != "" {
		t.Fatalf("event = %+v, want an empty final transcript", ev)
	}
	if !strings.Contains(ev.Reason, "no voiced audio") {
		t.Errorf("reason = %q, want it to say the capture had no voiced audio", ev.Reason)
	}
	if n := fx.spawns.Load(); n != 0 {
		t.Errorf("spawns = %d, want 0: silence must not wake a whisper-server", n)
	}
	if n := fx.inference.Load(); n != 0 {
		t.Errorf("inference calls = %d, want 0", n)
	}
}

// The bias reaches whisper-server as a `prompt` form field rather than an
// argv flag, so this is a separate journey for the same sentence and gets its
// own pin. The handler asserts the field arrived, then answers with the echo.
func TestWarmPathDiscardsTheEchoOfItsMultipartPrompt(t *testing.T) {
	// The handler answers with the prompt it was given, which is exactly what
	// the real engine does on silence, and reports it over a channel — the
	// handler runs on its own goroutine, and this package's other tests
	// already learned to hand its observations across rather than share them.
	prompts := make(chan string, 1)
	fx := newWarmFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sent := r.FormValue("prompt")
		prompts <- sent
		// Whitespace and a full stop of its own, as whisper-server returns.
		_, _ = io.WriteString(w, " "+sent+"\n")
	})
	fx.tr.PromptFunc = func() string { return biasSentence }

	events, err := fx.tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: spokenWAV(t)})
	if err != nil {
		t.Fatal(err)
	}
	ev := drain(t, events)
	if sent := <-prompts; sent != biasSentence {
		t.Fatalf("prompt field = %q, want the bias sentence", sent)
	}
	if ev.Type != stt.EventFinal || ev.Text != "" {
		t.Fatalf("event = %+v, want the echo discarded", ev)
	}
	if !strings.Contains(ev.Reason, "bias prompt") {
		t.Errorf("reason = %q, want it to name the bias prompt", ev.Reason)
	}
}

func TestWarmPathKeepsARealUtteranceContainingTheName(t *testing.T) {
	fx := newWarmFixture(t, okTranscript("Jarvix, what is the assistant called?"))
	fx.tr.Prompt = biasSentence

	events, err := fx.tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: spokenWAV(t)})
	if err != nil {
		t.Fatal(err)
	}
	if ev := drain(t, events); ev.Text != "Jarvix, what is the assistant called?" {
		t.Errorf("event = %+v, want the question through unchanged", ev)
	}
}
