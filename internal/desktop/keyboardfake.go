package desktop

import (
	"context"
	"sync"
)

// FakeKeyboard is a scripted keystroke injector: it records what it was asked
// to type and never touches a keyboard, real or virtual.
//
// It exists because of the house rule this capability is built under: no test
// in this tree may synthesise a keystroke into the session running it. The
// person running `go test` is working in that session, and a test that typed
// into it would type into whatever they had open.
//
// The one piece of machinery beyond recording is BeforeType, which runs at the
// moment of injection — after the caller has resolved and re-verified its
// target. A test uses it to prove the injector was reached, or, paired with
// FakeCompositor.SetWindows, that the caller looked once more before it did.
type FakeKeyboard struct {
	// Err, when set, fails every call — the "wtype is not installed, or the
	// compositor refuses a virtual keyboard" case.
	Err error
	// Name is what Describe reports. Empty means a plausible default.
	Name string

	// BeforeType runs at the start of Type and Press, before anything is
	// recorded, with the text (or key name) that was asked for.
	BeforeType func(text string)

	mu sync.Mutex
	// typed holds every text passed to Type, in order.
	typed []string
	// pressed holds every key name passed to Press, in order.
	pressed []string
}

// Describe implements Keyboard.
func (f *FakeKeyboard) Describe(context.Context) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	if f.Name != "" {
		return f.Name, nil
	}
	return "FakeKeyboard (no keystrokes leave this process)", nil
}

// Type implements Keyboard.
func (f *FakeKeyboard) Type(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.BeforeType != nil {
		f.BeforeType(text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.typed = append(f.typed, text)
	return nil
}

// Press implements Keyboard. The fake resolves the keysym exactly as the real
// injector does, so a test that passes an unknown key name gets the refusal
// the user would.
func (f *FakeKeyboard) Press(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.BeforeType != nil {
		f.BeforeType(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	sym, ok := Keysym(key)
	if !ok {
		return ErrNoKeyboard
	}
	f.pressed = append(f.pressed, sym)
	return nil
}

// Typed returns every text that was typed, in order. The assertion behind
// "nothing was typed" is len(Typed()) == 0.
func (f *FakeKeyboard) Typed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed...)
}

// Pressed returns every keysym that was pressed, in order.
func (f *FakeKeyboard) Pressed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pressed...)
}
