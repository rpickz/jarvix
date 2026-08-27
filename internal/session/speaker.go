package session

import (
	"context"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/tts"
)

// sentencer splits a streaming token feed into complete sentences so speech
// can begin before the whole answer is generated.
//
// Its contract is byte-exact: the sentences it emits, concatenated, are the
// pushed text with ASCII whitespace removed at the sentence seams — nothing
// else is added, dropped, or reordered. Provider deltas are chunked wherever
// the network happens to break them, which can land in the middle of a
// multi-byte rune, so the splitter is careful about two things:
//
//   - it never cuts inside a rune (drain holds a truncated trailing encoding
//     back until the bytes that complete it arrive, and flush emits it rather
//     than discarding it), so no sentence ever reaches TTS as broken UTF-8;
//   - it trims only ASCII whitespace, never Unicode whitespace. Trimming a
//     non-ASCII space would make the splitter's output depend on how bytes
//     decode *after* the seams are removed, which is not stable under
//     concatenation: an incomplete 0xC2 and a stray 0xA0 either side of a
//     removed separator fuse into U+00A0. The ASCII rule keeps "content in ==
//     content out" a statement about bytes (issue #28).
type sentencer struct {
	buf strings.Builder
}

// isSeamSpace reports whether b is the ASCII whitespace a streamed token feed
// separates sentences with. Deliberately not Unicode whitespace — see the
// sentencer doc comment.
func isSeamSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r'
}

