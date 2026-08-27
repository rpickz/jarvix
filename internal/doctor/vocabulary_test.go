package doctor

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// The bias budget line (issue #129): doctor reports how much of the finite
// hard-to-hear list is spent, and warns when it is full — the one state a
// user cannot see from behaviour, because the next "listen for the word X"
// simply refuses.

func TestVocabularyBiasBudgetReportsUsage(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{State: dir}
	store := vocabulary.NewStore(filepath.Join(dir, "vocabulary.toml"),
		vocabulary.StoreOptions{}, nil)
	entry, _, err := store.Teach("quid", "pounds", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SetHardToHear(entry.ID, true); err != nil {
		t.Fatal(err)
	}

	r := checkVocabularyBias(config.Default(), paths)
	if r.Status != OK {
		t.Fatalf("result = %+v, want OK with room in the budget", r)
	}
	want := fmt.Sprintf("1 of %d hard-to-hear phrases", vocabulary.MaxHardToHear)
	if !strings.Contains(r.Detail, want) {
		t.Errorf("detail = %q, want %q", r.Detail, want)
	}
}

func TestVocabularyBiasBudgetWarnsWhenFull(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{State: dir}
	store := vocabulary.NewStore(filepath.Join(dir, "vocabulary.toml"),
		vocabulary.StoreOptions{}, nil)
	for i := 0; i < vocabulary.MaxHardToHear; i++ {
		entry, _, err := store.Teach(fmt.Sprintf("word%d", i), "m", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.SetHardToHear(entry.ID, true); err != nil {
			t.Fatal(err)
		}
	}

	r := checkVocabularyBias(config.Default(), paths)
	if r.Status != Warn {
		t.Fatalf("result = %+v, want a warning at the cap", r)
	}
	if !strings.Contains(r.Fix, "stt.vocabulary") {
		t.Errorf("fix = %q, want the way out named", r.Fix)
	}
}

func TestVocabularyBiasBudgetWhenDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Vocabulary.Enabled = false
	r := checkVocabularyBias(cfg, config.Paths{State: t.TempDir()})
	if r.Status != OK || !strings.Contains(r.Detail, "vocabulary off") {
		t.Errorf("result = %+v, want an honest OK naming the switch", r)
	}
}
