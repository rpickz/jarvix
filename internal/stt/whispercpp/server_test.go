package whispercpp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/warm"
)

// whisper.cpp is never required. A warm worker is two things — a supervised
// process and an HTTP endpoint — so the tests supply both: a shell stub that
// is a real long-lived child (so pids, reuse, and killing are real) and an
// in-process HTTP server standing in for whisper-server's /inference.
const whisperServerStub = `#!/bin/sh
while IFS= read -r _; do :; done
`

func serverTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// warmFixture wires a ServerTranscriber whose warm worker is the stub process
// plus handler, and whose cold fallback is the whisper-cli stub.
type warmFixture struct {
	tr        *ServerTranscriber
	dir       string
	spawns    *atomic.Int64
	inference *atomic.Int64
	wav       string
}

func newWarmFixture(t *testing.T, handler http.HandlerFunc) *warmFixture {
	t.Helper()
	dir := t.TempDir()

	stub := filepath.Join(dir, "whisper-server-stub")
	if err := os.WriteFile(stub, []byte(whisperServerStub), 0o755); err != nil {
		t.Fatal(err)
	}
	cliStub := filepath.Join(dir, "whisper-cli-stub")
	if err := os.WriteFile(cliStub, []byte(whisperStub), 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(dir, "rec.wav")
	if err := os.WriteFile(wav, []byte("RIFF....WAVEfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHISPER_STUB_DIR", dir)

	fx := &warmFixture{dir: dir, wav: wav, spawns: &atomic.Int64{}, inference: &atomic.Int64{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.inference.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	fx.tr = &ServerTranscriber{
		Binary:    stub,
		ModelPath: model,
		Language:  "en",
		Cold:      &Transcriber{Binary: cliStub, ModelPath: model, Language: "en"},
		Log:       serverTestLogger(),
	}
	fx.tr.spawn = func(context.Context) (*serverChild, error) {
		fx.spawns.Add(1)
		proc, err := warm.StartProcess(warm.ProcessSpec{Path: stub})
		if err != nil {
			return nil, err
		}
		return &serverChild{
			proc:   proc,
			base:   srv.URL,
			client: srv.Client(),
			stderr: warm.DrainStderr(proc.Stderr, 5),
		}, nil
	}
	t.Cleanup(func() { _ = fx.tr.Close() })
	return fx
}

// transcribe runs one transcription to completion.
func (fx *warmFixture) transcribe(t *testing.T, ctx context.Context) (string, error) {
	t.Helper()
	events, err := fx.tr.Transcribe(ctx, stt.AudioInput{WAVPath: fx.wav})
	if err != nil {
		return "", err
	}
	var text string
	var streamErr error
	for ev := range events {
		switch ev.Type {
		case stt.EventFinal:
			text = ev.Text
		case stt.EventError:
			streamErr = ev.Err
		}
	}
	return text, streamErr
}

func okTranscript(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.FormValue("response_format") != "text" {
			http.Error(w, "want response_format=text", http.StatusBadRequest)
			return
		}
		if _, _, err := r.FormFile("file"); err != nil {
			http.Error(w, "no clip uploaded", http.StatusBadRequest)
			return
		}
		// Whitespace on purpose: whisper-server pads its plain-text answers,
		// and the adapter is responsible for trimming them.
		_, _ = fmt.Fprintf(w, " %s \n", text)
	}
}

func TestWarmTranscribeReusesOneServerAcrossQuestions(t *testing.T) {
	fx := newWarmFixture(t, okTranscript("what time is it"))

	for i := range 3 {
		text, err := fx.transcribe(t, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if text != "what time is it" {
			t.Fatalf("question %d transcript = %q", i, text)
		}
	}
	// The point of the warm path: three questions, one model load. The
	// fixture's own spawn counter is the measure — the stub process's SPAWN
	// marker file is written by a shell that races this read (it flaked under
	// -count=2), and "how many times did the supervisor spawn" is the claim
	// anyway.
	if got := fx.spawns.Load(); got != 1 {
		t.Errorf("whisper-server spawns = %d, want 1 — the model would reload per question", got)
	}
	if got := fx.inference.Load(); got != 3 {
		t.Errorf("inference requests = %d, want 3", got)
	}
	// And whisper-cli was never run: the stub records its argv when it is.
	if _, err := os.Stat(filepath.Join(fx.dir, "whisper.args")); err == nil {
		t.Error("the cold whisper-cli path ran while a warm worker was healthy")
	}
}

// The bias prompt is the warm half of issue #83. It rides in each /inference
// request as the `prompt` form field — whisper-server's per-request name for
// the same parameter --prompt sets process-wide — so warm and cold cannot
// bias differently, and a reloaded vocabulary needs no model reload.
func TestWarmTranscribeCarriesTheBiasPromptPerRequest(t *testing.T) {
	prompts := make(chan string, 1)
	fx := newWarmFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		prompts <- r.FormValue("prompt")
		_, _ = fmt.Fprintln(w, "what time is it")
	})
	fx.tr.Prompt = "The assistant is called Jarvix."

	if _, err := fx.transcribe(t, context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-prompts; got != "The assistant is called Jarvix." {
		t.Errorf("/inference prompt field = %q, want the bias prompt", got)
	}
}

func TestWarmTranscribeOmitsThePromptFieldWhenUnset(t *testing.T) {
	fields := make(chan bool, 1)
	fx := newWarmFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, present := r.MultipartForm.Value["prompt"]
		fields <- present
		_, _ = fmt.Fprintln(w, "ok")
	})

	if _, err := fx.transcribe(t, context.Background()); err != nil {
		t.Fatal(err)
	}
	if <-fields {
		t.Error("/inference carried a prompt field with no bias configured")
	}
}

func TestWarmTranscribeFallsBackWhenTheServerFails(t *testing.T) {
	fx := newWarmFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ggml backend blew up", http.StatusInternalServerError)
	})

	text, err := fx.transcribe(t, context.Background())
	if err != nil {
		t.Fatalf("a broken warm worker must not fail the session: %v", err)
	}
	if text != "scripted transcript" {
		t.Errorf("transcript = %q, want whisper-cli's output", text)
	}
	if got := fx.tr.WarmStatus().Restarts; got != 1 {
		t.Errorf("restarts = %d, want the failed worker retired", got)
	}
}

