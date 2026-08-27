package tools

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rpickz/jarvix/internal/desktop"
)

// This file resolves what the user said into which window they meant.
//
// The design rule is honesty over cleverness. People do not name windows the
// way a compositor does — they say "my browser", "the editor", "the terminal
// running the tests" — so matching has to be loose: case-insensitive,
// substring-tolerant, across both the application class and the title, with a
// small vocabulary of categories ("browser") mapped to the applications that
// are one. But looseness produces ties, and a tie must never be broken by
// guessing: focusing the wrong window costs a second, and teaches the user
// that Jarvix does not know what it is doing, which costs the feature. So
// several matches means "which one?" — naming them — and no match means
// "nothing matches", said plainly.
//
// The tiers below are the whole of the ranking. A better tier always wins
// outright: an exact application name beats a substring, which beats a
// category guess. Only a tie *within* the winning tier is ambiguous, which is
// what stops "firefox" from being ambiguous merely because a category alias
// also matched Chromium.

// matchTier ranks how directly a window answers to what was said. Higher is
// a better answer.
type matchTier int

const (
	tierNone matchTier = iota
	// tierAlias: the query named a category ("browser") and this window is
	// one of the applications in it. The weakest tier, because the user never
	// said this application's name.
	tierAlias
	// tierAllWords: every word of the query appears somewhere in the class or
	// title, in any order — "terminal tests" finding "Alacritty — go test".
	tierAllWords
	// tierSubstring: the query appears inside the class or the title.
	tierSubstring
	// tierPrefix: the class or title starts with the query.
	tierPrefix
	// tierExactTitle: the query is the whole title.
	tierExactTitle
	// tierExactClass: the query is the application's name. The strongest
	// signal there is — the user said what it is called.
	tierExactClass
)

// resolveKind is how a reference resolved.
type resolveKind int

const (
	// resolveNone: nothing matched.
	resolveNone resolveKind = iota
	// resolveOne: exactly one window matched at the winning tier.
	resolveOne
	// resolveMany: several windows tied at the winning tier, so the user has
	// to choose.
	resolveMany
	// resolveReleased: nothing matched, but the reference is a nickname
	// (#126) whose window has closed — a different honest answer from "never
	// heard of it".
	resolveReleased
)

// resolution is one attempt to turn a spoken reference into a window.
type resolution struct {
	Kind resolveKind
	// Window is the match, valid only when Kind is resolveOne. Its Address is
	// captured here and is the only thing ever dispatched: from this point on
	// the query is irrelevant, so a window appearing or moving later cannot
	// change what gets acted on.
	Window desktop.Window
	// Candidates are the tied matches when Kind is resolveMany.
	Candidates []desktop.Window
	// Query is the normalised reference, for the messages the model is given.
	Query string
}

// deicticWords are the ways a user refers to the window they are already in.
// They resolve to the focused window rather than to any matching, because
// "move this to workspace three" is about where the user is looking, not about
// anything they named.
var deicticWords = map[string]bool{
	"this": true, "that": true, "current": true, "active": true,
	"focused": true, "focus": true, "it": true, "here": true,
	"mine": true, "front": true, "foreground": true,
}

// stopWords are dropped before matching: they carry no identity, and left in
// they would defeat every exact match ("my firefox" is "firefox").
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "my": true, "our": true, "your": true,
	"window": true, "windows": true, "app": true, "application": true,
	"program": true, "please": true, "one": true, "to": true, "of": true,
	"on": true, "in": true, "for": true, "with": true, "running": true,
}

