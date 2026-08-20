package hotkey

import (
	"encoding/binary"
	"testing"
)

func TestResolveChord(t *testing.T) {
	codes, err := ResolveChord([]string{"LeftMeta", "leftalt", "V"})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{125, 56, 47}
	for i, c := range want {
		if codes[i] != c {
			t.Errorf("codes = %v, want %v", codes, want)
		}
	}
	if _, err := ResolveChord([]string{"hyperkey"}); err == nil {
		t.Error("unknown key must error")
	}
	if _, err := ResolveChord(nil); err == nil {
		t.Error("empty chord must error")
	}
	// Duplicates (e.g. "super" and "leftmeta") collapse.
	codes, _ = ResolveChord([]string{"super", "leftmeta"})
	if len(codes) != 1 {
		t.Errorf("duplicate aliases not collapsed: %v", codes)
	}
}

func track(t *testing.T, codes []uint16) (*ChordTracker, *int, *int) {
	t.Helper()
	presses, releases := 0, 0
	tr := NewChordTracker(codes)
	tr.OnPress = func() { presses++ }
	tr.OnRelease = func() { releases++ }
	return tr, &presses, &releases
}

func TestChordFiresWhenAllKeysDown(t *testing.T) {
	tr, presses, releases := track(t, []uint16{125, 56, 47})
	tr.Handle(keyEvent{code: 125, value: valPress})
	tr.Handle(keyEvent{code: 56, value: valPress})
	if *presses != 0 {
		t.Fatal("fired before chord complete")
	}
	tr.Handle(keyEvent{code: 47, value: valPress})
	if *presses != 1 {
		t.Fatal("did not fire on chord completion")
	}
	// Releasing any chord key ends the hold — whichever finger lifts first.
	tr.Handle(keyEvent{code: 56, value: valRelease})
	if *releases != 1 {
		t.Fatal("did not fire on first release")
	}
	// The remaining releases do not fire again.
	tr.Handle(keyEvent{code: 125, value: valRelease})
	tr.Handle(keyEvent{code: 47, value: valRelease})
	if *releases != 1 {
		t.Errorf("release fired %d times", *releases)
	}
}

func TestChordRepeatEventsIgnored(t *testing.T) {
	tr, presses, releases := track(t, []uint16{68}) // f10
	tr.Handle(keyEvent{code: 68, value: valPress})
	for i := 0; i < 10; i++ {
		tr.Handle(keyEvent{code: 68, value: valRepeat})
	}
	if *presses != 1 || *releases != 0 {
		t.Errorf("presses=%d releases=%d after repeats", *presses, *releases)
	}
	tr.Handle(keyEvent{code: 68, value: valRelease})
	if *releases != 1 {
		t.Errorf("releases = %d", *releases)
	}
}

func TestChordNonChordKeysDiscarded(t *testing.T) {
	tr, presses, _ := track(t, []uint16{125, 47})
	// A full typing session on other keys must not affect chord state.
	for code := uint16(16); code < 40; code++ {
		tr.Handle(keyEvent{code: code, value: valPress})
		tr.Handle(keyEvent{code: code, value: valRelease})
	}
	tr.Handle(keyEvent{code: 125, value: valPress})
	tr.Handle(keyEvent{code: 47, value: valPress})
	if *presses != 1 {
		t.Errorf("presses = %d", *presses)
	}
}

func TestChordFullCycleRearms(t *testing.T) {
	tr, presses, releases := track(t, []uint16{125, 47})
	for cycle := 0; cycle < 3; cycle++ {
		tr.Handle(keyEvent{code: 125, value: valPress})
		tr.Handle(keyEvent{code: 47, value: valPress})
		tr.Handle(keyEvent{code: 47, value: valRelease})
		tr.Handle(keyEvent{code: 125, value: valRelease})
	}
	if *presses != 3 || *releases != 3 {
		t.Errorf("presses=%d releases=%d, want 3/3", *presses, *releases)
	}
}

func TestChordPartialReleaseAndRepress(t *testing.T) {
	// Hold mods, tap the letter twice: two hold cycles.
	tr, presses, releases := track(t, []uint16{125, 56, 47})
	tr.Handle(keyEvent{code: 125, value: valPress})
	tr.Handle(keyEvent{code: 56, value: valPress})
	tr.Handle(keyEvent{code: 47, value: valPress})
	tr.Handle(keyEvent{code: 47, value: valRelease})
	tr.Handle(keyEvent{code: 47, value: valPress})
	tr.Handle(keyEvent{code: 47, value: valRelease})
	if *presses != 2 || *releases != 2 {
		t.Errorf("presses=%d releases=%d, want 2/2", *presses, *releases)
	}
}

func TestDecodeEvents(t *testing.T) {
	buf := make([]byte, eventSize*3)
	put := func(i int, typ, code uint16, value uint32) {
		off := i * eventSize
		binary.LittleEndian.PutUint16(buf[off+16:], typ)
		binary.LittleEndian.PutUint16(buf[off+18:], code)
		binary.LittleEndian.PutUint32(buf[off+20:], value)
	}
	put(0, evKey, 47, valPress)
	put(1, 0, 0, 0) // EV_SYN — must be skipped
	put(2, evKey, 47, valRelease)

	events := decodeEvents(buf)
	if len(events) != 2 {
		t.Fatalf("decoded %d events, want 2", len(events))
	}
	if events[0].code != 47 || events[0].value != valPress {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].value != valRelease {
		t.Errorf("event 1 = %+v", events[1])
	}
	// Truncated trailing bytes are ignored, not misparsed.
	if got := decodeEvents(buf[:eventSize+5]); len(got) != 1 {
		t.Errorf("truncated decode = %d events", len(got))
	}
}
