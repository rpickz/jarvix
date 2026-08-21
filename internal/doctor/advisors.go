package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
)

// advisorChecks reports one result per configured advisor: is the CLI it
// names actually here? Detection is exec.LookPath (or a stat for an absolute
// path) and nothing else — no invocation, no network — because `jarvix
// doctor` must stay instant and must never spend the user's advisor budget.
//
// A missing advisor is a Warn, not a Fail: delegation is an enhancement, and
// Jarvix answers perfectly well without it. What would be a silent failure is
// the config pointing at a binary that moved, so it is worth saying.
func advisorChecks(cfg config.Config) []Result {
	names := cfg.AdvisorNames()
	if len(names) == 0 {
		return []Result{{Status: OK, Name: "advisors configured",
			Detail: "none — `jarvix setup` records the assistant CLIs it finds on PATH"}}
	}
	results := make([]Result, 0, len(names))
	for _, name := range names {
		a := cfg.Advisors[name]
		label := fmt.Sprintf("advisor %q available", name)
		path, err := lookAdvisor(a.Binary)
		if err != nil {
			results = append(results, Result{Status: Warn, Name: label,
				Detail: fmt.Sprintf("%s not found (%v)", a.Binary, err),
				Fix: fmt.Sprintf("Install %s, point advisors.%s.binary at it, or remove the "+
					"[advisors.%s] table", name, name, name)})
			continue
		}
		detail := path
		if !a.ReadOnly {
			// Worth surfacing: it is the difference between a silent
			// consultation and one that stops to ask the user first.
			detail += " (confirmed before each use)"
		}
		results = append(results, Result{Status: OK, Name: label, Detail: detail})
	}
	return results
}

// lookAdvisor resolves an advisor binary the same way the tool does at call
// time, so doctor's answer is the one the daemon will get.
func lookAdvisor(binary string) (string, error) {
	if strings.TrimSpace(binary) == "" {
		return "", fmt.Errorf("no binary configured")
	}
	if filepath.IsAbs(binary) {
		info, err := os.Stat(binary)
		if err != nil {
			return "", err
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("not executable")
		}
		return binary, nil
	}
	return exec.LookPath(binary)
}
