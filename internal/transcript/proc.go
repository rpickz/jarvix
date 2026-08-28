package transcript

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Window-to-directory resolution. A terminal window's process is the
// emulator; the session lives in what the emulator hosts — a shell, the AI
// CLI it launched, and whatever tool children that CLI is running. The
// transcript's location is derived from the directory the CLI was started
// in, so the resolution collects the working directories of the window's
// whole descendant tree and lets the caller try each as a discovery
// candidate.
//
// Ordering is the correctness decision here, not a tidy-up:
//
//   - Shallowest first. The shell sits one level under the emulator and its
//     cwd is where the user launched (and usually still is); the CLI sits
//     under the shell in the same directory. A tool child at the bottom of
//     the tree may be off in a subdirectory — a worktree, a build dir — that
//     hosts its own transcripts for some *other* session, and trying it
//     first would recap the wrong work.
//   - The window process itself last. An emulator's own cwd is typically its
//     launch directory (often $HOME), and $HOME tends to accumulate a stale
//     transcript dir of its own; it is the least credible candidate, not the
//     most.
//
// Everything reads the injected ProcDir, so tests build a procfs shape in a
// tempdir and no test walks the real process table.

// maxProcDescendants bounds the walk. A terminal hosting more processes than
// this is a fork storm, not a session, and an unbounded walk of it would
// spend the recap budget on /proc.
const maxProcDescendants = 64

// candidateCwds returns the unique working directories of pid's descendants,
// shallowest first, with pid's own directory last. Unreadable entries are
// skipped silently: /proc is racy by nature — a process may exit between the
// listing and the readlink — and a vanished child is absence, not failure.
func (f *Finder) candidateCwds(pid int) []string {
	children := f.childrenByParent()
	// Breadth-first from the window process, so depth order falls out of the
	// queue order.
	order := make([]int, 0, maxProcDescendants)
	queue := append([]int(nil), children[pid]...)
	for len(queue) > 0 && len(order) < maxProcDescendants {
		next := queue[0]
		queue = queue[1:]
		order = append(order, next)
		queue = append(queue, children[next]...)
	}
	order = append(order, pid)

	var cwds []string
	seen := make(map[string]bool, len(order))
	for _, p := range order {
		cwd, err := os.Readlink(filepath.Join(f.ProcDir, strconv.Itoa(p), "cwd"))
		if err != nil || cwd == "" || seen[cwd] {
			continue
		}
		seen[cwd] = true
		cwds = append(cwds, cwd)
	}
	return cwds
}

// childrenByParent reads the process table once into a parent-to-children
// index. Children are sorted by pid so the walk is deterministic — pids
// generally rise with spawn order, which puts the long-lived shell before
// the tool child forked a moment ago.
func (f *Finder) childrenByParent() map[int][]int {
	entries, err := os.ReadDir(f.ProcDir)
	if err != nil {
		return nil
	}
	children := make(map[int][]int)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process entry
		}
		ppid, ok := parentOf(f.ProcDir, pid)
		if !ok {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	for _, kids := range children {
		sort.Ints(kids)
	}
	return children
}

// parentOf reads one process's parent pid from its stat line. The comm field
// is parenthesised and may itself contain spaces and parentheses, so the
// parse anchors on the *last* closing parenthesis — the documented way to
// read /proc/<pid>/stat, not an optimisation.
func parentOf(procDir string, pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	line := string(data)
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return 0, false
	}
	// After ") " the fields are: state, ppid, …
	fields := strings.Fields(line[end+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
