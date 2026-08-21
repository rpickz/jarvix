package desktop

import (
	"context"
	"sync"
)

// FakeNotifier records notifications and returns a scripted "click", letting
// tests assert on notification content and click-through behaviour without a
// notification daemon. Safe for concurrent use: the daemon dispatches each
// notification from its own goroutine.
type FakeNotifier struct {
	// InvokeAction is returned from every Send as the action the user chose
	// ("" simulates dismissal or expiry).
	InvokeAction string
	// Err, when set, is returned from Send — simulating a missing
	// notification daemon so tests can cover the log-only degradation.
	Err error

	mu   sync.Mutex
	sent []Notification
}

// Send implements Notifier.
func (f *FakeNotifier) Send(_ context.Context, n Notification) (string, error) {
	f.mu.Lock()
	f.sent = append(f.sent, n)
	f.mu.Unlock()
	if f.Err != nil {
		return "", f.Err
	}
	return f.InvokeAction, nil
}

// Sent returns a copy of every notification delivered so far.
func (f *FakeNotifier) Sent() []Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Notification(nil), f.sent...)
}
