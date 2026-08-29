package desktop

import (
	"os"
	"strings"
	"testing"
)

// The conversation window must be *recreated*, never merely re-shown, after
// the compositor destroys its toplevel (issue #106).
//
// Why is pinned in the comment block above the LazyLoader in
// JarvixOverlay.qml, but the short form: Quickshell's FloatingWindow writes
// `visible` into a change-callback property (bWantsVisible) and reads it from
// the backing QWindow. A compositor kill (super+W / killactive) turns the
// QWindow invisible through plain Qt without resetting bWantsVisible, so
// every later `visible = true` is a same-value write that does nothing — the
// window can never be opened again without a shell restart. The fix is to
// discard the killed instance on FloatingWindow's closed() signal (emitted
// only for external closes) and build a fresh one, which is what the
// LazyLoader + onClosed arrangement does.
//
// This is a text scan because that is all a Go test can do to QML (see
// composer_test.go). It watches for the three load-bearing pieces: the window
// lives under a LazyLoader (so an instance can be discarded), the closed()
// signal is handled (so a kill is detected), and the IPC entry points reach
// the window through the reviving accessor rather than a fixed object id
// (so no path can poke a dead instance).
//
// Kept after the QML suite landed (#174). This is the one guard the runner
// could not approach even in principle: the wedge is a Quickshell
// bWantsVisible state reached by a *compositor* closing a mapped toplevel,
// and the headless suite has neither a compositor nor a real window. The
// accessor's call sites are what survives the kill, and counting them is a
// source-level question.
func TestConversationWindowIsRecreatedAfterCompositorKill(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixOverlay.qml"))
	if err != nil {
		t.Fatalf("reading JarvixOverlay.qml: %v", err)
	}
	qml := string(source)

	if !strings.Contains(qml, "LazyLoader {") {
		t.Error("JarvixOverlay.qml no longer owns the conversation window through a " +
			"LazyLoader; a directly declared window cannot be recreated after a " +
			"compositor kill and wedges until shell restart (issue #106)")
	}
	if !strings.Contains(qml, "onClosed:") {
		t.Error("JarvixOverlay.qml no longer handles the window's closed() signal; " +
			"a compositor kill goes undetected and every later open is swallowed " +
			"(issue #106)")
	}
	// Each of openWindow, closeWindow, toggleWindow, openSettings, plus the
	// shell-contract open()/close(), must converge on the reviving accessor.
	if got := strings.Count(qml, "conversationWindow()"); got < 6 {
		t.Errorf("conversationWindow() call sites = %d, want >= 6 (the accessor is "+
			"how every entry point survives a killed window; addressing the window "+
			"object directly reintroduces the wedge)", got)
	}
}
