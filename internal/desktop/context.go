package desktop

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// This file implements desktop context gathering (ADR 0018): the daemon looks
// at what the user is looking at — the focused window, the primary selection,
// the clipboard — and offers it to the assistant, so "what does this error
// mean?" answers the stack trace on screen instead of the question in the
// abstract.
//
// Three properties shape everything here, and each is a requirement rather
// than a nicety:
//
//   - Opt-in per source. A disabled source has no gatherer at all, so it is
//     not merely ignored at the end: nothing is executed, and there is no
//     path on which its content could come to exist. The clipboard defaults
//     off because it is the source most likely to hold something the user
//     never meant to show anyone.
//   - Bounded. Sources are gathered in parallel, each under its own timeout,
//     all inside one budget. Missing, slow, or hung sources degrade to *no
//     context* — never to a slower session. Context is a bonus; latency is
//     the product.
//   - Disclosed. Every capture is retained for `jarvix status --last` and the
//     context.last IPC method, so the user can always see what Jarvix saw.
//     Contents are never logged, at any level.

// Context-gathering defaults. The timeout is both the per-source timeout and
// the whole-attempt budget: sources run in parallel, so one number expresses
// both, and it is the hard ceiling on what context may cost a session.
const (
	// DefaultTimeout bounds gathering. 300ms is the point past which the user
	// would notice the assistant hesitating.
	DefaultTimeout = 300 * time.Millisecond
	// DefaultMaxChars caps how much of one source reaches the model.
	DefaultMaxChars = 2000
	// maxCaptureBytes caps what is read from a gatherer before truncation, so
	// a clipboard holding a video never becomes a clipboard holding a video
	// in the daemon's heap. Generous against DefaultMaxChars: multi-byte text
	// is still text.
	maxCaptureBytes = 256 * 1024
)

// Source names one context source. The string values are the config keys and
// the IPC/event labels — one vocabulary everywhere.
type Source string

// Context sources.
const (
	SourceWindow    Source = "window"
	SourceSelection Source = "selection"
	SourceClipboard Source = "clipboard"
)

// Label is the human phrase for a source, used in the message the model sees
// and in `jarvix status --last`.
func (s Source) Label() string {
	switch s {
	case SourceWindow:
		return "active window"
	case SourceSelection:
		return "selected text"
	case SourceClipboard:
		return "clipboard"
	}
	return string(s)
}

// Item is one source's contribution to a capture, exactly as it reached the
// model: already truncated, already redacted. Nothing anywhere else holds the
// raw text, so what the user is shown afterwards is what was actually sent.
type Item struct {
	Source Source
	Text   string
	// Chars is the length of the source's text as captured, before
	// truncation — so the audit surfaces can say how much was withheld.
	Chars int
	// Truncated marks text cut at the configured per-source cap; the cut is
	// also marked inside Text, so every surface carries it.
	Truncated bool
	// Redacted marks text replaced wholesale by RedactedMarker because it
	// looked like a secret.
	Redacted bool
}

// Snapshot is one capture attempt: what was gathered, when, and how long it
// took. An attempt that found nothing is still a Snapshot — "Jarvix saw
// nothing" is an audit answer too.
type Snapshot struct {
	Items   []Item
	At      time.Time
	Elapsed time.Duration
}

// Sources lists the sources that contributed, in gathering order.
func (s Snapshot) Sources() []string {
	out := make([]string, 0, len(s.Items))
	for _, item := range s.Items {
		out = append(out, string(item.Source))
	}
	return out
}

// contextPreamble introduces the capture to the model. It says where the
// content came from, that it was gathered automatically rather than typed,
// and that ignoring it is a valid outcome — a focused terminal is not an
// invitation to talk about the terminal.
const contextPreamble = "Desktop context: what the user is looking at on this computer right now, " +
	"captured automatically when they spoke. Use it only if it helps answer what they actually " +
	"asked, and never mention it otherwise."