// trimSeam strips the ASCII whitespace around one emitted sentence. It is
// byte-safe on invalid UTF-8: every trimmed byte is ASCII, so a continuation
// byte is never mistaken for a trimmable one. Hand-rolled rather than
// strings.Trim because this runs per sentence on the streaming path and
// strings.Trim rebuilds its cutset table on every call.
func trimSeam(s string) string {
	start, end := 0, len(s)
	for start < end && isSeamSpace(s[start]) {
		start++
	}
	for end > start && isSeamSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

// incompleteTail reports how many bytes at the end of s begin a UTF-8
// encoding whose remaining bytes have not arrived yet — 0 when s ends on a
// rune boundary, or when the trailing bytes are not a truncated encoding at
// all (a stray continuation byte is content, and is passed through as-is).
//
// Only the last three bytes can matter: the longest encoding is four bytes,
// so at most three of them can still be outstanding. The ASCII check first
// keeps the common case (English prose, one byte per rune) to a single
// comparison — this runs on every streamed chunk.
func incompleteTail(s string) int {
	if len(s) == 0 || s[len(s)-1] < utf8.RuneSelf {
		return 0
	}
	for n := 1; n <= 3 && n <= len(s); n++ {
		b := s[len(s)-n]
		if !utf8.RuneStart(b) {
			continue // a continuation byte: keep walking back to its lead byte
		}
		if utf8.FullRuneInString(s[len(s)-n:]) {
			return 0
		}
		return n
	}
	return 0
}

// push adds streamed text and returns any newly-complete sentences.
func (sc *sentencer) push(text string) []string { sc.buf.WriteString(text); return sc.drain(false) }

// flush returns whatever remains as a final sentence.
func (sc *sentencer) flush() []string { return sc.drain(true) }

// maxSentenceRunon flushes an unpunctuated buffer once it grows this long, so
// a wall of text without terminators does not hold speech hostage.
const maxSentenceRunon = 240

func (sc *sentencer) drain(final bool) []string {
	s := sc.buf.String()
	// Everything up to `end` is whole runes. A truncated trailing encoding is
	// held in the buffer for the next chunk to complete; on a final drain
	// nothing is held back, because there is no next chunk — a truncated
	// stream must still say its last character rather than swallow it.
	held := 0
	if !final {
		held = incompleteTail(s)
	}
	scan := s[:len(s)-held]

	var out []string
	start := 0
	for i := 0; i < len(scan); i++ {
		c := scan[i]
		if c != '.' && c != '!' && c != '?' && c != '\n' && c != ':' {
			continue
		}
		// A boundary only counts when whitespace (or end of a final buffer)
		// follows — this avoids splitting decimals like "3.5" and mid-word
		// colons like "http://".
		if i+1 < len(scan) {
			if n := scan[i+1]; n != ' ' && n != '\n' && n != '\t' {
				continue
			}
		} else if !final {
			break // wait for the next token to confirm the boundary
		}
		if sentence := trimSeam(scan[start : i+1]); sentence != "" {
			out = append(out, sentence)
		}
		start = i + 1
	}
	// rem is taken from s, not scan, so any held bytes stay buffered.
	rem := s[start:]
	if !final && len(rem) > maxSentenceRunon {
		if r := trimSeam(rem[:len(rem)-held]); r != "" {
			out = append(out, r)
		}
		rem = rem[len(rem)-held:]
	}
	if final {
		if r := trimSeam(rem); r != "" {
			out = append(out, r)
		}
		rem = ""
	}
	sc.buf.Reset()
	sc.buf.WriteString(rem)
	return out
}

// utterance is one thing to say on the speaker's single playback stream.
type utterance struct {
	text string
	// aside marks something Jarvix says that is not part of the answer: a tool
	// confirmation question (ADR 0014), a "still working" reassurance while a
	// slow tool runs (ADR 0016). It travels the same queue — that is the whole
	// point, so it cannot talk over audio already playing (issue #52) — but it
	// must not claim the Speaking state or emit tts.* events, because it is not
	// the answer starting: the session is in AwaitingConfirmation or Thinking
	// while it plays, and the overlay already has its own event for each.
	aside bool
	// turn is which speech turn of this speaker committed the utterance —
	// stamped at enqueue from the speaker's monotonic turn counter (issue
	// #120). It exists so that when a later turn commits its first sentence,
	// this one can be recognised at dequeue as stale narration the
	// conversation has already moved past, and dropped unplayed. Asides carry
	// the turn they were queued during for the same test; whether an aside may
	// actually be dropped is keep's decision, not turn's.
	turn int
	// keep exempts an aside from supersession — it plays however far the
	// answer has moved on. The policy (issue #120), pinned here because this
	// flag is where it is enforced:
	//
	//   - A confirmation question keeps. It gates progress: the turn is parked
	//     in AwaitingConfirmation until it is answered or times out, so
	//     dropping it would leave the user a silent countdown they were never
	//     asked about. (In practice the asking goroutine is blocked while the
	//     question is queued, so no later turn can commit speech around it —
	//     but the guarantee must hold by construction, not by the current
	//     shape of the caller.)
	//   - A receipt of an executed action must keep, if one is ever spoken as
	//     an aside: the action happened, and saying so is the record — honesty
	//     outranks freshness. There is no such call site today; this is the
	//     doctrine a future one inherits.
	//   - A progress reassurance does not keep. "Still working" is only true
	//     while the tool runs, and a later turn's sentence can only have been
	//     committed after that tool returned — so a reassurance still queued
	//     by then would announce work already finished. Stale comfort is
	//     noise, and it drops.
	//
	// Answer sentences never set keep: within a turn nothing is ever skipped
	// (a sentence of the newest turn is never stale by definition), and across
	// turns dropping them is the whole feature.
	keep bool
	// ctx, non-nil only for an aside, bounds that one utterance: cancelling it
	// stops the aside's synthesis and drops whatever of it has not reached the
	// player, without touching the stream or the answer around it (issue #119
	// — a resolved confirmation silences the rest of its own question). Always
	// a child of the session context, so a dying session still cancels asides
	// exactly as before.
	ctx context.Context
	// done, when non-nil, is closed once this utterance has been handed to the
	// player (or abandoned). It is how awaitConfirmation waits for the question
	// to have been asked before it starts the user's clock.
	done chan struct{}
}

// streamingSpeaker synthesizes and plays sentences as they arrive, over a
// single continuous playback stream, so Jarvix begins speaking while the rest
// of the answer is still being generated. One speaker serves a whole think()
// call (all tool rounds), keeping audio gapless and ordered.
//
// "One stream" is a correctness property, not an optimisation: everything the
// turn says goes through this one queue and this one audio.Player.Play, so two
// voices can never be heard at once. A confirmation question raised mid-answer
// is therefore queued here rather than played on a stream of its own.
type streamingSpeaker struct {
	e   *Engine
	s   *sess
	in  chan utterance
	res chan error

	// The speaker is the one component that owns the playback stream, so it is
	// the one component that can answer "is audio playing?" — the question
	// CancelSpeech used to answer by reading the session state, which describes
	// what the turn is doing, not whether the device is busy. The two diverge
	// routinely: a mid-answer tool round puts the session back in Thinking or
	// Responding while sentences already queued here are still draining, which
	// is exactly when "stop" used to do nothing (issue #54).
	//
	// mu guards the three flags below and nothing else; it is only ever taken
	// as a leaf, so it can never deadlock against Engine.mu.
	mu sync.Mutex
	// accepted is set once the first utterance is committed to the queue:
	// from that moment there is audio in flight or committed to be, even
	// while synthesis is still working — killing playback must not wait on
	// synthesis. It is set *before* the utterance is visible to run(), which
	// makes an invariant hold by construction: any subscriber that observes
	// tts.started (published downstream of the handoff) finds speaking()
	// live until playback drains, because accepted happened-before the event
	// and drained cannot be set while the answer's chunks are still gated
	// (issue #80).
	accepted bool
	// announced is set once the answer has claimed Speaking and published
	// tts.started — by announce, at enqueue, on the goroutine that commits
	// the answer's first sentence to the voice (see announce for why that
	// placement is the issue #111 fix). A cancel reads it to know whether a
	// tts.finished bookend is owed, and run reads it back for the same
	// bookend on a clean drain.
	announced bool
	// drained is set once run() has finished: nothing is playing and nothing
	// ever will be again on this speaker.
	drained bool
	// turn is the speech turn utterances are being committed for — 1 for the
	// answer's first provider round, advanced by nextTurn each time the tool
	// loop opens another round (issue #120). Only the enqueueing goroutine
	// ever moves it, but it sits under mu because run() reads floor (below)
	// against the turn stamped on each utterance, and the two must move under
	// one lock or a drop decision could read a torn pair.
	turn int
	// floor is the newest turn that has committed an answer sentence to the
	// queue, and therefore the oldest turn still allowed to play: at dequeue,
	// anything below it that does not keep is stale — narration for a turn
	// the conversation has already moved past — and is dropped unplayed
	// (issue #120). It rises only when a *sentence* of a newer turn is
	// actually enqueued, never merely because a new round opened: a round
	// that says nothing (tool calls only) supersedes nothing, because there
	// is no newer speech for the stale sentences to be holding back. Raised
	// on the enqueueing goroutine before the sentence is handed to run(), the
	// same before-the-channel-send discipline accepted uses (#80), so by the
	// time run() can dequeue anything the floor that governs it is already
	// visible.
	floor int
}

func newStreamingSpeaker(e *Engine, s *sess) *streamingSpeaker {
	sp := &streamingSpeaker{e: e, s: s, in: make(chan utterance, 64), res: make(chan error, 1), turn: 1}
	// Register with the session so CancelSpeech can find the turn's voice.
	// Registration replaces any previous speaker: a session has at most one
	// live speaker at a time (one per think() call, or the prompt and ack
	// speakers of an intent turn in sequence), so the newest is the only one
	// that can still have audio.
	e.mu.Lock()
	s.speaker = sp
	e.mu.Unlock()
	go sp.run()
	return sp
}

// speaking reports whether this speaker still has speech in flight — an
// utterance has been accepted and playback has not fully drained — and whether
// the answer announced itself (claimed Speaking / published tts.started).
//
// "Accepted" rather than "audible" is deliberate: an utterance still inside
// the synthesizer is about to be heard, and a stop that waited for the first
// sample would let it through. The one asymmetry a caller must know about:
// after drained, both are false forever.
func (sp *streamingSpeaker) speaking() (live, announced bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.accepted && !sp.drained, sp.announced
}

// noteAnnounced records that the answer has claimed Speaking and published
// tts.started.
func (sp *streamingSpeaker) noteAnnounced() {
	sp.mu.Lock()
	sp.announced = true
	sp.mu.Unlock()
}

// wasAnnounced reports whether the answer has announced itself.
func (sp *streamingSpeaker) wasAnnounced() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.announced
}

