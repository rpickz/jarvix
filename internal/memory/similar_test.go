package memory

import "testing"

// The supersede matcher is tested in both directions, like the redaction
// table (ADR 0019): a false negative accumulates a contradiction the user
// has to notice, and a false positive costs an extra tool round on every
// unrelated remember — so both lists are pinned with realistic facts.
func TestSimilar(t *testing.T) {
	alike := [][2]string{
		// The headline case from the issue: a correction phrased fresh.
		{"the staging server is called atlas", "the staging server is called helios"},
		// Same subject, only two significant words each.
		{"my terminal is ghostty", "my terminal is alacritty"},
		// Word order and glue words differ; the subject does not.
		{"the user's partner's birthday is June 3rd", "the birthday of the user's partner is June 4th"},
		// Case and punctuation must not defeat the match.
		{"The Staging Server is Atlas.", "staging server: helios"},
	}
	for _, pair := range alike {
		if !similar(pair[0], pair[1]) {
			t.Errorf("similar(%q, %q) = false, want true", pair[0], pair[1])
		}
		if !similar(pair[1], pair[0]) {
			t.Errorf("similar(%q, %q) = false, want true (must be symmetric)", pair[1], pair[0])
		}
	}

	unalike := [][2]string{
		// Different subjects entirely.
		{"the staging server is called atlas", "the user's partner's birthday is June 3rd"},
		// One shared word among many is coincidence, not identity: both
		// mention a server, but they are different facts.
		{"the staging server is called atlas", "the production server certificate renews in March"},
		// Stopwords alone must never relate facts.
		{"it is actually the best", "this is not that"},
		// "the user's" glue must not make everything about the user similar.
		{"the user's terminal is ghostty", "the user's dog is called biscuit"},
	}
	for _, pair := range unalike {
		if similar(pair[0], pair[1]) {
			t.Errorf("similar(%q, %q) = true, want false", pair[0], pair[1])
		}
		if similar(pair[1], pair[0]) {
			t.Errorf("similar(%q, %q) = true, want false (must be symmetric)", pair[1], pair[0])
		}
	}
}

// matchesQuery is deliberately looser than similar — a recall that
// over-matches costs a line in a listing, not a contradiction in the store.
func TestMatchesQuery(t *testing.T) {
	fact := "the user's terminal is Ghostty on workspace one"
	for _, q := range []string{
		"ghostty",                             // substring, case-insensitive
		"what do you know about my terminal",  // shared significant word
		"which workspace is the terminal on?", // punctuation stripped
	} {
		if !matchesQuery(q, fact) {
			t.Errorf("matchesQuery(%q) = false, want true", q)
		}
	}
	for _, q := range []string{"the staging server", "kubernetes"} {
		if matchesQuery(q, fact) {
			t.Errorf("matchesQuery(%q) = true, want false", q)
		}
	}
	if !matchesQuery("", fact) {
		t.Error("an empty query must match everything (it means list all)")
	}
}
