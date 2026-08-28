package intent

// This file is the router's half of answer provenance (issue #168): the
// deterministic phrases that ask what went into the last answer.
//
// It is read-only, and deliberately so. "Where did that come from?" is a
// question about the record, and the record was written when the turn ran —
// no phrase here can add a source, edit one, or ask the model to explain
// itself. Routing it deterministically also keeps the answer honest by
// construction: the listing is composed from what the daemon collected, and
// the model never sees the question, so it can never be tempted to invent a
// citation on the way past (#71).

// ProvenanceIntentName identifies "where did that come from".
const ProvenanceIntentName = "provenance.list"

// provenancePatterns are the asking utterances. Fully literal — owned, so a
// custom intent or routine wanting one is refused naming this owner, exactly
// like the vocabulary and approvals listing phrases.
//
// The spellings are the ones people reach for when they are checking a number
// they were just told: the plain "where did that come from" first, and the
// variants that name the answer rather than the fact.
var provenancePatterns = []string{
	"where did that come from",
	"where did this come from",
	"where does that come from",
	"where did you get that",
	"where did you get that from",
	"what went into that",
	"what went into that answer",
	"what went into this answer",
	"what did you use to answer that",
	"what were your sources",
	"what are your sources",
}