// deliver marks the speaker fully drained and hands the result to close().
// The order matters: once res carries the result nothing is playing, so a
// CancelSpeech racing this must already see live == false and no-op.
func (sp *streamingSpeaker) deliver(err error) {
	sp.mu.Lock()
	sp.drained = true
	sp.mu.Unlock()
	sp.res <- err
}

// speak queues one sentence of the answer. Sentences are normalized for speech
// and empty ones are dropped.
func (sp *streamingSpeaker) speak(sentence string) {
	sp.enqueue(utterance{text: sentence})
}

// nextTurn opens a new speech turn: every sentence committed from here on
// belongs to it, and the first one actually enqueued supersedes whatever
// earlier turns still have queued unplayed (issue #120). The engine calls it
// once per provider round after the first — a tool round is the model going
// back to work, and what it says on returning is the newer message the user
// asked speech to keep up with.
//
// Opening a turn is deliberately not what supersedes: the floor only rises
// when the new turn commits a sentence (see enqueue). Only the enqueueing
// goroutine calls this, in strict alternation with the round's speak calls,
// so turn needs no ordering subtlety beyond the shared lock.
func (sp *streamingSpeaker) nextTurn() {
	sp.mu.Lock()
	sp.turn++
	sp.mu.Unlock()
}

// superseded reports whether u is stale at dequeue: committed for a turn the
// answer has since moved past, and not exempt. This is the one place a queued
// utterance can be discarded, and it is deliberately at dequeue rather than
// enqueue — the queue is a channel, and only run() ever takes from it, so
// dequeue is the single point where every stale utterance is guaranteed to
// pass exactly once.
//
// The utterance currently being synthesized or played is never touched: a
// sentence that has begun is finished, not cut. Cutting mid-word means either
// tearing down the turn's one playback stream (the #52/#53 doctrine exists to
// prevent a second stream) or injecting silence mid-buffer, and buys at most
// one sentence of latency — while a word chopped in half is audibly broken in
// every exchange it happens in. Supersession pays for its wins at the queue,
// never at the device (issue #120).
//
// The floor's writ runs within one speaker — one turn's voice — and no
// further. Between turns, supersession is session interruption: a new
// utterance cancels the session still speaking, instantly, because there the
// user has acted and the cut is the response they asked for. A speech replay
// (issue #122) composes at that level — its own session, superseded by live
// speech or a newer replay through cancellation — while this queue-level
// mechanism governs the sentences inside it exactly as inside any turn.
func (sp *streamingSpeaker) superseded(u utterance) bool {
	if u.keep {
		return false
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return u.turn < sp.floor
}

// supersedingTurn is the newest turn with committed speech — the turn on
// whose behalf stale utterances are being dropped, reported in the
// tts.superseded event.
func (sp *streamingSpeaker) supersedingTurn() int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.floor
}

