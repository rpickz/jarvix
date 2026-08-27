package session

import (
	"context"
	"fmt"

	"github.com/rpickz/jarvix/internal/intent"
)

// This file is the engine half of window nicknames (#126): the router
// decides an utterance is "call this window <name>" or "what are my windows
// called", and the engine turns that into one seam call and one spoken
// sentence. Everything about windows — resolution, normalisation, collision
// checks, the release-on-close honesty — lives behind WindowNamer, so
// session tests substitute a fake and never read a desktop.

// WindowNamer is the engine's view of the window-nickname seam, implemented
// by the window tools' shared state (tools.Desktop). Both methods return the
// sentence to speak; err is a spoken-ready refusal ("a nickname is a single
// word …") that intentFailureAck frames as "Sorry, …".
type WindowNamer interface {
	// AssignNickname names the window reference resolves to (empty means the
	// focused window) and returns the spoken confirmation — including the
	// common-word caution when one applies.
	AssignNickname(ctx context.Context, reference, name string) (spoken string, err error)
	// NicknameListing returns the one spoken listing of current nicknames
	// with their windows.
	NicknameListing(ctx context.Context) (spoken string, err error)
}

// runNicknameAssign carries out a matched "call this window <name>" phrase.
// The reference is always the focused window: the deictic phrasing is the
// pattern, and a spoken assignment to some *other* window is a job for the
// model and its desktop.name_window tool, which resolves references the
// user actually said.
func (e *Engine) runNicknameAssign(s *sess, m intent.Match) (ack string, runErr error) {
	if e.opts.WindowNames == nil {
		return "", fmt.Errorf("naming windows is not available on this daemon")
	}
	return e.opts.WindowNames.AssignNickname(s.ctx, "", m.WindowName)
}

// runNicknameList carries out a matched "what are my windows called" phrase.
func (e *Engine) runNicknameList(s *sess) (ack string, runErr error) {
	if e.opts.WindowNames == nil {
		return "", fmt.Errorf("naming windows is not available on this daemon")
	}
	return e.opts.WindowNames.NicknameListing(s.ctx)
}
