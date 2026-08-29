package jobs

import (
	"os"
	"path/filepath"
)

// resolved answers where a path really is, following symlinks as far as the
// filesystem will allow.
//
// It exists because containment is the whole of the filesystem half of a
// scope, and a containment check done on the string a caller handed in is a
// check a symlink walks straight through: `~/code/jobs/out -> /etc` is a path
// that reads as inside the scope and writes outside it.
//
// For a path that does not exist yet — which is most of them, since a job
// writing a new file is the ordinary case — the deepest ANCESTOR that does
// exist is resolved instead and the remaining segments are re-joined onto it.
// That is the right place to look: a file cannot be a symlink before it
// exists, so the only escape available is through one of its directories, and
// the deepest existing directory is exactly where that escape would be.
//
// An unresolvable path comes back cleaned rather than empty. Scope.holds then
// compares it against roots that WERE resolved, so a mismatch reads as "not
// inside", which is the refusing direction and the only safe one.
func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	dir, rest := filepath.Dir(path), filepath.Base(path)
	for dir != "" && dir != string(filepath.Separator) && dir != filepath.Dir(dir) {
		if _, err := os.Lstat(dir); err == nil {
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				return filepath.Join(real, rest)
			}
			return filepath.Join(dir, rest)
		}
		dir, rest = filepath.Dir(dir), filepath.Join(filepath.Base(dir), rest)
	}
	return path
}
