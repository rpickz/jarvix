package whispercpp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/warm"
)

// ServerTranscriber is the warm STT path: whisper.cpp's whisper-server holds
// the model in memory and answers over a loopback HTTP request, instead of
// whisper-cli re-reading the ggml file for every question.
//
// Why whisper-server rather than a stdin protocol: whisper-cli has no
// long-lived mode — one invocation, one file, exit — so keeping the model warm
// means the binary whisper.cpp ships for exactly that, and it is the option
// the engine's own maintainers support. The cost is a loopback port and an
// HTTP round trip, both of which are noise next to the model load it saves.
//
// The adapter is never load-bearing on its own: Cold is the whisper-cli
// transcriber, and every path that cannot reach a healthy warm worker (not
// installed, still restarting, socket died mid-request) falls through to it.
// A session must not fail because a warm worker did.
type ServerTranscriber struct {
	// Binary is the whisper-server executable.
	Binary string
	// ModelPath is the absolute path to the ggml model file.
	ModelPath string
	// Language is the spoken language ("en", or "auto").
	Language string
	// Cold is the per-transcription fallback. Required.
	Cold *Transcriber
	// MemoryCap and IdleAfter configure the supervisor ([performance]).
	MemoryCap uint64
	IdleAfter time.Duration
	// Log receives warm-worker lifecycle lines. Nil uses the default logger.
	Log *slog.Logger
	// StartTimeout bounds the model load before the warm path is declared
	// unavailable for this session. Zero uses defaultServerStart.
	StartTimeout time.Duration

	once sync.Once
	sup  *warm.Supervisor[*serverChild]

	// spawn overrides child creation. Tests point it at an in-process HTTP
	// server so the protocol can be exercised without whisper.cpp installed;
	// production leaves it nil and gets startServer.
	spawn func(context.Context) (*serverChild, error)
}

// ServerBinaryFor derives the whisper-server path from the configured
// whisper-cli one. whisper.cpp packages both binaries together, so a user who
// pointed stt.whisper.binary at a custom build should get the server from the
// same build — without a second configuration key that only exists because the
// warm path needs a different executable name.
func ServerBinaryFor(cliBinary string) string {
	if cliBinary == "" {
		return "whisper-server"
	}
	dir, file := filepath.Split(cliBinary)
	switch {
	case file == "whisper-cli":
		file = "whisper-server"
	case strings.HasSuffix(file, "-cli"):
		file = strings.TrimSuffix(file, "-cli") + "-server"
	default:
		// An unrecognised name (a wrapper script, say) says nothing about
		// where the server lives; fall back to the packaged name on PATH.
		return "whisper-server"
	}
	return dir + file
}

// defaultServerStart is how long whisper-server gets to load its model and
// start listening. Generous because a first run reads hundreds of megabytes
// off a cold disk; if it lapses, this session simply runs whisper-cli.
const defaultServerStart = 30 * time.Second

// serverReadyPoll is the gap between readiness probes during start-up. Short
// enough that a warm start costs a poll interval at most.
const serverReadyPoll = 20 * time.Millisecond

// serverChild is one running whisper-server: the supervised process plus the
// loopback endpoint it answers on.
type serverChild struct {
	proc   *warm.Process
	base   string
	client *http.Client
	stderr *warm.StderrTail
}

func (c *serverChild) PID() int {
	if c.proc == nil {
		return 0
	}
	return c.proc.PID()
}

func (c *serverChild) Close() {
	if c.proc != nil {
		c.proc.Close()
	}
}

// Name implements stt.Transcriber. The name does not change with the warm
// path: it is the same engine, and status output should not flap.
func (s *ServerTranscriber) Name() string { return "whisper.cpp" }

func (s *ServerTranscriber) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// supervisor lazily builds the supervisor so a ServerTranscriber constructed
// from config costs nothing until the first question is asked.
func (s *ServerTranscriber) supervisor() *warm.Supervisor[*serverChild] {
	s.once.Do(func() {
		spawn := s.spawn
		if spawn == nil {
			spawn = s.startServer
		}
		s.sup = &warm.Supervisor[*serverChild]{
			Name:      "whisper",
			Spawn:     spawn,
			MemoryCap: s.MemoryCap,
			IdleAfter: s.IdleAfter,
			Log:       s.logger(),
		}
	})
	return s.sup
}

// WarmStatus reports the warm worker for `jarvix doctor` and status.get.
func (s *ServerTranscriber) WarmStatus() warm.Status { return s.supervisor().Status() }

// Close shuts the warm worker down. The daemon calls it on exit and whenever a
// config reload replaces the adapter, so no whisper-server ever outlives the
// jarvixd that started it.
func (s *ServerTranscriber) Close() error {
	s.supervisor().Close()
	return nil
}

