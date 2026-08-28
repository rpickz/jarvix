package provenance

import (
	"context"
	"sync"
	"testing"
)

// TestStrengthWordingIsPinned is the honesty criterion of #168 as a test.
//
// The two strengths are two different claims and the wording is the only
// place the difference reaches a person. "available to the answer" says the
// thing was in front of the model and nothing more; "returned during this
// turn" says something ran and its output went in. Neither may drift into the
// other's territory, and neither may become the word "source", "cited", or
// "used", all of which claim knowledge nobody has.
func TestStrengthWordingIsPinned(t *testing.T) {
	if AvailablePhrase != "available to the answer" {
		t.Errorf("the weaker claim was reworded: %q", AvailablePhrase)
	}
	if ReturnedPhrase != "returned during this turn" {
		t.Errorf("the stronger claim was reworded: %q", ReturnedPhrase)
	}
	if Phrase(Available) != AvailablePhrase {
		t.Errorf("Phrase(Available) = %q", Phrase(Available))
	}
	if Phrase(Returned) != ReturnedPhrase {
		t.Errorf("Phrase(Returned) = %q", Phrase(Returned))
	}
	// Two phrases, never one: a single word for both would be the exact
	// conflation this feature exists to prevent.
	if AvailablePhrase == ReturnedPhrase {
		t.Fatal("the two strengths must never share one wording")
	}
}

// TestUnknownStrengthTakesTheCautiousClaim: overstating what Jarvix knows is
// the one failure mode that matters here, so anything unrecognised is
// reported as the weaker claim rather than the stronger one.
func TestUnknownStrengthTakesTheCautiousClaim(t *testing.T) {
	for _, s := range []string{"", "cited", "used", "definitely"} {
		if got := Phrase(s); got != AvailablePhrase {
			t.Errorf("Phrase(%q) = %q, want the weaker claim", s, got)
		}
	}
}

func TestBoundOfNothingIsNothing(t *testing.T) {
	if rec := Bound(nil); rec != nil {
		t.Errorf("nothing collected produced a record: %+v", rec)
	}
	// A reference with no kind is not a reference.
	if rec := Bound([]Reference{{Ref: "m1"}}); rec != nil {
		t.Errorf("a kindless reference produced a record: %+v", rec)
	}
}

func TestBoundKeepsCollectionOrder(t *testing.T) {
	rec := Bound([]Reference{
		{Kind: KindFact, Strength: Available, Ref: "m1"},
		{Kind: KindFeed, Strength: Available, Ref: "prices"},
		{Kind: KindTool, Strength: Returned, Tool: "shell.run", Subject: "git status"},
	})
	if rec == nil || len(rec.Sources) != 3 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Sources[0].Ref != "m1" || rec.Sources[2].Tool != "shell.run" {
		t.Errorf("order changed: %+v", rec.Sources)
	}
	if rec.Truncated != 0 {
		t.Errorf("truncated = %d with nothing dropped", rec.Truncated)
	}
}

// TestBoundDeduplicatesAndPrefersTheStrongerClaim: a fact that was injected
// and then found again by memory.search is one source, and the honest thing
// to say about it is the stronger of the two claims — it really was returned
// during this turn.
func TestBoundDeduplicatesAndPrefersTheStrongerClaim(t *testing.T) {
	rec := Bound([]Reference{
		{Kind: KindFact, Strength: Available, Ref: "m1"},
		{Kind: KindFeed, Strength: Available, Ref: "prices"},
		{Kind: KindFact, Strength: Returned, Ref: "m1", Tool: "memory.search"},
	})
	if len(rec.Sources) != 2 {
		t.Fatalf("the same fact was listed twice: %+v", rec.Sources)
	}
	if rec.Sources[0].Ref != "m1" {
		t.Fatalf("the first mention did not keep its place: %+v", rec.Sources)
	}
	if rec.Sources[0].Strength != Returned {
		t.Errorf("the weaker claim won: %q", rec.Sources[0].Strength)
	}
	if rec.Sources[0].Tool != "memory.search" {
		t.Errorf("the tool that returned it was lost: %q", rec.Sources[0].Tool)
	}
}