// interject queues something that is not part of the answer — a confirmation
// question, a "still working" reassurance — behind everything already waiting
// to be said, and returns once it has been handed to the player (or at once if
// the session ended). Blocking is the point for a question: the model's turn
// pauses until it has actually been asked, and only then does the user's
// confirmation clock start.
//
// ctx bounds this one utterance (see utterance.ctx): cancelling it stops the
// aside — skipped entirely if it has not started, cut short if it is mid
// synthesis — and releases this wait, so a confirmation answered while its
// question is still being said resumes the turn immediately (issue #119).
//
// "Handed to the player" is the strongest ordering this design can offer and
// the one that matters: it cannot begin until every earlier sentence has been
// synthesized and pushed into the same stream, so the user never hears two
// things at once. It is deliberately not "finished playing" — the stream is
// shared and stays open for the rest of the answer, so there is no
// per-utterance moment at which the device has fallen silent, and inventing
// one would mean tearing the stream down and starting a second (which is the
// overlap this exists to prevent).
//
// keep exempts the aside from cross-turn supersession (issue #120) — see
// utterance.keep for the policy of who keeps and why. A dropped aside still
// releases this wait exactly as a cancelled one does: run() sees it once and
// closes done.
func (sp *streamingSpeaker) interject(ctx context.Context, text string, keep bool) {
	done := make(chan struct{})
	if !sp.enqueue(utterance{text: text, aside: true, keep: keep, ctx: ctx, done: done}) {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
		// Cancelled while still queued (or mid synthesis): the caller's turn
		// resumes now rather than waiting for the queue to reach an utterance
		// that will only be skipped. run() still sees it and closes done —
		// nothing waits on it twice.
	}
}

