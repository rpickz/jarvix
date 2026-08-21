package daemon

import (
	"encoding/json"
	"errors"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

// registerTextMethods adds `session.text`: one typed turn, submitted the way
// the conversation window's composer submits it (issue #35).
//
// It is deliberately not a new session path. It is the pair of calls
// `jarvix ask` already makes — `session.start` then `session.submit {text}` —
// with the decision between them ("is Jarvix waiting on a confirmation?")
// taken daemon-side, under one lock, where it can be tested. A client that
// made both calls itself would have to read the state first and act on it a
// round trip later; in that gap the confirmation it was about to answer can
// time out, and `session.start` would then quietly abandon it.
//
// Privacy: typed text is conversation content, so it is never logged here.
// The engine treats it exactly like a transcript (docs/architecture.md).
func (d *Daemon) registerTextMethods() {
	d.server.Handle("session.text", func(params json.RawMessage) (any, error) {
		var p struct {
			Text string `json:"text"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "session.text params: %v", err)
			}
		}
		result, err := d.engine.SubmitText(p.Text)
		switch {
		case errors.Is(err, session.ErrEmptyText):
			// A params problem, not a session one: nothing was started, nothing
			// was interrupted, and the client should not retry it as-is.
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "session.text: %v", err)
		case err != nil:
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		out := map[string]any{
			"session_id":   result.SessionID,
			"confirmation": result.Confirmation,
		}
		if result.Confirmation {
			out["approved"] = result.Approved
		}
		return out, nil
	})
}
