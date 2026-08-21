package session

import (
	"strings"
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

// streamingSpeaker synthesizes and plays sentences as they arrive, over a
// single continuous playback stream, so Jarvix begins speaking while the rest
// of the answer is still being generated. One speaker serves a whole think()
// call (all tool rounds), keeping audio gapless and ordered.
type streamingSpeaker struct {
	e   *Engine
	s   *sess
	in  chan string
	res chan error
}

func newStreamingSpeaker(e *Engine, s *sess) *streamingSpeaker {
	sp := &streamingSpeaker{e: e, s: s, in: make(chan string, 64), res: make(chan error, 1)}
	go sp.run()
	return sp
}

// speak queues one sentence. Sentences are normalized for speech and empty
// ones are dropped.
func (sp *streamingSpeaker) speak(sentence string) {
	if strings.TrimSpace(sentence) == "" {
		return
	}
	select {
	case sp.in <- sentence:
	case <-sp.s.ctx.Done():
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
	)

	// synth renders one sentence and forwards its PCM into the shared stream,
	// starting playback (and the Speaking state) lazily on the first audio.
	synth := func(sentence string) error {
		spoken := speechText(sentence)
		if spoken == "" {
			return nil
		}
		f, chunks, err := sp.e.tts.Speak(sp.s.ctx, tts.Request{Text: spoken})
		if err != nil {
			return err
		}
		if pcm == nil {
			if !sp.e.advance(sp.s, StateSpeaking) {
				for range chunks { // superseded/cancelled: let the synth exit
				}
				return sp.s.ctx.Err()
			}
			format = f
			pcm = make(chan []byte, 8)
			playDone = make(chan error, 1)
			sp.e.publish(Event{Type: "tts.started", Data: map[string]any{"session_id": sp.s.id}})
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
			sp.s.timings.markFirstPCM()
			select {
			case pcm <- c.PCM:
			case <-sp.s.ctx.Done():
				return sp.s.ctx.Err()
			}
		}
		return nil
	}

	for sentence := range sp.in {
		if synthErr != nil || sp.s.ctx.Err() != nil {
			continue // drain input without speaking once we've stopped
		}
		if err := synth(sentence); err != nil {
			synthErr = err
		}
	}

	if pcm == nil {
		// Nothing was ever spoken.
		if sp.s.ctx.Err() != nil {
			sp.res <- sp.s.ctx.Err()
			return
		}
		sp.res <- synthErr
		return
	}
	close(pcm)
	playErr := <-playDone
	if sp.s.ctx.Err() != nil {
		sp.res <- sp.s.ctx.Err()
		return
	}
	if synthErr == nil {
		synthErr = playErr
	}
	if synthErr == nil {
		sp.e.publish(Event{Type: "tts.finished", Data: map[string]any{"session_id": sp.s.id}})
	}
	sp.res <- synthErr
}
