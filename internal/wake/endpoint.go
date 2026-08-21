package wake

import (
	"math"
	"time"
)

// Endpointer decides when the user has finished speaking, so a hands-free
// request submits itself. It is the counterpart of letting go of the
// push-to-talk chord: with no key to release, silence is the only signal
// there is.
//
// The rule is energy-based on purpose. A neural voice-activity model would be
// a second model to install, a second thing to go missing, and a second
// licence to reason about — for a decision that only has to answer "is anyone
// still talking?" over audio that has already been claimed by a wake word.
// Root-mean-square level against an adapting noise floor answers that, costs
// microseconds, and is deterministic enough to test with synthetic silence.
//
// Not safe for concurrent use; the Listener owns one.
type Endpointer struct {
	// Silence is how long a lull must last before the utterance is complete.
	// The user-facing knob (activation.endpoint_silence_ms, default 800 ms):
	// too short clips people who pause mid-sentence, too long makes every
	// request feel like it ends in an awkward wait.
	Silence time.Duration
	// Lead is how long to wait for speech to start at all before giving up.
	// It is what stops a false activation from opening a session and holding
	// it: no speech, no transcript, no provider call.
	Lead time.Duration
	// Max is the hard ceiling on one utterance, which is both a courtesy to
	// whisper and a bound on how much audio can exist at once.
	Max time.Duration
	// NoiseRatio is how far above the running noise floor a frame must sit to
	// count as speech. Zero uses DefaultNoiseRatio.
	NoiseRatio float64

	noise    float64
	primed   bool
	speech   bool
	silentFr int
	totalFr  int
}

// DefaultNoiseRatio is the multiple of the ambient floor that counts as
// speech. Chosen from the shape of the problem rather than tuned against a
// corpus: speech runs roughly 15-25 dB above room tone, and 3x amplitude is
// ~10 dB — low enough to keep quiet talkers, high enough that fan noise and
// keyboard hum do not hold a capture open forever.
const DefaultNoiseRatio = 3.0

// minNoiseFloor keeps the ratio meaningful in a silent room. Without it a
// digitally-silent input drives the floor to zero, and every frame with a
// single non-zero sample becomes "speech".
const minNoiseFloor = 40.0

// noiseAdapt is how quickly the floor follows the room. Slow: the floor
// should track a fan switching on over a second or two, not chase the gaps
// between words.
const noiseAdapt = 0.05

// Endpoint is what the endpointer concluded about the utterance so far.
type Endpoint int

// Endpoint outcomes.
const (
	// EndpointContinue: still capturing.
	EndpointContinue Endpoint = iota
	// EndpointComplete: the user spoke and has now stopped. Submit.
	EndpointComplete
	// EndpointNoSpeech: nothing was ever said. Almost always a false
	// activation; discard without transcribing.
	EndpointNoSpeech
	// EndpointMax: the utterance hit the ceiling. Submit what there is —
	// truncated speech still beats losing the request.
	EndpointMax
)

// Reset prepares the endpointer for a new utterance, keeping the learned
// noise floor: the room has not changed between one request and the next, and
// re-learning it would cost the first half-second of every capture.
func (e *Endpointer) Reset() {
	e.speech = false
	e.silentFr = 0
	e.totalFr = 0
}

// Prime seeds the noise floor from audio captured before the wake word — the
// ring's contents. It is the cheapest possible calibration and it is free:
// that audio was recorded anyway, and it is by definition the room without
// anybody addressing Jarvix.
func (e *Endpointer) Prime(pcm []int16) {
	if len(pcm) < FrameSamples {
		return
	}
	// The quietest frame of the pre-roll, not the average: the pre-roll ends
	// with the wake word itself, and averaging that in would set the floor at
	// speech level.
	quietest := math.Inf(1)
	for off := 0; off+FrameSamples <= len(pcm); off += FrameSamples {
		if r := rms(pcm[off : off+FrameSamples]); r < quietest {
			quietest = r
		}
	}
	if !math.IsInf(quietest, 1) {
		e.noise = math.Max(quietest, minNoiseFloor)
		e.primed = true
	}
}

// Push consumes one frame and reports the state of the utterance.
func (e *Endpointer) Push(frame []int16) Endpoint {
	level := rms(frame)
	if !e.primed {
		e.noise = math.Max(level, minNoiseFloor)
		e.primed = true
	}
	ratio := e.NoiseRatio
	if ratio <= 0 {
		ratio = DefaultNoiseRatio
	}

	e.totalFr++
	if level > math.Max(e.noise, minNoiseFloor)*ratio {
		e.speech = true
		e.silentFr = 0
	} else {
		e.silentFr++
		// The floor only ever learns from quiet frames, so a long answer
		// cannot drag it up to speech level and deafen the endpointer.
		e.noise = e.noise*(1-noiseAdapt) + level*noiseAdapt
	}

	switch {
	case e.totalFr >= frames(e.Max) && frames(e.Max) > 0:
		return EndpointMax
	case e.speech && e.silentFr >= frames(e.Silence):
		return EndpointComplete
	case !e.speech && e.totalFr >= frames(e.Lead) && frames(e.Lead) > 0:
		return EndpointNoSpeech
	default:
		return EndpointContinue
	}
}

// Speech reports whether any speech has been heard in this utterance yet.
func (e *Endpointer) Speech() bool { return e.speech }

// frames converts a duration to whole analysis frames, rounding up so a
// threshold is never silently shortened by the frame size.
func frames(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + FrameDuration - 1) / FrameDuration)
}

// rms is the root-mean-square level of a frame, in raw sample units.
// Computed in float64 rather than integer arithmetic because the sum of
// squares of 1280 full-scale samples overflows int32 and the accumulated
// rounding of an integer square root would matter at the quiet end, which is
// precisely the end this decision turns on.
func rms(frame []int16) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		v := float64(s)
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(frame)))
}
