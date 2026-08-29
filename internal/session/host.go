package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

// This file is the host cascade (issue #161, ADR 0064): the instant tier stops
// being a cheap answerer and becomes the **handler for the user** while a
// heavier model produces the real answer.
//
// The inversion is the whole idea. A bigger model means dead air, and dead air
// in a voice interface is worse than a mediocre answer — so instead of making
// the user choose between quick and good, a different model owns each. The
// host owns *presence*: acknowledging, setting the expectation, asking for a
// detail that is genuinely missing. The answering tier owns *correctness*. The
// host is never asked the question in the sense of being asked to answer it.
//
// The shape, in order:
//
//  1. **The answer goes first.** think() issues the answering tier's request on
//     its own goroutine and the host waits for that to have happened
//     (sess.answerIssued) before it opens its own. The host can therefore never
//     cost the answer a millisecond, which is the condition on which this whole
//     feature is allowed to exist.
//  2. **Both run.** The host's request is in flight during the wait, so when
//     the grace expires the line is already there. Calling the host only *after*
//     the grace would put the small model's own latency on top of it and the
//     holding line would land a second late, covering nothing.
//  3. **The grace decides whether it is spoken at all.** If the answering tier
//     has begun streaming before the grace elapses, the host is cancelled and
//     says nothing — a fast turn has no chatter on it, and that is pinned.
//  4. **The guard decides whether it *may* be spoken** (hostguard.go). A line
//     that asserts, guesses or claims an action is discarded, not spoken.
//  5. **A question takes the turn.** If the accepted line is a clarifying
//     question the answer attempt is abandoned, the question becomes this
//     turn's reply, and the user's answer to it continues the same conversation
//     through ordinary history — a clarification must never strand the original
//     question.
//
// Nothing here opens a second playback path. A holding line goes through the
// turn's one streamingSpeaker like every other sentence Jarvix says (the
// one-stream doctrine of #52/#53), which is what makes "the host's sentence
// finishes and the answer follows" true by construction rather than by timing:
// a begun sentence is never cut, and a holding line the answer overtook while
// it sat in the queue is dropped unplayed by the supersession floor of #120 /
// #133. Both behaviours are inherited, not rebuilt.

// hostMaxTokens caps the host's reply. A holding line is one short sentence, so
// the budget is set just above one — and the cap is part of the guard as well
// as a latency measure: a model that starts writing an answer is cut off
// mid-sentence, and a sentence with no terminator and too many words is refused
// by hostLineVerdict rather than spoken.
const hostMaxTokens = 48

// hostSystemPrompt is the host's whole instruction, pinned here as a constant
// because it is a contract rather than a setting. It is quoted almost verbatim
// by the guard's allowlist (hostguard.go): the examples it gives are shapes the
// guard accepts, so a model that does as it is told lands inside the rules.
//
// It is *not* the enforcement. A prompt is a request, and the host is by
// definition the weakest model in the house — the one least likely to honour
// one. The guard is the enforcement; this makes the guard's job easy.
const hostSystemPrompt = "You are the voice that keeps someone company while a stronger model works out " +
	"the answer to their question. You are NOT answering the question, and you must not try.\n\n" +
	"Reply with ONE short sentence and nothing else. It must be one of these two things:\n" +
	"  1. A holding line, saying only that you are thinking about it. For example: " +
	"\"Let me think about that properly.\" \"One moment.\" \"Give me a second.\" " +
	"\"Bear with me.\"\n" +
	"  2. A clarifying question, and ONLY when the request is genuinely ambiguous and you " +
	"cannot tell which of two things is meant. For example: " +
	"\"Do you mean the deploy script or the deploy thread?\"\n\n" +
	"You must never state a fact about the subject, never guess at the answer, never say " +
	"what something is or does, and never say that you have done anything: you have no " +
	"tools, you cannot act on this computer, and nothing has happened.\n" +
	"If you are in any doubt at all, give a holding line."

// The host outcomes the turn's record can carry. Only the three that mean the
// host actually produced something are ever written: a host that stood down
// because the answer was quick did nothing, and a record key on every fast turn
// saying so would be noise on the exact turns this feature is proud of.
const (
	// hostOutcomeHeld: a holding line was committed to the voice. Read it
	// beside superseded_sentences, which is what says whether the answer
	// overtook it before it was heard.
	hostOutcomeHeld = "held"
	// hostOutcomeClarified: the host asked for a missing detail and took the
	// turn; the answering tier's attempt was abandoned.
	hostOutcomeClarified = "clarified"
	// hostOutcomeRefused: the host said something it is not allowed to say and
	// it was discarded unspoken. This is the key an operator needs when the
	// feature looks like it is not working: it is working, and the host is
	// misbehaving.
	hostOutcomeRefused = "refused"
)

