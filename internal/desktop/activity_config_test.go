package desktop

import (
	"strings"
	"testing"
)

// The self-configuration rows (issue #105): who changed the configuration is
// part of what the feed attests, so the assistant's saves say so while the
// window's keep their original wording — and the settings row carries the
// key and the source, never the value.

func TestEntryChangedRowNamesTheAssistantAsSource(t *testing.T) {
	rows := ActivityRowsFor("config.entry_changed", map[string]any{
		"action": "created", "family": "scripts", "kind": "script",
		"name": "deploy", "source": "assistant",
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Label != "Script created: deploy" {
		t.Errorf("label = %q", rows[0].Label)
	}
	if rows[0].Detail != "config.toml, saved by the assistant" {
		t.Errorf("detail = %q, want the assistant named as the writer", rows[0].Detail)
	}

	// The window's wording is unchanged — including for events from a daemon
	// predating the source field.
	rows = ActivityRowsFor("config.entry_changed", map[string]any{
		"action": "deleted", "family": "routines", "kind": "routine", "name": "morning setup",
	})
	if rows[0].Detail != "config.toml, removed from the window" {
		t.Errorf("window detail = %q", rows[0].Detail)
	}
}

func TestSettingChangedRowCarriesKeyAndSourceNeverTheValue(t *testing.T) {
	rows := ActivityRowsFor("config.setting_changed", map[string]any{
		"key": "tts.kokoro.speed", "source": "assistant",
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Label != "Setting changed: tts.kokoro.speed" {
		t.Errorf("label = %q", rows[0].Label)
	}
	if rows[0].Detail != "config.toml, changed by the assistant" {
		t.Errorf("detail = %q", rows[0].Detail)
	}
	rows = ActivityRowsFor("config.setting_changed", map[string]any{
		"key": "ui.notifications", "source": "user",
	})
	if rows[0].Detail != "config.toml, changed by you" {
		t.Errorf("user detail = %q", rows[0].Detail)
	}
	// Values never travel on the event, so no row can leak one; assert the
	// row also ignores a value should a future event grow it.
	rows = ActivityRowsFor("config.setting_changed", map[string]any{
		"key": "ai.system_prompt", "source": "user", "value": "SALTED-CONTENT",
	})
	if strings.Contains(rows[0].Label+rows[0].Detail, "SALTED") {
		t.Errorf("the row leaked a value: %+v", rows[0])
	}
}