// enqueue puts one utterance on the queue, reporting whether it got there.
func (sp *streamingSpeaker) enqueue(u utterance) bool {
	if sp.e.spokenForm(u.text) == "" {
		// Nothing would be said for this text (run() skips it on the same
		// test), so it must not claim the Speaking state below either: a turn
		// whose every sentence normalizes to silence announces nothing.
		return false
	}
	// accepted is recorded before the utterance is handed over, never after.
	// The moment run() can see it, everything downstream — synthesis,
	// publishing tts.started, playback — can happen before this goroutine is
	// scheduled again, and a stop arriving on that event must find speaking()
	// live (issue #80: a CI runner parked exactly here and CancelSpeech read
	// accepted == false while held audio sat in the synthesizer). If the
	// handoff then loses to session teardown, the stale mark is unobservable:
	// every path that cancels the session context also clears Engine.current
	// under the lock, so CancelSpeech bails before it would ask this speaker.
	//
	// The turn stamp and the supersession floor ride the same critical
	// section and the same before-the-send ordering (issue #120): an answer
	// sentence raises the floor to its own turn the moment it is committed,
	// so by the time run() can dequeue anything at all, every older queued
	// sentence is already condemned — there is no window in which run() races
	// past a stale sentence the commit meant to drop. If the handoff below
	// then fails, the raised floor is as unobservable as the stale accepted
	// mark: enqueue fails only when the session has ended, and a dead
	// session's speaker drains everything regardless.
	sp.mu.Lock()
	sp.accepted = true
	u.turn = sp.turn
	if !u.aside && sp.floor < u.turn {
		sp.floor = u.turn
	}
	needsAnnounce := !u.aside && !sp.announced
	sp.mu.Unlock()
	if needsAnnounce && !sp.announce() {
		// The session ended underneath the turn (superseded, cancelled, or a
		// refused transition that failLocked has already reported): there is
		// no one to speak to and nothing further to queue.
		return false
	}
	select {
	case sp.in <- u:
		if queued := sp.e.speakerQueued; queued != nil {
			queued()
		}
		return true
	case <-sp.s.ctx.Done():
		return false
	}
}

