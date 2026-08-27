package doctor

import (
	"fmt"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// checkVocabularyBias reports the bias half of the taught vocabulary (issue
// #129) in one line: how much of the finite hard-to-hear budget is spent.
// The budget exists because whisper's conditioning prompt is small and the
// assistant's name plus stt.vocabulary already draw on it; a full list is
// the one state a user cannot see from behaviour — the next "listen for the
// word X" simply refuses — so doctor names it before that conversation
// happens.
func checkVocabularyBias(cfg config.Config, paths config.Paths) Result {
	const name = "vocabulary bias budget"
	if !cfg.Vocabulary.Enabled {
		return Result{Status: OK, Name: name,
			Detail: "vocabulary off (vocabulary.enabled = false); no taught phrases bias recognition"}
	}
	// A read-only view over the daemon's own file: the store is stat-fresh
	// by design, so this reads exactly what the daemon would.
	store := vocabulary.NewStore(paths.VocabularyFile(), vocabulary.StoreOptions{
		MaxEntries:        cfg.Vocabulary.MaxEntries,
		MaxInjectedTokens: cfg.Vocabulary.MaxInjectedTokens,
	}, nil)
	flagged, max := store.BiasCount()
	taught, _ := store.Count()
	detail := fmt.Sprintf("%d of %d hard-to-hear phrases in the recognition bias (%d taught %s total)",
		flagged, max, taught, pluralDoctor(taught, "word", "words"))
	if flagged >= max {
		return Result{Status: Warn, Name: name, Detail: detail,
			Fix: "The list is full, so \"listen for the word …\" will refuse until there is room.\n" +
				"Unflag a word Jarvix now hears fine (the window's Memory tab), or move bare\n" +
				"recognition terms — words with no taught meaning — into stt.vocabulary."}
	}
	return Result{Status: OK, Name: name, Detail: detail}
}

// pluralDoctor picks the grammatical form for n.
func pluralDoctor(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
