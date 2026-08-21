// Package warm keeps an external engine process alive between sessions.
//
// ADR 0002 made every engine an external process, spawned per use: a crash in
// native code cannot take the daemon down, and each engine upgrades on its own
// schedule. That isolation is worth keeping, but paying the model load on every
// interaction is not — whisper reloads base.en per transcription and the Kokoro
// helper boots Python + ONNX per utterance, which together dominate the
// release-to-first-audio budget.
//
// The supervised persistent worker (ADR 0018) keeps the process boundary and
// removes the repeated cold start: one long-lived child per engine, owned by
// the daemon, restarted transparently when it dies or outgrows its memory cap,
// and reaped when nobody has spoken for a while. Callers never see the
// lifecycle — Get hands back a ready child or an error, and an error simply
// means "fall back to the cold path this once".
package warm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Child is one warm engine process the Supervisor owns. Adapters implement it
// over whatever protocol their engine speaks; the supervisor only needs to
// know how to measure it and how to kill it.
type Child interface {
	// PID is the process id of the child, used for memory accounting. A
	// non-positive value opts the child out of the memory cap.
	PID() int
	// Close terminates the process (and every descendant) and releases the
	// pipes, files, and goroutines it owns. It must be safe to call twice.
	Close()
}

// Defaults for the supervisor's own timing. They are deliberately not
// configuration: the user-facing knobs are the memory cap and the idle-reap
// period, and a restart storm should be invisible rather than tunable.
const (
	// backoffBase is the wait after the first failed spawn; it doubles per
	// consecutive failure up to backoffMax. Between attempts Get fails fast so
	// the caller takes the cold path instead of blocking on a dead engine.
	backoffBase = 500 * time.Millisecond
	backoffMax  = 30 * time.Second
)

// Supervisor owns at most one warm child of a given engine.
//
// Every method is safe for concurrent use, but callers are expected to
// serialise their own protocol traffic: one child, one conversation at a time.
type Supervisor[C Child] struct {
	// Name labels the engine in logs and in `jarvix doctor` ("whisper",
	// "kokoro", "piper").
	Name string
	// Spawn starts a child and completes its handshake. The context bounds
	// start-up only — the returned child must outlive it, because it belongs
	// to the supervisor from that moment on.
	Spawn func(ctx context.Context) (C, error)
	// MemoryCap is the resident-set ceiling in bytes. A child over it is
	// retired before it is handed out again, so a leaking engine costs one
	// cold start rather than the machine's memory. Zero disables the check.
	MemoryCap uint64
	// IdleAfter reaps a child that has not been used for this long, giving the
	// memory back to the desktop; the next session pays one cold start. Zero
	// keeps the child until the daemon exits.
	IdleAfter time.Duration
	// Log receives the one-line warnings about restarts and reaping. Nil uses
	// the default logger.
	Log *slog.Logger

	// Injectable seams — production leaves them nil. Tests replace them so the
	// idle reaper, the memory cap, and the restart backoff are all exercised
	// without a single sleep.
	Now   func() time.Time
	RSS   func(pid int) (uint64, error)
	Timer func(d time.Duration) (<-chan time.Time, func())

	mu       sync.Mutex
	child    C
	live     bool
	closed   bool
	started  time.Time
	lastUsed time.Time

	// Restart bookkeeping. failures counts the current streak of failed
	// spawns (reset by a success) and drives the backoff; restarts counts
	// every child after the first, which is what doctor reports.
	failures    int
	restarts    int
	nextAttempt time.Time
	lastErr     string

	// idle reaping: stopIdle cancels the armed timer, and idleGen makes a
	// timer that fires after its child was already replaced a no-op.
	stopIdle func()
	idleGen  uint64
}

// Status is a snapshot of one engine's warm worker, as reported by
// `jarvix doctor` and `jarvix status`.
type Status struct {
	// Name is the engine label ("whisper", "kokoro", "piper").
	Name string `json:"name"`
	// Running reports whether a child is currently warm.
	Running bool `json:"running"`
	// PID is the warm child's process id, 0 when nothing is running.
	PID int `json:"pid"`
	// RSSBytes is the child's current resident set size, 0 when unknown.
	RSSBytes uint64 `json:"rss_bytes"`
	// UptimeSec is how long the current child has been warm.
	UptimeSec int `json:"uptime_sec"`
	// Restarts counts children after the first: crashes, memory-cap retires,
	// and post-reap re-spawns.
	Restarts int `json:"restarts"`
	// LastError is the most recent spawn or crash message, "" when clean.
	LastError string `json:"last_error"`
}

