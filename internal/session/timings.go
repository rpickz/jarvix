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
// That is what makes "first delta" and "first PCM" mean what they say, and it
// keeps the tool-call loop (which streams several times per session) from
// resetting the clock halfway through an answer.
type timings struct {
	mu sync.Mutex

	// captureStop is the moment the user released push-to-talk. Zero for a
	// typed question (`jarvix ask`), which never captures audio.
	captureStop time.Time
	// transcript is when the words were known — from STT, or immediately for
	// a typed question.
	transcript time.Time
	// contextDone is when desktop context finished being gathered (ADR 0019).
	// Zero when context is disabled or the turn never reached the model.
	//
	// It exists so gathering is charged to Jarvix rather than to the model:
	// it happens between the transcript and the provider request, so without
	// its own mark it would sit inside transcript_to_first_delta_ms — the one
	// span that is *subtracted* from jarvix_ms as the user's choice of model.
	// A cost that hides inside the number it inflates is the one kind of
	// measurement worse than none.
	contextDone time.Time
	// firstDelta is the provider's first token. The gap before it is the
	// model's thinking time: reported, never included in Jarvix's own budget.
	firstDelta time.Time
	// firstPCM is the first audio sample the synthesizer produced.
	firstPCM time.Time
	// audioOut is when that sample reached the audio device (audio.Trace).
	audioOut time.Time
}

func (t *timings) markCaptureStop() { t.set(&t.captureStop) }
func (t *timings) markTranscript()  { t.set(&t.transcript) }
func (t *timings) markContext()     { t.set(&t.contextDone) }
func (t *timings) markFirstDelta()  { t.set(&t.firstDelta) }
func (t *timings) markFirstPCM()    { t.set(&t.firstPCM) }
func (t *timings) markAudioOut()    { t.set(&t.audioOut) }

func (t *timings) set(field *time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if field.IsZero() {
		*field = time.Now()
	}
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
	// gathering, when there was any) to the provider's first token — the
	// model's thinking time, which is the user's choice of model rather
	// than Jarvix's latency.
	StageTranscriptToDelta = "transcript_to_first_delta_ms"
	// StageDeltaToFirstPCM is the first token to the first synthesized sample.
	StageDeltaToFirstPCM = "first_delta_to_first_pcm_ms"
	// StageFirstPCMToAudioOut is that sample reaching the audio device.
	StageFirstPCMToAudioOut = "first_pcm_to_audio_out_ms"
	// StageReleaseToFirstAudio is the headline: release to the first sound.
	StageReleaseToFirstAudio = "release_to_first_audio_ms"
	// StageJarvixOverhead is the headline minus thinking time — the part this
	// codebase is accountable for, and the number the 1.5s budget is set on.
	StageJarvixOverhead = "jarvix_ms"
)

// StageOrder is the order stages are reported in, so every surface prints the
// pipeline in pipeline order.
var StageOrder = []string{
	StageCaptureToTranscript,
	StageContext,
	StageTranscriptToDelta,
	StageDeltaToFirstPCM,
	StageFirstPCMToAudioOut,
	StageReleaseToFirstAudio,
	StageJarvixOverhead,
}

// report renders the marks as the session.timings payload. Stages that did not
// happen are absent rather than zero: a typed question has no capture, and a
// silent answer has no audio, and reporting either as "0 ms" would quietly
// flatter the numbers.
func (t *timings) report() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := map[string]any{}
	span := func(name string, from, to time.Time) {
		if from.IsZero() || to.IsZero() || to.Before(from) {
			return
		}
		out[name] = to.Sub(from).Milliseconds()
	}
	span(StageCaptureToTranscript, t.captureStop, t.transcript)
	// Context gathering sits between the transcript and the request, so the
	// model's clock starts where gathering stopped. Reported separately, and
	// therefore counted in jarvix_ms rather than excused as thinking time.
	modelFrom := t.transcript
	if !t.contextDone.IsZero() {
		span(StageContext, t.transcript, t.contextDone)
		modelFrom = t.contextDone
	}
	span(StageTranscriptToDelta, modelFrom, t.firstDelta)
	span(StageDeltaToFirstPCM, t.firstDelta, t.firstPCM)
	span(StageFirstPCMToAudioOut, t.firstPCM, t.audioOut)
	// The headline: release to the first sound, which is what "answers begin
	// within 1.5 seconds" means.
	span(StageReleaseToFirstAudio, t.captureStop, t.audioOut)
	// Thinking time is the user's choice of model, not Jarvix's latency, so
	// subtracting it gives the number this codebase is accountable for.
	if thinking, ok := out[StageTranscriptToDelta].(int64); ok {
		if total, ok := out[StageReleaseToFirstAudio].(int64); ok {
			out[StageJarvixOverhead] = total - thinking
		}
	}
	return out
}
