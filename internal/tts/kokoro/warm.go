package kokoro

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/warm"
)

// WarmSynthesizer speaks through a persistent Kokoro helper: one Python
// interpreter, one ONNX model load, many utterances (ADR 0018).
//
// The cold adapter pays ~0.9s before the first sample of every answer on this
// hardware — Python start-up plus the model load — and none of that work
// depends on what is being said. Keeping the helper alive turns that into
// ~0.4s of actual synthesis, which is most of the release-to-first-audio
// budget this ticket exists to spend well.
//
// Cancellation is the reason the helper speaks a protocol rather than just
// looping on stdin. Killing the process would still silence Jarvix instantly,
// but it would also throw away the model load, so an interrupted sentence
// would make the *next* answer slow — precisely backwards. Instead an
// utterance carries an id, ABORT names it, and the helper checks between PCM
// frames: speech stops within one frame and the worker stays warm.
type WarmSynthesizer struct {
	// Cold is the per-utterance adapter: the source of the helper's location
	// and configuration, and the fallback whenever the warm path is not
	// available. Required.
	Cold *Synthesizer
	// MemoryCap and IdleAfter configure the supervisor ([performance]).
	MemoryCap uint64
	IdleAfter time.Duration
	// Log receives warm-worker lifecycle lines. Nil uses the default logger.
	Log *slog.Logger
	// StartTimeout bounds the helper's model load before the warm path is
	// declared unavailable. Zero uses defaultKokoroStart.
	StartTimeout time.Duration

	once sync.Once
	sup  *warm.Supervisor[*kokoroChild]
	// spawn overrides child creation for tests (a stub speaking the same
	// protocol); production leaves it nil and gets startHelper.
	spawn func(context.Context) (*kokoroChild, error)

	// utter serialises utterances: one child, one PCM stream, so a second
	// Speak waits for the first to finish or abort.
	utter sync.Mutex
	seq   atomic.Uint64
}

const (
	// defaultKokoroStart is how long the helper gets to import kokoro-onnx and
	// load the model before the session gives up and synthesizes cold.
	defaultKokoroStart = 30 * time.Second
	// abortDrain bounds how long an aborted utterance may take to acknowledge.
	// Past it the helper is assumed wedged and retired — kill and respawn is
	// the fallback, never the primary cancellation path.
	abortDrain = 3 * time.Second
	// maxFrame rejects an absurd CHUNK length rather than allocating it. One
	// Kokoro chunk is a sentence of 24 kHz mono audio: tens of kilobytes.
	maxFrame = 16 << 20
	// helperProtocol is the serve-protocol version this adapter speaks. A
	// helper announcing anything else is treated as unusable, so an old
	// kokoro_stream.py left in ~/.local/share/jarvix degrades to the cold
	// path instead of hanging on a protocol it does not know.
	helperProtocol = 1
)

// kokoroChild is one running helper in serve mode.
type kokoroChild struct {
	proc       *warm.Process
	sampleRate int
	stderr     *warm.StderrTail
}

func (c *kokoroChild) PID() int {
	if c.proc == nil {
		return 0
	}
	return c.proc.PID()
}

func (c *kokoroChild) Close() {
	if c.proc != nil {
		// QUIT lets the helper exit cleanly; Close's stdin close and process
		// group signal handle a helper that ignores it.
		_, _ = io.WriteString(c.proc.Stdin, "QUIT\n")
		c.proc.Close()
	}
}

// Name implements tts.Synthesizer.
func (w *WarmSynthesizer) Name() string { return "kokoro" }

// Ready implements the readiness check doctor uses; it is the cold adapter's,
// because the same files back both paths.
func (w *WarmSynthesizer) Ready() error { return w.Cold.Ready() }

func (w *WarmSynthesizer) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *WarmSynthesizer) supervisor() *warm.Supervisor[*kokoroChild] {
	w.once.Do(func() {
		spawn := w.spawn
		if spawn == nil {
			spawn = w.startHelper
		}
		w.sup = &warm.Supervisor[*kokoroChild]{
			Name:      "kokoro",
			Spawn:     spawn,
			MemoryCap: w.MemoryCap,
			IdleAfter: w.IdleAfter,
			Log:       w.logger(),
		}
	})
	return w.sup
}