func (s *Supervisor[C]) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Supervisor[C]) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Supervisor[C]) rss(pid int) (uint64, error) {
	if s.RSS != nil {
		return s.RSS(pid)
	}
	return ResidentBytes(pid)
}

func (s *Supervisor[C]) timer(d time.Duration) (<-chan time.Time, func()) {
	if s.Timer != nil {
		return s.Timer(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Get returns the warm child, spawning it if necessary.
//
// It never blocks on a broken engine: after a failed spawn the next attempt is
// refused until the backoff elapses, so a caller that has a cold path (every
// adapter here does) falls back immediately instead of stalling the session.
func (s *Supervisor[C]) Get(ctx context.Context) (C, error) {
	var zero C
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return zero, fmt.Errorf("%s warm worker is shut down", s.Name)
	}
	if s.Spawn == nil {
		s.mu.Unlock()
		return zero, fmt.Errorf("%s warm worker has no spawn function", s.Name)
	}
	s.disarmIdleLocked()

	if s.live {
		// A child that has outgrown its cap is retired here rather than by a
		// background watchdog: the check costs one small /proc read on a path
		// that is about to do far more expensive work anyway, and it happens
		// exactly where the replacement can be made invisible.
		if over, size := s.overCapLocked(); over {
			s.retireLocked(fmt.Sprintf("resident memory %s exceeds the %s cap",
				humanBytes(size), humanBytes(s.MemoryCap)))
		}
	}
	if s.live {
		child := s.child
		s.lastUsed = s.now()
		s.mu.Unlock()
		return child, nil
	}
	if now := s.now(); now.Before(s.nextAttempt) {
		err := fmt.Errorf("%s warm worker is restarting (%s)", s.Name, s.lastErr)
		s.mu.Unlock()
		return zero, err
	}
	s.mu.Unlock()

	// Spawn off the lock: a handshake can take a second, and Status() must
	// stay answerable while an engine is loading its model.
	child, err := s.Spawn(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failures++
		s.lastErr = err.Error()
		s.nextAttempt = s.now().Add(backoffFor(s.failures))
		// One warning per streak: a machine without the engine installed must
		// not fill the journal with a line per interaction.
		if s.failures == 1 {
			s.log().Warn("warm worker could not start; falling back to a cold engine",
				"component", "warm", "engine", s.Name, "error", err.Error())
		} else {
			s.log().Debug("warm worker still down", "component", "warm",
				"engine", s.Name, "failures", s.failures, "error", err.Error())
		}
		return zero, err
	}
	if s.closed {
		// Shutdown raced the handshake; the child belongs to nobody.
		child.Close()
		return zero, fmt.Errorf("%s warm worker is shut down", s.Name)
	}
	s.child, s.live = child, true
	s.failures = 0
	s.lastErr = ""
	s.started = s.now()
	s.lastUsed = s.started
	s.log().Info("warm worker ready", "component", "warm",
		"engine", s.Name, "pid", child.PID(), "restarts", s.restarts)
	return child, nil
}

// Release tells the supervisor the caller has finished with the child for now,
// arming the idle reaper. Callers that never call it simply keep their worker
// until shutdown.
func (s *Supervisor[C]) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live || s.closed {
		return
	}
	s.lastUsed = s.now()
	s.armIdleLocked()
}

// Discard retires the current child — the caller found it broken (a closed
// pipe, a protocol violation, a crash). The next Get spawns a replacement;
// that session pays a cold start and this is the one line about it.
func (s *Supervisor[C]) Discard(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live {
		return
	}
	s.retireLocked(reason)
}

// Close shuts the supervisor down for good and kills the warm child. Every
// engine process jarvixd started is a supervised child of it: nothing survives
// the daemon, on a clean exit or a reload that rebuilt the adapters.
func (s *Supervisor[C]) Close() {
	s.mu.Lock()
	s.closed = true
	s.disarmIdleLocked()
	if !s.live {
		s.mu.Unlock()
		return
	}
	child := s.child
	s.live = false
	var zero C
	s.child = zero
	// Terminate off the lock — killing a child waits on it, and Status() must
	// not block behind a shutdown — but *do* wait for it here rather than
	// leaving it to a goroutine nobody joins. Close has to mean closed: the
	// child owns a scratch directory it removes on the way out, and a caller
	// that returned early would leave it behind. That is the daemon's
	// shutdown path (which now drains under a deadline, ADR 0018 + #29) and a
	// config reload that rebuilds the adapters, so an unwaited teardown
	// litters the runtime directory a little more on every restart.
	s.mu.Unlock()
	child.Close()
}

// Status reports the worker for doctor and status.get.
func (s *Supervisor[C]) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Name: s.Name, Restarts: s.restarts, LastError: s.lastErr}
	if !s.live {
		return st
	}
	st.Running = true
	st.PID = s.child.PID()
	st.UptimeSec = int(s.now().Sub(s.started) / time.Second)
	if size, err := s.rss(st.PID); err == nil {
		st.RSSBytes = size
	}
	return st
}

