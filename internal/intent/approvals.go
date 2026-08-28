package intent

// This file is the router's half of remembered command approvals (issue
// #162, ADR 0052): the deterministic phrases that read back what the user has
// pre-approved.
//
// Only the listing is here, and that is the point. There is no phrase that
// *adds* a rule and there never will be one: a standing grant is made by
// answering the card that shows the exact pattern, and a spoken sentence
// carries no such display. Adding "always allow docker ps" to this table
// would create a second, weaker authoring path for the most consequential
// permission change in the product — one that a misheard word could take —
// and issue #162's whole argument is that the pattern must be on screen
// before the user commits. Revocation lives at the CLI and in the window for
// the mirror-image reason: it is safe, but it is also not urgent, and one
// place to do it is easier to trust than three.

// ApprovalsListIntentName identifies "what have i pre-approved".
const ApprovalsListIntentName = "approvals.list"

// approvalsListPatterns are the listing utterances. Fully literal — owned, so
// a custom intent or routine wanting one is refused naming this owner,
// exactly like the vocabulary listing phrases.
//
// The spellings are the ones people actually reach for when they are
// suspicious, which is when this question gets asked: "what can you run
// without asking" is the plain-language version and matters more than the
// jargon one.
var approvalsListPatterns = []string{
	"what have i pre approved",
	"what have i preapproved",
	"what have i approved",
	"what commands have i pre approved",
	"what commands have i approved",
	"what can you run without asking",
	"what can you run without asking me",
	"list my approvals",
	"what are my approvals",
	"what rules have i added",
}
