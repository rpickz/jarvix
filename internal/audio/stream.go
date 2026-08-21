package audio

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
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

// This file is the *streaming* half of capture, added for background
// wake-word listening (ADR 0024).
//
// The Recorder/Recording pair above records to a file and hands the finished
// WAV to whisper. That shape is right for push-to-talk — a capture has a
// beginning and an end, decided by a human finger — and completely wrong for
// listening in the background, where audio must be examined as it arrives and
// then, overwhelmingly, thrown away. A wake listener that wrote WAV files
// would be writing everything it ever heard to disk, which is the one thing
// the feature promises never to happen.
//
// So the streaming path never touches the filesystem: pw-record writes raw
// s16le PCM to its stdout, Jarvix reads it frame by frame into a fixed
// in-memory buffer, and only audio that follows a wake detection is ever
// materialised — by WriteWAV, into the tmpfs runtime directory, deleted after
// transcription like every other recording.

// StreamBufferSize is the read buffer between pw-record and Jarvix. Sized to
// hold roughly a quarter-second of 16 kHz mono s16 so a scheduling hiccup in
// the daemon cannot make the kernel drop samples, and no larger: a big buffer
// here only means older audio sitting in memory for longer.
const StreamBufferSize = 8 << 10

// PipeWireStreamer opens a continuous microphone stream via pw-record. It is
// the production implementation of the source the wake listener reads.
type PipeWireStreamer struct {
	// Device is the PipeWire capture target; empty follows the default
	// source, which is what makes a headset being plugged in or unplugged
	// recoverable: the old pw-record exits, and the next one attaches to
	// whatever the default has become.
	Device string
}

// Open starts pw-record and returns its PCM stream. The context bounds
// start-up only; the returned stream outlives it and is owned by the caller,
// who must Close it.
func (s *PipeWireStreamer) Open(context.Context) (*PipeWireStream, error) {
	if _, err := exec.LookPath("pw-record"); err != nil {
		return nil, fmt.Errorf("pw-record not found on PATH (is PipeWire installed?)")
	}
	args := []string{
		"--rate", strconv.Itoa(captureRate),
		"--channels", strconv.Itoa(captureChannels),
		"--format", "s16",
		"--raw",
	}
	if s.Device != "" {
		args = append(args, "--target", s.Device)
	}
	args = append(args, "-") // stdout

	cmd := exec.Command("pw-record", args...)
	// Its own process group, for the same reason every other Jarvix child has
	// one (warm.Process): Close kills a group, so nothing can be left behind
	// holding the microphone open. "No capture process is running" is a
	// promise this feature makes, and a group kill is how it is kept.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// pw-record is quiet in raw mode, but an undrained stderr pipe eventually
	// blocks the child, so it goes to the same place a daemon's own noise
	// does rather than into a pipe nobody reads.
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("start pw-record (is PipeWire running?): %w", err)
	}

	stream := &PipeWireStream{cmd: cmd, pipe: stdout,
		buf: bufio.NewReaderSize(stdout, StreamBufferSize), waited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(stream.waited)
	}()
	return stream, nil
}

// PipeWireStream is a live microphone stream: raw 16 kHz mono s16le PCM,
// straight off pw-record's stdout.
type PipeWireStream struct {
	cmd    *exec.Cmd
	pipe   io.ReadCloser
	buf    *bufio.Reader
	waited chan struct{}

	closeOnce sync.Once
}

// Read implements io.Reader.
func (s *PipeWireStream) Read(p []byte) (int, error) { return s.buf.Read(p) }

// PID is the capture process's id — the number `jarvix status` prints and a
// suspicious user greps for in `ps`. Zero once the stream is closed.
func (s *PipeWireStream) PID() int {
	select {
	case <-s.waited:
		return 0
	default:
	}
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Close stops capture and returns only once the process is reaped, so a
// caller that has closed the stream can state — not hope — that no capture
// process of Jarvix's is running.
//
// It goes straight to SIGKILL, unlike the engine helpers, which get a polite
// SIGTERM and half a second to finish. There is nothing to finish: the audio
// in flight is audio the user just asked Jarvix to stop listening to, and
// `jarvix mute` promises to take effect in milliseconds. Safe to call twice,
// and safe on a process that has already exited.
func (s *PipeWireStream) Close() {
	s.closeOnce.Do(func() {
		if s.cmd.Process != nil {
			pid := s.cmd.Process.Pid
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				// Setpgid can only fail in ways that leave a plain child.
				_ = s.cmd.Process.Kill()
			}
		}
		<-s.waited
		_ = s.pipe.Close()
	})
}

// wavHeaderSize is the size of the canonical 44-byte RIFF/WAVE header.
const wavHeaderSize = 44

// WriteWAV writes signed 16-bit PCM as a RIFF/WAVE file, so a buffer held in
// memory can be handed to whisper.cpp, which only reads files.
//
// This is the single point at which captured audio becomes a file, and it is
// deliberately the *only* one on the wake path: the caller passes the
// utterance and nothing else, the destination is the tmpfs runtime directory,
// and the engine deletes it as soon as transcription finishes. The file is
// created 0600 so nothing else on the machine can read it in the interim.
func WriteWAV(path string, pcm []int16, sampleRate, channels int) error {
	if sampleRate <= 0 || channels <= 0 {
		return fmt.Errorf("invalid WAV format: %d Hz, %d channels", sampleRate, channels)
	}
	dataBytes := len(pcm) * 2
	out := make([]byte, wavHeaderSize, wavHeaderSize+dataBytes)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(wavHeaderSize-8+dataBytes))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM, uncompressed
	binary.LittleEndian.PutUint16(out[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*channels*2)) // byte rate
	binary.LittleEndian.PutUint16(out[32:34], uint16(channels*2))            // block align
	binary.LittleEndian.PutUint16(out[34:36], 16)                            // bits per sample
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataBytes))

	var scratch [2]byte
	for _, s := range pcm {
		binary.LittleEndian.PutUint16(scratch[:], uint16(s))
		out = append(out, scratch[0], scratch[1])
	}
	return os.WriteFile(path, out, 0o600)
}

// SaveClip writes captured PCM into dir and presents it as a finished
// Recording, so audio that was gathered outside the Recorder interface — a
// wake-word utterance, held in memory until this moment — reaches the session
// engine through exactly the same path a push-to-talk capture does.
//
// dir should be the tmpfs runtime directory: the engine deletes the file as
// soon as transcription finishes, and until then it must not be somewhere
// that survives a reboot.
func SaveClip(dir string, pcm []int16) (Recording, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create recording dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("wake-%d.wav", time.Now().UnixNano()))
	if err := WriteWAV(path, pcm, captureRate, captureChannels); err != nil {
		return nil, fmt.Errorf("write capture: %w", err)
	}
	return clipRecording{clip: Clip{WAVPath: path, SampleRate: captureRate, Channels: captureChannels}}, nil
}

// clipRecording is a Recording over a file that already exists.
type clipRecording struct{ clip Clip }

// Stop implements Recording.
func (r clipRecording) Stop() (Clip, error) { return r.clip, nil }

// Cancel implements Recording: the capture is not wanted, so it stops
// existing. Nothing else holds a reference to the file, and it lives on
// tmpfs, so this is an unlink and no more.
func (r clipRecording) Cancel() { _ = os.Remove(r.clip.WAVPath) }
