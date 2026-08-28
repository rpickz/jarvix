package audio

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Capture format: whisper.cpp expects 16 kHz mono s16.
const (
	captureRate     = 16000
	captureChannels = 1
)

// PipeWireRecorder captures the microphone via pw-record.
type PipeWireRecorder struct {
	// Dir is where WAV files are written (should be tmpfs, e.g.
	// $XDG_RUNTIME_DIR/jarvix).
	Dir string
	// Device is the PipeWire capture target; empty uses the default source.
	Device string
	// MaxDuration is a safety cap; recording stops itself after this long.
	MaxDuration time.Duration
}

// Start implements Recorder.
func (r *PipeWireRecorder) Start(ctx context.Context) (Recording, error) {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create recording dir: %w", err)
	}
	path := filepath.Join(r.Dir, fmt.Sprintf("rec-%d.wav", time.Now().UnixNano()))

	args := []string{
		"--rate", strconv.Itoa(captureRate),
		"--channels", strconv.Itoa(captureChannels),
		"--format", "s16",
	}
	if r.Device != "" {
		args = append(args, "--target", r.Device)
	}
	args = append(args, path)

	cmd := exec.Command("pw-record", args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pw-record (is PipeWire running?): %w", err)
	}

	rec := &pipewireRecording{cmd: cmd, path: path, done: make(chan struct{})}
	// Enforce the safety cap and honour context cancellation without keeping
	// a goroutine alive past the recording's end.
	maxDur := r.MaxDuration
	if maxDur <= 0 {
		maxDur = time.Minute
	}
	go func() {
		select {
		case <-ctx.Done():
			rec.Cancel()
		case <-time.After(maxDur):
			rec.interrupt() // stop capture but keep the file for Stop()
		case <-rec.done:
		}
	}()
	return rec, nil
}

type pipewireRecording struct {
	cmd  *exec.Cmd
	path string
	// done is created with the recording: the watchdog goroutine reads it
	// concurrently with Stop/Cancel, so it must never be assigned lazily
	// (that was a data race, caught by `go test -race` on this package).
	done chan struct{}

	mu       sync.Mutex
	finished bool
}

// interrupt sends SIGINT so pw-record finalises the WAV header cleanly, then
// waits for exit (with a kill fallback).
func (r *pipewireRecording) interrupt() {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.mu.Unlock()

	_ = r.cmd.Process.Signal(syscall.SIGINT)
	waited := make(chan struct{})
	go func() {
		_ = r.cmd.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		_ = r.cmd.Process.Kill()
		<-waited
	}
	close(r.done)
}

// Stop implements Recording.
func (r *pipewireRecording) Stop() (Clip, error) {
	r.interrupt()
	info, err := os.Stat(r.path)
	if err != nil {
		return Clip{}, fmt.Errorf("recording produced no file: %w", err)
	}
	// A WAV header alone is 44 bytes; anything close to that captured nothing.
	if info.Size() < 1024 {
		_ = os.Remove(r.path)
		return Clip{}, fmt.Errorf("recording was empty — is a microphone connected and unmuted?")
	}
	return Clip{WAVPath: r.path, SampleRate: captureRate, Channels: captureChannels}, nil
}

// Cancel implements Recording.
func (r *pipewireRecording) Cancel() {
	r.interrupt()
	_ = os.Remove(r.path)
}

// PipeWirePlayer renders PCM via pw-play.
type PipeWirePlayer struct {
	// Device is the PipeWire playback target; empty uses the default sink.
	// Empty is the deliberate default: a stream that follows the default is
	// one WirePlumber moves live when the default changes, so speech survives
	// a headset being switched mid-sentence without Jarvix doing anything.
	Device string
	// Log receives playback-recovery lines. Nil uses the default logger.
	Log *slog.Logger
}

// maxPlaybackRestarts bounds how many times one Play call respawns pw-play
// after it dies mid-stream. Each respawn binds afresh — to whatever the
// default sink is by then — so a device vanishing under a live utterance
// costs at most the audio pw-play had buffered, not the rest of the answer.
// The bound exists for the pathological case (no session manager, PipeWire
// crash-looping) where every respawn dies instantly: past it the failure is
// reported instead of retried forever.
const maxPlaybackRestarts = 3

