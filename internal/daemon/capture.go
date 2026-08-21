package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/routine"
	"github.com/rpickz/jarvix/internal/session"
)

// This file is the capture service behind "save this as <name>" (#62): the
// composition of the pure snapshot (internal/routine), the comment-preserving
// config writer (internal/config, ADR 0015), and the daemon's runtime. The
// split the session engine sees — Plan, then Commit — exists so the replace
// confirmation (ADR 0014) sits between reading the desktop and writing the
// file, with nothing written until the user has answered.

// layoutCapturer implements session.RoutineCapturer against the real
// compositor and config file. Every collaborator is a field so tests build
// one against a FakeCompositor, a temp dir, and a canned clock — capture
// tests must never read the developer's desktop or write their config.
type layoutCapturer struct {
	paths      config.Paths
	compositor desktop.Compositor
	log        *slog.Logger
	// lookPath resolves candidate launch commands (exec.LookPath in
	// production); now dates the provenance comment. Both injectable.
	lookPath func(string) (string, error)
	now      func() time.Time
	// committed runs after a successful write with the file's new routine
	// tables, late-bound to the daemon so the running config and the
	// engine's router catch up (captureCommitted). Nil is a no-op.
	committed func(routines []config.Routine)
}

// newLayoutCapturer builds the production capture service.
func newLayoutCapturer(paths config.Paths, compositor desktop.Compositor, logger *slog.Logger) *layoutCapturer {
	if logger == nil {
		logger = slog.Default()
	}
	return &layoutCapturer{
		paths: paths, compositor: compositor, log: logger,
		lookPath: exec.LookPath, now: time.Now,
	}
}

// capturePlan is one prepared capture: the snapshot already derived, the
// question (if any) not yet asked, the file not yet touched.
type capturePlan struct {
	c    *layoutCapturer
	name string
	snap routine.Capture
	// exists records whether the config file held an entry of this name at
	// plan time — the replace question's trigger, and Commit's proof that an
	// approval covers what is about to be overwritten.
	exists bool
}

// Plan implements session.RoutineCapturer. Read-only by construction: one
// inventory read through the seam (which has no verb this function calls
// that could move a window), one config read, no writes.
func (c *layoutCapturer) Plan(ctx context.Context, name string) (session.CapturePlan, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("I did not catch a name to save this as")
	}
	callCtx, cancel := context.WithTimeout(ctx, desktop.DefaultCompositorTimeout)
	windows, err := c.compositor.Windows(callCtx)
	cancel()
	if err != nil {
		return nil, errors.New("I cannot reach the window manager")
	}
	snap := routine.Snapshot(name, windows, routine.CaptureOptions{LookPath: c.lookPath})
	if snap.Kept == 0 {
		return nil, errors.New("there is nothing on this desktop to save")
	}

	_, fileCfg, err := c.readConfig()
	if err != nil {
		return nil, err
	}
	existing := findRoutine(fileCfg, name)
	entry := captureEntry(snap, existing)
	// Validate the candidate before anything is asked or written, compiling
	// the real router: a name whose phrase collides with a built-in intent,
	// a custom intent, or another routine must be one spoken sentence now,
	// not a config file that refuses to load later.
	if err := c.validateCandidate(fileCfg, entry, existing != nil); err != nil {
		return nil, err
	}
	return &capturePlan{c: c, name: name, snap: snap, exists: existing != nil}, nil
}

// ReplaceQuestion implements session.CapturePlan.
func (p *capturePlan) ReplaceQuestion() (string, bool) {
	if !p.exists {
		return "", false
	}
	return fmt.Sprintf("A routine called %s already exists. Should I replace it with this layout?", p.name), true
}