func TestWarmTranscribeFallsBackWhenTheServerCannotStart(t *testing.T) {
	fx := newWarmFixture(t, okTranscript("unused"))
	fx.tr.spawn = func(context.Context) (*serverChild, error) {
		return nil, errors.New("whisper-server not found on PATH")
	}

	text, err := fx.transcribe(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text != "scripted transcript" {
		t.Errorf("transcript = %q, want the cold path to answer", text)
	}
}

func TestWarmTranscribeReportsCancellation(t *testing.T) {
	release := make(chan struct{})
	fx := newWarmFixture(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the request open until the test cancels
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	events, err := fx.tr.Transcribe(ctx, stt.AudioInput{WAVPath: fx.wav})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	var last stt.TranscriptEvent
	for ev := range events {
		last = ev
	}
	if last.Type != stt.EventError || !errors.Is(last.Err, context.Canceled) {
		t.Fatalf("last event = %+v, want a cancellation", last)
	}
	// Cancellation is not a worker failure: the model stays loaded.
	if got := fx.tr.WarmStatus().Restarts; got != 0 {
		t.Errorf("restarts = %d, want 0 — an interruption must not cost the model load", got)
	}
}

func TestWarmTranscribeRejectsAMissingRecording(t *testing.T) {
	fx := newWarmFixture(t, okTranscript("unused"))
	_, err := fx.tr.Transcribe(context.Background(), stt.AudioInput{
		WAVPath: filepath.Join(fx.dir, "gone.wav")})
	if err == nil {
		t.Error("a missing clip must fail at the call, not inside the stream")
	}
}

func TestWarmCloseKillsTheServer(t *testing.T) {
	fx := newWarmFixture(t, okTranscript("hello"))
	if _, err := fx.transcribe(t, context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := fx.tr.WarmStatus().PID
	if pid == 0 {
		t.Fatal("no warm worker to kill")
	}

	if err := fx.tr.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("whisper-server (pid %d) survived Close — jarvixd would leave an orphan", pid)
}

func TestServerBinaryForDerivesFromTheConfiguredCLI(t *testing.T) {
	for input, want := range map[string]string{
		"":                             "whisper-server",
		"whisper-cli":                  "whisper-server",
		"/opt/whisper/bin/whisper-cli": "/opt/whisper/bin/whisper-server",
		"/opt/build/main-cli":          "/opt/build/main-server",
		"my-whisper-wrapper":           "whisper-server",
	} {
		if got := ServerBinaryFor(input); got != want {
			t.Errorf("ServerBinaryFor(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStartServerReportsAMissingBinary(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := &ServerTranscriber{
		Binary: "jarvix-no-such-whisper-server", ModelPath: model, Log: serverTestLogger()}
	_, err := tr.startServer(context.Background())
	if err == nil || !strings.Contains(err.Error(), "jarvix-no-such-whisper-server") {
		t.Errorf("err = %v, want the missing binary named", err)
	}
}

func TestStartServerReportsAMissingModel(t *testing.T) {
	tr := &ServerTranscriber{
		Binary: "whisper-server", ModelPath: filepath.Join(t.TempDir(), "gone.bin"),
		Log: serverTestLogger()}
	_, err := tr.startServer(context.Background())
	if err == nil || !strings.Contains(err.Error(), "whisper model not found") {
		t.Errorf("err = %v, want an actionable missing-model message", err)
	}
}