// errHostToolCall is what a host that tries to call a tool gets. It cannot
// legitimately happen — hostRequest carries no tool definitions and has no
// parameter through which any could be passed — so a provider producing one is
// either a misconfigured endpoint or a model hallucinating a capability, and
// both are reasons to throw the line away rather than reason about it.
var errHostToolCall = errors.New("the host tier tried to call a tool")

// hostRun is one turn's host: the goroutine, the two contexts it arbitrates
// between, and the clarification it may take the turn with.
//
// It is always non-nil, even when the host stands down, so think() has one code
// path rather than a nil check at every use. A stood-down host has answerCtx ==
// s.ctx, a closed done channel and no goroutine, which makes every method below
// a no-op at the cost of nothing.
type hostRun struct {
	e *Engine
	s *sess
	// binding is the resolved instant tier — what the record names as the
	// thing that spoke. Zero on a stood-down host, which never writes one.
	binding TierBinding

	// answerCtx bounds the answering tier's first round. It is the session
	// context when the host stands down, and a child of it otherwise — the
	// child exists so a clarification can abandon the answer without touching
	// the session, which still has a reply to speak and an exchange to commit.
	answerCtx    context.Context
	cancelAnswer context.CancelFunc
	// cancelHost stops the host: its provider call, its wait, and any holding
	// line still queued but not yet spoken.
	cancelHost context.CancelFunc
	// done is closed when the host goroutine has exited. stop() waits on it,
	// which is what guarantees no host goroutine is still holding a reference
	// to the speaker when think() closes it — a send on a closed channel is
	// the one way this design could crash the daemon, and joining removes it.
	done     chan struct{}
	stopOnce sync.Once

	mu sync.Mutex
	// clarify is the question the host took the turn with, "" when it did not.
	clarify string
}

// hostGrace is how long the answering tier has before the host speaks. Zero or
// less stands the host down for every turn, which is what an engine built by
// anything other than a daemon holding `[ai.tiers] host_grace_ms` gets — see
// Options.HostGrace.
func (e *Engine) hostGrace() time.Duration { return e.opts.HostGrace }

