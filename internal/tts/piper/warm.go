package piper

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/warm"
)

// WarmSynthesizer speaks through a long-lived piper process instead of one per
// response (ADR 0018).
//
// Piper's own CLI is the protocol: given --output_dir it reads one utterance
// per line of stdin, writes a WAV, and prints that file's path on stdout — for
// as long as stdin stays open. That is a complete line-wise request/response
// protocol with an unambiguous end-of-utterance marker, which the streaming
// --output_raw mode does not have (raw bytes arrive with nothing to say where
// one sentence ends and the next begins). The trade is deliberate: a warm
// worker gives back piper's ~110ms voice load on every answer, and costs the
// ability to start playing a sentence before it is fully rendered. Piper's
// real-time factor is ~0.03, so a sentence renders in a few tens of
// milliseconds — far less than the load it replaces.
//
// Cancellation: piper's protocol has no abort, and this is the case the ADR
// calls out. It does not need one. Nothing is written until the utterance is
// complete, so an interrupted sentence produces no audio at all — silence is
// immediate by construction. The adapter simply stops waiting, drains the
// abandoned result in the background (deleting the WAV), and keeps the worker
// warm; a piper that has not produced it within abortDrain is killed and
// respawned, which is the documented fallback rather than the normal path.
type WarmSynthesizer struct {
	// Cold is the per-response adapter: the source of the binary and voice,
	// and the fallback whenever the warm path is unavailable. Required.
	Cold *Synthesizer
	// Dir is where the worker's WAV files are written; it should be tmpfs
	// (the daemon passes its runtime directory). Empty uses the system
	// temporary directory.
	Dir string
	// MemoryCap and IdleAfter configure the supervisor ([performance]).
	MemoryCap uint64
	IdleAfter time.Duration
	// Log receives warm-worker lifecycle lines. Nil uses the default logger.
	Log *slog.Logger
	// SettleWindow is how long a freshly spawned piper is watched for an
	// immediate exit before it is trusted. Zero uses piperSettle.
	SettleWindow time.Duration
	// AbortDrain bounds how long a cancelled utterance may take to arrive
	// before the worker is assumed wedged and replaced. Zero uses abortDrain.
	AbortDrain time.Duration

	once sync.Once
	sup  *warm.Supervisor[*piperChild]
	// spawn overrides child creation for tests; production uses startWorker.
	spawn func(context.Context) (*piperChild, error)

	// utter serialises utterances: replies are matched to requests by order
	// on a single stdout, so only one may be in flight.
	utter sync.Mutex
}

const (
	// abortDrain is how long an abandoned utterance may take to appear before
	// the worker is assumed wedged and replaced.
	abortDrain = 5 * time.Second
	// pcmChunk is the PCM handed to the player at a time, matching the cold
	// adapter's read size so playback starts on the same granularity.
	pcmChunk = 8192
)

// piperChild is one running piper worker plus the scratch directory it writes
// utterances into.
type piperChild struct {
	proc   *warm.Process
	dir    string
	stderr *warm.StderrTail
}

func (c *piperChild) PID() int {
	if c.proc == nil {
		return 0
	}
	return c.proc.PID()
}

func (c *piperChild) Close() {
	if c.proc != nil {
		c.proc.Close()
	}
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
}

// Name implements tts.Synthesizer.
func (w *WarmSynthesizer) Name() string { return "piper" }

// ResolveVoice delegates to the cold adapter, which owns voice lookup.
func (w *WarmSynthesizer) ResolveVoice() error { return w.Cold.ResolveVoice() }

