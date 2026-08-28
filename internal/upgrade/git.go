package upgrade

// Git state inspection — read-only, and the refusals. The checkout belongs
// to the user: an upgrade may look at anything, fast-forward when asked, and
// must refuse loudly — quoting the exact state — the moment a fast-forward
// would not be a pure catch-up. Never stash, never reset, never checkout.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// repoState is what inspection learned, before anything is decided.
type repoState struct {
	branch    string // current branch ("HEAD" when detached)
	porcelain string // `git status --porcelain`, "" when clean
	ahead     int    // commits on HEAD that origin/main lacks
	behind    int    // commits on origin/main that HEAD lacks
	head      string // HEAD sha before any pull, for the plugin diff
	available string // `git describe` of origin/main — the version on offer
}

// git runs one git command in the checkout and returns its stdout, trailing
// newline stripped. Failures carry git's own stderr: git already said what
// is wrong, and paraphrasing loses the searchable string.
func (u *Upgrader) git(ctx context.Context, args ...string) (string, error) {
	out, errOut, err := u.Run(ctx, u.Repo, "git", args...)
	if err != nil {
		return "", fmt.Errorf("git %s (in %s): %v: %s",
			strings.Join(args, " "), u.Repo, err, strings.TrimSpace(errOut))
	}
	return strings.TrimRight(out, "\n"), nil
}

func (u *Upgrader) gitCount(ctx context.Context, rangeSpec string) (int, error) {
	out, err := u.git(ctx, "rev-list", "--count", rangeSpec)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count %s returned %q, not a number", rangeSpec, out)
	}
	return n, nil
}

// inspect fetches origin (remote refs only) and reads the checkout's state.
// Nothing here writes to the working tree or any local branch.
func (u *Upgrader) inspect(ctx context.Context) (repoState, error) {
	var st repoState
	if _, err := u.git(ctx, "fetch", "--quiet", "origin"); err != nil {
		return st, err
	}
	var err error
	if st.branch, err = u.git(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return st, err
	}
	if st.porcelain, err = u.git(ctx, "status", "--porcelain"); err != nil {
		return st, err
	}
	if st.ahead, err = u.gitCount(ctx, "origin/main..HEAD"); err != nil {
		return st, err
	}
	if st.behind, err = u.gitCount(ctx, "HEAD..origin/main"); err != nil {
		return st, err
	}
	if st.head, err = u.git(ctx, "rev-parse", "HEAD"); err != nil {
		return st, err
	}
	if st.available, err = u.git(ctx, "describe", "--tags", "--always", "origin/main"); err != nil {
		return st, err
	}
	return st, nil
}

// refusal names the reason an upgrade must not proceed, with the exact git
// state, or returns "" when a fast-forward would be a pure catch-up. Dirty
// first: it is the state most likely to hold unsaved work.
func refusal(repo string, st repoState) string {
	if st.porcelain != "" {
		return fmt.Sprintf("the checkout at %s has uncommitted changes — your work is never stashed or reset:\n%s\n"+
			"Commit or stash them yourself, then re-run jarvix upgrade.",
			repo, indent(st.porcelain))
	}
	if st.branch != "main" {
		return fmt.Sprintf("the checkout at %s is on branch %q, not main — switch back yourself, then re-run jarvix upgrade.",
			repo, st.branch)
	}
	if st.ahead > 0 {
		return fmt.Sprintf("branch main at %s is %d commit(s) ahead of origin/main — "+
			"a fast-forward is impossible and your commits are never touched. "+
			"Push or rebase them yourself, then re-run jarvix upgrade.",
			repo, st.ahead)
	}
	return ""
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