// appCategories maps a category people say out loud to the applications that
// are one. It is deliberately a short, hand-written list rather than anything
// inferred: it exists so "focus my browser" works when exactly one browser is
// open, and it is the weakest tier, so naming an application always wins over
// it. When several applications in a category are open, the result is the
// honest one — a question.
var appCategories = map[string][]string{
	"browser": {"firefox", "chromium", "chrome", "brave", "vivaldi", "librewolf",
		"waterfox", "epiphany", "zen", "qutebrowser", "opera", "thorium"},
	"terminal": {"alacritty", "kitty", "foot", "ghostty", "wezterm", "konsole",
		"gnome-terminal", "xterm", "terminator", "tilix", "urxvt"},
	"editor": {"code", "vscode", "codium", "cursor", "zed", "sublime", "nvim", "vim",
		"neovim", "emacs", "helix", "kate", "gedit", "intellij", "pycharm", "goland", "webstorm"},
	"files": {"nautilus", "thunar", "dolphin", "nemo", "pcmanfm", "files", "yazi"},
	"music": {"spotify", "rhythmbox", "clementine", "audacious", "tidal", "amberol"},
	"chat":  {"slack", "discord", "signal", "telegram", "element", "matrix", "teams"},
	"mail":  {"thunderbird", "evolution", "geary", "mailspring", "outlook"},
	"notes": {"obsidian", "logseq", "notion", "joplin", "anytype"},
	"video": {"mpv", "vlc", "celluloid", "totem", "youtube"},
	"passwords": {"bitwarden", "1password", "keepassxc", "proton pass", "enpass",
		"lastpass"},
}

// categorySynonyms are the other words for a category. Matching is on single
// words, so a phrase like "file manager" is reached through its distinctive
// word rather than as a phrase.
var categorySynonyms = map[string]string{
	"browsers": "browser", "web": "browser", "internet": "browser",
	"terminals": "terminal", "console": "terminal", "shell": "terminal", "term": "terminal",
	"editors": "editor", "ide": "editor", "code": "editor",
	"file": "files", "filemanager": "files", "explorer": "files",
	"spotify": "music", "player": "music",
	"messages": "chat", "messenger": "chat", "chats": "chat",
	"email": "mail", "inbox": "mail",
	"note": "notes", "notebook": "notes",
	"videos": "video", "movie": "video",
	"password": "passwords", "vault": "passwords",
}

// resolveWindow turns a spoken reference into a window, or into an honest
// non-answer. windows is the inventory captured for this resolution and the
// only thing consulted: no second look, no compositor call, no state beyond
// the nickname registry, which is judged against this same inventory.
func resolveWindow(query string, windows []desktop.Window, names *desktop.Nicknames) resolution {
	tokens := normaliseTokens(query)
	res := resolution{Query: strings.Join(tokens, " ")}
	if len(windows) == 0 {
		return res
	}
	// "This one", "the current window", or nothing at all: the user means
	// where they already are. Answering that from the inventory rather than
	// from a second compositor call keeps one capture per resolution.
	if isDeictic(tokens) {
		for _, w := range windows {
			if w.Focused {
				return resolution{Kind: resolveOne, Window: w, Query: res.Query}
			}
		}
		return res
	}
	// The nickname seam (#126): a chosen name outranks every matching tier.
	// The precedence is deliberate and test-pinned — the user picked this
	// name to *stop* depending on what apps and titles happen to say, so a
	// title that contains the same word must never outbid it. Deictic words
	// stay above it only because they are reserved: no nickname can be one.
	if names != nil {
		if w, ok := names.Resolve(res.Query, windows); ok {
			return resolution{Kind: resolveOne, Window: w, Query: res.Query}
		}
	}

	best := tierNone
	var winners []desktop.Window
	for _, w := range windows {
		tier := scoreWindow(res.Query, tokens, w)
		switch {
		case tier == tierNone || tier < best:
			continue
		case tier > best:
			best, winners = tier, []desktop.Window{w}
		default:
			winners = append(winners, w)
		}
	}
	switch len(winners) {
	case 0:
		// A total miss that used to be a nickname is its own honest answer:
		// "nothing is called builds right now", not "never heard of builds".
		if names != nil && names.Released(res.Query, windows) {
			res.Kind = resolveReleased
		}
		return res
	case 1:
		res.Kind, res.Window = resolveOne, winners[0]
	default:
		res.Kind, res.Candidates = resolveMany, winners
	}
	return res
}

