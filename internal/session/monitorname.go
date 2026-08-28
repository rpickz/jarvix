package session

import (
	"context"
	"fmt"

	"github.com/rpickz/jarvix/internal/intent"
)

// This file is the engine half of monitor nicknames (#180), and it is
// nickname.go with the noun changed — on purpose. The router decides an
// utterance is "call this monitor top", "forget the monitor called top" or
// "what are my screens called"; the engine turns that into one seam call and
// one spoken sentence, and everything about screens lives behind
// MonitorNamer so session tests substitute a fake and never read a compositor.

// MonitorNamer is the engine's view of the monitor-nickname seam, implemented
// by the window tools' shared state (tools.Desktop). Every method returns the
// sentence to speak; err is a spoken-ready refusal ("top is already the name
// of …") that intentFailureAck frames as "Sorry, …".
type MonitorNamer interface {
	// AssignMonitorNickname names the screen connector identifies — empty
	// means the one holding focus, which is what "this monitor" means.
	AssignMonitorNickname(ctx context.Context, name, connector string) (spoken string, err error)
	// ForgetMonitorNickname drops a screen name, and says so when nothing
	// answered to it.
	ForgetMonitorNickname(ctx context.Context, name string) (spoken string, err error)
	// MonitorNicknameListing returns the one spoken listing of screen names,
	// saying which of them are not plugged in right now.
	MonitorNicknameListing(ctx context.Context) (spoken string, err error)
}

// runMonitorNameAssign carries out a matched "call this monitor <name>"
// phrase. The screen is always the focused one: the deictic phrasing is the
// pattern, and naming some *other* screen is a job for the window's form,
// which shows every output and lets the user point at one.
func (e *Engine) runMonitorNameAssign(s *sess, m intent.Match) (ack string, runErr error) {
	if e.opts.MonitorNames == nil {
		return "", fmt.Errorf("naming screens is not available on this daemon")
	}
	return e.opts.MonitorNames.AssignMonitorNickname(s.ctx, m.MonitorName, "")
}

// runMonitorNameForget carries out a matched "forget the monitor called
// <name>" phrase.
func (e *Engine) runMonitorNameForget(s *sess, m intent.Match) (ack string, runErr error) {
	if e.opts.MonitorNames == nil {
		return "", fmt.Errorf("naming screens is not available on this daemon")
	}
	return e.opts.MonitorNames.ForgetMonitorNickname(s.ctx, m.MonitorForget)
}

// runMonitorNameList carries out a matched "what are my screens called".
func (e *Engine) runMonitorNameList(s *sess) (ack string, runErr error) {
	if e.opts.MonitorNames == nil {
		return "", fmt.Errorf("naming screens is not available on this daemon")
	}
	return e.opts.MonitorNames.MonitorNicknameListing(s.ctx)
}
