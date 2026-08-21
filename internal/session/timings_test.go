package session

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// collectTimings runs one interaction and returns the session.timings payload.
func collectTimings(t *testing.T, run func(e *Engine), opts Options) map[string]any {
	t.Helper()
	bus := NewBus(discardLogger())
	events, unsub := bus.Subscribe()
	defer unsub()
	engine := NewEngine(&ai.Fake{Response: "The answer is ready."}, &stt.Fake{Text: "what time is it"},
		&tts.Fake{}, &audio.FakeRecorder{}, &audio.FakePlayer{}, nil, nil, bus, discardLogger(), opts)

	run(engine)

	var timings map[string]any
	for ev := range events {
		if ev.Type == "session.timings" {
			timings = ev.Data
		}
		if ev.Type == "session.finished" {
			break
		}
	}
	return timings
}

func TestTimingsPublishedForASpokenVoiceSession(t *testing.T) {
	timings := collectTimings(t, func(e *Engine) {
		if _, err := e.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := e.StartVoice(); err != nil {
			t.Fatal(err)
		}
		if _, err := e.StopVoice(); err != nil {
			t.Fatal(err)
		}
		if err := e.Submit(""); err != nil {
			t.Fatal(err)
		}
	}, Options{Model: "m", SpeakResponses: true})

	if timings == nil {
		t.Fatal("no session.timings event was published")
	}
	// Every stage of the pipeline the ticket set a budget for.
	for _, stage := range []string{
		StageCaptureToTranscript,
		StageTranscriptToDelta,
		StageDeltaToFirstPCM,
		StageFirstPCMToAudioOut,
		StageReleaseToFirstAudio,
		StageJarvixOverhead,
	} {
		if _, ok := timings[stage]; !ok {
			t.Errorf("stage %q missing from %v", stage, timings)
		}
	}
	if timings["session_id"] != "s1" {
		t.Errorf("session_id = %v", timings["session_id"])
	}
}

func TestTimingsOmitStagesThatDidNotHappen(t *testing.T) {
	// `jarvix ask` never captures audio, so reporting a capture stage would be
	// a fabricated zero rather than a measurement.
	timings := collectTimings(t, func(e *Engine) {
		if _, err := e.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := e.Submit("what time is it"); err != nil {
			t.Fatal(err)
		}
	}, Options{Model: "m", SpeakResponses: true})

	if timings == nil {
		t.Fatal("no session.timings event was published")
	}
	if _, ok := timings[StageCaptureToTranscript]; ok {
		t.Error("a typed question reported a capture stage")
	}
	if _, ok := timings[StageReleaseToFirstAudio]; ok {
		t.Error("a typed question reported a release-to-audio total")
	}
	if _, ok := timings[StageDeltaToFirstPCM]; !ok {
		t.Error("the synthesis stage is measurable for a typed question and must be reported")
	}
	// No tool ran and nothing was asked, so the excluded spans are absent —
	// not zero — like every other stage that did not happen.
	if _, ok := timings[StageToolRuns]; ok {
		t.Error("a turn without tools reported tool_ms")
	}
	if _, ok := timings[StageConfirmWait]; ok {
		t.Error("a turn without confirmations reported confirm_wait_ms")
	}
}

func TestTimingsOmitAudioStagesWhenSpeechIsOff(t *testing.T) {
	timings := collectTimings(t, func(e *Engine) {
		if _, err := e.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := e.Submit("what time is it"); err != nil {
			t.Fatal(err)
		}
	}, Options{Model: "m", SpeakResponses: false})

	if timings == nil {
		t.Fatal("no session.timings event was published")
	}
	if _, ok := timings[StageFirstPCMToAudioOut]; ok {
		t.Error("a silent answer reported an audio-out stage")
	}
	if _, ok := timings[StageTranscriptToDelta]; !ok {
		t.Error("the provider stage still applies to a silent answer")
	}
}

