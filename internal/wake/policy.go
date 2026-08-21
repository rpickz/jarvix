package wake

import "time"

// Policy turns a stream of per-frame model scores into activations.
//
// This is the half of false-positive control Jarvix owns. The other half —
// how well the model tells "Jarvix" from "Jarvis", "service", or a cough —
// belongs to whichever detector is installed, and no amount of Go can improve
// it. What Go can do is refuse to act on a single lucky frame, and refuse to
// act twice on one word; both of those are worth more than they look, because
// a model that spikes over threshold for one frame every few minutes goes
// from "unusable" to "never fires" under a two-frame rule.
//
// Being explicit about the split matters for the acceptance criterion.
// "≤1 false activation per hour" is a property of a model, a microphone, and
// a room. What this type makes testable is the gating: given a score stream,
// exactly which activations come out. ADR 0024 records how the end-to-end
// number is to be measured, and states plainly that Jarvix has not measured
// it for any particular model.
//
// Not safe for concurrent use; the Listener owns one.
type Policy struct {
	// Threshold is the score a frame must exceed. Derived from the
	// user-facing sensitivity by ThresholdFor.
	Threshold float64
	// Consecutive is how many frames in a row must exceed it. Zero means one
	// (no confirmation), which is not recommended and is not the default.
	Consecutive int
	// Refractory is how long after an activation the policy stays silent, so
	// one spoken wake word produces one activation rather than one per frame
	// for as long as the model's score stays high.
	Refractory time.Duration

	above  int
	hushed int
}

// DefaultConsecutive is how many frames above threshold confirm a wake word.
// Two 80 ms frames is 160 ms — shorter than any way of saying "Jarvix", so it
// costs no responsiveness, and it rejects the single-frame spikes that
// dominate an openWakeWord-class model's false positives.
const DefaultConsecutive = 2

// DefaultRefractory is the quiet period after an activation. Long enough to
// cover the tail of the wake word and the model's own decay, short enough
// that a user who was ignored can simply say it again.
const DefaultRefractory = 1500 * time.Millisecond

// ThresholdFor maps the user-facing sensitivity (0..1, higher = more eager)
// onto a score threshold. Linear from 0.95 down to 0.05, so the shipped
// default of 0.5 lands on 0.5 — the threshold every openWakeWord example
// uses, which makes published guidance about that number directly applicable
// to Jarvix's setting.
func ThresholdFor(sensitivity float64) float64 {
	switch {
	case sensitivity < 0:
		sensitivity = 0
	case sensitivity > 1:
		sensitivity = 1
	}
	return 0.95 - 0.9*sensitivity
}

// Fire consumes one frame's score and reports whether the wake word just
// activated.
func (p *Policy) Fire(score float64) bool {
	need := p.Consecutive
	if need <= 0 {
		need = 1
	}
	if p.hushed > 0 {
		p.hushed--
		// The run is reset during the hush too, so the frame immediately
		// after it cannot inherit a run built up while Jarvix was ignoring
		// the model.
		p.above = 0
		return false
	}
	if score <= p.Threshold {
		p.above = 0
		return false
	}
	p.above++
	if p.above < need {
		return false
	}
	p.above = 0
	p.hushed = frames(p.Refractory)
	return true
}

// Reset clears the run and the refractory period — used when capture stops,
// so a restart never inherits half a wake word from the stream that died.
func (p *Policy) Reset() {
	p.above = 0
	p.hushed = 0
}