// TestBoundDropsTheWeakerClaimFirst is the cap's whole argument: when only
// some references fit, the ones a tool actually returned are worth more than
// the ones that were merely in context, so the Available tail leaves first
// and the count of what left is disclosed.
func TestBoundDropsTheWeakerClaimFirst(t *testing.T) {
	var refs []Reference
	for i := 0; i < MaxSources; i++ {
		refs = append(refs, Reference{Kind: KindFact, Strength: Available, Ref: factID(i)})
	}
	refs = append(refs,
		Reference{Kind: KindTool, Strength: Returned, Tool: "shell.run", Subject: "git status"},
		Reference{Kind: KindArtifact, Strength: Returned, Ref: "/tmp/chart.png"},
	)
	rec := Bound(refs)
	if len(rec.Sources) != MaxSources {
		t.Fatalf("kept %d sources, cap is %d", len(rec.Sources), MaxSources)
	}
	if rec.Truncated != 2 {
		t.Errorf("truncated = %d, want the two that did not fit", rec.Truncated)
	}
	// Both mechanically causal references survived, in their collected order.
	tail := rec.Sources[len(rec.Sources)-2:]
	if tail[0].Tool != "shell.run" || tail[1].Ref != "/tmp/chart.png" {
		t.Errorf("a returned source was dropped before an available one: %+v", rec.Sources)
	}
	for _, s := range rec.Sources[:len(rec.Sources)-2] {
		if s.Strength != Available {
			t.Errorf("unexpected survivor: %+v", s)
		}
	}
}

// TestBoundCapsEvenWhenEverythingIsCausal: with nothing weak left to drop the
// tail goes, and the cut is still disclosed rather than silent.
func TestBoundCapsEvenWhenEverythingIsCausal(t *testing.T) {
	var refs []Reference
	for i := 0; i < MaxSources+3; i++ {
		refs = append(refs, Reference{Kind: KindArtifact, Strength: Returned, Ref: factID(i)})
	}
	rec := Bound(refs)
	if len(rec.Sources) != MaxSources || rec.Truncated != 3 {
		t.Fatalf("kept %d, truncated %d", len(rec.Sources), rec.Truncated)
	}
	if rec.Sources[0].Ref != factID(0) {
		t.Errorf("the head was dropped instead of the tail: %+v", rec.Sources[0])
	}
}

// TestSinkCollectsThroughTheContext pins the transport a tool uses to report
// what only it knows.
func TestSinkCollectsThroughTheContext(t *testing.T) {
	var sink Sink
	ctx := WithSink(context.Background(), &sink)
	Note(ctx, Reference{Kind: KindArtifact, Ref: "/tmp/a.png"})
	Note(ctx, Reference{Kind: KindConversation, Ref: "c1"})
	got := sink.Drain()
	if len(got) != 2 || got[0].Ref != "/tmp/a.png" || got[1].Ref != "c1" {
		t.Fatalf("drained %+v", got)
	}
	if again := sink.Drain(); len(again) != 0 {
		t.Errorf("a drained sink still held %+v", again)
	}
}

// TestNoteWithoutASinkIsHarmless: a tool run outside a turn — the CLI, a test
// — reports to nobody and must not care.
func TestNoteWithoutASinkIsHarmless(t *testing.T) {
	Note(context.Background(), Reference{Kind: KindArtifact, Ref: "/tmp/a.png"})
	Note(nil, Reference{Kind: KindArtifact, Ref: "/tmp/a.png"}) //nolint:staticcheck // the nil case is the point
	var nilSink *Sink
	nilSink.Note(Reference{Kind: KindFact, Ref: "m1"})
	if got := nilSink.Drain(); got != nil {
		t.Errorf("a nil sink drained %+v", got)
	}
}

// TestSinkIsSafeUnderConcurrentNotes: a tool may report from whatever
// goroutine it runs on, and the turn drains from its own.
func TestSinkIsSafeUnderConcurrentNotes(t *testing.T) {
	var sink Sink
	ctx := WithSink(context.Background(), &sink)
	var wg sync.WaitGroup
	// Every writer is started and joined explicitly, so the ordering this
	// test relies on is the one it establishes and not the scheduler's.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			Note(ctx, Reference{Kind: KindFact, Ref: factID(i)})
		}(i)
	}
	wg.Wait()
	if got := sink.Drain(); len(got) != 8 {
		t.Fatalf("drained %d references, want 8", len(got))
	}
}

func factID(i int) string {
	return "m" + string(rune('0'+i%10)) + string(rune('a'+i/10))
}
