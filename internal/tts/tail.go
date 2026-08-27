package tts

import (
	"strings"
	"sync"
)

// Tail keeps the last bounded stretch of an engine's stderr so a failure can
// quote the engine's own words instead of a bare exit status.
//
// It exists because "piper failed: exit status 1" tells the user nothing
// while the process just explained itself on stderr — and because doctor's
// voice probes (issue #113) promise to surface exactly that explanation. The
// buffer is byte-bounded, not line-bounded: a crashing engine can emit an
// unbounded traceback, and holding all of it would let a broken tool decide
// how much memory an error costs.
//
// Safe for concurrent use: the process's stderr is written from a pipe-reader
// goroutine while the caller reads the tail after Wait returns.
type Tail struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

// NewTail returns a Tail that retains at most limit bytes.
func NewTail(limit int) *Tail {
	return &Tail{limit: limit}
}

// Write implements io.Writer; it never fails.
func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		// Copy rather than reslice so the discarded prefix is actually freed.
		kept := make([]byte, t.limit)
		copy(kept, t.buf[len(t.buf)-t.limit:])
		t.buf = kept
	}
	return len(p), nil
}

// Add records one already-scanned line, for callers that read the stream
// through a bufio.Scanner rather than handing the Tail to exec directly.
func (t *Tail) Add(line string) {
	_, _ = t.Write([]byte(line + "\n"))
}

// String renders the retained tail as a single line — newlines collapse to
// " | " — so it can sit inside an error message or a doctor detail without
// breaking either's one-line formatting. Empty when nothing was written.
func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.buf))
	if s == "" {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return strings.Join(lines, " | ")
}
