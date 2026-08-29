package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file is the bridge behind the assistant's self-configuration tools
// (issue #105, ADR 0036): the daemon's implementation of tools.ConfigAdmin.
// Its whole design is that the WRITE methods contain no writing — they
// marshal the tool's arguments into the same params the window's IPC sends
// and invoke the same named handlers (entryAdminUpsert, entryAdminDelete,
// configSet), so the fingerprint guard, the whole-document validation, the
// atomic write, the standard reload, and the events are shared verbatim.
// Zero new write paths is the architecture requirement, and this file is
// where it is visible: change the pipeline and every client moves together.
//
// The reads (list, get, settings view) are re-expressed here rather than
// routed through handlers, because they need shapes the window never asked
// for (typed summaries, a NotFound the tool can distinguish) and a read can
// drift nothing — the parser is the same config.ParseBytes either way.

// assistantConfigAdmin implements tools.ConfigAdmin over one daemon.
type assistantConfigAdmin struct{ d *Daemon }

// translateAdminError converts the handlers' *ipc.Error refusals into the
// structured *tools.ConfigAdminError the tools act on, preserving the
// field-keyed problems exactly — they were built for pinning to form fields,
// and the model reads the same {field, message} pairs as "fix this field".
func translateAdminError(err error) error {
	if err == nil {
		return nil
	}
	var ipcErr *ipc.Error
	if !errors.As(err, &ipcErr) {
		return err
	}
	out := &tools.ConfigAdminError{Message: ipcErr.Message}
	switch ipcErr.Code {
	case ipc.CodeConfigInvalid:
		out.Invalid = true
		out.Problems = adminErrorProblems(ipcErr.Data)
	case ipc.CodeConfigConflict:
		out.Conflict = true
		if data, ok := ipcErr.Data.(map[string]any); ok {
			out.Fingerprint, _ = data["fingerprint"].(string)
		}
	}
	return out
}

// adminErrorProblems extracts {field, message} problems from an error's data,
// whatever Go shape they were attached in — the entry pipeline attaches
// []entryProblem, the settings path []string — via one JSON round trip, the
// same normalisation the wire would apply.
func adminErrorProblems(data any) []tools.ConfigProblem {
	container, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(container["problems"])
	if err != nil {
		return nil
	}
	var structured []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &structured); err == nil {
		out := make([]tools.ConfigProblem, 0, len(structured))
		for _, p := range structured {
			out = append(out, tools.ConfigProblem{Field: p.Field, Message: p.Message})
		}
		return out
	}
	var flat []string
	if err := json.Unmarshal(raw, &flat); err == nil {
		out := make([]tools.ConfigProblem, 0, len(flat))
		for _, msg := range flat {
			out = append(out, tools.ConfigProblem{Message: msg})
		}
		return out
	}
	return nil
}

