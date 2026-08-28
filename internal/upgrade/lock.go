package upgrade

// The concurrency lock. Two upgrades interleaving their flips and restarts
// could leave the install torn, so one lock file — created O_EXCL, holding
// the owner's pid — serialises them: the second invocation refuses and says
// who holds it. A lock whose owner is dead (crashed mid-upgrade, machine
// rebooted) is stale and taken over, once; a live owner is always respected.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// acquireLock takes the upgrade lock and returns its release. On contention
// with a live owner it refuses with the pid and the lock path.
func (u *Upgrader) acquireLock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(u.LockPath), 0o700); err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(u.LockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := fmt.Fprintf(f, "%d\n", os.Getpid())
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(u.LockPath)
				return nil, fmt.Errorf("writing the upgrade lock %s: %w", u.LockPath, errors.Join(werr, cerr))
			}
			return func() { _ = os.Remove(u.LockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		raw, rerr := os.ReadFile(u.LockPath)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		if rerr == nil && attempt == 0 && (pid <= 0 || !u.alive(pid)) {
			fprintf(u.Out, "removing a stale upgrade lock left behind by dead pid %d\n", pid)
			_ = os.Remove(u.LockPath)
			continue
		}
		return nil, fmt.Errorf("another upgrade is already running (pid %d holds %s) — let it finish", pid, u.LockPath)
	}
}

// alive reports whether pid is a running process. Signal 0 probes without
// signalling; EPERM still means "exists, just not ours".
func (u *Upgrader) alive(pid int) bool {
	if u.Alive != nil {
		return u.Alive(pid)
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