func TestMarksAreOneWay(t *testing.T) {
	// "First output" and "first PCM" must mean the first, even though the tool
	// loop streams several times in one session.
	var ti timings
	ti.markFirstDelta()
	first := ti.firstDelta.at
	time.Sleep(time.Millisecond)
	ti.markFirstDelta()
	if !ti.firstDelta.at.Equal(first) {
		t.Error("a later mark overwrote the first one")
	}
}

func TestReportSkipsInvertedSpans(t *testing.T) {
	// Marks land from several goroutines; a report must never invent a
	// negative duration out of an out-of-order pair.
	now := time.Now()
	ti := timings{captureStop: mark{at: now}, transcript: mark{at: now.Add(-time.Second)}}
	if _, ok := ti.report()[StageCaptureToTranscript]; ok {
		t.Error("an inverted span was reported")
	}
	// The same rule for a span whose exclusion accounting is impossible: a
	// net-negative span is dropped, never published below zero.
	ti = timings{
		firstDelta: mark{at: now, excluded: 0},
		firstPCM:   mark{at: now.Add(time.Second), excluded: 2 * time.Second},
	}
	if _, ok := ti.report()[StageDeltaToFirstPCM]; ok {
		t.Error("a net-negative span was reported")
	}
}

func TestAudioTraceReportsThroughTheFakePlayer(t *testing.T) {
	// The audio.Trace seam is what makes "first PCM → audio out" measurable at
	// all; if a player stops honouring it the stage silently disappears.
	fired := make(chan struct{}, 1)
	ctx := audio.WithTrace(context.Background(), &audio.Trace{
		FirstAudio: func() { fired <- struct{}{} },
	})
	chunks := make(chan []byte, 1)
	chunks <- []byte{0, 1, 2, 3}
	close(chunks)
	if err := (&audio.FakePlayer{}).Play(ctx, 24000, 1, chunks); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	default:
		t.Error("the player never reported its first chunk")
	}
}

// manualClock is a deterministic clock for driving the timing arithmetic:
// Now() reads without advancing, Advance moves it. Race-safe because marks
// land from several goroutines in the engine tests that share the type.
type manualClock struct {
	mu sync.Mutex
	at time.Time
}

