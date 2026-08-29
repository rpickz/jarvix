package session

import (
	"sync"
	"time"
)

// timings records when each stage of one interaction completed.
//
// The product's premise is that the computer feels present, and presence is a
// number: how long after the user stops speaking does Jarvix start speaking.
// Until it is measured on every session it can only be argued about, so every
// session carries these marks and publishes them (ADR 0018) — the same
// instrument for a developer benchmarking a change and for a user wondering
// why their machine feels slow today.
//
// Marks are one-way: the first write for a stage wins, later ones are ignored.
// That is what makes "first output" and "first PCM" mean what they say, and it
// keeps the tool-call loop (which streams several times per session) from
// resetting the clock halfway through an answer.
//
// Two spans inside a turn belong to neither Jarvix nor the model: time spent
// executing a tool (the user's command running is the user's work, not
// scaffolding latency) and time spent waiting for the user to answer a tool
// confirmation (that is the user thinking — issue #72's negative jarvix_ms
// was a confirmation wait mis-attributed to the model). Both are accumulated
// here as *excluded* spans: reported as their own stages so the cost is
// visible — the same honesty the context_ms split bought in #34 — and
// subtracted from the pipeline spans they fall inside, so no stage can go
// negative and jarvix_ms stays the number this codebase is accountable for.
type timings struct {
	mu sync.Mutex

	// now is the clock every mark and excluded span reads. Injectable so
	// tests drive the arithmetic deterministically; nil means time.Now.
	now func() time.Time

	// captureStop is the moment the user released push-to-talk. Zero for a
	// typed question (`jarvix ask`), which never captures audio.
	captureStop mark
	// transcript is when the words were known — from STT, or immediately for
	// a typed question.
	transcript mark
	// contextDone is when desktop context finished being gathered (ADR 0019).
	// Zero when context is disabled or the turn never reached the model.
	//
	// It exists so gathering is charged to Jarvix rather than to the model:
	// it happens between the transcript and the provider request, so without
	// its own mark it would sit inside transcript_to_first_delta_ms — the one
	// span that is *subtracted* from jarvix_ms as the user's choice of model.
	// A cost that hides inside the number it inflates is the one kind of
	// measurement worse than none.
	contextDone mark
	// firstDelta is the provider's first output — a text token or a tool
	// call, whichever the model produces first. The gap before it is the
	// model's thinking time: reported, never included in Jarvix's own budget.
	// Counting a tool call keeps the marks in pipeline order on a turn whose
	// first round narrates nothing (issue #72): the confirmation question's
	// audio must never precede "the first thing the model said".
	firstDelta mark
	// firstPCM is the first audio sample the synthesizer produced.
	firstPCM mark
	// audioOut is when that sample reached the audio device (audio.Trace).
	audioOut mark

	// supersededSentences counts queued utterances the speaker dropped
	// unplayed because a newer turn's speech superseded them (issue #120).
	// A count, not a duration — the one non-clock fact this record carries,
	// and it belongs here because it is the same kind of honesty as the
	// excluded spans: the turn's audio was shorter than its text, and a
	// number that quietly omitted why would flatter the transcript. Noted
	// per drop by the speaker goroutine, which is why it sits under mu like
	// the marks rather than riding a speaker-local counter: it must survive
	// into the report even when the turn is cancelled before the speaker's
	// own tts.superseded event could be published.
	supersededSentences int

	// nothingHeard, when set, is why the capture produced no words at all
	// (issue #191): no voiced audio in the clip, or a transcript that was
	// only the bias prompt handed back. A string, and the record's only one,
	// because the number a user needs after pressing the key and getting
	// nothing is not a duration — it is the reason. It sits under mu beside
	// the marks for the same reason the superseded count does: it must reach
	// the report even though the turn ends the moment it is set.
	nothingHeard string

	// tier, tierModel, tierReason, tierWanted and tierContextDropped are which
	// model answered this turn, and why (issue #159). Strings and a count
	// rather than durations, like nothingHeard above and for the same reason:
	// the question "why did that answer feel like that" is not answered by a
	// number of milliseconds, it is answered by naming the model.
	//
	// They sit under mu with the marks because they are written on the think
	// goroutine and read by report() from whichever path ends the session.
	// First write wins, like every mark: a turn is served by one tier, and a
	// failover rewrites the record through noteTier before the turn ends
	// rather than adding a second one.
	tier               string
	tierModel          string
	tierReason         string
	tierWanted         string
	tierContextDropped int

	// hostTier, hostModel and hostOutcome are what the host — the instant tier
	// covering the wait — did on this turn (issue #161, ADR 0064). They sit
	// beside the tier keys because they answer the same question from the other
	// end: those name the model that produced the *answer*, these name the
	// model that produced the *holding line*, and a record that could not tell
	// them apart could not answer "why did it say that?".
	//
	// Written only when the host actually produced something (a line held, a
	// clarification taken, or a line refused by the honesty guard). A host that
	// stood down because the answer was quick did nothing, and a key on every
	// fast turn saying so would be noise on the turns this feature is proudest
	// of. Under mu with the marks, for the marks' reason: written from the
	// host's own goroutine and read by report() from whichever path ends the
	// session.
	hostTier    string
	hostModel   string
	hostOutcome string

	// excluded accumulates the completed excluded spans by stage name
	// (StageToolRuns, StageConfirmWait). Presence means the span happened,
	// even when it rounded to zero milliseconds.
	excluded map[string]time.Duration
	// activeStage / activeStart is the one excluded span currently open.
	// Tool executions and confirmation waits run sequentially on the think
	// goroutine, so at most one is ever open.
	activeStage string
	activeStart time.Time
}

