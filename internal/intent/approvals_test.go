package intent

import "testing"

// The approvals grammar is read-only and owned. These tests pin all three
// facts: the phrases match, the near-misses deliberately do not, and nothing
// in this router can ADD a grant.

func TestApprovalsListPhrasesMatch(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range approvalsListPatterns {
		m, ok := r.Match(phrase)
		if !ok || !m.ApprovalsList {
			t.Errorf("Match(%q) = %+v ok=%v, want the approvals listing", phrase, m, ok)
		}
		if m.Name != ApprovalsListIntentName {
			t.Errorf("Match(%q).Name = %q, want %q", phrase, m.Name, ApprovalsListIntentName)
		}
	}
}

// The misses belong to the model. Every deterministic grammar here ships one
// of these, and this one matters more than most: an utterance about
// permissions that the router half-claims would answer the wrong question
// about the most sensitive thing in the product.
func TestApprovalsMissesBelongToTheModel(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"what have i approved for the budget",
		"approve the pull request",
		"why did you run that without asking",
		"should i pre approve docker",
		"pre approve docker ps",
		"always allow docker ps",
		"stop asking me about docker",
		"forget my approvals",
	} {
		if m, ok := r.Match(phrase); ok && m.ApprovalsList {
			t.Errorf("Match(%q) claimed the approvals listing; it belongs to the model", phrase)
		}
	}
}

// No phrase in this router grants anything. The card is the only authoring
// surface, because it is the only one that can show the exact rule beside the
// command that provoked it (see approvals.go).
func TestNoSpokenPhraseGrantsAnApproval(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"always allow docker ps",
		"never ask me about docker ps again",
		"pre approve docker ps",
		"add docker ps to my approvals",
		"don't ask again",
		"remember that",
	} {
		if m, ok := r.Match(phrase); ok && m.ApprovalsList {
			t.Errorf("Match(%q) reached the approvals family; only listing lives here", phrase)
		}
	}
}

// The listing phrases sit in the collision set, so a routine or custom intent
// claiming one is a configuration error naming both owners rather than a coin
// toss.
func TestApprovalsPhrasesAreOwnedLiterals(t *testing.T) {
	if _, err := New(Options{Routines: []RoutinePhrases{{
		Name: "lister", Phrases: []string{"list my approvals"},
	}}}); err == nil {
		t.Fatal("a routine claimed an approvals listing phrase without an error")
	}
}
