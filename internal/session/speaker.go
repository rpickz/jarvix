package session

import (
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
	// accepted is set once the first utterance is queued: from that moment
	// there is audio in flight or committed to be, even while synthesis is
	// still working — killing playback must not wait on synthesis.
	accepted bool
	// announced is set once the answer has claimed Speaking and published
	// tts.started. Tracked here (not only in run's locals) so a cancel knows
	// whether a tts.finished bookend is owed.
	announced bool
	// drained is set once run() has finished: nothing is playing and nothing
	// ever will be again on this speaker.
	drained bool
}

func newStreamingSpeaker(e *Engine, s *sess) *streamingSpeaker {
	sp := &streamingSpeaker{e: e, s: s, in: make(chan utterance, 64), res: make(chan error, 1)}
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

// noteAnnounced records that the answer has claimed the Speaking state.
func (sp *streamingSpeaker) noteAnnounced() {
	sp.mu.Lock()
	sp.announced = true
	sp.mu.Unlock()
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

// interject queues something that is not part of the answer — a confirmation
// question, a "still working" reassurance — behind everything already waiting
// to be said, and returns once it has been handed to the player (or at once if
// the session ended). Blocking is the point for a question: the model's turn
// pauses until it has actually been asked, and only then does the user's
// confirmation clock start.
//
// "Handed to the player" is the strongest ordering this design can offer and
// the one that matters: it cannot begin until every earlier sentence has been
// synthesized and pushed into the same stream, so the user never hears two
// things at once. It is deliberately not "finished playing" — the stream is
// shared and stays open for the rest of the answer, so there is no
// per-utterance moment at which the device has fallen silent, and inventing
// one would mean tearing the stream down and starting a second (which is the
// overlap this exists to prevent).
func (sp *streamingSpeaker) interject(text string) {
	done := make(chan struct{})
	if !sp.enqueue(utterance{text: text, aside: true, done: done}) {
		return
	}
	select {
	case <-done:
	case <-sp.s.ctx.Done():
	}
}

// enqueue puts one utterance on the queue, reporting whether it got there.
func (sp *streamingSpeaker) enqueue(u utterance) bool {
	if strings.TrimSpace(u.text) == "" {
		return false
	}
	select {
	case sp.in <- u:
		sp.mu.Lock()
		sp.accepted = true
		sp.mu.Unlock()
		return true
	case <-sp.s.ctx.Done():
		return false
	}
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
		// announced is set once the answer has claimed the Speaking state and
		// published tts.started. It is tracked separately from pcm because a
		// confirmation question can open the stream before the answer does
		// (the model asked for a tool before saying anything), and the answer
		// that follows must still announce itself — the overlay's tts.started
		// belongs to the answer, not to the question.
		announced bool
	)

	// synth renders one utterance and forwards its PCM into the shared stream,
	// starting playback lazily on the first audio.
	synth := func(u utterance) error {
		spoken := sp.e.spokenForm(u.text)
		if spoken == "" {
			return nil
		}
		f, chunks, err := sp.e.tts.Speak(sp.s.ctx, tts.Request{Text: spoken})
		if err != nil {
			return err
		}
		if !u.aside && !announced {
			if !sp.e.advance(sp.s, StateSpeaking) {
				for range chunks { // superseded/cancelled: let the synth exit
				}
				return sp.s.ctx.Err()
			}
			announced = true
			sp.noteAnnounced()
			sp.e.publish(Event{Type: "tts.started", Data: map[string]any{"session_id": sp.s.id}})
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
			case <-sp.s.ctx.Done():
				return sp.s.ctx.Err()
			}
		}
		return nil
	}

	for u := range sp.in {
		if synthErr != nil || sp.s.ctx.Err() != nil {
			// Drain input without speaking once we've stopped — but still
			// release anyone waiting, or a caller blocked on a question that
			// will never be said would wait out the session context.
			sp.release(u)
			continue
		}
		if err := synth(u); err != nil {
			synthErr = err
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
	if synthErr == nil && announced {
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