// scoreWindow ranks one window against an already-normalised query.
func scoreWindow(query string, tokens []string, w desktop.Window) matchTier {
	if query == "" {
		return tierNone
	}
	class := strings.ToLower(strings.TrimSpace(w.Class))
	app := strings.ToLower(desktop.AppName(w.Class))
	title := strings.ToLower(strings.TrimSpace(w.Title))
	switch {
	case query == class || query == app:
		return tierExactClass
	case query == title:
		return tierExactTitle
	case strings.HasPrefix(class, query) || strings.HasPrefix(app, query) || strings.HasPrefix(title, query):
		return tierPrefix
	case strings.Contains(class, query) || strings.Contains(title, query):
		return tierSubstring
	}
	// Word-wise, against a haystack normalised exactly as the query was, so
	// punctuation in a class ("chrome-chatgpt.com__-Profile_3") cannot hide a
	// word the user actually said.
	haystack := strings.Join(normaliseTokensKeepAll(class+" "+title), " ")
	if allWordsPresent(tokens, haystack) {
		return tierAllWords
	}
	for _, term := range categoryTerms(tokens) {
		if strings.Contains(class, term) || strings.Contains(title, term) {
			return tierAlias
		}
	}
	return tierNone
}

// allWordsPresent reports whether every query word appears in the haystack as
// a word prefix — "term" matching "terminal", but never "xterm" matching
// "term", which would make half the desktop a match for half the words.
func allWordsPresent(tokens []string, haystack string) bool {
	if len(tokens) == 0 {
		return false
	}
	words := strings.Fields(haystack)
	for _, token := range tokens {
		found := false
		for _, w := range words {
			if strings.HasPrefix(w, token) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// categoryTerms expands category words in the query into application names.
func categoryTerms(tokens []string) []string {
	var terms []string
	for _, token := range tokens {
		category := token
		if canonical, ok := categorySynonyms[token]; ok {
			category = canonical
		}
		terms = append(terms, appCategories[category]...)
	}
	return terms
}

// ReservedWindowWords is the matcher's own vocabulary (#126): every word
// that already means something in a window reference, each with the
// description a nickname refusal names as its owner. Supplied to the
// nickname registry from here — the one place the vocabulary lives — so the
// matcher and the refusals can never disagree about what is reserved.
// Application names are deliberately absent: nicknaming a window "firefox"
// is allowed, and the nickname's precedence over app matching is the
// deterministic contract, pinned by test.
func ReservedWindowWords() map[string]string {
	out := make(map[string]string, len(deicticWords)+len(stopWords)+len(appCategories)+len(categorySynonyms))
	for w := range deicticWords {
		out[w] = "it is how a reference says \"the window I am in\""
	}
	for w := range stopWords {
		out[w] = "it is a filler word window references ignore"
	}
	for c := range appCategories {
		out[c] = fmt.Sprintf("it is the word for any %s window", c)
	}
	for s, c := range categorySynonyms {
		out[s] = fmt.Sprintf("it is another word for any %s window", c)
	}
	return out
}

// isDeictic reports whether the reference points at the window the user is
// already in — including an empty reference, which is the same thing said by
// saying nothing.
func isDeictic(tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	for _, t := range tokens {
		if !deicticWords[t] {
			return false
		}
	}
	return true
}

// normaliseTokens lowercases, splits on anything that is not a letter or
// digit, and drops the words that carry no identity.
func normaliseTokens(s string) []string {
	out := make([]string, 0, 8)
	for _, w := range normaliseTokensKeepAll(s) {
		if stopWords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// normaliseTokensKeepAll is normaliseTokens without the stop-word filter, used
// for the haystack: dropping "running" from a *title* would be dropping
// content, not noise.
func normaliseTokensKeepAll(s string) []string {
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

// FindWindow resolves an app reference from a routine step against an
// inventory with the same identity logic desktop.focus_window uses, adapted
// to a caller that cannot ask a question. Two adaptations, both deliberate:
//
//   - Ties are broken by focus recency (the inventory's order) instead of
//     being surfaced. A routine runs unattended — "which Firefox did you
//     mean?" has no one to answer it — and the most recently used window is
//     the one the user thinks of as "the" Firefox.
//   - The category-alias tier does not apply. A model resolving "my editor"
//     wants "editor" to reach IntelliJ; a routine step that says `app =
//     "code"` names a program, and letting the editor alias claim an open
//     GoLand window would move the wrong application and then never launch
//     the right one — the exact duplicate-window annoyance dedupe exists to
//     prevent, inverted.
//
// ok is false when nothing matches, which is the caller's cue to launch.
func FindWindow(query string, windows []desktop.Window) (desktop.Window, bool) {
	tokens := normaliseTokens(query)
	normalised := strings.Join(tokens, " ")
	best := tierAlias // exclusive floor: only tiers above it count
	var winner desktop.Window
	var found bool
	for _, w := range windows {
		tier := scoreWindow(normalised, tokens, w)
		if tier > best {
			best, winner, found = tier, w, true
		}
	}
	return winner, found
}

// maxNamedCandidates bounds how many windows an ambiguity question names.
// Past a handful, reading the list aloud is worse than the ambiguity.
const maxNamedCandidates = 5

// describeCandidates lists tied matches for the model to read back. Ordered as
// the inventory was (most-recently-focused first), so the window the user most
// likely means is named first.
func describeCandidates(windows []desktop.Window) string {
	named := windows
	extra := 0
	if len(named) > maxNamedCandidates {
		extra = len(named) - maxNamedCandidates
		named = named[:maxNamedCandidates]
	}
	parts := make([]string, 0, len(named))
	for _, w := range named {
		parts = append(parts, w.Describe()+" on workspace "+workspaceLabel(w))
	}
	list := strings.Join(parts, "; ")
	if extra > 0 {
		list += "; and " + plural(extra, "other window", "other windows")
	}
	return list
}

// workspaceLabel names a workspace the way the user would: its number, or its
// name when it has one that is not just the number (Hyprland's special
// workspaces).
func workspaceLabel(w desktop.Window) string {
	if name := strings.TrimSpace(w.WorkspaceName); name != "" {
		return name
	}
	return strconv.Itoa(w.Workspace)
}

// summariseWindows renders the whole inventory for the model, grouped so a
// spoken summary can be built from it without arithmetic. Addresses are
// deliberately absent: nothing in this string may be read aloud by accident,
// and a string that never contains an address cannot leak one. nicknames
// (#126) maps a window's address to its nickname — the address is the lookup
// key here and nothing more; it still never enters the string.
func summariseWindows(windows []desktop.Window, nicknames map[string]string) string {
	byApp := map[string][]desktop.Window{}
	order := make([]string, 0, len(windows))
	for _, w := range windows {
		app := desktop.AppName(w.Class)
		if app == "" {
			app = "an unnamed application"
		}
		if _, seen := byApp[app]; !seen {
			order = append(order, app)
		}
		byApp[app] = append(byApp[app], w)
	}
	sort.SliceStable(order, func(i, j int) bool { return len(byApp[order[i]]) > len(byApp[order[j]]) })

	var b strings.Builder
	for _, app := range order {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(app)
		b.WriteString(": ")
		titles := make([]string, 0, len(byApp[app]))
		for _, w := range byApp[app] {
			label := strings.TrimSpace(w.Title)
			if label == "" {
				label = "no title"
			}
			label += " (workspace " + workspaceLabel(w) + ")"
			if nick := nicknames[w.Address]; nick != "" {
				label += " — the user calls it " + nick
			}
			if w.Focused {
				label += " — the one the user is in"
			}
			titles = append(titles, label)
		}
		b.WriteString(strings.Join(titles, ", "))
	}
	return b.String()
}
