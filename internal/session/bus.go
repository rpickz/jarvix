package session

import (
	"log/slog"
	"sync"
)

// Event is one daemon event, broadcast to every subscriber (IPC clients, the
// overlay, tests). Type uses dotted names matching the IPC protocol, e.g.
// "state.changed", "assistant.delta".
type Event struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

// Bus fans events out to subscribers. Publishing never blocks: a subscriber
// that stops draining loses events (and a warning is logged) rather than
// wedging the session engine.
type Bus struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
	log  *slog.Logger
}

// NewBus creates an event bus. logger may be nil.
func NewBus(logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bus{subs: make(map[int]chan Event), log: logger}
}

// Subscribe registers a subscriber. The returned cancel function must be
// called to release it.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, 256)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub)
		}
	}
}

// Publish broadcasts an event to all subscribers.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			b.log.Warn("dropping event for slow subscriber",
				"component", "bus", "subscriber", id, "event", ev.Type)
		}
	}
}
