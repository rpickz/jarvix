package audio

import (
	"context"
	"fmt"
	"io"
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
	Device string
}

// Play implements Player. It pipes chunks into pw-play's stdin; closing stdin
// lets pw-play drain and exit, and cancellation kills the process for
// immediate silence.
func (p *PipeWirePlayer) Play(ctx context.Context, sampleRate, channels int, chunks <-chan []byte) error {
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
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pw-play (is PipeWire running?): %w", err)
	}

	writeErr := func() error {
		defer func() { _ = stdin.Close() }()
		first := true
		for {
			select {
			case c, ok := <-chunks:
				if !ok {
					return nil
				}
				if _, err := stdin.Write(c); err != nil {
					// pw-play died (or was cancelled); stop feeding it.
					return err
				}
				if first {
					// The last mark of the latency budget: audio has left
					// Jarvix. Everything after this is PipeWire's latency,
					// which we neither own nor can observe.
					first = false
					firstAudio(ctx)
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}()

	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("pw-play failed: %w", err)
	}
	if writeErr != nil && writeErr != io.ErrClosedPipe {
		return writeErr
	}
	return nil
}