// mark is one pipeline moment: when it happened, and how much excluded time
// had accumulated by then. The snapshot is what lets a span between two marks
// be reported net of the pauses that fell inside it, without keeping a list
// of intervals.
type mark struct {
	at       time.Time
	excluded time.Duration
}

func (t *timings) markCaptureStop() { t.set(&t.captureStop) }
func (t *timings) markTranscript()  { t.set(&t.transcript) }
func (t *timings) markContext()     { t.set(&t.contextDone) }
func (t *timings) markFirstDelta()  { t.set(&t.firstDelta) }
func (t *timings) markFirstPCM()    { t.set(&t.firstPCM) }
func (t *timings) markAudioOut()    { t.set(&t.audioOut) }

func (t *timings) set(field *mark) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if field.at.IsZero() {
		now := t.clock()
		*field = mark{at: now, excluded: t.excludedTotalLocked(now)}
	}
}

// noteSupersededDrop records one queued sentence dropped unplayed by
// cross-turn supersession (issue #120).
func (t *timings) noteSupersededDrop() {
	t.mu.Lock()
	t.supersededSentences++
	t.mu.Unlock()
}

// noteNothingHeard records why a capture produced no transcript (issue #191).
// First write wins, like every mark: a turn hears nothing once.
func (t *timings) noteNothingHeard(reason string) {
	t.mu.Lock()
	if t.nothingHeard == "" {
		t.nothingHeard = reason
	}
	t.mu.Unlock()
}

// noteTier records which tier answered and what actually served it (issue
// #159). Unlike the marks it is last-write-wins, and deliberately: a turn that
// failed over calls this again with the tier that really answered, and a
// record still naming the tier that could not be reached would be exactly the
// false claim this feature exists to prevent.
func (t *timings) noteTier(tier, model, reason, wanted string, contextDropped int) {
	t.mu.Lock()
	t.tier, t.tierModel = tier, model
	t.tierReason, t.tierWanted = reason, wanted
	t.tierContextDropped = contextDropped
	t.mu.Unlock()
}

// noteHost records what the host did with this turn (issue #161). First write
// wins: a host speaks at most once, and the one thing it did is the thing the
// record has to keep.
func (t *timings) noteHost(tier, model, outcome string) {
	t.mu.Lock()
	if t.hostOutcome == "" {
		t.hostTier, t.hostModel, t.hostOutcome = tier, model, outcome
	}
	t.mu.Unlock()
}

// modelStart is the instant this turn's model clock starts — the same origin
// transcript_to_first_delta_ms is measured from, which is why it is taken from
// here rather than read off a wall clock at the call site (issue #161).
//
// The host's grace is measured from it deliberately: the number a user sets the
// grace against is then exactly the number `jarvix status --last` already prints
// for them, rather than a second, differently-anchored measurement of the same
// wait.
//
// Zero when neither mark has been made — a typed question with no context
// collector reaches the provider without either — and the caller substitutes
// its own now, which for that turn is the same instant to within the cost of
// assembling a prompt.
func (t *timings) modelStart() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.contextDone.at.IsZero() {
		return t.contextDone.at
	}
	return t.transcript.at
}