func (p *PipeWirePlayer) logger() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Play implements Player. It pipes chunks into pw-play's stdin; closing stdin
// lets pw-play drain and exit, and cancellation kills the process for
// immediate silence.
//
// A pw-play that dies mid-stream — its sink removed while pinned, PipeWire
// restarting under it — is detected at the next write and replaced: a fresh
// pw-play binds to the current default sink and playback resumes with the
// exact bytes the dead process never accepted (issue #142). Bytes it had
// accepted but not yet rendered die with it; that loss is logged, never
// papered over, and nothing is ever written twice.
//
// On a terminal failure Play keeps consuming the channel until it closes, so
// a producer feeding a dead stream is released rather than blocked forever —
// the drain the Player contract promises and the fakes already honoured.
func (p *PipeWirePlayer) Play(ctx context.Context, sampleRate, channels int, chunks <-chan []byte) error {
	var leftover []byte
	announced := false
	for restarts := 0; ; restarts++ {
		resume, resumable, err := p.playOnce(ctx, sampleRate, channels, chunks, leftover, &announced)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if !resumable || restarts >= maxPlaybackRestarts {
			drainChunks(ctx, chunks)
			return err
		}
		leftover = resume
		p.logger().Warn("pw-play died mid-stream; resuming on the current default sink",
			"component", "audio", "error", err.Error(), "restart", restarts+1)
	}
}

// playOnce runs one pw-play process. A clean end (or cancellation, or an exit
// after every chunk was delivered) returns resumable false; a death with
// bytes still to play returns resumable true and the unwritten remainder, so
// the caller can hand it to a fresh process. announced keeps the FirstAudio
// trace mark to once per Play, however many processes serve it.
func (p *PipeWirePlayer) playOnce(ctx context.Context, sampleRate, channels int,
	chunks <-chan []byte, leftover []byte, announced *bool) (resume []byte, resumable bool, err error) {
	args := []string{
		"--rate", strconv.Itoa(sampleRate),
		"--channels", strconv.Itoa(channels),
		"--format", "s16",
		"--raw",
	}
	if p.Device != "" {
		args = append(args, "--target", p.Device)
	}
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, "pw-play", args...)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start pw-play (is PipeWire running?): %w", err)
	}

	// write feeds one buffer, marking the trace on the first byte that leaves
	// Jarvix. On failure it returns the bytes the pipe did not accept — the
	// byte-exact continuation point, so a resumed stream never skips a sample
	// boundary and never repeats one.
	write := func(b []byte) ([]byte, error) {
		n, werr := stdin.Write(b)
		if n > 0 && !*announced {
			// The last mark of the latency budget: audio has left Jarvix.
			// Everything after this is PipeWire's latency, which we neither
			// own nor can observe.
			*announced = true
			firstAudio(ctx)
		}
		if werr != nil {
			if n < 0 || n > len(b) {
				n = 0
			}
			return append([]byte(nil), b[n:]...), werr
		}
		return nil, nil
	}

	// died reaps a process that failed mid-stream and reports the remainder.
	died := func(rest []byte, werr error) ([]byte, bool, error) {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if waitErr == nil {
			waitErr = werr
		}
		return rest, true, fmt.Errorf("pw-play died mid-stream: %v", waitErr)
	}

	if len(leftover) > 0 {
		if rest, werr := write(leftover); werr != nil {
			return died(rest, werr)
		}
	}
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				_ = stdin.Close()
				err := cmd.Wait()
				if ctx.Err() != nil {
					return nil, false, ctx.Err()
				}
				if err != nil {
					return nil, false, fmt.Errorf("pw-play failed: %w", err)
				}
				return nil, false, nil
			}
			if rest, werr := write(c); werr != nil {
				return died(rest, werr)
			}
		case <-ctx.Done():
			_ = stdin.Close()
			_ = cmd.Wait()
			return nil, false, ctx.Err()
		}
	}
}

// drainChunks consumes the rest of a failed playback's stream so producers
// blocked on the channel are released. Without it a synthesizer feeding a
// dead player would block on the handoff until its session died — the exact
// wedge issue #142's diagnosis found: the speaker only reads the player's
// result after the chunk channel closes, so an early error return here left
// the whole rest of the answer stuck behind a channel nobody was reading.
func drainChunks(ctx context.Context, chunks <-chan []byte) {
	for {
		select {
		case _, ok := <-chunks:
			if !ok {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
