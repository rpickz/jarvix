package doctor

import (
	"fmt"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
)

// checkConversationSearch reports whether searching past conversations can
// work here (issue #59).
//
// The distinction it draws is state versus damage. Retention off with an
// empty archive means search is *inactive by choice* — an OK that says so
// next to the switch, exactly how the desktop-context check treats a
// disabled feature — while an archive directory that cannot be read, or
// records inside it that cannot be, is a Warn with the fix. Never a Fail:
// search degrades to "nothing found", and the assistant is otherwise
// untouched.
func checkConversationSearch(cfg config.Config, paths config.Paths) Result {
	store := &conversations.FileStore{Dir: paths.ConversationsDir()}
	metas, unreadable, err := store.List()
	if err != nil {
		return Result{Status: Warn, Name: "conversation search",
			Detail: err.Error(),
			Fix:    "Check permissions on " + paths.ConversationsDir()}
	}
	retentionOff := cfg.Conversation.Retention == config.RetentionOff
	if retentionOff && len(metas) == 0 && len(unreadable) == 0 {
		return Result{Status: OK, Name: "conversation search",
			Detail: "inactive — retention is off and nothing is archived (conversation.retention)"}
	}
	detail := fmt.Sprintf("%d conversation(s) searchable", len(metas))
	if retentionOff {
		detail += "; retention is off, so new conversations are not being added"
	}
	if len(unreadable) > 0 {
		return Result{Status: Warn, Name: "conversation search",
			Detail: fmt.Sprintf("%s, %d unreadable", detail, len(unreadable)),
			Fix: "Run: jarvix conversations list — the unreadable records are named there,\n" +
				"and jarvix conversations delete <id> removes one for good"}
	}
	return Result{Status: OK, Name: "conversation search", Detail: detail}
}
