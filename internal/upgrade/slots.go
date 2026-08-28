package upgrade

// Release slots (ADR 0044). Each build lands whole in its own directory —
// SlotsDir/<version>/{jarvix,jarvixd} — and ~/.local/bin/jarvix{,d} are
// symlinks into the live slot. Switching versions is a symlink flip done as
// create-beside-then-rename, so each binary's path is never missing and
// never half-written; rollback is the same flip pointed at the previous
// slot, no copying on the recovery path. A pre-slot install (regular files
// from `make install`) is adopted into a slot named after its version before
// the first flip, so a rollback target exists from the very first upgrade.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// binaries is the installed pair, always moved together.
var binaries = [2]string{"jarvix", "jarvixd"}

// slot names one installed release: its version string and its directory.
// The zero slot means "no usable previous release".
type slot struct {
	name string
	dir  string
}

// previousSlot identifies the binaries currently serving, as the rollback
// target. Three shapes of BinDir/jarvix:
//   - a symlink into a slot that exists → that slot;
//   - a regular file (`make install` predates slots) → adopt: copy the pair
//     into SlotsDir/<Installed> so a rollback target exists;
//   - missing, or a symlink whose slot is gone → no previous. The upgrade
//     may proceed, but a failed gate then stops loudly instead of rolling
//     back (rollback-of-rollback refusal).
func (u *Upgrader) previousSlot() (slot, error) {
	bin := filepath.Join(u.BinDir, binaries[0])
	fi, err := os.Lstat(bin)
	if err != nil {
		if os.IsNotExist(err) {
			return slot{}, nil
		}
		return slot{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(bin)
		if err != nil {
			return slot{}, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(u.BinDir, target)
		}
		if _, err := os.Stat(target); err != nil {
			fprintf(u.Out, "warning: the current install links to %s, which is missing — no previous release to roll back to\n", target)
			return slot{}, nil
		}
		dir := filepath.Dir(target)
		return slot{name: filepath.Base(dir), dir: dir}, nil
	}
	// A pre-slot install: adopt it.
	dir := filepath.Join(u.SlotsDir, u.Installed)
	for _, b := range binaries {
		if err := copyFile(filepath.Join(u.BinDir, b), filepath.Join(dir, b)); err != nil {
			return slot{}, fmt.Errorf("adopting the current install into a rollback slot: %w", err)
		}
	}
	fprintf(u.Out, "adopted the current install (%s) into the rollback slot %s\n", u.Installed, dir)
	return slot{name: u.Installed, dir: dir}, nil
}

// stage copies the freshly built pair from the checkout's bin/ into the
// version's slot, and returns the slot directory.
func (u *Upgrader) stage(version string) (string, error) {
	dir := filepath.Join(u.SlotsDir, version)
	for _, b := range binaries {
		if err := copyFile(filepath.Join(u.Repo, "bin", b), filepath.Join(dir, b)); err != nil {
			return "", fmt.Errorf("staging the build into %s: %w", dir, err)
		}
	}
	return dir, nil
}

// flip points the BinDir symlinks at slotDir, one atomic rename per binary:
// the new symlink is created beside the old name and renamed over it, so no
// path is ever missing or half-written. This is also the whole rollback
// mechanism — flipping back needs no copying, only the previous slot's
// continued existence.
func (u *Upgrader) flip(slotDir string) error {
	if err := os.MkdirAll(u.BinDir, 0o755); err != nil {
		return err
	}
	for _, b := range binaries {
		tmp := filepath.Join(u.BinDir, "."+b+".upgrade")
		_ = os.Remove(tmp)
		if err := os.Symlink(filepath.Join(slotDir, b), tmp); err != nil {
			return fmt.Errorf("installing the %s symlink: %w", b, err)
		}
		if err := os.Rename(tmp, filepath.Join(u.BinDir, b)); err != nil {
			return fmt.Errorf("installing the %s symlink: %w", b, err)
		}
	}
	return nil
}

// prune keeps the retention promise: the live slot and the previous one
// stay, anything older goes. Only ever called after a green gate — a failed
// upgrade prunes nothing.
func (u *Upgrader) prune(keep ...string) {
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		if k != "" {
			keepSet[k] = true
		}
	}
	entries, err := os.ReadDir(u.SlotsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || keepSet[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(u.SlotsDir, e.Name())); err == nil {
			fprintf(u.Out, "pruned old release %s\n", e.Name())
		}
	}
}

// copyFile copies src to dst (0755 — these are binaries), creating dst's
// directory. Write-then-rename so a crash never leaves a half-copied binary
// under the final name.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