// Transcribe implements stt.Transcriber. It answers from the warm server when
// there is one and silently runs whisper-cli when there is not.
func (s *ServerTranscriber) Transcribe(ctx context.Context, input stt.AudioInput) (<-chan stt.TranscriptEvent, error) {
	child, err := s.supervisor().Get(ctx)
	if err != nil {
		s.logger().Debug("warm whisper unavailable; using whisper-cli",
			"component", "stt", "error", err.Error())
		return s.Cold.Transcribe(ctx, input)
	}

	// Read the clip before handing the session a channel: a missing recording
	// is the caller's bug and should surface as a start error, not a stream
	// error, exactly as the cold path's missing-model check does.
	body, contentType, err := multipartWAV(input.WAVPath)
	if err != nil {
		return nil, err
	}

	ch := make(chan stt.TranscriptEvent, 1)
	go func() {
		defer close(ch)
		text, err := s.infer(ctx, child, body, contentType)
		if err == nil {
			s.supervisor().Release()
			ch <- stt.TranscriptEvent{Type: stt.EventFinal, Text: text}
			return
		}
		if ctx.Err() != nil {
			// The user interrupted. whisper-server has no per-request abort,
			// so the child keeps decoding this clip until it finishes and the
			// result is dropped — a bounded, silent waste of a few hundred
			// milliseconds on a worker nobody is waiting for.
			ch <- stt.TranscriptEvent{Type: stt.EventError, Err: ctx.Err()}
			return
		}
		// The warm worker let us down. Retire it, then answer this session
		// from the cold path: the interaction must not die with the worker.
		s.supervisor().Discard(err.Error())
		s.logger().Warn("warm whisper failed; falling back to whisper-cli for this question",
			"component", "stt", "error", err.Error())
		events, coldErr := s.Cold.Transcribe(ctx, input)
		if coldErr != nil {
			ch <- stt.TranscriptEvent{Type: stt.EventError, Err: coldErr}
			return
		}
		for ev := range events {
			ch <- ev
		}
	}()
	return ch, nil
}

// infer POSTs one clip to the warm server and returns the transcript.
func (s *ServerTranscriber) infer(ctx context.Context, child *serverChild, body []byte, contentType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, child.base+"/inference", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := child.client.Do(req)
	if err != nil {
		if detail := child.stderr.String(); detail != "" {
			return "", fmt.Errorf("whisper-server request failed: %w (%s)", err, detail)
		}
		return "", fmt.Errorf("whisper-server request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read whisper-server response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper-server returned %s: %s", resp.Status, truncate(string(out), 200))
	}
	return strings.TrimSpace(string(out)), nil
}

// multipartWAV builds the /inference request body: the clip plus the two
// fields that make whisper-server answer with a bare transcript.
func multipartWAV(path string) (body []byte, contentType string, err error) {
	wav, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read recording: %w", err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", "recording.wav")
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(wav); err != nil {
		return nil, "", err
	}
	for field, value := range map[string]string{
		"response_format": "text",
		"temperature":     "0.0",
	} {
		if err := w.WriteField(field, value); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// startServer spawns whisper-server on a free loopback port and waits for it
// to accept requests.
func (s *ServerTranscriber) startServer(ctx context.Context) (*serverChild, error) {
	if _, err := os.Stat(s.ModelPath); err != nil {
		return nil, fmt.Errorf("whisper model not found at %s (run: jarvix setup whisper)", s.ModelPath)
	}
	bin := s.Binary
	if bin == "" {
		bin = "whisper-server"
	}
	path, err := warm.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%w; install whisper.cpp (pacman -S whisper.cpp) or set performance.warm_engines = false", err)
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, err
	}

	args := []string{
		"--model", s.ModelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--no-timestamps",
	}
	if s.Language != "" {
		args = append(args, "--language", s.Language)
	}
	proc, err := warm.StartProcess(warm.ProcessSpec{Path: path, Args: args})
	if err != nil {
		return nil, err
	}
	child := &serverChild{
		proc: proc,
		base: "http://127.0.0.1:" + strconv.Itoa(port),
		// No global timeout on the client: a long recording legitimately takes
		// a while, and the session context is what bounds the request.
		client: &http.Client{},
		stderr: warm.DrainStderr(proc.Stderr, 5),
	}
	// whisper-server writes its ggml banner to stdout; nobody reads it, so
	// drain it or the pipe fills and the server wedges mid-transcription.
	go func() { _, _ = io.Copy(io.Discard, proc.Stdout) }()

	if err := s.awaitReady(ctx, child, port); err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

// awaitReady polls the port until whisper-server accepts connections. The
// model load happens before it listens, so a successful connect means the
// engine is warm — no probe request is needed, and none is sent.
func (s *ServerTranscriber) awaitReady(ctx context.Context, child *serverChild, port int) error {
	limit := s.StartTimeout
	if limit <= 0 {
		limit = defaultServerStart
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	addr := "127.0.0.1:" + strconv.Itoa(port)
	var dialer net.Dialer
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-child.proc.Exited():
			return fmt.Errorf("whisper-server exited during start-up: %s", child.stderr.String())
		case <-ctx.Done():
			return fmt.Errorf("whisper-server did not start within %s: %s", limit, child.stderr.String())
		case <-time.After(serverReadyPoll):
		}
	}
}

// freeLoopbackPort reserves an ephemeral port by binding and releasing it.
// There is a window in which something else could claim it; whisper-server
// then fails to bind, the supervisor reports a failed spawn, and the session
// runs cold — which is why the window is acceptable rather than fatal.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve a loopback port for whisper-server: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
