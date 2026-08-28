package main

// `jarvix upgrade` (issue #139, ADR 0044): the CLI face of
// internal/upgrade, wiring the state machine's seams to the real world —
// real commands, the real XDG paths, this binary's own stamped version.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/rpickz/jarvix/internal/build"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/upgrade"
)

// cmdUpgrade updates the installed pair to origin/main behind the health
// gate, or with check set only reports what an upgrade would do.
func cmdUpgrade(paths config.Paths, check bool) error {
	repo, err := resolveRepo()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	u := &upgrade.Upgrader{
		Repo:      repo,
		BinDir:    filepath.Join(home, ".local", "bin"),
		SlotsDir:  filepath.Join(paths.Data, "releases"),
		LockPath:  filepath.Join(paths.State, "upgrade.lock"),
		Installed: build.Version,
		Run:       upgrade.ExecRun,
		Out:       os.Stdout,
	}
	if check {
		return u.Check(context.Background())
	}
	return u.Upgrade(context.Background())
}

// resolveRepo locates the user's checkout without any new configuration.
// The Omarchy plugin already knows: install-plugin.sh symlinks
// ~/.config/omarchy/plugins/jarvix at <checkout>/plugin/omarchy precisely so
// a pull updates the shell's half, and following that link backwards names
// the checkout. Without the plugin (a daemon-only install), running from
// inside the checkout works too.
func resolveRepo() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	link := filepath.Join(home, ".config", "omarchy", "plugins", "jarvix")
	if target, err := os.Readlink(link); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		if repo := filepath.Dir(filepath.Dir(target)); isJarvixCheckout(repo) {
			return repo, nil
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if isJarvixCheckout(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("cannot find the jarvix checkout: install the Omarchy plugin (make install-plugin) or run jarvix upgrade from inside the checkout")
}

// isJarvixCheckout recognises the checkout by its module line and its .git —
// a directory that merely contains a copy of the sources is not one an
// upgrade may pull in.
func isJarvixCheckout(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(first) == "module github.com/rpickz/jarvix"
}
