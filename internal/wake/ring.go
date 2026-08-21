package wake

import "time"

// Ring is the pre-roll buffer: the only place audio captured *before* a wake
// word has ever existed, and the reason background listening can be honest
// about what it keeps.
//
// A wake word is recognised after it has been said, and people do not pause
// between "Jarvix" and "what's my disk usage?". Without a short look-back the
// first syllables of every request would be lost. So a fixed window of recent
// audio is held, overwritten in place, and consumed only when a detection
// fires.
//
// Two properties matter more than the implementation:
//
//   - The allocation is made once, in NewRing, and never grows. There is no
//     path — no configuration, no long utterance, no wedged consumer — that
//     makes Jarvix retain more recent audio than the window it was built with.
//   - Reset zeroes the samples rather than merely rewinding the cursor, so a
//     mute or an ended capture leaves nothing recoverable in the process's
//     memory.
//
// Not safe for concurrent use; the Listener serialises access under its lock.
type Ring struct {
	buf    []int16
	cursor int
	filled int
}

// NewRing builds a ring holding d of 16 kHz mono audio, rounded down to whole
// frames and clamped to MaxRingDuration. A non-positive duration gives a ring
// of zero capacity — look-back disabled, which is a legitimate (and the most
// private) choice.
func NewRing(d time.Duration) *Ring {
	if d > MaxRingDuration {
		d = MaxRingDuration
	}
	frames := 0
	if d > 0 {
		frames = int(d / FrameDuration)
	}
	return &Ring{buf: make([]int16, frames*FrameSamples)}
}

// Cap is the ring's capacity in samples.
func (r *Ring) Cap() int { return len(r.buf) }

// Duration is the ring's capacity as wall-clock time.
func (r *Ring) Duration() time.Duration {
	return time.Duration(len(r.buf)) * time.Second / SampleRate
}

// Write appends a frame, discarding whatever it displaces. A frame longer
// than the whole ring keeps only its most recent tail — the same rule as
// every other write, applied to the degenerate case rather than rejected.
func (r *Ring) Write(frame []int16) {
	if len(r.buf) == 0 {
		return
	}
	if len(frame) > len(r.buf) {
		frame = frame[len(frame)-len(r.buf):]
	}
	for _, s := range frame {
		r.buf[r.cursor] = s
		r.cursor = (r.cursor + 1) % len(r.buf)
	}
	if r.filled += len(frame); r.filled > len(r.buf) {
		r.filled = len(r.buf)
	}
}

// AppendTo appends the retained audio, oldest sample first, to dst and
// returns the extended slice. The ring is left untouched — callers wipe it
// explicitly with Reset, so that erasing pre-wake audio is a visible step
// rather than a side effect of reading it.
func (r *Ring) AppendTo(dst []int16) []int16 {
	if r.filled == 0 {
		return dst
	}
	start := (r.cursor - r.filled + len(r.buf)) % len(r.buf)
	if start+r.filled <= len(r.buf) {
		return append(dst, r.buf[start:start+r.filled]...)
	}
	dst = append(dst, r.buf[start:]...)
	return append(dst, r.buf[:r.filled-(len(r.buf)-start)]...)
}

// Len is how many samples are currently retained.
func (r *Ring) Len() int { return r.filled }

// Reset erases the ring. The zeroing is the point: after this call the
// samples are gone from the process's memory, not merely unreachable through
// the cursor.
func (r *Ring) Reset() {
	wipe(r.buf)
	r.cursor = 0
	r.filled = 0
}
