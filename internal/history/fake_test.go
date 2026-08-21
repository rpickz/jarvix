package history

import (
	"errors"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

// Contract tests for the Fake the engine's persistence tests lean on: the
// double must behave like a Store, or those tests prove nothing.

func TestFakeRoundTripsHistory(t *testing.T) {
	f := NewFake()
	msgs := []ai.Message{
		{Role: ai.RoleUser, Content: "why is the build red?"},
		{Role: ai.RoleAssistant, Content: "a test failed."},
	}
	turn := time.Now().Truncate(time.Second)
	if err := f.Save(msgs, turn); err != nil {
		t.Fatal(err)
	}
	got, gotTurn, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != msgs[0].Content || got[0].Role != msgs[0].Role ||
		got[1].Content != msgs[1].Content || got[1].Role != msgs[1].Role || !gotTurn.Equal(turn) {
		t.Errorf("loaded %+v at %v", got, gotTurn)
	}
	if f.Saves() != 1 {
		t.Errorf("saves = %d", f.Saves())
	}
	// The save was announced so tests can wait without sleeping.
	select {
	case op := <-f.Ops:
		if op != "save" {
			t.Errorf("op = %q", op)
		}
	default:
		t.Error("Save did not notify Ops")
	}
}

func TestFakeSeedDoesNotCountAsSave(t *testing.T) {
	f := NewFake()
	f.Seed([]ai.Message{{Role: ai.RoleUser, Content: "earlier"}}, time.Now())
	if f.Saves() != 0 {
		t.Errorf("Seed counted as a save: %d", f.Saves())
	}
	got, _, err := f.Load()
	if err != nil || len(got) != 1 || got[0].Content != "earlier" {
		t.Errorf("Load = %+v, %v", got, err)
	}
}

func TestFakeClearEmptiesHistory(t *testing.T) {
	f := NewFake()
	if err := f.Save([]ai.Message{{Role: ai.RoleUser, Content: "x"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.Clear(); err != nil {
		t.Fatal(err)
	}
	got, turn, err := f.Load()
	if err != nil || len(got) != 0 || !turn.IsZero() {
		t.Errorf("after Clear: %+v, %v, %v", got, turn, err)
	}
	if f.Clears() != 1 {
		t.Errorf("clears = %d", f.Clears())
	}
}

func TestFakeScriptedFailures(t *testing.T) {
	f := NewFake()
	f.LoadErr = errors.New("load broke")
	f.SaveErr = errors.New("save broke")
	f.ClearErr = errors.New("clear broke")

	if _, _, err := f.Load(); !errors.Is(err, f.LoadErr) {
		t.Errorf("Load err = %v", err)
	}
	if err := f.Save(nil, time.Time{}); !errors.Is(err, f.SaveErr) {
		t.Errorf("Save err = %v", err)
	}
	if err := f.Clear(); !errors.Is(err, f.ClearErr) {
		t.Errorf("Clear err = %v", err)
	}
	// Failed attempts still count and still notify: the engine's degradation
	// paths are waited on exactly like successes.
	if f.Saves() != 1 || f.Clears() != 1 {
		t.Errorf("saves = %d, clears = %d", f.Saves(), f.Clears())
	}
	if len(f.Ops) != 2 {
		t.Errorf("ops queued = %d, want save+clear", len(f.Ops))
	}
}
