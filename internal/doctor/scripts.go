package doctor

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/script"
)

// scriptChecks reports one result per configured script ([[scripts]], ADR
// 0030): does the file its phrase would run actually exist, as an absolute
// path, executable — and is the entry itself sound? Detection is a stat and
// nothing else — never an invocation, because doctor must not run the user's
// backup for them.
//
// A problem here is a Fail, not a Warn, unlike a missing advisor CLI: the
// same rules are config validation errors, so the daemon will refuse this
// file at startup — doctor's job is to say which entry and what to do about
// it before that refusal is discovered as a silent assistant.
func scriptChecks(cfg config.Config) []Result {
	if len(cfg.Scripts) == 0 {
		return nil
	}
	// One validation pass over the whole list (uniqueness needs all of it),
	// then each entry claims its own problems by index label — the same
	// labels config validation prints, so the two surfaces cannot disagree.
	defs := cfg.ScriptDefinitions()
	problems := script.Problems(defs)
	results := make([]Result, 0, len(defs))
	for i, def := range defs {
		name := strings.TrimSpace(def.Name)
		if name == "" {
			name = fmt.Sprintf("scripts[%d]", i)
		}
		label := fmt.Sprintf("script %q runnable", name)
		prefix := fmt.Sprintf("scripts[%d]", i)
		var mine []string
		for _, p := range problems {
			if strings.HasPrefix(p, prefix+":") || strings.HasPrefix(p, prefix+" ") {
				mine = append(mine, p)
			}
		}
		if len(mine) > 0 {
			results = append(results, Result{Status: Fail, Name: label,
				Detail: strings.Join(mine, "; "),
				Fix: "Fix the [[scripts]] entry in config.toml — the daemon refuses invalid script\n" +
					"config at startup, so this blocks everything, not just the one phrase."})
			continue
		}
		results = append(results, Result{Status: OK, Name: label,
			Detail: fmt.Sprintf("%s (report %s, timeout %ds)",
				def.Path, def.Report, int(def.Timeout.Seconds()))})
	}
	return results
}
