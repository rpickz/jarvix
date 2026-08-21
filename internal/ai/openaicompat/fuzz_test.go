package openaicompat

import (
	"context"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

// FuzzReadStream throws arbitrary bytes at the SSE parser — the surface that
// eats whatever a remote (possibly broken) gateway sends. Invariants: no
// panic, no hang, and every emitted delta is well-formed.
func FuzzReadStream(f *testing.F) {
	f.Add("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n")
	f.Add("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"run\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n")
	f.Add("data: {\"error\":{\"message\":\"quota exceeded\"}}\n")
	f.Add("data: {broken json\n")
	f.Add(": comment\nevent: ping\n\ndata:[DONE]")
	f.Add("data:    {\"choices\":[]}   \ndata: [DONE]")
	f.Add("")
	f.Add("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":7,\"function\":{\"arguments\":\"frag\"}}]}}]}\n")
	f.Fuzz(func(t *testing.T, stream string) {
		c := New("fuzz", "http://unused.invalid", "")
		ch := make(chan ai.Event)
		drained := make(chan []ai.Event, 1)
		go func() {
			var events []ai.Event
			for ev := range ch {
				events = append(events, ev)
			}
			drained <- events
		}()
		calls, err := c.readStream(context.Background(), strings.NewReader(stream), ch)
		close(ch)
		events := <-drained

		if err != nil && calls != nil {
			t.Fatalf("returned both calls and error: %v / %+v", err, calls)
		}
		for _, call := range calls {
			_ = call // assembled calls may be partial; they must simply exist
		}
		for _, ev := range events {
			if ev.Type != ai.EventDelta {
				t.Fatalf("readStream forwarded a non-delta event: %+v", ev)
			}
			if ev.Content == "" {
				t.Fatalf("readStream forwarded an empty delta")
			}
		}
	})
}
