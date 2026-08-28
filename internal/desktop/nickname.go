package desktop

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// This file holds the window-nickname registry (#126): short, user-chosen
// names ("call this window builds") that every window reference resolves
// before any fuzzy app/title matching.
//
// Two decisions shape it, both recorded in ADR 0040. It is in-memory and
// session-scoped: windows are ephemeral, so a name outliving the daemon
// would sooner or later point at nothing — or worse, at the wrong window
// wearing a recycled address. And release is *lazy revalidation*, not an
// event subscription: every operation is handed the live inventory and drops
// the names whose window is no longer in it, so "builds" can never resolve
// to a closed window no matter how the close happened, and there is no event
// stream to fall behind on. A released name is remembered by name alone, so
// referring to it gets an honest "nothing is called builds right now" rather
// than a shrug.

// Nicknames is the registry. Safe for concurrent use; every consumer of a
// window reference shares one instance so a nickname means the same window
// everywhere.
type Nicknames struct {
	reserved map[string]string
	// phraseOwner asks the intent router whether a whole utterance is
	// already spoken for; nil means no router to collide with.
	phraseOwner func(phrase string) (owner string, taken bool)

	mu sync.Mutex
	// byName holds each nickname's window as it was at assignment. Identity,
	// not state: resolution always answers with the window as the inventory
	// reports it now.
	byName map[string]Window
	// released remembers names whose window has gone (or that were renamed
	// away), for the honest "nothing is called builds right now".
	released map[string]bool
}

// NicknameOptions configure the registry.
type NicknameOptions struct {
	// Reserved maps each word a nickname may not be to a human description
	// of what owns it — the window matcher's own vocabulary ("this",
	// "browser"), supplied by the matcher so the two can never disagree.
	Reserved map[string]string
	// PhraseOwner reports whether an utterance already belongs to the intent
	// grammar, naming the owner (intent.Router.Owner). Nil skips the check.
	PhraseOwner func(phrase string) (owner string, taken bool)
}

// NewNicknames builds an empty registry.
func NewNicknames(opts NicknameOptions) *Nicknames {
	return &Nicknames{
		reserved:    opts.Reserved,
		phraseOwner: opts.PhraseOwner,
		byName:      make(map[string]Window),
		released:    make(map[string]bool),
	}
}

// NamedWindow is one live nickname with the window as the inventory reports
// it now.
type NamedWindow struct {
	Name   string
	Window Window
}

