package stt

import "context"

// Fake is a scripted Transcriber for tests.
type Fake struct {
	// Text is the final transcript to return.
	Text string
	// Reason scripts the explanation a real adapter attaches to a
	// deliberately empty transcript (issue #191) — a capture with no voiced
	// audio, or a transcript that was only the bias prompt. Set it with an
	// empty Text; it is ignored otherwise, exactly as the engine ignores it.
	Reason string
	// Partials are emitted before the final transcript.
	Partials []string
	// Fail, when set, makes the stream end with an error event.
	Fail error
	// LastInput records the most recent input for assertions.
	LastInput AudioInput
}

// Name implements Transcriber.
func (f *Fake) Name() string { return "fake" }

// Transcribe implements Transcriber.
func (f *Fake) Transcribe(ctx context.Context, input AudioInput) (<-chan TranscriptEvent, error) {
	f.LastInput = input
	ch := make(chan TranscriptEvent)
	go func() {
		defer close(ch)
		if f.Fail != nil {
			ch <- TranscriptEvent{Type: EventError, Err: f.Fail}
			return
		}
		for _, p := range f.Partials {
			select {
			case ch <- TranscriptEvent{Type: EventPartial, Text: p}:
			case <-ctx.Done():
				ch <- TranscriptEvent{Type: EventError, Err: ctx.Err()}
				return
			}
		}
		select {
		case ch <- TranscriptEvent{Type: EventFinal, Text: f.Text, Reason: f.Reason}:
		case <-ctx.Done():
			ch <- TranscriptEvent{Type: EventError, Err: ctx.Err()}
		}
	}()
	return ch, nil
}