// clock reads the injected clock, defaulting to the real one.
func (t *timings) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// beginExcluded opens an excluded span. A span already open is left alone:
// the caller sites run sequentially, so a second begin is a programming
// mistake that must not corrupt the first span's accounting.
func (t *timings) beginExcluded(stage string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.activeStage != "" {
		return
	}
	t.activeStage, t.activeStart = stage, t.clock()
}

// endExcluded closes the open excluded span and banks its duration. A close
// with nothing open is ignored.
func (t *timings) endExcluded() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.activeStage == "" {
		return
	}
	if t.excluded == nil {
		t.excluded = make(map[string]time.Duration)
	}
	t.excluded[t.activeStage] += t.clock().Sub(t.activeStart)
	t.activeStage = ""
}

// excludedTotalLocked is the excluded time accumulated by now, including the
// elapsed part of a span still open — a mark landing mid-wait (the
// confirmation question's audio playing while the user thinks) must snapshot
// the wait up to that moment, not zero.
func (t *timings) excludedTotalLocked(now time.Time) time.Duration {
	var total time.Duration
	for _, d := range t.excluded {
		total += d
	}
	if t.activeStage != "" && now.After(t.activeStart) {
		total += now.Sub(t.activeStart)
	}
	return total
}

// Stage names as they appear in the session.timings event, the structured log,
// and `jarvix status --last`. One vocabulary everywhere: a number a user reads
// in the CLI must be greppable in the journal.
const (
	// StageCaptureToTranscript is push-to-talk release to the final transcript.
	StageCaptureToTranscript = "capture_to_transcript_ms"
	// StageContext is the transcript to the end of desktop-context gathering
	// (ADR 0019). Absent when context is disabled or nothing was gathered —
	// which is also how a reader tells the two apart.
	StageContext = "context_ms"
	// StageTranscriptToDelta is the transcript (or the end of context
	// gathering, when there was any) to the provider's first output — a text
	// token or a tool call. The model's thinking time, which is the user's
	// choice of model rather than Jarvix's latency.
	StageTranscriptToDelta = "transcript_to_first_delta_ms"
	// StageDeltaToFirstPCM is the first output to the first synthesized
	// sample, net of any excluded spans that fell inside it.
	StageDeltaToFirstPCM = "first_delta_to_first_pcm_ms"
	// StageFirstPCMToAudioOut is that sample reaching the audio device.
	StageFirstPCMToAudioOut = "first_pcm_to_audio_out_ms"
	// StageToolRuns is the turn's total time inside tool executions. Excluded
	// from jarvix_ms: how long `docker ps` takes is the command's runtime,
	// not scaffolding latency, and an instrument that swings with it cannot
	// catch regressions. Absent when no tool ran.
	StageToolRuns = "tool_ms"
	// StageConfirmWait is the turn's total time waiting for the user to
	// answer tool confirmations, from each question being put to them to
	// their answer (or the timeout). It is the user thinking — attributed to
	// neither Jarvix nor the model, the same honest split context_ms made in
	// #34, but reported so the turn's real length is still on the record.
	// Absent when nothing was asked.
	StageConfirmWait = "confirm_wait_ms"
	// StageReleaseToFirstAudio is the headline: release to the first sound,
	// wall clock — what the user actually waited, pauses included.
	StageReleaseToFirstAudio = "release_to_first_audio_ms"
	// StageJarvixOverhead is the headline minus thinking time and minus the
	// excluded spans that fell before the first sound — the part this
	// codebase is accountable for, and the number the 1.5s budget is set on.
	// Non-negative by construction: it equals the sum of the Jarvix-owned
	// pipeline spans, each of which is a real elapsed interval.
	StageJarvixOverhead = "jarvix_ms"
	// StageSupersededSentences is how many queued sentences were dropped
	// unplayed because a newer turn's speech superseded them (issue #120) —
	// a count, the record's one key that is not milliseconds, which is why
	// the name carries no _ms. Absent when nothing was dropped, like every
	// stage that did not happen.
	StageSupersededSentences = "superseded_sentences"
	// StageNothingHeard is why the capture produced no words (issue #191) —
	// the record's only string, which is why the name carries no _ms either.
	// Absent from every turn that produced a transcript, so its presence is
	// the whole message and its text is the detail.
	StageNothingHeard = "nothing_heard"
	// StageTier is the model tier that answered (issue #159): "instant",
	// "medium" or "deep". Absent on every turn of a configuration with no
	// tiers, which is how a reader tells "medium answered" from "there is no
	// routing here at all" — a key that said "medium" on every turn would be
	// claiming a decision nobody made.
	StageTier = "tier"
	// StageTierModel is what actually served it — the model name, or
	// "advisor <name>" for an advisor-backed tier. The thing that answered,
	// never the thing that was asked for.
	StageTierModel = "tier_model"
	// StageTierReason is why that tier: default, pinned, asked, unavailable,
	// or tools (the never-instant-with-tools rule firing). It is what makes
	// the routing auditable after the fact — two turns on medium for
	// completely different reasons are not the same turn.
	StageTierReason = "tier_reason"
	// StageTierWanted is the tier that was asked for and not served, absent
	// when nothing was refused.
	StageTierWanted = "tier_wanted"
	// StageTierContextDropped is how many prior exchanges the serving tier's
	// tighter context budget left out of the prompt (ADR 0037's stance: a
	// budget that trims discloses it). Absent when nothing was dropped.
	StageTierContextDropped = "tier_context_dropped"
	// StageHostTier is the tier that spoke while the answer was being worked
	// out (issue #161) — always "instant", because the host *is* the instant
	// tier, and named anyway so the record states it rather than implying it.
	// Absent from every turn on which the host did not produce a line.
	StageHostTier = "host_tier"
	// StageHostModel is the model that produced that line, on StageTierModel's
	// exact terms: the thing that spoke, never the thing that was asked for.
	StageHostModel = "host_model"
	// StageHostOutcome is what became of it: "held" (a holding line went to the
	// voice), "clarified" (the host asked for a missing detail and took the
	// turn), or "refused" (the honesty guard discarded it unspoken). Read
	// "held" beside superseded_sentences, which is what says whether the answer
	// overtook the line before it was heard.
	StageHostOutcome = "host_outcome"
)