// Assign gives target the nickname spoken, judged against the live
// inventory. name is the normalised nickname actually assigned; previous is
// the name this window gave up for it ("" when it had none); warning is a
// short spoken caution when the name is a common English word, "" otherwise.
// Every error is a spoken-ready refusal that starts lowercase, so callers
// can frame it ("Sorry, …") without rewording it.
func (n *Nicknames) Assign(spoken string, target Window, windows []Window) (name, previous, warning string, err error) {
	words := nicknameWords(spoken)
	switch {
	case len(words) == 0:
		return "", "", "", fmt.Errorf("I did not catch a name to use")
	case len(words) > 1:
		// Single-word is the point of the feature: a multi-word handle is
		// exactly the fragile reference nicknames exist to replace.
		return "", "", "", fmt.Errorf("a nickname is a single word, so it stays easy to say — try just %q", words[0])
	}
	name = words[0]
	if owner, taken := n.reserved[name]; taken {
		return "", "", "", fmt.Errorf("%q already means something in a window reference — %s; choose a different name", name, owner)
	}
	if n.phraseOwner != nil {
		if owner, taken := n.phraseOwner(name); taken {
			return "", "", "", fmt.Errorf("%q is already %s; choose a different name", name, owner)
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked(windows)
	if held, taken := n.byName[name]; taken && !sameWindow(held, target) {
		return "", "", "", fmt.Errorf("%q is already the name of %s; choose a different name", name, held.Describe())
	}
	// One name per window: taking a new one releases the old, and the old
	// name answers honestly from then on, exactly as if its window had gone.
	for existing, held := range n.byName {
		if existing != name && sameWindow(held, target) {
			delete(n.byName, existing)
			n.released[existing] = true
			previous = existing
		}
	}
	n.byName[name] = target
	delete(n.released, name)
	return name, previous, commonWordWarning(name), nil
}

// Resolve answers a window reference from the nicknames: the current
// inventory's window when the reference is a live nickname, ok false
// otherwise. This is the first stop of every resolution — nickname before
// any fuzzy matching, precedence pinned by test in the matcher.
func (n *Nicknames) Resolve(reference string, windows []Window) (Window, bool) {
	words := nicknameWords(reference)
	if n == nil || len(words) != 1 {
		return Window{}, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked(windows)
	held, ok := n.byName[words[0]]
	if !ok {
		return Window{}, false
	}
	// Answer with the window as it is now — its title may have moved on
	// since assignment; only its identity is held.
	for _, w := range windows {
		if sameWindow(held, w) {
			return w, true
		}
	}
	return Window{}, false // unreachable after the prune, but never a stale answer
}

// Released reports whether the reference is a nickname whose window has gone
// — the "nothing is called builds right now" case, as distinct from a name
// that never meant anything.
func (n *Nicknames) Released(reference string, windows []Window) bool {
	words := nicknameWords(reference)
	if n == nil || len(words) != 1 {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked(windows)
	return n.released[words[0]]
}

// List returns the live nicknames with their windows as the inventory
// reports them now, sorted by name so every surface lists them identically.
func (n *Nicknames) List(windows []Window) []NamedWindow {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked(windows)
	out := make([]NamedWindow, 0, len(n.byName))
	for name, held := range n.byName {
		for _, w := range windows {
			if sameWindow(held, w) {
				out = append(out, NamedWindow{Name: name, Window: w})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Count reports how many nicknames are held right now, deliberately without
// pruning — no inventory is consulted. It exists as the window-overlay feed's
// cheap enrolment gate (#127): "is there anything a poll could possibly
// overlay?" must be answerable without a compositor call, or the gate would
// cost exactly what it exists to save. The price is honesty at the margin: a
// name whose window has closed still counts until the next pruning operation
// (any Assign/Resolve/List against a live inventory) drops it — which the
// overlay poll itself does, so an over-count converges to zero rather than
// polling forever.
func (n *Nicknames) Count() int {
	if n == nil {
		return 0
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.byName)
}

// pruneLocked is the release mechanism: any name whose window is not in the
// inventory is dropped and remembered as released. Must be called with n.mu
// held, with the inventory the answer will be judged against.
func (n *Nicknames) pruneLocked(windows []Window) {
	for name, held := range n.byName {
		alive := false
		for _, w := range windows {
			if sameWindow(held, w) {
				alive = true
				break
			}
		}
		if !alive {
			delete(n.byName, name)
			n.released[name] = true
		}
	}
}

// sameWindow is window identity, the same three facts the window tools
// verify before acting: an address is a reusable handle, so it never stands
// alone.
func sameWindow(a, b Window) bool {
	return a.Address == b.Address && a.StableID == b.StableID && strings.EqualFold(a.Class, b.Class)
}

// nicknameWords normalises a spoken name exactly as window references are
// normalised — lower case, split on anything that is not a letter or digit —
// so the name assigned is byte-for-byte the token resolution will look up.
func nicknameWords(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// commonSpeechWords are everyday English words likely to appear in normal
// speech near Jarvix. Choosing one as a nickname is allowed — precedence is
// deterministic either way — but worth a warning, because "open work" and
// "close notes" read as sentences before they read as window commands. A
// deliberately short, curated list, like the matcher's category table: each
// entry is a word people actually say to an assistant, not a frequency cut.
var commonSpeechWords = map[string]bool{
	"yes": true, "no": true, "okay": true, "ok": true, "right": true,
	"sure": true, "thanks": true, "hello": true, "hey": true, "help": true,
	"open": true, "close": true, "move": true, "switch": true, "start": true,
	"launch": true, "list": true, "show": true, "call": true, "name": true,
	"work": true, "home": true, "time": true, "today": true, "tomorrow": true,
	"now": true, "next": true, "back": true, "again": true, "done": true,
	"more": true, "less": true, "up": true, "down": true, "left": true,
	"good": true, "new": true, "old": true, "big": true, "little": true,
}

// commonWordWarning is the caution suffixed to an assignment confirmation
// when the chosen name is a common English word — a warning, never a
// refusal: the name is the user's to choose.
func commonWordWarning(name string) string {
	if !commonSpeechWords[name] {
		return ""
	}
	return fmt.Sprintf("Just so you know, %s is a common word, so it may come up when you do not mean this window.", name)
}