// WarmStatus reports the warm worker for `jarvix doctor` and status.get.
func (w *WarmSynthesizer) WarmStatus() warm.Status { return w.supervisor().Status() }

// Close shuts the warm helper down — daemon exit, or a config reload that
// replaced this adapter. No Python process outlives the jarvixd that spawned
// it.
func (w *WarmSynthesizer) Close() error {
	w.supervisor().Close()
	return nil
}

// Speak implements tts.Synthesizer.
func (w *WarmSynthesizer) Speak(ctx context.Context, req tts.Request) (tts.Format, <-chan tts.Chunk, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return tts.Format{}, nil, fmt.Errorf("nothing to speak")
	}
	// The protocol is line-wise; an utterance is one line by construction.
	text = strings.Join(strings.Fields(text), " ")

	w.utter.Lock()
	child, err := w.supervisor().Get(ctx)
	if err != nil {
		w.utter.Unlock()
		w.logger().Debug("warm kokoro unavailable; spawning a one-shot helper",
			"component", "tts", "error", err.Error())
		return w.Cold.Speak(ctx, req)
	}
	id := strconv.FormatUint(w.seq.Add(1), 10)
	if _, err := fmt.Fprintf(child.proc.Stdin, "SPEAK %s %s\n", id, text); err != nil {
		w.utter.Unlock()
		w.supervisor().Discard(fmt.Sprintf("helper stdin closed: %v", err))
		w.logger().Warn("warm kokoro failed; using a one-shot helper for this sentence",
			"component", "tts", "error", err.Error())
		return w.Cold.Speak(ctx, req)
	}

	format := tts.Format{SampleRate: child.sampleRate, Channels: 1}
	// Capacity one so the terminal error frame can always be delivered: a
	// cancelled speaker stops reading the moment its own context fires, and a
	// blocked send here would strand this goroutine — with the utterance lock
	// still held.
	out := make(chan tts.Chunk, 1)
	go func() {
		defer close(out)
		defer w.utter.Unlock()
		spoken, err := w.stream(ctx, child, id, out)
		switch {
		case err == nil:
			return
		case !spoken && ctx.Err() == nil:
			// The warm helper died before a single sample came out, so nothing
			// has been heard and a one-shot helper can still deliver this
			// sentence intact. A session must not lose its voice because a
			// worker did.
			w.logger().Warn("warm kokoro died mid-sentence; speaking it with a one-shot helper",
				"component", "tts", "error", err.Error())
			w.coldFallback(ctx, req, out)
		default:
			sendErr(ctx, out, err)
		}
	}()
	return format, out, nil
}

// coldFallback renders a sentence with the per-utterance helper and forwards
// it, after the warm path failed before producing audio.
func (w *WarmSynthesizer) coldFallback(ctx context.Context, req tts.Request, out chan<- tts.Chunk) {
	_, chunks, err := w.Cold.Speak(ctx, req)
	if err != nil {
		sendErr(ctx, out, err)
		return
	}
	for c := range chunks {
		select {
		case out <- c:
		case <-ctx.Done():
			return
		}
	}
}

