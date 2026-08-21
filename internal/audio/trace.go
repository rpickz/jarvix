package audio

import "context"

// Trace carries optional callbacks a Player invokes as playback progresses.
//
// The latency budget needs a mark that only the player can produce: the moment
// the first PCM of an answer actually left Jarvix for the audio device. That is
// not something the session engine can time from the outside — handing a chunk
// to a buffered channel says nothing about when the device got it — and adding
// it to the Player interface would break every fake and every other
// implementation for a measurement most of them do not make.
//
// So it travels in the context, the way net/http/httptrace carries its hooks:
// additive, ignorable, and scoped to exactly one playback.
type Trace struct {
	// FirstAudio is called once, when the first chunk of a playback stream has
	// been written to the audio device. It runs on the player's goroutine and
	// must not block.
	FirstAudio func()
}

// traceKey is the private context key for a Trace.
type traceKey struct{}

// WithTrace returns a context carrying t, for a Player to find.
func WithTrace(ctx context.Context, t *Trace) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFromContext returns the Trace in ctx, or nil.
func TraceFromContext(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// firstAudio invokes the FirstAudio hook if one is present. Players call it
// exactly once per stream; a nil trace or a nil hook is the common case and
// costs an interface comparison.
func firstAudio(ctx context.Context) {
	if t := TraceFromContext(ctx); t != nil && t.FirstAudio != nil {
		t.FirstAudio()
	}
}