// ListEntries implements tools.ConfigAdmin: one family's entries from the
// file, summarised. The file, not the running config, because the entry
// verbs edit the file — a listing that disagreed with what get/upsert see
// would send the model editing entries that "aren't there".
func (a *assistantConfigAdmin) ListEntries(family string) ([]tools.ConfigEntrySummary, error) {
	if _, ipcErr := assistantEntryFamily(family); ipcErr != nil {
		return nil, translateAdminError(ipcErr)
	}
	raw, err := os.ReadFile(a.d.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	cfg, err := config.ParseBytes(raw)
	if err != nil {
		return nil, err
	}
	switch family {
	case "routines":
		out := make([]tools.ConfigEntrySummary, 0, len(cfg.Routines))
		for _, r := range cfg.Routines {
			out = append(out, tools.ConfigEntrySummary{
				Name: r.Name, Enabled: r.IsEnabled(),
				Phrases: append([]string(nil), r.Phrases...), Schedule: r.Schedule,
			})
		}
		return out, nil
	case "scripts":
		out := make([]tools.ConfigEntrySummary, 0, len(cfg.Scripts))
		for _, s := range cfg.Scripts {
			out = append(out, tools.ConfigEntrySummary{
				Name: s.Name, Enabled: s.IsEnabled(),
				Phrases: append([]string(nil), s.Phrases...), Schedule: s.Schedule,
				Path: s.Path,
			})
		}
		return out, nil
	case "knowledge.feeds":
		out := make([]tools.ConfigEntrySummary, 0, len(cfg.Knowledge.Feeds))
		for _, f := range cfg.Knowledge.Feeds {
			out = append(out, tools.ConfigEntrySummary{
				Name: f.Name, Enabled: f.IsEnabled(),
				Description: f.Description, Command: append([]string(nil), f.Command...),
			})
		}
		return out, nil
	}
	// entryFamily above admits only the three cases; belt and braces.
	return nil, nil
}

// GetEntry implements tools.ConfigAdmin: entryAdminGet's read, with absence
// as a typed NotFound the tool can word ("no routine is named…") instead of
// a generic error.
func (a *assistantConfigAdmin) GetEntry(family, name string) (tools.ConfigEntry, error) {
	spec, ipcErr := assistantEntryFamily(family)
	if ipcErr != nil {
		return tools.ConfigEntry{}, translateAdminError(ipcErr)
	}
	raw, err := os.ReadFile(a.d.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return tools.ConfigEntry{}, err
	}
	entry, found, err := config.EntryValue(raw, spec.family, spec.identity(), name)
	if err != nil {
		return tools.ConfigEntry{}, err
	}
	if !found {
		return tools.ConfigEntry{}, &tools.ConfigAdminError{
			NotFound: true,
			Message:  "no [[" + spec.family + "]] entry is named " + strings.TrimSpace(name),
		}
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	// The entry's canonical name comes from the entry itself (matching is
	// case-insensitive, so the asked-for spelling may differ).
	canonical, _ := entry["name"].(string)
	if strings.TrimSpace(canonical) == "" {
		canonical = name
	}
	return tools.ConfigEntry{Family: spec.family, Name: canonical, Entry: entry, Fingerprint: fp}, nil
}

// Path implements tools.ConfigAdmin: the file every one of these verbs
// writes. It is what the account snapshots before a write, so "what would
// restore this" is the previous bytes of the real file rather than a guess
// (#201, ADR 0064).
func (a *assistantConfigAdmin) Path() string { return a.d.paths.ConfigFile() }

// UpsertEntry implements tools.ConfigAdmin by invoking the window's own
// config.upsert_entry handler with the assistant source — the shared
// pipeline end to end (shape whitelist, whole-document validation,
// fingerprint guard, atomic write, reload, events).
func (a *assistantConfigAdmin) UpsertEntry(family, name string, entry map[string]any,
	fingerprint string) (tools.ConfigWriteReceipt, error) {
	// The exclusion wall, re-checked here for the reason WriteSetting states:
	// this bridge is the last code before the shared write path, so even a
	// tool bug cannot hand an [ai] or [advisors] family through (#163).
	if _, ipcErr := assistantEntryFamily(family); ipcErr != nil {
		return tools.ConfigWriteReceipt{}, translateAdminError(ipcErr)
	}
	params, err := json.Marshal(map[string]any{
		"family": family, "name": name, "entry": entry, "fingerprint": fingerprint,
	})
	if err != nil {
		return tools.ConfigWriteReceipt{}, err
	}
	result, err := a.d.entryAdminUpsert(params, entrySourceAssistant)
	if err != nil {
		return tools.ConfigWriteReceipt{}, translateAdminError(err)
	}
	return a.entryReceipt(result), nil
}

// DeleteEntry implements tools.ConfigAdmin by invoking the window's own
// config.delete_entry handler with the assistant source.
func (a *assistantConfigAdmin) DeleteEntry(family, name, fingerprint string) (tools.ConfigWriteReceipt, error) {
	if _, ipcErr := assistantEntryFamily(family); ipcErr != nil {
		return tools.ConfigWriteReceipt{}, translateAdminError(ipcErr)
	}
	params, err := json.Marshal(map[string]any{
		"family": family, "name": name, "fingerprint": fingerprint,
	})
	if err != nil {
		return tools.ConfigWriteReceipt{}, err
	}
	result, err := a.d.entryAdminDelete(params, entrySourceAssistant)
	if err != nil {
		return tools.ConfigWriteReceipt{}, translateAdminError(err)
	}
	return a.entryReceipt(result), nil
}

// entryReceipt reads the handlers' result map into the tools' receipt, and
// finishes the apply story for the assistant's situation: an assistant write
// is by construction MID-session (it is a tool call), which is exactly when
// the engine refuses to reconfigure — so a not-applied result arms the same
// post-session reload a layout capture uses (#62), and the reason is reworded
// to the truth the model should speak: the change lands the moment this
// exchange ends, not "run config.reload". Reasons that are not the
// session-busy refusal (the first feed's restart boundary) pass through
// untouched — deferring is harmless there, and the honest wording is the
// daemon's own.
func (a *assistantConfigAdmin) entryReceipt(result map[string]any) tools.ConfigWriteReceipt {
	receipt := tools.ConfigWriteReceipt{}
	receipt.Created, _ = result["created"].(bool)
	receipt.Applied, _ = result["applied"].(bool)
	receipt.Reason, _ = result["reason"].(string)
	if !receipt.Applied {
		a.deferReloadToSessionEnd()
		if strings.Contains(receipt.Reason, "session is active") {
			receipt.Reason = "it takes effect the moment this exchange ends"
		}
	}
	return receipt
}

// deferReloadToSessionEnd arms the post-session reload (the captureReload
// mechanism, shared with #62's layout capture): the session watcher re-reads
// config.toml on session.finished and rebuilds the engine's collaborators,
// which is what makes an assistant-written phrase or setting live by the
// time the user can next speak it.
func (a *assistantConfigAdmin) deferReloadToSessionEnd() {
	a.d.cfgMu.Lock()
	a.d.captureReload = true
	a.d.cfgMu.Unlock()
}

// Settings implements tools.ConfigAdmin: the assistant's pruned registry view
// (config.AssistantSettings — the [ai] space never enters it) with running
// values, the same values config.get reports, because "what the daemon
// actually does" is what a spoken answer about a setting must state.
func (a *assistantConfigAdmin) Settings() []tools.ConfigSettingView {
	running := a.d.runningConfig().Redact()
	settings := config.AssistantSettings()
	out := make([]tools.ConfigSettingView, 0, len(settings))
	for _, s := range settings {
		out = append(out, tools.ConfigSettingView{
			Key:       s.Key,
			Label:     s.Label,
			Type:      string(s.Type),
			Value:     s.Get(running),
			Enum:      append([]string(nil), s.Enum...),
			Reload:    string(s.Reload),
			Dangerous: s.Dangerous,
		})
	}
	return out
}

// WriteSetting implements tools.ConfigAdmin by invoking the settings screen's
// own config.set path with the assistant source. The excluded-space check
// runs here as well as in the tool: this bridge is the last code before the
// shared write path, so even a tool bug cannot hand an [ai] key through.
func (a *assistantConfigAdmin) WriteSetting(key string, value any) (tools.ConfigSettingReceipt, error) {
	if reason, excluded := config.AssistantExcludedSettingReason(key); excluded {
		return tools.ConfigSettingReceipt{}, &tools.ConfigAdminError{Message: reason}
	}
	setting, known := config.SettingFor(key)
	if !known {
		return tools.ConfigSettingReceipt{}, &tools.ConfigAdminError{
			NotFound: true, Message: "unknown setting " + key,
		}
	}
	params, err := json.Marshal(map[string]any{"changes": map[string]any{key: value}})
	if err != nil {
		return tools.ConfigSettingReceipt{}, err
	}
	result, err := a.d.configSet(params, settingSourceAssistant)
	if err != nil {
		return tools.ConfigSettingReceipt{}, translateAdminError(err)
	}
	receipt := tools.ConfigSettingReceipt{}
	receipt.Applied, _ = result["applied"].(bool)
	receipt.Reason, _ = result["reason"].(string)
	if !receipt.Applied {
		// Same deferral as the entry writes (entryReceipt): a settings tool
		// call is mid-session by construction, so the standard reload it
		// needs is armed for session end.
		a.deferReloadToSessionEnd()
		if strings.Contains(receipt.Reason, "session is active") {
			receipt.Reason = "it takes effect the moment this exchange ends"
		}
	}
	if pending, ok := result["needs_restart"].([]string); ok {
		for _, k := range pending {
			if k == key {
				receipt.NeedsRestart = true
			}
		}
	}
	// The confirmed value is read back from the file the write landed in —
	// never echoed from the request — so the spoken confirmation states what
	// was actually saved, and a restart-class setting is reported from the
	// file its restart will read.
	raw, rerr := os.ReadFile(a.d.paths.ConfigFile())
	if rerr != nil && !os.IsNotExist(rerr) {
		return receipt, nil
	}
	fileCfg, perr := config.ParseBytes(raw)
	if perr != nil {
		return receipt, nil
	}
	receipt.Value = setting.Get(fileCfg.Redact())
	return receipt, nil
}

// ExcludedSetting implements tools.ConfigAdmin: the registry's own exclusion
// wall, so the tool's refusal and the bridge's re-check can never disagree.
func (a *assistantConfigAdmin) ExcludedSetting(key string) (string, bool) {
	return config.AssistantExcludedSettingReason(key)
}
