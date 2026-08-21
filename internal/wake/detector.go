package wake

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/rpickz/jarvix/internal/warm"
)

// This file is the production detector: an external helper process, scored
// frame by frame over its stdio.
//
// It is a separate process for the reason every Jarvix engine is (ADR 0002).
// A wake-word model means an ONNX or TensorFlow-Lite runtime, which means
// cgo, a C++ toolchain in the build, and a segfault in third-party native
// code taking jarvixd — and with it push-to-talk, the conversation, and the
// daemon's own supervision — down with it. A pipe costs one round trip per
// 80 ms frame and buys complete isolation, which is a trade Jarvix has
// already made four times.
//
// Protocol, version 1. The helper's stdout is line-oriented; its stdin is raw
// audio; stderr is free for diagnostics and is drained into the log tail.
//
//	stdout:  READY 1 <frame_samples> <model>\n   once, after the model loads
//	         SCORE <score>\n                     one per frame, 0..1
//	         ERROR <message>\n                   fatal; the helper is replaced
//
//	stdin:   <frame_samples> samples of 16 kHz mono s16le, repeatedly
//
// Thresholds, confirmation, and the refractory period are **not** in the
// protocol. They stay in Go (Policy) where they are tested and where changing
// them does not mean reinstalling a helper.

// DetectorProtocolVersion is the version this build speaks.
const DetectorProtocolVersion = 1

// ProcessDetector scores frames using an external helper process.
type ProcessDetector struct {
	name  string
	model string
	pid   int

	in    io.WriteCloser
	out   *bufio.Reader
	stop  func()
	tail  *warm.StderrTail
	frame []byte

	closeOnce sync.Once
}

// StartDetector spawns the helper named by argv and completes its handshake.
// It is the Spawn function the listener's supervisor calls, so a helper that
// crashes or is missing produces a backoff and a warning rather than a
// crash-loop.
func StartDetector(_ context.Context, argv []string, word string, log *slog.Logger) (*ProcessDetector, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("activation.wake_command is empty; nothing to run")
	}
	path, err := warm.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	args := append(append([]string(nil), argv[1:]...),
		"--word", word,
		"--frame", strconv.Itoa(FrameSamples),
		"--rate", strconv.Itoa(SampleRate))
	proc, err := warm.StartProcess(warm.ProcessSpec{Path: path, Args: args})
	if err != nil {
		return nil, err
	}
	// Models are chatty on stderr (ONNX provider banners); keep the last few
	// lines so a crash can be explained in the helper's own words.
	tail := warm.DrainStderr(proc.Stderr, 5)
	d, err := newProcessDetector(argv[0], proc.Stdin, proc.Stdout, proc.PID(), proc.Close, tail)
	if err != nil {
		proc.Close()
		if detail := tail.String(); detail != "" {
			return nil, fmt.Errorf("%w (%s)", err, detail)
		}
		return nil, err
	}
	if log != nil {
		log.Info("wake detector ready", "component", "wake",
			"detector", d.name, "model", d.model, "pid", d.pid)
	}
	return d, nil
}

// newProcessDetector wires an already-open transport and performs the
// handshake. It is the seam the protocol tests drive with in-memory pipes, so
// every branch of the wire format is covered without spawning anything.
func newProcessDetector(name string, in io.WriteCloser, out *bufio.Reader, pid int,
	stop func(), tail *warm.StderrTail) (*ProcessDetector, error) {
	line, err := out.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("%s did not complete its handshake: %w", name, err)
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 3 || fields[0] != "READY" {
		return nil, fmt.Errorf("%s sent %q, expected a READY line", name, strings.TrimSpace(line))
	}
	version, err := strconv.Atoi(fields[1])
	if err != nil || version != DetectorProtocolVersion {
		return nil, fmt.Errorf("%s speaks wake protocol %q; this build speaks %d",
			name, fields[1], DetectorProtocolVersion)
	}
	// A frame-size mismatch is the silent failure worth refusing loudly: the
	// helper would score windows that are not the ones Jarvix thinks it sent,
	// and the only symptom would be a wake word that never fires.
	samples, err := strconv.Atoi(fields[2])
	if err != nil || samples != FrameSamples {
		return nil, fmt.Errorf("%s wants %q-sample frames; Jarvix sends %d",
			name, fields[2], FrameSamples)
	}
	model := ""
	if len(fields) > 3 {
		model = strings.Join(fields[3:], " ")
	}
	return &ProcessDetector{
		name: name, model: model, pid: pid,
		in: in, out: out, stop: stop, tail: tail,
		frame: make([]byte, FrameBytes),
	}, nil
}

// Name implements Detector.
func (d *ProcessDetector) Name() string {
	if d.model == "" {
		return d.name
	}
	return d.name + " (" + d.model + ")"
}

// PID implements Detector.
func (d *ProcessDetector) PID() int { return d.pid }

// Score implements Detector: one frame in, one score out.
func (d *ProcessDetector) Score(frame []int16) (float64, error) {
	if len(frame) != FrameSamples {
		return 0, fmt.Errorf("wake detector expects %d samples, got %d", FrameSamples, len(frame))
	}
	for i, s := range frame {
		u := uint16(s)
		d.frame[2*i] = byte(u)
		d.frame[2*i+1] = byte(u >> 8)
	}
	if _, err := d.in.Write(d.frame); err != nil {
		return 0, d.wrap(fmt.Errorf("writing a frame to %s: %w", d.name, err))
	}
	line, err := d.out.ReadString('\n')
	if err != nil {
		return 0, d.wrap(fmt.Errorf("reading a score from %s: %w", d.name, err))
	}
	fields := strings.Fields(strings.TrimSpace(line))
	switch {
	case len(fields) == 0:
		return 0, fmt.Errorf("%s sent an empty line", d.name)
	case fields[0] == "ERROR":
		return 0, fmt.Errorf("%s: %s", d.name, strings.Join(fields[1:], " "))
	case fields[0] != "SCORE" || len(fields) < 2:
		return 0, fmt.Errorf("%s sent %q, expected SCORE", d.name, strings.TrimSpace(line))
	}
	score, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%s sent an unreadable score %q", d.name, fields[1])
	}
	return score, nil
}

// Close implements Detector.
func (d *ProcessDetector) Close() {
	d.closeOnce.Do(func() {
		if d.stop != nil {
			d.stop()
		}
	})
}

// wrap adds the helper's own last words to a pipe failure, which is otherwise
// reported as a bare "broken pipe" that explains nothing.
func (d *ProcessDetector) wrap(err error) error {
	if d.tail == nil {
		return err
	}
	if detail := d.tail.String(); detail != "" {
		return fmt.Errorf("%w (%s)", err, detail)
	}
	return err
}

// DetectorReady reports whether the configured helper can be run at all. It
// is the cheap check the daemon makes before starting the listener and that
// `jarvix doctor` reports: a missing helper should degrade to push-to-talk
// with one actionable line, not with a supervisor quietly retrying forever.
func DetectorReady(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("activation.wake_command is empty")
	}
	if _, err := warm.LookPath(argv[0]); err != nil {
		return err
	}
	return nil
}
