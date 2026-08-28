// Package conversations is the durable conversation archive (ADR 0027):
// every finished exchange is kept on disk until the user deletes it, so a
// conversation survives `jarvix new`, the follow-up window, and any number of
// daemon restarts.
//
// It deliberately sits beside internal/history rather than replacing it. The
// history file is the live head — the capped, bounded working memory the
// engine reloads at boot — while this package is the unbounded record behind
// it: the retention cap (`history_turns`) governs what the model is sent,
// never what is archived. The content is a transcript of what was said in the
// user's home, so every file is private (0600 in a 0700 directory), contents
// are never logged, and deletion actually deletes — no tombstones.
package conversations

import (
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/provenance"
)

// SchemaVersion is the on-disk document version, carried by both files of a
// conversation so later readers — the search ticket's index above all — can
// recognise what they are looking at without guessing. Bumped only on an
// incompatible change; an unrecognised version is reported as unreadable
// rather than misparsed.
const SchemaVersion = 1

// PreviewChars caps the listing preview. One line of the first user turn is
// what a person needs to recognise a conversation in a list; more would turn
// the metadata file into a second copy of the transcript.
const PreviewChars = 120

// RoleConfirmation marks a turn that is not an utterance but the record of a
// permission-gate exchange (issue #118): what Jarvix asked leave to run,
// verbatim, and how the user answered. It sits in the transcript at the
// position the question was asked — between the user turn that provoked the
// tool call and the assistant turn that followed the answer — because the
// moments the user authorised Jarvix to act are part of the conversation,
// not ephemeral window state. A turn with this role carries the Confirmation
// payload; its Text is the spoken question, so search finds the exchange the
// way it finds anything else that was said.
const RoleConfirmation = "confirmation"

// How a recorded confirmation resolved. Three distinct words on disk because
// they are three different things the user did — said yes, said no, said
// nothing — and conflating any two would put words in the user's mouth in
// the one record whose whole point is what the user actually authorised.
const (
	ConfirmationApproved = "approved"
	ConfirmationDeclined = "declined"
	ConfirmationTimedOut = "timed_out"
)

// Confirmation is the payload of a RoleConfirmation turn: the permission
// question exactly as the card showed it, and its answer.
type Confirmation struct {
	// Tool is the gated identity — a tool name for a model call,
	// the intent tool name for a user-defined voice intent.
	Tool string `json:"tool"`
	// Command is the exact command the user was shown, verbatim — never a
	// summary, for the same reason the live card must not reword it
	// (ADR 0014): this line is the ground truth of what was approved.
	Command string `json:"command"`
	// Rule names the policy rule that decided to ask.
	Rule string `json:"rule,omitempty"`
	// Outcome is one of the three vocabulary constants above.
	Outcome string `json:"outcome"`
	// Source is what resolved it — cli, text, voice, timeout, interrupted,
	// error — the same closed vocabulary the tool.confirmed/tool.declined
	// events carry.
	Source string `json:"source,omitempty"`
	// TimeoutSec is the confirmation window that applied, so a timed-out
	// record can say how long the user was given.
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// Remembered is the word-prefix rule the user added by answering with
	// "don't ask again" (issue #162), and RememberScope how long it stands
	// ("always" or "conversation"). Both empty for an ordinary approve-once,
	// which is nearly every record.
	//
	// They are here because a record that said only "approved" would be
	// dishonest about the most consequential answer the card can take: the
	// user did not approve one command, they changed what runs without
	// asking, and the transcript of the moment they did that is the only
	// place the two facts sit together. Additive with omitempty exactly like
	// Interrupted and Confirmation itself — every existing line stays
	// byte-identical, an old archive loads with both empty, and
	// SchemaVersion stays 1.
	Remembered    string `json:"remembered,omitempty"`
	RememberScope string `json:"remember_scope,omitempty"`
}