// stream pumps one utterance's frames to the caller and owns its cancellation.
// spoken reports whether any audio reached the caller, which decides whether a
// failure can still be retried cold.
func (w *WarmSynthesizer) stream(ctx context.Context, child *kokoroChild, id string,
	out chan<- tts.Chunk) (spoken bool, err error) {
	frames, readErr := readFrames(child.proc.Stdout, id)

	forward := func(f frame) bool {
		select {
		case out <- tts.Chunk{PCM: f.payload}:
			spoken = true
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		select {
		case f, ok := <-frames:
			if !ok {
				if err := <-readErr; err != nil {
					w.fail(child, err)
					return spoken, err
				}
				w.supervisor().Release()
				return spoken, nil
			}
			switch f.verb {
			case "CHUNK":
				if !forward(f) {
					w.abort(child, id, frames, readErr)
					return spoken, ctx.Err()
				}
			case "ERROR":
				// The helper survived a bad utterance; the session should too.
				w.logger().Warn("warm kokoro rejected an utterance",
					"component", "tts", "error", f.text)
				return spoken, fmt.Errorf("kokoro helper: %s", f.text)
			}
		case <-ctx.Done():
			w.abort(child, id, frames, readErr)
			return spoken, ctx.Err()
		}
	}
}

// abort stops the utterance in the helper and waits for it to say so, keeping
// the worker warm for the next question. A helper that will not acknowledge
// within abortDrain is retired instead — the kill-and-respawn fallback, which
// costs the next session one cold start and nothing else.
func (w *WarmSynthesizer) abort(child *kokoroChild, id string,
	frames <-chan frame, readErr <-chan error) {
	if _, err := fmt.Fprintf(child.proc.Stdin, "ABORT %s\n", id); err != nil {
		w.fail(child, fmt.Errorf("abort could not reach the helper: %w", err))
		return
	}
	deadline := time.NewTimer(abortDrain)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				if err := <-readErr; err != nil {
					w.fail(child, err)
					return
				}
				w.supervisor().Release()
				return
			}
			// Discard whatever was already in flight: the user has moved on.
		case <-deadline.C:
			w.fail(child, fmt.Errorf("helper did not acknowledge ABORT within %s", abortDrain))
			return
		}
	}
}

// sendErr delivers a stream's terminal error without stranding the goroutine.
//
// The channel carries one slot of buffer precisely so this send lands even
// when the consumer has already walked away: a cancelled speaker abandons its
// chunk channel the moment its own context fires, and a blocked send here
// would hold the utterance lock for the lifetime of the daemon. The
// non-blocking attempt is tried first so a cancellation error is delivered
// deterministically rather than racing ctx.Done for the same select.
func sendErr(ctx context.Context, out chan<- tts.Chunk, err error) {
	select {
	case out <- tts.Chunk{Err: err}:
		return
	default:
	}
	select {
	case out <- tts.Chunk{Err: err}:
	case <-ctx.Done():
	}
}

// fail retires the child so the next utterance spawns a healthy one.
func (w *WarmSynthesizer) fail(child *kokoroChild, err error) {
	detail := err.Error()
	if tail := child.stderr.String(); tail != "" {
		detail += " (" + tail + ")"
	}
	w.supervisor().Discard(detail)
}

// ---------------------------------------------------------------- protocol

// frame is one decoded stdout frame of the serve protocol.
type frame struct {
	verb    string // CHUNK, END, ABORTED, ERROR
	payload []byte // CHUNK only
	text    string // ERROR only
}

// readFrames decodes frames for one utterance on its own goroutine, so the
// caller can react to cancellation while the helper is still synthesizing.
// The returned channel closes when the utterance reaches a terminal frame or
// the stream breaks; the error channel then carries the reason (nil on a clean
// end).
func readFrames(r *bufio.Reader, id string) (<-chan frame, <-chan error) {
	frames := make(chan frame)
	errc := make(chan error, 1)
	go func() {
		defer close(frames)
		for {
			verb, frameID, n, text, err := readHeader(r)
			if err != nil {
				errc <- err
				return
			}
			if verb == "CHUNK" {
				payload := make([]byte, n)
				if _, err := io.ReadFull(r, payload); err != nil {
					errc <- fmt.Errorf("kokoro helper truncated a PCM frame: %w", err)
					return
				}
				if frameID != id {
					continue // a stale utterance's tail; not ours to play
				}
				frames <- frame{verb: verb, payload: payload}
				continue
			}
			if frameID != id {
				continue
			}
			switch verb {
			case "END", "ABORTED":
				errc <- nil
				return
			case "ERROR":
				frames <- frame{verb: verb, text: text}
				errc <- nil
				return
			}
		}
	}()
	return frames, errc
}