// StageOrder is the order stages are reported in, so every surface prints the
// pipeline in pipeline order (the excluded spans sit between the pipeline and
// its totals: they happened inside the turn but belong to no pipeline stage).
var StageOrder = []string{
	StageCaptureToTranscript,
	StageContext,
	StageTranscriptToDelta,
	StageDeltaToFirstPCM,
	StageFirstPCMToAudioOut,
	StageToolRuns,
	StageConfirmWait,
	StageReleaseToFirstAudio,
	StageJarvixOverhead,
	// The superseded count sits last: it is not a span at all, and it
	// qualifies everything above it — the audio the stages measure is the
	// audio that survived.
	StageSupersededSentences,
	// And the reason a turn heard nothing sits last of all: it qualifies the
	// spans above it hardest, because when it is present there was no
	// question and the pipeline stopped at the microphone.
	StageNothingHeard,
	// The tier keys sit after it, together, because they qualify the numbers
	// above from a different direction: not how long each stage took, but
	// which model the model-time belongs to (issue #159).
	StageTier,
	StageTierModel,
	StageTierReason,
	StageTierWanted,
	StageTierContextDropped,
	// And the host's keys after those, in the order the turn happened: the
	// host spoke first, but it is read last, because everything above it is
	// about the answer and this is the qualifier on it (issue #161).
	StageHostTier,
	StageHostModel,
	StageHostOutcome,
}