// Turn is one archived utterance: who said it, what they said, and when.
// This is the schema the acceptance golden file pins — role, text, and
// timestamp per turn — so search and RAG can index the archive later without
// a migration.
type Turn struct {
	Role string    `json:"role"`
	Text string    `json:"text"`
	Time time.Time `json:"ts"`
	// Interrupted marks both halves of an exchange the user cut off before
	// the assistant finished — a new push-to-talk, `jarvix cancel`, or the
	// stop word (issue #117). The exchange is committed rather than dropped,
	// because the archive's whole promise is that a conversation only ends
	// when the user says so, and an interruption is not the user saying so.
	//
	// Additive on purpose: omitempty keeps every completed turn's line — and
	// the golden files — byte-identical, an old archive without the key
	// unmarshals to false, and SchemaVersion stays 1 because a reader that
	// ignores the key still reads the turn correctly.
	Interrupted bool `json:"interrupted,omitempty"`
	// Confirmation carries the record of a permission-gate exchange when Role
	// is RoleConfirmation (issue #118), and is nil on every utterance.
	// Additive exactly like Interrupted: omitempty keeps every ordinary
	// turn's line byte-identical, an old archive without the key loads with
	// nil, and SchemaVersion stays 1 because a reader that ignores the key
	// still reads every utterance correctly — it merely does not render the
	// approvals, which is all any reader could do before this field existed.
	Confirmation *Confirmation `json:"confirmation,omitempty"`
	// Provenance is what went into the answer (issue #168): the references
	// the turn collected while it was assembled — what was injected, and what
	// a tool returned. Set on assistant turns that consumed something
	// retrievable, and nil everywhere else, including every user turn and
	// every turn that used nothing.
	//
	// Additive on the same terms as the two fields above, and last in the
	// struct so the key order of every line already on disk is untouched:
	// omitempty keeps a turn that consumed nothing byte-identical to one
	// written before this field existed, an old archive loads with nil, and
	// SchemaVersion stays 1 because a reader that ignores the key still reads
	// every utterance correctly.
	//
	// It holds references — ids, names, paths — and never content. Nothing a
	// fact, a feed value, a captured window, or a session transcript said
	// reaches this record; the readable name is resolved from the live store
	// when somebody looks, which is also what lets a source that has since
	// been deleted say so.
	Provenance *provenance.Record `json:"provenance,omitempty"`
}

// Meta describes one archived conversation without its turns — everything a
// listing needs, sized so listing a large library never reads a transcript.
type Meta struct {
	// Schema is SchemaVersion at write time.
	Schema int `json:"schema"`
	// ID is the conversation's stable identity: assigned at creation, never
	// reused, and the handle every CLI and IPC operation takes.
	ID string `json:"id"`
	// Started is when the first turn was archived; LastActive when the most
	// recent one was.
	Started    time.Time `json:"started"`
	LastActive time.Time `json:"last_active"`
	// TurnCount is how many utterances the conversation holds (a user
	// question and its answer count as two).
	TurnCount int `json:"turns"`
	// Preview is the first line of the first user turn, capped at
	// PreviewChars — enough to recognise the conversation in a list.
	Preview string `json:"preview"`
}

// Conversation is one fully read archive record: its metadata and every turn.
type Conversation struct {
	Meta  Meta
	Turns []Turn
}

// Unreadable names a conversation the store could not parse. Listing reports
// these alongside the readable rest: one bad file never hides the library,
// and never silently vanishes either — the user deserves to know a record
// exists even when it cannot be opened.
type Unreadable struct {
	ID  string
	Err string
}

// Store is the archive. Implementations must be safe for concurrent use —
// the engine appends from session tails while IPC handlers list and delete —
// and must tolerate the files changing underneath them (the CLI operates on
// the same directory when the daemon is down).
type Store interface {
	// Append adds turns to conversation id, creating it when id is "" (or
	// names a conversation that no longer exists) and returning the id the
	// turns actually landed in. Appending no turns is a no-op.
	Append(id string, turns []Turn) (string, error)
	// Active returns the id of the conversation the live head belongs to, or
	// "" when there is none. It is how a restarted daemon keeps appending to
	// the same conversation instead of forking a new one per boot. Failures
	// degrade to "": a fresh conversation is always a safe answer.
	Active() string
	// SetActive records id as the conversation the live head belongs to.
	// Append maintains this itself; the explicit call exists for reopening,
	// where the pointer must move even before the next turn is archived.
	SetActive(id string) error
	// List returns every readable conversation's metadata, newest first, plus
	// the ones that could not be read. It must not load any conversation's
	// turns — that is the whole point of Meta being its own file.
	List() ([]Meta, []Unreadable, error)
	// Read returns one whole conversation, turns and all.
	Read(id string) (Conversation, error)
	// Delete removes conversation id from disk. Deleting an unknown id is an
	// error — a deletion the user asked for must never silently do nothing.
	Delete(id string) error
	// DeleteAll removes every conversation and reports how many went.
	DeleteAll() (int, error)
}

// preview derives a listing preview from the first user turn among turns, or
// "" when none is there yet.
func preview(turns []Turn) string {
	for _, t := range turns {
		if t.Role != "user" {
			continue
		}
		line := t.Text
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if len(line) > PreviewChars {
			// Cut on a rune boundary: a preview that ends in half a character
			// would corrupt the JSON-encoded metadata for non-ASCII speech.
			cut := PreviewChars
			for cut > 0 && !isRuneStart(line[cut]) {
				cut--
			}
			line = line[:cut]
		}
		if line != "" {
			return line
		}
	}
	return ""
}

// isRuneStart reports whether b begins a UTF-8 sequence.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// validID rejects ids that could escape the archive directory. Ids arrive
// over IPC and from CLI arguments, and they become file names — so anything
// with a path separator, a traversal, or a leading dot is refused before it
// touches the filesystem.
func validID(id string) error {
	if id == "" {
		return fmt.Errorf("a conversation id is required")
	}
	if strings.HasPrefix(id, ".") || strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("invalid conversation id %q", id)
	}
	return nil
}
