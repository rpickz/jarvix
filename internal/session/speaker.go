package session

import (
	"strings"

	"github.com/rpickz/jarvix/internal/tts"
)

// sentencer splits a streaming token feed into complete sentences so speech
// can begin before the whole answer is generated.
type sentencer struct {
	buf strings.Builder
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
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '.' && c != '!' && c != '?' && c != '\n' && c != ':' {
			continue
		}
		// A boundary only counts when whitespace (or end of a final buffer)
		// follows — this avoids splitting decimals like "3.5" and mid-word
		// colons like "http://".
		if i+1 < len(s) {
			if n := s[i+1]; n != ' ' && n != '\n' && n != '\t' {
				continue
			}
		} else if !final {
			break // wait for the next token to confirm the boundary
		}
		if sentence := strings.TrimSpace(s[start : i+1]); sentence != "" {
			out = append(out, sentence)
		}
		start = i + 1
	}
	rem := s[start:]
	if !final && len(rem) > maxSentenceRunon {
		if r := strings.TrimSpace(rem); r != "" {
			out = append(out, r)
		}
		rem = ""
	}
	if final {
		if r := strings.TrimSpace(rem); r != "" {
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
			go func(rate, ch int, in <-chan []byte) {
				playDone <- sp.e.player.Play(sp.s.ctx, rate, ch, in)
			}(format.SampleRate, format.Channels, pcm)
		}
		for c := range chunks {
			if c.Err != nil {
				return c.Err
			}
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