// ------------------------------------------------------------------ internals

// retireLocked closes the current child and records why, so the next Get
// spawns a fresh one. It is the single path every replacement goes through:
// crash, memory cap, and idle reap all end up here.
func (s *Supervisor[C]) retireLocked(reason string) {
	child := s.child
	var zero C
	s.child, s.live = zero, false
	s.restarts++
	s.lastErr = reason
	s.disarmIdleLocked()
	s.log().Warn("warm worker retired; the next interaction pays a cold start",
		"component", "warm", "engine", s.Name, "pid", child.PID(), "reason", reason)
	go child.Close()
}

// overCapLocked reports whether the live child exceeds MemoryCap. An
// unreadable /proc entry is not a reason to kill a working engine, so a read
// error leaves the child alone.
func (s *Supervisor[C]) overCapLocked() (bool, uint64) {
	if s.MemoryCap == 0 || s.child.PID() <= 0 {
		return false, 0
	}
	size, err := s.rss(s.child.PID())
	if err != nil {
		return false, 0
	}
	return size > s.MemoryCap, size
}

// armIdleLocked starts the reap countdown for the live child. The generation
// counter means a timer that fires after its child was already replaced does
// nothing, which is what keeps reaping race-free without a second lock.
func (s *Supervisor[C]) armIdleLocked() {
	if s.IdleAfter <= 0 {
		return
	}
	s.disarmIdleLocked()
	s.idleGen++
	gen := s.idleGen
	fire, stop := s.timer(s.IdleAfter)
	cancelled := make(chan struct{})
	s.stopIdle = func() {
		stop()
		close(cancelled)
	}
	go func() {
		select {
		case <-fire:
		case <-cancelled:
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || !s.live || s.idleGen != gen {
			return
		}
		s.stopIdle = nil
		child := s.child
		var zero C
		s.child, s.live = zero, false
		s.restarts++
		s.log().Info("warm worker reaped after idle period",
			"component", "warm", "engine", s.Name, "pid", child.PID(),
			"idle_sec", int(s.IdleAfter/time.Second))
		go child.Close()
	}()
}

func (s *Supervisor[C]) disarmIdleLocked() {
	if s.stopIdle != nil {
		s.stopIdle()
		s.stopIdle = nil
	}
}

// backoffFor doubles the wait per consecutive failure, capped, so an engine
// that is missing entirely is retried rarely instead of on every session.
func backoffFor(failures int) time.Duration {
	d := backoffBase
	for i := 1; i < failures && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

// ResidentBytes reads a process's resident set size from /proc/<pid>/statm.
// statm rather than status: two fields to parse instead of fifty lines, on a
// path walked before every transcription and every utterance.
func ResidentBytes(pid int) (uint64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, fmt.Errorf("unreadable /proc/%d/statm", pid)
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * uint64(os.Getpagesize()), nil
}

// humanBytes renders a byte count the way a warning line should read.
func humanBytes(n uint64) string {
	const mb = 1 << 20
	if n < mb {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%d MB", n/mb)
}