// announce moves the session to Speaking for the answer's first sentence and
// publishes tts.started, reporting whether the claim landed (false only when
// the session has ended).
//
// It runs on the *enqueueing* goroutine — synchronously inside speak() — and
// that placement is the whole fix for issue #111, the third state wedge of the
// #52/#63 family. The claim used to live in run()'s synth, on the speaker
// goroutine, which made "a committed sentence has claimed Speaking" a race
// rather than an invariant: the tool loop reads the state the instant
// streamOnce returns (backToThinking), and when a round's only sentence
// reached the speaker at end-of-stream — a short narration followed by tool
// calls, the exact shape the #109 config tools invite — backToThinking would
// force Responding → Thinking before the speaker got scheduled, the speaker
// would then request Thinking → Speaking, and the table refused it (rightly:
// speech legally goes Thinking → Responding → Speaking) — killing a live
// session mid-turn.
//
// Claiming here means the claim is ordered before anything the turn does
// next: by the time streamOnce returns, every sentence it committed to the
// voice has already moved the session to Speaking, so backToThinking's
// "Speaking stays put while audio drains" reading (#52) holds by
// construction, and a still-Responding state really does mean nothing was
// committed to the voice this round. No new table edge, exactly as #52/#53
// and #63 resolved: route the speech through the legal path rather than
// widen the table for a caller.
//
// tts.started moves with the claim rather than staying behind on the run
// loop, and deliberately so: the two have been published together since the
// event existed, and everything that reasons about the pair — the state
// waits in this package's tests, CancelSpeech's bookend, any client treating
// the event and the state as one moment — assumes a session observed in
// Speaking has announced itself. Splitting them would open a window in which
// that assumption fails. Nothing is owed to "synthesis has begun" by this
// event: real synthesizers hand their stream channel back immediately, so
// tts.started has always fired before any audio existed — committing the
// sentence to the one playback queue is the moment it reports.
//
// The check-then-act on sp.announced in enqueue is single-goroutine by
// construction: every non-aside utterance of a speaker is enqueued from the
// one goroutine that owns its turn (think() for an answer, runIntent for an
// acknowledgement), which is the same reason spokenTurn needs no lock.
func (sp *streamingSpeaker) announce() bool {
	if !sp.e.advance(sp.s, StateSpeaking) {
		return false
	}
	sp.noteAnnounced()
	sp.e.publish(Event{Type: "tts.started", Data: map[string]any{"session_id": sp.s.id}})
	return true
}

// close signals no more sentences and waits for playback to drain, returning
// any synthesis/playback error (nil on success or clean cancellation).
func (sp *streamingSpeaker) close() error {
	close(sp.in)
	return <-sp.res
}

