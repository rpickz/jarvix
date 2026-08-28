// Package fakesbad reproduces #149: an exported field on a test fake that the
// fake writes about itself while a production goroutine is driving it. The
// guard's own test asserts every field here is reported.
//
// It lives under testdata so the go tool never builds it.
package fakesbad

import "sync"

// Fake is the tts.Fake of the day before #149 was fixed.
type Fake struct {
	// Chunks is scripting: the test writes it at construction and the fake
	// only reads it. Not a finding, and the fixture would be worthless
	// without it, because the whole precision claim is that scripting fields
	// are left alone.
	Chunks [][]byte

	// LastRequest is the defect: assigned inside Speak, which runs on the
	// speaker's goroutine, and read by the test's.
	LastRequest string

	// Speaks is the same defect wearing a counter's clothes.
	Speaks int

	mu sync.Mutex
}

func (f *Fake) Speak(req string) [][]byte {
	f.mu.Lock()
	f.LastRequest = req
	f.Speaks++
	f.mu.Unlock()
	return f.Chunks
}

// StubStore records what it was asked to do — the same shape under a different
// naming convention, which is why the rule matches Stub as well as Fake.
type StubStore struct {
	Saved []string
}

func (s *StubStore) Save(name string) {
	s.Saved = append(s.Saved, name)
}