// Commit implements session.CapturePlan: write through the comment-preserving
// rewrite, atomically, and say what was kept. The file is re-read here rather
// than carried from Plan — the replace question waits up to thirty seconds,
// and a config.set or hand edit in that window must be rebased onto, never
// clobbered (the same rule handleConfigSet applies).
func (p *capturePlan) Commit(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c := p.c
	raw, fileCfg, err := c.readConfig()
	if err != nil {
		return "", err
	}
	existing := findRoutine(fileCfg, p.name)
	if existing != nil && !p.exists {
		// An entry of this name appeared between the plan and now, and nobody
		// was asked about replacing it. Refusing is the only honest move.
		return "", fmt.Errorf("a routine called %s appeared in config.toml while we spoke, "+
			"so I have not overwritten it; say it again to replace it", p.name)
	}
	entry := captureEntry(p.snap, existing)
	if err := c.validateCandidate(fileCfg, entry, existing != nil); err != nil {
		return "", err
	}

	provenance := "captured " + c.now().Format("2006-01-02")
	rewritten, err := config.UpsertRoutineTOML(raw, entry, provenance, p.snap.Notes)
	if err != nil {
		c.log.Error("capture rewrite failed; config untouched", "component", "routine",
			"routine", p.name, "error", err.Error())
		return "", errors.New("I could not rewrite config.toml, so nothing was saved")
	}
	if err := config.WriteFileAtomic(c.paths.ConfigFile(), rewritten); err != nil {
		c.log.Error("capture write failed; config untouched", "component", "routine",
			"routine", p.name, "error", err.Error())
		return "", errors.New("I could not write config.toml, so nothing was saved")
	}

	// Counts and workspaces only — never titles — matching what the spoken
	// confirmation is allowed to say.
	c.log.Info("layout captured", "component", "routine", "routine", p.name,
		"windows", p.snap.Kept, "workspaces", p.snap.Workspaces,
		"excluded", p.snap.Excluded, "placeholders", len(p.snap.Placeholders),
		"replaced", existing != nil)
	if c.committed != nil {
		final, err := config.ParseBytes(rewritten)
		if err == nil {
			c.committed(final.Routines)
		}
	}
	return captureSpoken(p.name, p.snap), nil
}

// readConfig reads and parses config.toml. A missing file is an empty
// document — the entry becomes its first table.
func (c *layoutCapturer) readConfig() ([]byte, config.Config, error) {
	raw, err := os.ReadFile(c.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return nil, config.Config{}, errors.New("I could not read config.toml, so nothing was saved")
	}
	fileCfg, err := config.ParseBytes(raw)
	if err != nil {
		return nil, config.Config{}, errors.New("config.toml does not parse; fix it by hand first")
	}
	return raw, fileCfg, nil
}

// validateCandidate checks the configuration that would exist after the
// write, so every rule that guards a hand edit guards a capture too.
func (c *layoutCapturer) validateCandidate(fileCfg config.Config, entry config.Routine, replaces bool) error {
	candidate := fileCfg
	candidate.Routines = append([]config.Routine{}, fileCfg.Routines...)
	if replaces {
		for i := range candidate.Routines {
			if strings.EqualFold(strings.TrimSpace(candidate.Routines[i].Name), strings.TrimSpace(entry.Name)) {
				candidate.Routines[i] = entry
			}
		}
	} else {
		candidate.Routines = append(candidate.Routines, entry)
	}
	candidate.Voices = candidate.InstalledVoices(c.paths)
	if err := candidate.Validate(); err != nil {
		c.log.Info("capture rejected by validation", "component", "routine",
			"routine", entry.Name, "problems", strings.Join(validationProblems(err), "; "))
		return fmt.Errorf("I cannot save it as %s — that name collides with a phrase "+
			"that already triggers something", entry.Name)
	}
	return nil
}

// captureEntry converts the snapshot for writing. Replacing keeps the old
// entry's trigger phrases: the user may have curated them, the name they
// spoke is unchanged, and "wholesale replace" is about the steps — throwing
// away working triggers would punish the confirmation they just gave.
func captureEntry(snap routine.Capture, existing *config.Routine) config.Routine {
	entry := config.RoutineFromDefinition(snap.Definition)
	if existing != nil && len(existing.Phrases) > 0 {
		entry.Phrases = append([]string(nil), existing.Phrases...)
	}
	return entry
}