// report renders the marks as the session.timings payload. Stages that did not
// happen are absent rather than zero: a typed question has no capture, and a
// silent answer has no audio, and reporting either as "0 ms" would quietly
// flatter the numbers.
//
// Every published number is non-negative by construction. Pipeline spans are
// wall clock between consecutive marks minus the excluded time inside them
// (which can never exceed the wall clock, since excluded time accrues at most
// one second per second); a span whose marks landed out of order — possible
// only through an accounting bug, since marks are one-way and the pipeline is
// ordered — is dropped rather than published negative, the "consistently or
// not at all" rule for partial timings.
func (t *timings) report() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := map[string]any{}
	span := func(name string, from, to mark) {
		if from.at.IsZero() || to.at.IsZero() || to.at.Before(from.at) {
			return
		}
		net := to.at.Sub(from.at) - (to.excluded - from.excluded)
		if net < 0 {
			return
		}
		out[name] = net.Milliseconds()
	}
	span(StageCaptureToTranscript, t.captureStop, t.transcript)
	// Context gathering sits between the transcript and the request, so the
	// model's clock starts where gathering stopped. Reported separately, and
	// therefore counted in jarvix_ms rather than excused as thinking time.
	modelFrom := t.transcript
	if !t.contextDone.at.IsZero() {
		span(StageContext, t.transcript, t.contextDone)
		modelFrom = t.contextDone
	}
	span(StageTranscriptToDelta, modelFrom, t.firstDelta)
	span(StageDeltaToFirstPCM, t.firstDelta, t.firstPCM)
	span(StageFirstPCMToAudioOut, t.firstPCM, t.audioOut)

	// The superseded count (issue #120): present exactly when sentences were
	// dropped, absent otherwise — "0 skipped" would read as an event that
	// happened and rounded down, and no such event exists.
	if t.supersededSentences > 0 {
		out[StageSupersededSentences] = t.supersededSentences
	}

	// The reason a capture produced nothing (issue #191): present exactly
	// when there was one. It is also what keeps this report non-empty on a
	// turn that never reached the model, so publishTimings still publishes
	// and the record shows that a capture happened.
	if t.nothingHeard != "" {
		out[StageNothingHeard] = t.nothingHeard
	}

	// Which model answered (issue #159), present exactly when a tier decided
	// it. A turn from a configuration with no tiers reports nothing here, so
	// the key's presence is itself the statement that routing happened.
	if t.tier != "" {
		out[StageTier] = t.tier
		out[StageTierModel] = t.tierModel
		out[StageTierReason] = t.tierReason
		if t.tierWanted != "" {
			out[StageTierWanted] = t.tierWanted
		}
		if t.tierContextDropped > 0 {
			out[StageTierContextDropped] = t.tierContextDropped
		}
	}

	// What the host did (issue #161), present exactly when it did something.
	// Its absence is the statement that the answer arrived inside the grace and
	// nothing was said over it — which is the outcome this feature wants most.
	if t.hostOutcome != "" {
		out[StageHostTier] = t.hostTier
		out[StageHostModel] = t.hostModel
		out[StageHostOutcome] = t.hostOutcome
	}

	// The excluded spans, published whenever they happened — a tool that ran
	// in under a millisecond still ran. A span still open (the session was
	// cancelled mid-wait) is settled at the report's clock, so partial
	// timings publish consistently rather than vanishing or going negative.
	for stage, d := range t.excluded {
		out[stage] = d.Milliseconds()
	}
	if t.activeStage != "" {
		var open time.Duration
		if now := t.clock(); now.After(t.activeStart) {
			open = now.Sub(t.activeStart)
		}
		prior, _ := out[t.activeStage].(int64)
		out[t.activeStage] = prior + open.Milliseconds()
	}

	// The headline: release to the first sound, which is what "answers begin
	// within 1.5 seconds" means. Deliberately wall clock — the user waited
	// through every pause.
	if t.captureStop.at.IsZero() || t.audioOut.at.IsZero() || t.audioOut.at.Before(t.captureStop.at) {
		return out
	}
	total := t.audioOut.at.Sub(t.captureStop.at)
	out[StageReleaseToFirstAudio] = total.Milliseconds()
	// Thinking time is the user's choice of model, and the excluded spans
	// before the first sound are the user's command or the user's decision;
	// subtracting both leaves the number this codebase is accountable for.
	if _, ok := out[StageTranscriptToDelta]; !ok {
		return out
	}
	thinking := t.firstDelta.at.Sub(modelFrom.at) - (t.firstDelta.excluded - modelFrom.excluded)
	window := t.audioOut.excluded - t.captureStop.excluded
	overhead := total - thinking - window
	if overhead < 0 {
		return out
	}
	out[StageJarvixOverhead] = overhead.Milliseconds()
	return out
}
