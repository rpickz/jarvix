package wake

import (
	"math/rand"
	"testing"
	"time"
)

func newPolicy() *Policy {
	return &Policy{
		Threshold:   ThresholdFor(DefaultSensitivity),
		Consecutive: DefaultConsecutive,
		Refractory:  DefaultRefractory,
	}
}

// The sensitivity a user sets is not the number a model documentation talks
// about, so the mapping is part of the contract: the shipped default has to
// land on 0.5, the threshold every openWakeWord example uses, or published
// advice about that number would not transfer.
func TestSensitivityMapsOntoTheDocumentedThreshold(t *testing.T) {
	approx(t, "default sensitivity", ThresholdFor(DefaultSensitivity), 0.5, 0.0001)
	if ThresholdFor(0) <= ThresholdFor(1) {
		t.Error("higher sensitivity must mean a lower threshold")
	}
	// Out-of-range values are clamped rather than producing a threshold
	// outside 0..1, which would make the wake word either impossible or
	// permanent.
	for _, s := range []float64{-5, 5} {
		if got := ThresholdFor(s); got < 0 || got > 1 {
			t.Errorf("sensitivity %.0f gave threshold %.2f, outside 0..1", s, got)
		}
	}
}

// One frame over the line is not a wake word. This is the single most
// valuable rule in the package: an openWakeWord-class model's false positives
// are dominated by isolated spikes, and requiring two consecutive frames
// costs 160 ms — shorter than any way of saying the word — while removing
// almost all of them.
func TestPolicyRequiresConsecutiveFramesAboveThreshold(t *testing.T) {
	p := newPolicy()
	if p.Fire(0.99) {
		t.Fatal("a single frame over threshold activated Jarvix")
	}
	if !p.Fire(0.99) {
		t.Fatal("two consecutive frames over threshold did not activate")
	}

	// A run broken by one quiet frame starts again from zero — otherwise a
	// model that spikes every few seconds would eventually accumulate enough
	// separate spikes to fire.
	p2 := newPolicy()
	for _, score := range []float64{0.99, 0.01, 0.99} {
		if p2.Fire(score) {
			t.Fatalf("a broken run activated on score %.2f", score)
		}
	}
}

// One spoken wake word must produce one activation, not one per frame for as
// long as the model's score stays high. Without a refractory period a single
// "Jarvix" would start a session, then interrupt it, then interrupt that.
func TestPolicyFiresOncePerWakeWord(t *testing.T) {
	p := newPolicy()
	activations := 0
	for i := 0; i < framesIn(DefaultRefractory)+4; i++ {
		if p.Fire(0.99) {
			activations++
		}
	}
	if activations != 1 {
		t.Fatalf("a continuous high score produced %d activations, want 1", activations)
	}

	// Once the hush has elapsed the user can be heard again: someone who was
	// ignored should be able to simply say it a second time.
	for i := 0; i < framesIn(DefaultRefractory)+2; i++ {
		p.Fire(0.0)
	}
	// Two frames, because the consecutive rule still applies to the second
	// wake word exactly as it did to the first.
	fired := false
	for i := 0; i < DefaultConsecutive; i++ {
		if p.Fire(0.99) {
			fired = true
		}
	}
	if !fired {
		t.Error("after the refractory period the wake word never fires again")
	}
}

// A run built up *during* the hush must not be inherited by the frame after
// it, or the refractory period would end in an immediate second activation —
// exactly the double-trigger it exists to prevent.
func TestPolicyDoesNotInheritARunFromTheHush(t *testing.T) {
	p := newPolicy()
	p.Fire(0.99)
	if !p.Fire(0.99) {
		t.Fatal("setup: expected an activation")
	}
	for i := 0; i < framesIn(DefaultRefractory); i++ {
		if p.Fire(0.99) {
			t.Fatal("activated during the refractory period")
		}
	}
	if p.Fire(0.99) {
		t.Error("the frame after the hush inherited a run and fired immediately")
	}
}

// Reset is called when capture stops, so a restarted stream never inherits
// half a wake word from the one that died.
func TestPolicyResetClearsTheRun(t *testing.T) {
	p := newPolicy()
	p.Fire(0.99)
	p.Reset()
	if p.Fire(0.99) {
		t.Error("a run survived Reset")
	}
}

// The false-activation budget, measured against what this package can
// actually measure.
//
// The acceptance criterion is "≤1 false activation per hour at default
// sensitivity". That number is a property of a model, a microphone, and a
// room, and Jarvix has not measured it for any particular model — ADR 0024
// says so, and says how to. What *is* measurable, and what this test pins
// down, is the half Jarvix owns: given a stream of scores from an imperfect
// model, how many activations does the gating produce?
//
// The score stream below is deliberately pessimistic. Every frame draws from
// a distribution that puts 1 frame in 500 over the threshold — 90 spurious
// spikes an hour, a model far worse than any shipped one — and the assertion
// is that the consecutive-frame rule turns almost all of them into nothing.
func TestActivationPolicyHoldsTheFalseActivationBudget(t *testing.T) {
	const hours = 8
	frames := hours * int(time.Hour/FrameDuration)
	rng := rand.New(rand.NewSource(4242)) //nolint:gosec // fixture, not crypto
	p := newPolicy()

	activations := 0
	spikes := 0
	for i := 0; i < frames; i++ {
		score := rng.Float64() * 0.4 // ordinary ambient audio: nowhere near
		if rng.Intn(500) == 0 {      // ...except one frame in 500
			score = 0.5 + rng.Float64()*0.5
			spikes++
		}
		if p.Fire(score) {
			activations++
		}
	}

	perHour := float64(activations) / hours
	t.Logf("%d hours of synthetic scores: %d frames over threshold, %d activations (%.2f/hour)",
		hours, spikes, activations, perHour)
	if spikes < frames/1000 {
		t.Fatalf("the fixture produced only %d spikes; it is not exercising the gate", spikes)
	}
	if perHour > 1 {
		t.Errorf("the activation policy let %.2f false activations through per hour, budget is 1", perHour)
	}
}

// The same corpus with the gate turned off, so the test above cannot pass by
// accident. If a bare threshold also came in under budget, the consecutive
// rule would be proving nothing.
func TestTheFalseActivationBudgetNeedsTheGate(t *testing.T) {
	const hours = 8
	frames := hours * int(time.Hour/FrameDuration)
	rng := rand.New(rand.NewSource(4242)) //nolint:gosec // fixture, not crypto
	bare := &Policy{Threshold: ThresholdFor(DefaultSensitivity), Consecutive: 1}

	activations := 0
	for i := 0; i < frames; i++ {
		score := rng.Float64() * 0.4
		if rng.Intn(500) == 0 {
			score = 0.5 + rng.Float64()*0.5
		}
		if bare.Fire(score) {
			activations++
		}
	}
	if perHour := float64(activations) / hours; perHour <= 1 {
		t.Fatalf("an ungated threshold also stayed under budget (%.2f/hour); "+
			"the corpus is too kind for the gate test above to mean anything", perHour)
	}
}