func newManualClock() *manualClock {
	return &manualClock{at: time.Date(2026, 8, 21, 16, 18, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// spanRec is one excluded span as the test drove it, for computing the
// invariant independently of the implementation's snapshots.
type spanRec struct{ from, to time.Time }

// clip returns how much of the span falls inside [from, to].
func (s spanRec) clip(from, to time.Time) time.Duration {
	start, end := s.from, s.to
	if start.Before(from) {
		start = from
	}
	if end.After(to) {
		end = to
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// TestTimingsStagesNonNegativeAndAccountForTheWallClock is the #72 invariant,
// asserted across every turn shape: each published stage is ≥ 0, and the
// stages plus the explicitly-excluded spans account exactly for the wall
// clock between release and first audio.
func TestTimingsStagesNonNegativeAndAccountForTheWallClock(t *testing.T) {
	// exclude drives one excluded span of duration d and records it.
	exclude := func(c *manualClock, ti *timings, excl *[]spanRec, stage string, d time.Duration) {
		from := c.Now()
		ti.beginExcluded(stage)
		c.Advance(d)
		ti.endExcluded()
		*excl = append(*excl, spanRec{from: from, to: c.Now()})
	}

	shapes := []struct {
		name string
		run  func(c *manualClock, ti *timings, excl *[]spanRec)
	}{
		{"plain spoken answer", func(c *manualClock, ti *timings, excl *[]spanRec) {
			ti.markCaptureStop()
			c.Advance(120 * time.Millisecond)
			ti.markTranscript()
			c.Advance(30 * time.Millisecond)
			ti.markContext()
			c.Advance(400 * time.Millisecond)
			ti.markFirstDelta()
			c.Advance(80 * time.Millisecond)
			ti.markFirstPCM()
			c.Advance(20 * time.Millisecond)
			ti.markAudioOut()
		}},
		{"typed question", func(c *manualClock, ti *timings, excl *[]spanRec) {
			ti.markTranscript()
			c.Advance(300 * time.Millisecond)
			ti.markFirstDelta()
			c.Advance(60 * time.Millisecond)
			ti.markFirstPCM()
		}},
		{"allow-tier tool round", func(c *manualClock, ti *timings, excl *[]spanRec) {
			ti.markCaptureStop()
			c.Advance(100 * time.Millisecond)
			ti.markTranscript()
			c.Advance(250 * time.Millisecond)
			ti.markFirstDelta() // the round's tool call
			exclude(c, ti, excl, StageToolRuns, 500*time.Millisecond)
			c.Advance(300 * time.Millisecond) // second round streams the answer
			ti.markFirstPCM()
			c.Advance(15 * time.Millisecond)
			ti.markAudioOut()
		}},
		{"confirmation approved after a delay (the jarvix_ms=-3835 shape)",
			func(c *manualClock, ti *timings, excl *[]spanRec) {
				ti.markCaptureStop()
				c.Advance(150 * time.Millisecond)
				ti.markTranscript()
				c.Advance(300 * time.Millisecond)
				ti.markFirstDelta() // round one: a tool call, no narration
				c.Advance(200 * time.Millisecond)
				ti.markFirstPCM() // the question's audio
				c.Advance(25 * time.Millisecond)
				ti.markAudioOut()
				// The user thinks for 3835 ms — the span that was subtracted
				// from a much smaller total in the live incident.
				exclude(c, ti, excl, StageConfirmWait, 3835*time.Millisecond)
				exclude(c, ti, excl, StageToolRuns, 200*time.Millisecond)
				c.Advance(400 * time.Millisecond) // round two streams the answer
			}},
		{"confirmation wait overlapping the question's audio",
			func(c *manualClock, ti *timings, excl *[]spanRec) {
				ti.markCaptureStop()
				c.Advance(150 * time.Millisecond)
				ti.markTranscript()
				c.Advance(300 * time.Millisecond)
				ti.markFirstDelta()
				c.Advance(50 * time.Millisecond)
				ti.markFirstPCM()
				// The wait opens while the question is still playing; first
				// audio lands 500 ms into it, so only that much is inside the
				// release→audio window.
				from := c.Now()
				ti.beginExcluded(StageConfirmWait)
				c.Advance(500 * time.Millisecond)
				ti.markAudioOut()
				c.Advance(3300 * time.Millisecond)
				ti.endExcluded()
				*excl = append(*excl, spanRec{from: from, to: c.Now()})
			}},
		{"confirmation declined", func(c *manualClock, ti *timings, excl *[]spanRec) {
			ti.markCaptureStop()
			c.Advance(150 * time.Millisecond)
			ti.markTranscript()
			c.Advance(300 * time.Millisecond)
			ti.markFirstDelta()
			c.Advance(40 * time.Millisecond)
			ti.markFirstPCM()
			c.Advance(20 * time.Millisecond)
			ti.markAudioOut()
			exclude(c, ti, excl, StageConfirmWait, 1200*time.Millisecond)
			c.Advance(350 * time.Millisecond) // the model acknowledges
		}},
		{"confirmation timed out", func(c *manualClock, ti *timings, excl *[]spanRec) {
			ti.markCaptureStop()
			c.Advance(150 * time.Millisecond)
			ti.markTranscript()
			c.Advance(300 * time.Millisecond)
			ti.markFirstDelta()
			c.Advance(40 * time.Millisecond)
			ti.markFirstPCM()
			c.Advance(20 * time.Millisecond)
			ti.markAudioOut()
			exclude(c, ti, excl, StageConfirmWait, 30*time.Second)
			c.Advance(350 * time.Millisecond)
		}},
		{"long advisor-style tool, speech off", func(c *manualClock, ti *timings, excl *[]spanRec) {
			ti.markTranscript()
			c.Advance(280 * time.Millisecond)
			ti.markFirstDelta()
			exclude(c, ti, excl, StageToolRuns, 90*time.Second)
			c.Advance(500 * time.Millisecond)
		}},
		{"intent hit", func(c *manualClock, ti *timings, excl *[]spanRec) {
			ti.markCaptureStop()
			c.Advance(90 * time.Millisecond)
			ti.markTranscript()
		}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			c := newManualClock()
			ti := &timings{now: c.Now}
			var excl []spanRec
			shape.run(c, ti, &excl)
			rep := ti.report()

			// Every published stage is ≥ 0, whatever the shape did.
			for stage, v := range rep {
				ms, ok := v.(int64)
				if !ok {
					t.Fatalf("stage %q is %T, want int64", stage, v)
				}
				if ms < 0 {
					t.Errorf("stage %q = %d ms; no stage may read negative", stage, ms)
				}
			}

			release, hasRelease := rep[StageReleaseToFirstAudio].(int64)
			if !hasRelease {
				return // no release→audio window; the ≥0 property is the whole claim
			}
			ms := func(stage string) int64 {
				v, _ := rep[stage].(int64)
				return v
			}
			// The excluded time that fell inside the window, from the test's
			// own record of what it drove — independent of the snapshots the
			// implementation keeps.
			var clipped time.Duration
			for _, s := range excl {
				clipped += s.clip(ti.captureStop.at, ti.audioOut.at)
			}

			// The accountability identity: the model's span, Jarvix's span,
			// and the excluded time inside the window sum to the wall clock.
			if jarvix, ok := rep[StageJarvixOverhead].(int64); ok {
				got := ms(StageTranscriptToDelta) + jarvix + clipped.Milliseconds()
				if got != release {
					t.Errorf("thinking (%d) + jarvix (%d) + excluded-in-window (%d) = %d ms; "+
						"want the wall clock %d ms",
						ms(StageTranscriptToDelta), jarvix, clipped.Milliseconds(), got, release)
				}
			}
			// When the whole pipeline chain was marked in order, the chained
			// stages plus the excluded time tile the window exactly.
			chain := []string{StageCaptureToTranscript, StageTranscriptToDelta,
				StageDeltaToFirstPCM, StageFirstPCMToAudioOut}
			complete := true
			for _, stage := range chain {
				if _, ok := rep[stage]; !ok {
					complete = false
				}
			}
			if complete {
				sum := ms(StageContext) + clipped.Milliseconds()
				for _, stage := range chain {
					sum += ms(stage)
				}
				if sum != release {
					t.Errorf("chained stages + excluded-in-window = %d ms; want %d ms (%v)",
						sum, release, rep)
				}
			}
		})
	}
}

// TestJarvixOverheadIsDroppedNotNegativeWhenMarksDisagree: the old incident's
// mark order — first audio before the model's first output — must never
// again produce a negative jarvix_ms. If the marks ever land that way, the
// number is withheld ("consistently or not at all"), never published below
// zero.
func TestJarvixOverheadIsDroppedNotNegativeWhenMarksDisagree(t *testing.T) {
	base := time.Date(2026, 8, 21, 16, 18, 0, 0, time.UTC)
	ti := timings{
		captureStop: mark{at: base},
		transcript:  mark{at: base.Add(150 * time.Millisecond)},
		firstPCM:    mark{at: base.Add(400 * time.Millisecond)},
		audioOut:    mark{at: base.Add(425 * time.Millisecond)},
		// The model's first output marked AFTER the first audio — the exact
		// disorder the old accounting produced on a confirmation turn.
		firstDelta: mark{at: base.Add(4260 * time.Millisecond)},
	}
	rep := ti.report()
	if v, ok := rep[StageJarvixOverhead]; ok {
		t.Errorf("jarvix_ms = %v from disordered marks; it must be withheld", v)
	}
	for stage, v := range rep {
		if ms, ok := v.(int64); ok && ms < 0 {
			t.Errorf("stage %q = %d ms", stage, ms)
		}
	}
}

// TestTimingsSettleAnOpenWaitConsistently: a session cancelled mid-wait
// publishes the wait accrued so far — consistently, never negative (#72's
// "publish consistently or not at all").
func TestTimingsSettleAnOpenWaitConsistently(t *testing.T) {
	c := newManualClock()
	ti := &timings{now: c.Now}
	ti.markTranscript()
	c.Advance(200 * time.Millisecond)
	ti.markFirstDelta()
	ti.beginExcluded(StageConfirmWait)
	c.Advance(1500 * time.Millisecond)
	// Cancelled here: report runs with the span still open.
	rep := ti.report()
	if got, _ := rep[StageConfirmWait].(int64); got != 1500 {
		t.Errorf("open confirm_wait_ms = %d, want 1500", got)
	}
}

// markingTool records when it executed, so the test can prove the model's
// first output was marked before any tool ran.
type markingTool struct {
	namedTool
	executedAt time.Time
}

func (m *markingTool) Execute(ctx context.Context, in json.RawMessage) (string, error) {
	m.executedAt = time.Now()
	return m.namedTool.Execute(ctx, in)
}

// TestFirstOutputMarkIncludesToolCalls pins the mark itself: a round that
// produces only a tool call has still produced the model's first output, and
// the mark lands before the tool executes. Without it, a confirmation turn's
// marks fall out of pipeline order — the arithmetic behind issue #72's
// negative jarvix_ms.
func TestFirstOutputMarkIncludesToolCalls(t *testing.T) {
	rec := &markingTool{namedTool: namedTool{name: "shell.run", result: "3 containers"}}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "docker ps", "Three containers are running.")

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	s := h.engine.current
	h.engine.mu.Unlock()
	if err := h.engine.Submit("what's in docker"); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	s.timings.mu.Lock()
	firstOutput := s.timings.firstDelta.at
	s.timings.mu.Unlock()
	if firstOutput.IsZero() {
		t.Fatal("a tool-call-only round never marked the model's first output")
	}
	if firstOutput.After(rec.executedAt) {
		t.Errorf("first output marked at %v, after the tool ran at %v — the mark must land "+
			"on the tool call itself", firstOutput, rec.executedAt)
	}
}

// TestTimingsNeverNegativeAfterConfirmationWait is the named regression for
// the live incident (journal 16:18:46, jarvix_ms=-3835): a voice turn whose
// first round produced only a tool call, whose confirmation was approved
// after a delay, and whose timings then read negative. Every clock read
// advances the injected clock, so every wait in the turn is a strictly
// positive, deterministic span — under the old accounting this exact shape
// made thinking time swallow the confirmation wait and drove jarvix_ms
// below zero.
func TestTimingsNeverNegativeAfterConfirmationWait(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{SpeakResponses: true}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "The build directory is gone.")
	// A stepping clock: every read moves time 100 ms forward, so the spans
	// between marks are large, deterministic, and impossible to dismiss as
	// rounding — the confirmation wait alone spans several reads.
	step := newManualClock()
	h.engine.now = func() time.Time {
		step.Advance(100 * time.Millisecond)
		return step.Now()
	}

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StopVoice(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(""); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}

	var timings map[string]any
	deadline := time.After(5 * time.Second)
	for timings == nil {
		select {
		case ev := <-h.events:
			if ev.Type == "session.timings" {
				timings = ev.Data
			}
		case <-deadline:
			t.Fatal("timed out waiting for session.timings")
		}
	}
	h.waitIdle(t)

	for stage, v := range timings {
		ms, ok := v.(int64)
		if !ok {
			continue // session_id
		}
		if ms < 0 {
			t.Errorf("stage %q = %d ms; the incident's negative reading is back", stage, ms)
		}
	}
	if _, ok := timings[StageJarvixOverhead]; !ok {
		t.Errorf("jarvix_ms missing from %v; the accountability number must survive a confirmation", timings)
	}
	if wait, _ := timings[StageConfirmWait].(int64); wait <= 0 {
		t.Errorf("confirm_wait_ms = %v, want the wait on the user's answer to be visible", timings[StageConfirmWait])
	}
	if _, ok := timings[StageToolRuns]; !ok {
		t.Errorf("tool_ms missing from %v; the tool execution must be on the record", timings)
	}
}
