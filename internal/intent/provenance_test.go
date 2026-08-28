package intent

import "testing"

// The provenance grammar is read-only and owned. These tests pin the three
// facts that matter: the phrases match, the near-misses deliberately do not,
// and the phrases are owned so nothing else can claim one.

func TestProvenancePhrasesMatch(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range provenancePatterns {
		m, ok := r.Match(phrase)
		if !ok || !m.ProvenanceList {
			t.Errorf("Match(%q) = %+v ok=%v, want the provenance listing", phrase, m, ok)
		}
		if m.Name != ProvenanceIntentName {
			t.Errorf("Match(%q).Name = %q, want %q", phrase, m.Name, ProvenanceIntentName)
		}
	}
}

// The misses belong to the model. "Where did that come from" is a fixed
// question about the last answer; anything that names a subject is a real
// question about that subject and the router must not answer it with a list
// of sources.
func TestProvenanceMissesBelongToTheModel(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"where did that file come from",
		"where did you get that idea about docker",
		"what went into the build",
		"what are your sources for the amd price",
		"where does the deploy script come from",
		"forget where that came from",
	} {
		if m, ok := r.Match(phrase); ok && m.ProvenanceList {
			t.Errorf("Match(%q) claimed the provenance listing; it belongs to the model", phrase)
		}
	}
}

// The phrases are owned, so a custom intent, routine or window nickname that
// wants one is refused naming this owner rather than silently shadowing it.
func TestProvenancePhrasesAreOwned(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range provenancePatterns {
		if owner, taken := r.Owner(phrase); !taken || owner == "" {
			t.Errorf("%q is not owned (owner=%q taken=%v)", phrase, owner, taken)
		}
	}
}