func (w *WarmSynthesizer) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *WarmSynthesizer) supervisor() *warm.Supervisor[*piperChild] {
	w.once.Do(func() {
		spawn := w.spawn
		if spawn == nil {
			spawn = w.startWorker
		}
		w.sup = &warm.Supervisor[*piperChild]{
			Name:      "piper",
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

// Close shuts the warm worker down and removes its scratch directory.
func (w *WarmSynthesizer) Close() error {
	w.supervisor().Close()
	return nil
}

// Speak implements tts.Synthesizer.
func (w *WarmSynthesizer) Speak(ctx context.Context, req tts.Request) (tts.Format, <-chan tts.Chunk, error) {
	if err := w.Cold.ResolveVoice(); err != nil {
		return tts.Format{}, nil, err
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return tts.Format{}, nil, fmt.Errorf("nothing to speak")
	}
	// One utterance is one line: piper's protocol says so, and so does ours.
	text = strings.Join(strings.Fields(text), " ")

	w.utter.Lock()
	child, err := w.supervisor().Get(ctx)
	if err != nil {
		w.utter.Unlock()
		w.logger().Debug("warm piper unavailable; spawning piper for this response",
			"component", "tts", "error", err.Error())
		return w.Cold.Speak(ctx, req)
	}
	if _, err := fmt.Fprintln(child.proc.Stdin, text); err != nil {
		w.utter.Unlock()
		w.supervisor().Discard(fmt.Sprintf("piper stdin closed: %v", err))
		w.logger().Warn("warm piper failed; spawning piper for this response",
			"component", "tts", "error", err.Error())
		return w.Cold.Speak(ctx, req)
	}

	format := tts.Format{SampleRate: w.Cold.sampleRate, Channels: 1}
	// Capacity one so a terminal error always lands, even when the speaker
	// has already stopped reading because its own context fired.
	out := make(chan tts.Chunk, 1)
	paths, readErr := readPaths(child)

	go func() {
		defer close(out)
		w.stream(ctx, child, req, paths, readErr, out)
	}()
	return format, out, nil
}

// stream waits for the rendered utterance, then feeds it to the player.
func (w *WarmSynthesizer) stream(ctx context.Context, child *piperChild, req tts.Request,
	paths <-chan string, readErr <-chan error, out chan<- tts.Chunk) {
	select {
	case path, ok := <-paths:
		if !ok {
			err := <-readErr
			w.utter.Unlock()
			w.fail(child, err)
			if ctx.Err() == nil {
				// Nothing was heard — piper emits only completed utterances —
				// so a fresh process can still deliver this response intact.
				w.coldFallback(ctx, req, out)
				return
			}
			sendErr(ctx, out, err)
			return
		}
		defer w.utter.Unlock()
		pcm, err := readWAVData(path)
		_ = os.Remove(path)
		if err != nil {
			w.fail(child, err)
			if ctx.Err() == nil {
				w.coldFallback(ctx, req, out)
				return
			}
			sendErr(ctx, out, err)
			return
		}
		w.supervisor().Release()
		for start := 0; start < len(pcm); start += pcmChunk {
			end := min(start+pcmChunk, len(pcm))
			select {
			case out <- tts.Chunk{PCM: pcm[start:end]}:
			case <-ctx.Done():
				sendErr(ctx, out, ctx.Err())
				return
			}
		}
	case <-ctx.Done():
		// Interrupted before piper produced anything: no audio was ever
		// emitted, so the user already has their silence. Give the abandoned
		// utterance its own goroutine to land in, and hand the lock over with
		// it so the next question cannot read this one's reply.
		go w.drainAbandoned(child, paths, readErr)
		sendErr(ctx, out, ctx.Err())
	}
}

// coldFallback renders a response with a fresh piper process and forwards it,
// after the warm worker failed before producing audio.
func (w *WarmSynthesizer) coldFallback(ctx context.Context, req tts.Request, out chan<- tts.Chunk) {
	w.logger().Warn("warm piper produced nothing; speaking this response with a fresh process",
		"component", "tts")
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

// drainAbandoned consumes and deletes the result of a cancelled utterance so
// the worker stays warm and its scratch directory stays empty. It owns the
// utterance lock until the reply lands.
func (w *WarmSynthesizer) drainAbandoned(child *piperChild, paths <-chan string, readErr <-chan error) {
	defer w.utter.Unlock()
	limit := w.AbortDrain
	if limit <= 0 {
		limit = abortDrain
	}
	timeout := time.NewTimer(limit)
	defer timeout.Stop()
	select {
	case path, ok := <-paths:
		if !ok {
			w.fail(child, <-readErr)
			return
		}
		_ = os.Remove(path)
		w.supervisor().Release()
	case <-timeout.C:
		// piper cannot be told to stop mid-utterance, so a worker that has not
		// finished by now is replaced outright — the kill-and-respawn fallback.
		w.fail(child, fmt.Errorf("piper did not finish a cancelled utterance within %s", limit))
	}
}

// fail retires the child so the next response spawns a healthy worker.
func (w *WarmSynthesizer) fail(child *piperChild, err error) {
	detail := err.Error()
	if tail := child.stderr.String(); tail != "" {
		detail += " (" + tail + ")"
	}
	w.supervisor().Discard(detail)
	w.logger().Warn("warm piper retired; this response used a fresh process",
		"component", "tts", "error", detail)
}

// readPaths reads exactly one output path — piper's reply to one utterance —
// on its own goroutine, so the caller can react to cancellation while piper is
// still rendering.
func readPaths(child *piperChild) (<-chan string, <-chan error) {
	paths := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		defer close(paths)
		line, err := child.proc.Stdout.ReadString('\n')
		if err != nil {
			errc <- fmt.Errorf("piper closed its output: %w", err)
			return
		}
		path := strings.TrimSpace(line)
		if path == "" {
			errc <- fmt.Errorf("piper printed no output path")
			return
		}
		paths <- path
	}()
	return paths, errc
}

// startWorker spawns the long-lived piper and waits for its voice to load.
func (w *WarmSynthesizer) startWorker(ctx context.Context) (*piperChild, error) {
	if err := w.Cold.ResolveVoice(); err != nil {
		return nil, err
	}
	bin := w.Cold.Binary
	if bin == "" {
		bin = "piper-tts"
	}
	path, err := warm.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%w; install piper-tts or set performance.warm_engines = false", err)
	}
	dir, err := os.MkdirTemp(w.Dir, "jarvix-piper-")
	if err != nil {
		return nil, fmt.Errorf("create piper scratch directory: %w", err)
	}
	proc, err := warm.StartProcess(warm.ProcessSpec{
		Path: path,
		Args: []string{"--model", w.Cold.modelPath, "--output_dir", dir, "--quiet"},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	child := &piperChild{proc: proc, dir: dir, stderr: warm.DrainStderr(proc.Stderr, 5)}

	// piper prints nothing until it has an utterance, so readiness is "the
	// process is still alive after it has had time to fail". Waiting for a
	// probe utterance would burn a synthesis on every start; watching for an
	// immediate exit is enough, because a missing or broken voice exits at
	// once and anything subtler surfaces on the first real sentence.
	settle := w.SettleWindow
	if settle <= 0 {
		settle = piperSettle
	}
	select {
	case <-proc.Exited():
		child.Close()
		return nil, fmt.Errorf("piper exited during start-up: %s", child.stderr.String())
	case <-ctx.Done():
		child.Close()
		return nil, ctx.Err()
	case <-time.After(settle):
	}
	return child, nil
}

// piperSettle is how long a freshly spawned piper is watched for an immediate
// exit (a missing voice, an unreadable model) before it is trusted. Short: a
// failure here is a crash, not a slow load, and the first utterance would
// surface anything else.
const piperSettle = 50 * time.Millisecond

// readWAVData extracts the PCM payload from a RIFF/WAVE file by walking its
// chunks. Not "skip 44 bytes": piper is free to emit a LIST or fact chunk, and
// a fixed offset would turn metadata into a burst of noise.
func readWAVData(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read piper output: %w", err)
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("piper produced %s, which is not a WAVE file", filepath.Base(path))
	}
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		if size < 0 || body+size > len(data) {
			size = len(data) - body // a truncated final chunk is still audio
		}
		if id == "data" {
			out := make([]byte, size)
			copy(out, data[body:body+size])
			return out, nil
		}
		pos = body + size
		if size%2 == 1 {
			pos++ // RIFF chunks are word-aligned
		}
	}
	return nil, fmt.Errorf("piper produced a WAVE file with no data chunk")
}
