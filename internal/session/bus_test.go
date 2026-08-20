package session

import (
	"testing"
	"time"
)

func TestBusFanOut(t *testing.T) {
	bus := NewBus(nil)
	a, cancelA := bus.Subscribe()
	b, cancelB := bus.Subscribe()
	defer cancelA()
	defer cancelB()

	bus.Publish(Event{Type: "state.changed"})
	for _, ch := range []<-chan Event{a, b} {
		select {
		case ev := <-ch:
			if ev.Type != "state.changed" {
				t.Errorf("got %q", ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestBusUnsubscribeClosesChannel(t *testing.T) {
	bus := NewBus(nil)
	ch, cancel := bus.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after unsubscribe")
	}
	cancel() // double-cancel is safe
	bus.Publish(Event{Type: "x"})
}

func TestBusSlowSubscriberDoesNotBlock(t *testing.T) {
	bus := NewBus(nil)
	_, cancel := bus.Subscribe() // never drained
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			bus.Publish(Event{Type: "flood"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow subscriber")
	}
}