// hostTimer is the grace clock, injectable so a test can expire the grace at an
// exact point in an interleaving rather than sleeping through it. Never
// overridden in production.
func (e *Engine) hostTimer(d time.Duration) (<-chan time.Time, func()) {
	if e.opts.HostGraceTimer != nil {
		return e.opts.HostGraceTimer(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// hostBinding decides whether this turn has a host at all, and what it is.
//
// Every "no" here is silent and costs nothing: the turn takes exactly the path
// it took before this feature existed. That is the degradation requirement
// stated as code — an unconfigured or unreachable host must not add a delay, an
// error, or a sentence.
//
// The reasons to stand down:
//
//   - Tiering is off, or there is no instant tier. There is no host to be.
//   - **The user chose Quick.** The host's entire purpose is covering a wait,
//     and Quick is the user saying they would rather not have one. This covers
//     both the turn instant actually serves and the turn where the
//     never-instant-with-tools rule bumped it to medium: the choice was still
//     made, and answering it with chatter would be answering a different
//     question.
//   - There is no voice on this turn (speech off, or a quiet scheduled
//     session). A host with nothing to speak through is a provider call spent
//     on silence.
//   - The turn already owes the user a sentence — a tier notice. It has been
//     told what is happening; being told twice is worse than once.
//   - The grace is configured off — which, since zero is off, is also every
//     engine that was never told about a grace at all.
func (e *Engine) hostBinding(serve serving, speaker *streamingSpeaker, notice string) (TierBinding, bool) {
	if !serve.on || speaker == nil || notice != "" || e.hostGrace() <= 0 {
		return TierBinding{}, false
	}
	if serve.route.Tier == ai.TierInstant || serve.route.Wanted == ai.TierInstant {
		return TierBinding{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	binding, ok := e.opts.Tiers.Bindings[ai.TierInstant]
	if !ok || binding.Provider == nil {
		return TierBinding{}, false
	}
	return binding, true
}

// startHost arms the host for this turn and returns immediately.
//
// It is called on think()'s goroutine directly before the answering tier's
// request is issued, and it does no blocking work at all — everything it starts
// runs on a goroutine that waits for the answer to have been issued before it
// does anything of its own.
func (e *Engine) startHost(s *sess, serve serving, speaker *streamingSpeaker,
	notice, userText string) *hostRun {
	h := &hostRun{e: e, s: s, answerCtx: s.ctx, done: make(chan struct{})}
	binding, ok := e.hostBinding(serve, speaker, notice)
	if !ok {
		close(h.done)
		return h
	}
	answerCtx, cancelAnswer := context.WithCancel(s.ctx)
	hostCtx, cancelHost := context.WithCancel(s.ctx)
	h.binding = binding
	h.answerCtx, h.cancelAnswer, h.cancelHost = answerCtx, cancelAnswer, cancelHost
	go h.run(hostCtx, speaker, userText)
	return h
}

// ctx is the context the answering tier's first round runs under.
func (h *hostRun) ctx() context.Context { return h.answerCtx }

// stop joins the host goroutine and releases the answer's context. It is
// idempotent and safe on a stood-down host.
//
// think() calls it the moment the first provider round returns, which is before
// anything in this package closes the speaker. That ordering is the whole
// reason it exists: enqueue sends on a channel the speaker's close() closes,
// and joining here means no host goroutine can be inside enqueue by then.
func (h *hostRun) stop() {
	h.stopOnce.Do(func() {
		if h.cancelHost != nil {
			h.cancelHost()
		}
		<-h.done
		if h.cancelAnswer != nil {
			// Released only now: the round this context bounded has finished,
			// and cancelling it any earlier would cut a live answer.
			h.cancelAnswer()
		}
	})
}

// clarification reports the question the host took the turn with. Only
// meaningful after stop(), which is where the happens-before edge comes from;
// the mutex is here so the race detector can see it too.
func (h *hostRun) clarification() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clarify, h.clarify != ""
}

// run is the host's whole life.
func (h *hostRun) run(ctx context.Context, speaker *streamingSpeaker, userText string) {
	defer close(h.done)
	// What this host ended up doing, reported to the test seam as the goroutine
	// exits — after every decision, before stop() can observe the join. "" is a
	// host that stood down without saying anything, which is the common case
	// and the one a fast turn wants.
	outcome := ""
	defer func() {
		if hook := h.e.hostFinished; hook != nil {
			hook(outcome)
		}
	}()

	// The answer goes first. Always, and by waiting rather than by ordering
	// two statements and hoping: the host's request cannot be issued until the
	// answering tier's has been, so no scheduling accident can put the small
	// model's connection setup in front of the one that matters.
	select {
	case <-h.s.answerIssued():
	case <-ctx.Done():
		return
	}

	// The host's request runs during the wait, not after it. Calling it only
	// once the grace had expired would add the small model's own latency to the
	// grace and the holding line would arrive covering a silence that had
	// already ended.
	//
	// This goroutine is deliberately not joined by stop(), and cannot be: it may
	// be parked inside the provider, and the thing that releases it is the very
	// cancellation stop() performs — waiting for it here would deadlock against
	// exactly that. It is *bounded* instead, which is what actually matters. It
	// writes to a buffered channel nobody has to read, it touches nothing the
	// turn owns, and above all it never speaks: every path to the voice runs on
	// run()'s own goroutine, and that one stop() does join.
	lines := make(chan string, 1)
	go func() {
		defer close(lines)
		line, err := h.consult(ctx, userText)
		if err != nil {
			if ctx.Err() == nil {
				// Unreachable, or a host that tried to act. Logged and
				// forgotten: the turn is already being answered by somebody
				// else, and the user is owed nothing about a model that was
				// only ever going to say "one moment".
				h.e.log.Warn("host tier did not answer", "component", "assistant",
					"session_id", h.s.id, "error", err.Error())
			}
			return
		}
		lines <- line
	}()

	// The grace, measured from the same instant transcript_to_first_delta_ms
	// is measured from, so the number a user sets it against is the number the
	// record already prints for them. A turn with neither mark — a typed
	// question on a daemon with no context collector reaches the provider
	// without either — is measured from here, which for that turn is the same
	// instant to within the cost of assembling a prompt.
	from := h.s.timings.modelStart()
	if from.IsZero() {
		from = time.Now()
	}
	timer, stopTimer := h.e.hostTimer(time.Until(from.Add(h.e.hostGrace())))
	defer stopTimer()
	select {
	case <-h.s.answerBegun():
		// The answering tier beat the grace. The host says nothing at all —
		// this is the no-chatter-on-fast-turns pin, and it is the common case
		// on a warm machine.
		return
	case <-ctx.Done():
		return
	case <-timer:
	}

	// Past the grace with nothing from the answering tier. Take the host's
	// line when it comes, and stand down the moment the answer starts: the
	// host is never waited *for*, it is only ever raced against.
	var raw string
	select {
	case line, ok := <-lines:
		if !ok {
			return
		}
		raw = line
	case <-h.s.answerBegun():
		return
	case <-ctx.Done():
		return
	}
	// Both selects above can have two ready cases at once — the grace expiring
	// in the same instant the first token lands — and Go picks between ready
	// cases at random. So the answer is asked once more here, where nothing is
	// racing it: a select is a choice, and this is the decision. The remaining
	// window (the answer beginning after this check) is not closed here on
	// purpose; it is closed at the queue by supersession, which drops the line
	// unplayed rather than trying to win a race at the microphone.
	if answerBegan(h.s) {
		return
	}

	line, kind, why := hostLineVerdict(raw)
	if kind == hostLineRefused {
		// Discarded, never spoken, and said so on the record. The refusal is
		// logged with the offending text because "the host is saying things it
		// should not" is a thing an operator has to be able to see; it is
		// deliberately not published as an event, because an event carrying it
		// is a client displaying it, and a line this guard refused must not
		// reach the user by any route.
		h.e.log.Warn("host line refused", "component", "assistant", "session_id", h.s.id,
			"reason", why, "line", raw)
		outcome = hostOutcomeRefused
		h.note(hostOutcomeRefused)
		return
	}

	if kind == hostLineClarifying {
		if !h.s.claimHost() {
			// The answering tier began between the select above and here.
			// It wins: abandoning an answer that has already put words on the
			// screen to ask a question instead would be the worst of both.
			return
		}
		h.mu.Lock()
		h.clarify = line
		h.mu.Unlock()
		outcome = hostOutcomeClarified
		h.note(hostOutcomeClarified)
		h.publish("clarification", line)
		// The answer attempt ends here. think() speaks the question, records it
		// as this turn's reply, and the user's answer continues the same
		// conversation through ordinary history.
		h.cancelAnswer()
		return
	}

	outcome = hostOutcomeHeld
	h.note(hostOutcomeHeld)
	h.publish("holding", line)
	// Queued on the turn's one voice, at a speech turn below the answer's, so
	// the answer's first sentence supersedes it if it is still waiting when
	// that sentence is committed (#120/#133) — and never cuts it if it has
	// already begun (#52/#53). Blocking until it has been handed to the player
	// is interject's contract and is what keeps this goroutine alive until the
	// line is safely somebody else's problem.
	speaker.holdFor(ctx, line)
}

// answerBegan reports, without blocking, whether the answering tier has already
// produced output.
func answerBegan(s *sess) bool {
	select {
	case <-s.answerBegun():
		return true
	default:
		return false
	}
}

// consult runs the host's own provider call and returns its whole line.
//
// Whole, not streamed to the voice a sentence at a time: the guard has to see
// the entire line before any of it is spoken, because a refusal check that ran
// on the first half would pass "Let me think" and then be far too late for the
// clause after it.
func (h *hostRun) consult(ctx context.Context, userText string) (string, error) {
	events, err := h.binding.Provider.Chat(ctx, h.e.hostRequest(h.binding, userText))
	if err != nil {
		return "", err
	}
	var line strings.Builder
	for ev := range events {
		switch ev.Type {
		case ai.EventDelta:
			line.WriteString(ev.Content)
		case ai.EventToolCall:
			// The host holds no tools — see hostRequest. A provider that
			// produces a call anyway has been given capabilities from
			// somewhere this code does not control, and the safe reading of
			// that is to throw the whole line away.
			return "", errHostToolCall
		case ai.EventError:
			return "", ev.Err
		case ai.EventDone:
		}
	}
	return line.String(), nil
}

// hostRequest builds the host's provider request.
//
// **It carries no tools, and it takes no parameter through which any could be
// passed.** That is the hard rule of this feature made structural rather than
// remembered: ADR 0063 already refuses to let the instant tier hold tools when
// it is *answering*, and a model speaking first — before anything has been
// checked — is that risk at its sharpest. Issue #71 is the incident this is
// written against.
//
// The prompt is the host instruction and the user's question, and nothing else:
// no history, no remembered facts, no taught vocabulary, no desktop capture, no
// feed values. Three reasons, and they agree.
//
//   - **Latency.** First-token time scales with the prompt, and the host is
//     useless if it is not immediate.
//   - **Honesty.** A model that cannot see the screen, the knowledge base or
//     the conversation cannot state anything from them. The guard refuses
//     assertions; this makes most of them unavailable in the first place.
//   - **It does not need them.** Acknowledging a question and asking which of
//     two things it meant are both answerable from the question alone.
//
// Temperature is left at zero deliberately: the least inventive holding line is
// the one most likely to be a shape the guard permits, and there is nothing
// here worth being creative about.
func (e *Engine) hostRequest(binding TierBinding, userText string) ai.ChatRequest {
	return ai.ChatRequest{
		Model:     binding.Model,
		MaxTokens: hostMaxTokens,
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: hostSystemPrompt},
			{Role: ai.RoleUser, Content: userText},
		},
	}
}

// note writes what the host did into the turn's record, naming the tier and the
// model that produced the line — so "why did it say that?" is answerable, and
// answerable separately from "which model answered?" (see timings.noteHost).
func (h *hostRun) note(outcome string) {
	h.s.timings.noteHost(string(ai.TierInstant), hostModelName(h.binding), outcome)
}

// noteTookTurn rewrites the turn's *answering* tier as the host, for the one
// case where the host produced the reply: a clarifying question.
//
// It is not a duplicate of note() above. Those keys say what the host did;
// these say what answered the turn, and on a clarification the honest value is
// the host — the tier that was routed to was abandoned before it said a word,
// and a record naming it would claim an answer that never happened. Everything
// else about the turn is ordinary: assistant.started already said, truthfully,
// which tier had been asked.
func (h *hostRun) noteTookTurn() {
	h.s.timings.noteTier(string(ai.TierInstant), hostModelName(h.binding),
		string(ai.ReasonHost), "", 0)
}

// publish announces the host's line on the bus, distinct from the answer and
// labelled with the tier that produced it. Additive: a client that has never
// heard of the event ignores it, exactly as it ignores every other event type
// it does not know.
func (h *hostRun) publish(kind, line string) {
	h.e.publish(Event{Type: "assistant.host", Data: map[string]any{
		"session_id": h.s.id,
		"tier":       string(ai.TierInstant),
		"tier_label": ai.TierLabel(ai.TierInstant),
		"model":      hostModelName(h.binding),
		"kind":       kind,
		"content":    line,
	}})
}

// hostModelName is what the record says produced the host's line: the model for
// an endpoint-backed instant tier, "advisor <name>" for an advisor-backed one.
// The same rule serving.servedModel follows, and for the same reason — the
// record names what actually spoke.
func hostModelName(binding TierBinding) string {
	if binding.Advisor != "" {
		return "advisor " + binding.Advisor
	}
	return binding.Model
}

// speakHostClarification makes the host's question this turn's reply.
//
// It walks the same path a streamed answer walks — first-delta mark, the move
// to Responding, an empty delta then the content, then the sentence to the
// voice — because it *is* this turn's reply, and every surface that watches a
// turn (the overlay's streaming text, the state machine, the interrupted
// commit of #117) must see it as one. The alternative, a special "host reply"
// path, would be a second way for Jarvix to say something, which is exactly
// what #52/#53 spent two issues removing.
//
// It reports false when the session ended underneath it; that path owns its own
// events, as it always has.
func (e *Engine) speakHostClarification(s *sess, speaker *streamingSpeaker, line string) bool {
	s.timings.markFirstDelta()
	if !e.advance(s, StateResponding) {
		return false
	}
	e.publish(Event{Type: "assistant.delta", Data: map[string]any{"session_id": s.id, "content": ""}})
	s.recordDelta(line)
	e.publish(Event{Type: "assistant.delta", Data: map[string]any{"session_id": s.id, "content": line}})
	if speaker != nil {
		speaker.speak(line)
	}
	return true
}
