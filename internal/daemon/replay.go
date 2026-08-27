package daemon

// The speak-again surface (issue #122): one verb, `speech.replay`, and the
// activity row that records each use. The window's per-message control and
// `jarvix say-again` are thin clients of the verb; every decision — which
// text, whose speech wins, what supersedes what — is made in the engine
// (session/replay.go), where it is tested.

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

// registerReplayMethods adds `speech.replay`: speak one recorded turn of the
// live conversation again, addressed by its 1-based position in the
// conversation.get snapshot (turn 0 / absent means the newest assistant
// turn). The daemon resolves the text from its own record — a client sends an
// address, never text for the voice to read — and `role`, when sent, guards a
// stale address: a mismatch refuses rather than reading out the wrong turn.
func (d *Daemon) registerReplayMethods() {
	d.server.Handle("speech.replay", func(params json.RawMessage) (any, error) {
		p := struct {
			Turn int    `json:"turn"`
			Role string `json:"role"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "speech.replay params: %v", err)
			}
		}
		turn, role, err := d.engine.ReplaySpeech(p.Turn, p.Role)
		switch {
		case errors.Is(err, session.ErrReplayBusy):
			// Live conversation speech wins (the #122 precedence, pinned in
			// the engine): a session-level "wait", not a params problem.
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		case err != nil:
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"turn": turn, "role": role}, nil
	})
}

// replayActivityRow renders the feed's row for one speech.replayed event.
// Worded here, beside the verb that causes it, rather than in
// internal/desktop's event vocabulary: the replay verb, its event, and its
// row ship as one daemon unit, and the wording is composed and tested
// daemon-side exactly as the feed requires (ADR 0013) — the window renders it
// verbatim either way. The kind reuses the assistant glyph: a replay is
// Jarvix's own voice, re-reading its record.
func replayActivityRow(data map[string]any) desktop.ActivityRow {
	turn := 0
	switch v := data["turn"].(type) {
	case int:
		turn = v
	case float64:
		turn = int(v)
	}
	detail := ""
	switch data["role"] {
	case "user":
		detail = "your message, read back"
	case "assistant":
		detail = "Jarvix's message"
	case "confirmation":
		detail = "a permission question"
	}
	return desktop.ActivityRow{
		Kind:   desktop.ActivityKindAssistant,
		Label:  fmt.Sprintf("Spoke turn %d again", turn),
		Detail: detail,
	}
}