// readHeader parses one header line: "VERB <id> [<n>|<message>]".
func readHeader(r *bufio.Reader) (verb, id string, n int, text string, err error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return "", "", 0, "", fmt.Errorf("kokoro helper closed its output")
		}
		return "", "", 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")
	verb, rest, _ := strings.Cut(line, " ")
	switch verb {
	case "CHUNK":
		id, sizeText, _ := strings.Cut(rest, " ")
		size, convErr := strconv.Atoi(strings.TrimSpace(sizeText))
		if convErr != nil || size < 0 || size > maxFrame {
			return "", "", 0, "", fmt.Errorf("kokoro helper sent an unusable frame header %q", line)
		}
		return verb, id, size, "", nil
	case "END", "ABORTED":
		return verb, strings.TrimSpace(rest), 0, "", nil
	case "ERROR":
		id, message, _ := strings.Cut(rest, " ")
		return verb, id, 0, message, nil
	}
	return "", "", 0, "", fmt.Errorf("kokoro helper spoke an unknown frame %q", line)
}

// ---------------------------------------------------------------- lifecycle

// startHelper spawns the helper in serve mode and completes the handshake.
func (w *WarmSynthesizer) startHelper(ctx context.Context) (*kokoroChild, error) {
	if err := w.Cold.Ready(); err != nil {
		return nil, err
	}
	proc, err := warm.StartProcess(warm.ProcessSpec{
		Path: w.Cold.python(),
		Args: []string{w.Cold.script(), "--serve",
			"--voice", w.Cold.voice(),
			"--speed", strconv.FormatFloat(w.Cold.speed(), 'f', 2, 64)},
		Env: append(os.Environ(),
			"JARVIX_KOKORO_MODEL="+w.Cold.modelPath(),
			"JARVIX_KOKORO_VOICES="+w.Cold.voicesPath(),
			// Unbuffered stdout would mangle the binary framing on some
			// interpreters; the helper flushes explicitly, and this keeps
			// Python from adding a second layer of buffering on top.
			"PYTHONUNBUFFERED=1"),
	})
	if err != nil {
		return nil, err
	}
	child := &kokoroChild{proc: proc, stderr: warm.DrainStderr(proc.Stderr, 5)}
	rate, err := w.handshake(ctx, child)
	if err != nil {
		child.Close()
		return nil, err
	}
	child.sampleRate = rate
	return child, nil
}

// handshake waits for READY, which is also the compatibility check: a helper
// script predating the serve protocol exits on the unknown flag, and the
// adapter falls back to spawning it per utterance as before.
func (w *WarmSynthesizer) handshake(ctx context.Context, child *kokoroChild) (int, error) {
	limit := w.StartTimeout
	if limit <= 0 {
		limit = defaultKokoroStart
	}
	type result struct {
		rate int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := child.proc.Stdout.ReadString('\n')
		if err != nil {
			done <- result{err: fmt.Errorf("kokoro helper produced no READY line: %w", err)}
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 || fields[0] != "READY" {
			done <- result{err: fmt.Errorf("kokoro helper does not speak the serve protocol (said %q); "+
				"re-run scripts/setup-kokoro.sh to install the current helper", strings.TrimSpace(line))}
			return
		}
		version, verErr := strconv.Atoi(fields[1])
		if verErr != nil || version != helperProtocol {
			done <- result{err: fmt.Errorf("kokoro helper speaks protocol %q, this build speaks %d; "+
				"re-run scripts/setup-kokoro.sh", fields[1], helperProtocol)}
			return
		}
		rate, rateErr := strconv.Atoi(fields[2])
		if rateErr != nil || rate <= 0 {
			done <- result{err: fmt.Errorf("kokoro helper announced an unusable sample rate %q", fields[2])}
			return
		}
		done <- result{rate: rate}
	}()

	timeout := time.NewTimer(limit)
	defer timeout.Stop()
	select {
	case r := <-done:
		return r.rate, r.err
	case <-child.proc.Exited():
		// Read whatever the handshake goroutine salvaged before the pipe died.
		select {
		case r := <-done:
			if r.err == nil {
				return r.rate, nil
			}
		case <-time.After(50 * time.Millisecond):
		}
		return 0, fmt.Errorf("kokoro helper exited during start-up: %s", child.stderr.String())
	case <-timeout.C:
		return 0, fmt.Errorf("kokoro helper did not load its model within %s: %s", limit, child.stderr.String())
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