// Message renders the capture as the system-adjacent message the model
// receives: one clearly-delimited block per source, so content can never be
// mistaken for instructions and the model can tell the clipboard from the
// screen. Empty when nothing was captured — an empty capture must not cost a
// message.
func (s Snapshot) Message() string {
	if len(s.Items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(contextPreamble)
	for _, item := range s.Items {
		b.WriteString("\n\n--- ")
		b.WriteString(item.Source.Label())
		b.WriteString(" ---\n")
		b.WriteString(item.Text)
		b.WriteString("\n--- end ")
		b.WriteString(item.Source.Label())
		b.WriteString(" ---")
	}
	return b.String()
}

// Gatherer reads one context source. Implementations are short-lived
// subprocesses in production (ADR 0002/0003) and plain values in tests; every
// one of them must honour ctx, because the budget is enforced through it.
//
// A gatherer returning an error, or empty text, contributes nothing. There is
// deliberately no way for it to report a *problem* upwards: a missing
// compositor and an empty clipboard are the same outcome to a session.
type Gatherer interface {
	Source() Source
	Gather(ctx context.Context) (string, error)
}

// Options configure a Collector. The booleans are the per-source opt-in from
// [context] in config.toml; zero values for the rest take the defaults.
type Options struct {
	Window    bool
	Selection bool
	Clipboard bool
	// MaxChars caps each source's contribution. Zero means DefaultMaxChars.
	MaxChars int
	// Timeout bounds each source and the whole attempt. Zero means
	// DefaultTimeout.
	Timeout time.Duration
	// HyprctlBinary overrides the compositor query binary (tests, unusual
	// installs). Empty means "hyprctl" from PATH.
	HyprctlBinary string
	// WLPasteBinary overrides the selection/clipboard binary. Empty means
	// "wl-paste" from PATH.
	WLPasteBinary string
}

// Collector gathers the enabled sources in parallel and hands back one
// Snapshot. Build it with NewCollector: the guarantee that a disabled source
// is never executed is structural — it simply has no gatherer — and that only
// holds if the gatherer list is built in one place.
type Collector struct {
	gatherers []Gatherer
	maxChars  int
	timeout   time.Duration
	log       *slog.Logger
}

// NewCollector builds a collector for the enabled sources, or nil when none
// are enabled. Nil is the zero-cost case the caller checks for: with context
// switched off entirely there is no collector to call, no goroutines to
// start, and nothing added to a session at all.
func NewCollector(opts Options, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	var gatherers []Gatherer
	// Order here is the order sources appear in the model's message and in
	// every audit surface: where the user is, then what they highlighted,
	// then what they copied.
	if opts.Window {
		gatherers = append(gatherers, &ActiveWindow{Binary: opts.HyprctlBinary})
	}
	if opts.Selection {
		gatherers = append(gatherers, &PrimarySelection{Binary: opts.WLPasteBinary})
	}
	if opts.Clipboard {
		gatherers = append(gatherers, &Clipboard{Binary: opts.WLPasteBinary})
	}
	if len(gatherers) == 0 {
		return nil
	}
	maxChars := opts.MaxChars
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Collector{gatherers: gatherers, maxChars: maxChars, timeout: timeout, log: log}
}

// NewCollectorFrom builds a collector over explicit gatherers, for tests and
// for any future source that is not one of the three shipped ones. Nil when
// there are none, matching NewCollector.
func NewCollectorFrom(gatherers []Gatherer, opts Options, log *slog.Logger) *Collector {
	c := &Collector{gatherers: gatherers, maxChars: opts.MaxChars, timeout: opts.Timeout, log: log}
	if len(gatherers) == 0 {
		return nil
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.maxChars <= 0 {
		c.maxChars = DefaultMaxChars
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	return c
}

// Collect gathers every enabled source in parallel and returns what arrived
// inside the budget. It never returns an error: a source that fails, hangs,
// or has nothing to say contributes nothing, and the session proceeds exactly
// as it would with context switched off.
//
// The nil receiver is handled deliberately. A typed-nil *Collector stored in
// an interface is the classic way a "disabled" feature comes back to life as
// a panic; here it is simply an empty capture.
func (c *Collector) Collect(ctx context.Context) Snapshot {
	if c == nil || len(c.gatherers) == 0 {
		return Snapshot{}
	}
	start := time.Now()
	// The whole attempt shares one budget, and each source additionally gets
	// its own timeout of the same length. With parallel gathering the two
	// numbers coincide by construction, which is the point: adding a source
	// can never extend what context costs a session.
	budget, cancelBudget := context.WithTimeout(ctx, c.timeout)
	defer cancelBudget()

	found := make([]*Item, len(c.gatherers))
	var wg sync.WaitGroup
	for i, g := range c.gatherers {
		wg.Add(1)
		go func(i int, g Gatherer) {
			defer wg.Done()
			srcCtx, cancel := context.WithTimeout(budget, c.timeout)
			defer cancel()
			text, err := g.Gather(srcCtx)
			if err != nil {
				// Expected constantly: no compositor, empty clipboard, binary
				// not installed. Debug, never a warning — nothing is wrong.
				c.log.Debug("desktop context source unavailable", "component", "context",
					"source", string(g.Source()), "error", err.Error())
				return
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return
			}
			item := Item{Source: g.Source(), Chars: utf8.RuneCountInString(text)}
			// Redact before truncate: a key cut in half is still a leak, and
			// truncating first could hide the header the heuristic keys on.
			if redacted, ok := Redact(text); ok {
				item.Text, item.Redacted = redacted, true
			} else {
				item.Text, item.Truncated = truncate(text, c.maxChars)
			}
			found[i] = &item
		}(i, g)
	}
	wg.Wait()

	snap := Snapshot{At: start, Elapsed: time.Since(start)}
	for _, item := range found {
		if item != nil {
			snap.Items = append(snap.Items, *item)
		}
	}
	// Sizes and flags only. Captured content never reaches the log, at any
	// level — the journal outlives the conversation.
	attrs := []any{"component", "context", "sources", strings.Join(snap.Sources(), ","),
		"duration_ms", snap.Elapsed.Milliseconds()}
	for _, item := range snap.Items {
		attrs = append(attrs, string(item.Source)+"_chars", item.Chars)
		if item.Redacted {
			attrs = append(attrs, string(item.Source)+"_redacted", true)
		}
	}
	c.log.Debug("desktop context gathered", attrs...)
	return snap
}

// truncationMarker is appended to text cut at the per-source cap. It lives
// inside the text rather than beside it so every surface — the model, the
// window, `jarvix status --last` — carries the same admission.
const truncationMarker = "\n[truncated]"

// truncate cuts text to at most maxChars runes, marking the cut. Runes, not
// bytes: a cut through a multi-byte character would hand the model a
// replacement glyph and nobody would ever find out why.
func truncate(text string, maxChars int) (string, bool) {
	if utf8.RuneCountInString(text) <= maxChars {
		return text, false
	}
	kept := 0
	for i := range text {
		if kept == maxChars {
			return text[:i] + truncationMarker, true
		}
		kept++
	}
	return text + truncationMarker, true
}