// findRoutine returns the config entry matching name case-insensitively, the
// same looseness the runner and routines.run apply, or nil.
func findRoutine(cfg config.Config, name string) *config.Routine {
	want := strings.ToLower(strings.TrimSpace(name))
	for i := range cfg.Routines {
		if strings.ToLower(strings.TrimSpace(cfg.Routines[i].Name)) == want {
			return &cfg.Routines[i]
		}
	}
	return nil
}

// captureSpoken composes the one confirmation sentence: what was kept, where
// it lives, and — when derivation fell short — what still needs a human
// hand, by app name and in the same breath. Counts only ever cover kept
// windows; the excluded ones were never the user's layout.
func captureSpoken(name string, snap routine.Capture) string {
	var b strings.Builder
	if snap.Kept == 1 {
		b.WriteString("One window")
	} else {
		b.WriteString(capitaliseFirst(intent.SpokenNumber(snap.Kept)) + " windows")
	}
	if snap.Workspaces == 1 {
		b.WriteString(" on one workspace")
	} else {
		b.WriteString(" across " + intent.SpokenNumber(snap.Workspaces) + " workspaces")
	}
	b.WriteString(", saved as " + name + ".")
	if n := len(snap.Placeholders); n > 0 {
		if n == 1 {
			b.WriteString(" One of them needs a hand: I could not work out how to launch " +
				snap.Placeholders[0] + " — config.toml marks it.")
		} else {
			fmt.Fprintf(&b, " %s of them need a hand: I could not work out how to launch %s — config.toml marks them.",
				capitaliseFirst(intent.SpokenNumber(n)), strings.Join(snap.Placeholders, " or "))
		}
	}
	return b.String()
}

// capitaliseFirst upper-cases the first letter so a spoken number can open a
// sentence.
func capitaliseFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// captureCommitted is the capture service's post-write hook. The running
// config's tables catch up immediately — routines.list and routines.run must
// see the new entry the moment the confirmation is spoken — but the engine's
// router cannot be rebuilt under the very session that spoke the capture, so
// that half is flagged for the session watcher to finish (applyCapturedRoutines).
func (d *Daemon) captureCommitted(routines []config.Routine) {
	d.cfgMu.Lock()
	d.cfg.Routines = routines
	d.captureReload = true
	d.cfgMu.Unlock()
}

// consumeCaptureReload claims a pending capture reload, if one is flagged.
func (d *Daemon) consumeCaptureReload() bool {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	pending := d.captureReload
	d.captureReload = false
	return pending
}

// applyCapturedRoutines re-reads config.toml and rebuilds the engine's
// collaborators so a just-captured routine's phrase compiles into the router
// — the step that makes "saved as morning setup" mean "and you can say
// morning setup now". Runs from the session watcher after session.finished,
// because Reconfigure refuses under an active session; if a new session
// already started, the flag re-arms and the next finish tries again. Failure
// is a log line, never fatal: the file is written and valid, and a restart
// or config.reload picks it up regardless.
func (d *Daemon) applyCapturedRoutines() {
	raw, err := os.ReadFile(d.paths.ConfigFile())
	if err != nil {
		d.log.Warn("captured routine not applied; config unreadable",
			"component", "routine", "error", err.Error())
		return
	}
	fileCfg, err := config.ParseBytes(raw)
	if err != nil {
		d.log.Warn("captured routine not applied; config does not parse",
			"component", "routine", "error", err.Error())
		return
	}
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := fileCfg.Validate(); err != nil {
		d.log.Warn("captured routine not applied; config failed validation",
			"component", "routine", "error", err.Error())
		return
	}
	applied, reason := d.applyRuntime(fileCfg)
	if !applied {
		d.cfgMu.Lock()
		d.captureReload = true
		d.cfgMu.Unlock()
		d.log.Debug("captured routine reload deferred", "component", "routine", "reason", reason)
		return
	}
	d.publishConfigChanged(config.Fingerprint(raw))
	d.log.Info("captured routines applied", "component", "routine")
}
