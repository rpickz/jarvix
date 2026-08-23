// Package script runs user-configured executables behind spoken phrases
// (ADR 0030) — the deliberate revisit of ADR 0026's exclusion, shipped with
// the threat model as its spine rather than an afterthought.
//
// A script is a named `[[scripts]]` entry: trigger phrases, the absolute path
// of an executable the user wrote, a timeout, and a report mode. "Jarvix,
// backup my notes" matches in the deterministic intent router (zero model
// involvement in what runs), faces the permission gate under the `script.run`
// identity (default ask — the confirmation names the script and its path),
// and then this package executes the file with the advisor path's discipline:
// no shell, a scrubbed environment, capped output, and a process-group kill
// on timeout or cancellation.
//
// Three properties are the design, each answering a way speech-triggered
// execution could go wrong:
//
//   - Zero arguments, by construction. The exec call passes the path and
//     nothing else — there is no argv slice, no template, and no environment
//     addition anywhere in this package, so no code path exists by which the
//     transcript (or anything derived from it) could reach the script. Spoken
//     slots are future work with their own validation design, not a gap.
//   - Absolute paths only, verified twice. Config validation (and doctor)
//     refuse a relative, missing, or non-executable path before any phrase is
//     ever spoken, and the runner re-checks at run time — a file that changed
//     underneath a running daemon is a spoken failure, never a surprise exec.
//   - Failures are always audible. The report mode shapes what success says
//     (a summary, stdout's first line, or nothing); a non-zero exit, a
//     timeout, or a file that would not start is spoken in every mode,
//     because "silent" must never mean "silently broken".
package script

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Report names what a successful run says. Failure ignores it: every failure
// is spoken, whatever the mode.
type Report string

// The report modes.
const (
	// ReportSummary speaks one composed sentence: "Backup notes finished."
	ReportSummary Report = "summary"
	// ReportStdout speaks the first line of the script's stdout, capped —
	// the script authors its own acknowledgement.
	ReportStdout Report = "stdout"
	// ReportSilent speaks nothing on success. Failures are still spoken.
	ReportSilent Report = "silent"
)

// ValidReport reports whether s names a report mode.
func ValidReport(s Report) bool {
	switch s {
	case ReportSummary, ReportStdout, ReportSilent:
		return true
	}
	return false
}

// Timeout bounds. The default is generous for a "backup my notes" shape of
// job; the ceiling exists because a script is tied to a live session and a
// phrase must never be able to park one for an afternoon.
const (
	DefaultTimeout = 60 * time.Second
	MaxTimeout     = time.Hour
)

// Definition is one configured script, converted from its [[scripts]] table.
type Definition struct {
	// Name is what the user runs, what the gate's confirmation names, and
	// what every log and event line carries. Unique, case-insensitively.
	Name string
	// Phrases trigger the script through the intent router. Their grammar
	// (and collisions with built-ins, custom intents, and routines) are the
	// router's to validate; this package only requires that some exist.
	Phrases []string
	// Path is the executable to run: absolute, existing, executable. It is
	// the entire argv — v1 scripts take zero arguments, stated in docs and
	// enforced by the shape of the exec call (see run).
	Path string
	// Timeout bounds one run; expiry kills the script's whole process group.
	Timeout time.Duration
	// Report is what success says.
	Report Report
}

// Problems reports everything wrong with the definitions, one actionable
// message per problem, each naming the entry to fix. Phrase grammar and
// collisions are deliberately not checked here — the intent router owns the
// grammar, and configuration compiles the real router as its check.
//
// Unlike routine.Problems this one reads the filesystem: a script file that
// is missing or not executable is refused at load, not discovered by voice
// (the acceptance criterion is literal — "before any phrase is ever spoken").
// That makes a deleted script file a startup error, the same stance taken on
// a misconfigured routine: configuration describes this machine, and an entry
// promising a phrase the machine cannot honour is wrong, not latent.
func Problems(defs []Definition) []string {
	var problems []string
	seen := make(map[string]string, len(defs))
	for i, def := range defs {
		label := fmt.Sprintf("scripts[%d]", i)
		name := strings.TrimSpace(def.Name)
		if name != "" {
			label = fmt.Sprintf("scripts[%d] (%q)", i, name)
		}
		if name == "" {
			problems = append(problems, label+": name is empty; give the script a name to trigger and log under")
		} else if owner, dup := seen[strings.ToLower(name)]; dup {
			problems = append(problems, fmt.Sprintf("%s: name %q is already %s; script names must be unique",
				label, name, owner))
		} else {
			seen[strings.ToLower(name)] = fmt.Sprintf("scripts[%d]", i)
		}
		if len(def.Phrases) == 0 {
			problems = append(problems, label+": it has no phrases; add at least one trigger phrase")
		}
		if problem := pathProblem(def.Path); problem != "" {
			problems = append(problems, label+": "+problem)
		}
		if !ValidReport(def.Report) {
			problems = append(problems, fmt.Sprintf("%s: report %q is not a mode; use %q, %q, or %q",
				label, def.Report, ReportSummary, ReportStdout, ReportSilent))
		}
		if def.Timeout <= 0 || def.Timeout > MaxTimeout {
			problems = append(problems, fmt.Sprintf(
				"%s: timeout_sec must be between 1 and %d; omit it for the default (%d seconds)",
				label, int(MaxTimeout.Seconds()), int(DefaultTimeout.Seconds())))
		}
	}
	return problems
}

// pathProblem checks one script path the way the runner will: absolute,
// present, a regular file, executable. One string per call — the first
// problem is the one to fix, and "missing" makes the others unknowable.
func pathProblem(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "path is empty; set the absolute path of the script to run"
	}
	if !filepath.IsAbs(trimmed) {
		// Absolute-only is a security decision, not pedantry: a bare name
		// resolved on PATH would let whatever shadows it first own the phrase.
		return fmt.Sprintf("path %q is not absolute; scripts are named by full path so a phrase "+
			"can never run whatever happens to be on PATH", trimmed)
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return fmt.Sprintf("path %q does not exist; create it or remove the [[scripts]] entry", trimmed)
	}
	if info.IsDir() {
		return fmt.Sprintf("path %q is a directory, not an executable", trimmed)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Sprintf("path %q is not executable; chmod +x it", trimmed)
	}
	return ""
}