func (sp *streamingSpeaker) run() {
	var (
		pcm      chan []byte
		playDone chan error
		format   tts.Format
		synthErr error
	)

	// synth renders one utterance and forwards its PCM into the shared stream,
	// starting playback lazily on the first audio. The answer's Speaking claim
	// and tts.started do not happen here: they happened on the enqueueing
	// goroutine, before the utterance was ever visible to this loop — see
	// announce for why that ordering is load-bearing (issue #111).
	synth := func(u utterance) error {
		spoken := sp.e.spokenForm(u.text)
		if spoken == "" {
			return nil
		}
		// An aside synthesizes under its own context so a resolved
		// confirmation can cut its question short (issue #119); everything
		// else runs under the session's, exactly as before. u.ctx is always a
		// child of the session context, so this can only ever stop *sooner*.
		uctx := sp.s.ctx
		if u.ctx != nil {
			uctx = u.ctx
		}
		f, chunks, err := sp.e.tts.Speak(uctx, tts.Request{Text: spoken})
		if err != nil {
			return err
		}
		if pcm == nil {
			format = f
			pcm = make(chan []byte, 8)
			playDone = make(chan error, 1)
			// Only the player can say when audio actually left for the device;
			// nothing on this side of the channel can observe it (audio.Trace).
			playCtx := audio.WithTrace(sp.s.ctx, &audio.Trace{FirstAudio: sp.s.timings.markAudioOut})
			go func(rate, ch int, in <-chan []byte) {
				playDone <- sp.e.player.Play(playCtx, rate, ch, in)
			}(format.SampleRate, format.Channels, pcm)
		}
		for c := range chunks {
			if c.Err != nil {
				return c.Err
			}
			// The first synthesized sample of the answer — marked on the sample,
			// never on the Speak call. A warm engine hands back its channel
			// immediately while a cold one hands it back once the model has
			// loaded, so marking the call would time process start-up instead
			// of synthesis and report a warm worker as infinitely fast.
			//
			// An aside is not the answer and must not set the mark: a turn
			// that paused to ask the user something, or to say it was still
			// working, would otherwise report that audio as the answer's
			// latency and flatter the number (ADR 0018).
			if !u.aside {
				sp.s.timings.markFirstPCM()
			}
			select {
			case pcm <- c.PCM:
			case <-uctx.Done():
				return uctx.Err()
			}
		}
		return nil
	}

	// dropped counts stale utterances discarded since the last one that
	// played, so one supersession is one event however many sentences it
	// swallowed. Local to this loop on purpose: drops only ever happen here,
	// and the durable copy lives in the session's timings (noted per drop),
	// where it survives even a turn that dies before the event below could be
	// published — the cancel path owns that turn's events, but the record
	// still says what was dropped.
	dropped := 0
	for u := range sp.in {
		if synthErr != nil || sp.s.ctx.Err() != nil {
			// Drain input without speaking once we've stopped — but still
			// release anyone waiting, or a caller blocked on a question that
			// will never be said would wait out the session context.
			sp.release(u)
			continue
		}
		if u.ctx != nil && u.ctx.Err() != nil {
			// The aside was cancelled before its turn came — a confirmation
			// answered while its question was still queued behind the answer.
			// Skipped, not an error: the stream and the sentences after it
			// are untouched (issue #119).
			sp.release(u)
			continue
		}
		if sp.superseded(u) {
			// Stale: a newer turn has committed speech, and this utterance
			// belongs to a turn the conversation moved past while it sat in
			// the queue (issue #120). Dropped unplayed — audio only: the
			// transcript, the events, and the record all carried this text
			// when it streamed, so nothing is lost except the lag. The
			// utterance in flight when the floor rose is not this one and is
			// never cut — see superseded for why a begun sentence always
			// finishes.
			dropped++
			sp.s.timings.noteSupersededDrop()
			sp.release(u)
			continue
		}
		if dropped > 0 {
			// The queue has caught up to speech that still counts: account
			// for the skip before saying anything newer. One event per
			// supersession, carrying the turn that won and how many sentences
			// it cost — the shape replay/precedence work builds on, and the
			// activity feed's evidence that the silence was a decision, not a
			// glitch. Published here, at the first surviving utterance,
			// because only now is the count complete: a channel cannot be
			// counted at the moment the floor rises. A batch with no survivor
			// cannot happen on a live turn — the sentence that raised the
			// floor sits behind the stale ones it condemned and is itself
			// immune — and a turn that died first forfeits the event to the
			// cancel path like every other event it owns.
			turn := sp.supersedingTurn()
			sp.e.publish(Event{Type: "tts.superseded", Data: map[string]any{
				"session_id": sp.s.id, "turn": turn, "dropped": dropped}})
			sp.e.log.Info("stale queued speech superseded", "component", "tts",
				"session_id", sp.s.id, "turn", turn, "dropped", dropped)
			dropped = 0
		}
		if err := synth(u); err != nil {
			// A cancelled aside unwinds with its own context's error, and
			// that is a stopped question, not a broken voice: recording it as
			// synthErr would silence the rest of the answer — the exact
			// opposite of "speech resumes immediately". A session-level
			// cancellation still lands in u.ctx (it is a child), and the
			// drain branch above plus the s.ctx check at delivery handle it
			// exactly as they always have.
			if u.ctx == nil || u.ctx.Err() == nil {
				synthErr = err
			}
		}
		sp.release(u)
	}

	if pcm == nil {
		// Nothing was ever spoken.
		if sp.s.ctx.Err() != nil {
			sp.deliver(sp.s.ctx.Err())
			return
		}
		sp.deliver(synthErr)
		return
	}
	close(pcm)
	playErr := <-playDone
	if sp.s.ctx.Err() != nil {
		sp.deliver(sp.s.ctx.Err())
		return
	}
	if synthErr == nil {
		synthErr = playErr
	}
	// tts.finished is the answer's bookend, so it is published only when
	// tts.started was: a turn whose only audio was an aside emits neither,
	// exactly as the direct prompt path always did.
	if synthErr == nil && sp.wasAnnounced() {
		sp.e.publish(Event{Type: "tts.finished", Data: map[string]any{"session_id": sp.s.id}})
	}
	sp.deliver(synthErr)
}

// release wakes whoever is waiting on this utterance, whether it was spoken or
// abandoned. Closing is safe exactly once: only run() ever closes done, and it
// sees each utterance once.
func (sp *streamingSpeaker) release(u utterance) {
	if u.done != nil {
		close(u.done)
	}
}
