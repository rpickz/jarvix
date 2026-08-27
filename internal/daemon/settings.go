package daemon

// This file is the settings IPC surface: config.get / config.set /
// config.reload / doctor.get. All settings intelligence lives here in the
// daemon — the settings screen and the CLI are thin clients of these
// methods, so the whole feature is testable without a GUI (ADR 0015).

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/doctor"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

// runningConfig snapshots the configuration the daemon is operating with.
// Restart-class settings keep their booted values here even after the file
// changes, so config.get always reports what the daemon actually does.
func (d *Daemon) runningConfig() config.Config {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	return d.cfg
}

// notificationsEnabled reads the live ui.notifications switch.
func (d *Daemon) notificationsEnabled() bool {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	return d.cfg.UI.Notifications
}

// previewEnabled reads the live ui.notification_preview switch.
func (d *Daemon) previewEnabled() bool {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	return d.cfg.UI.NotificationPreview
}

// Setting-change sources, mirroring the entry-change sources in
// entry_admin.go: config.set over the socket is the settings screen or the
// CLI (settingSourceUser — a person, either way), while the assistant's
// config.write_setting tool (issue #105) lands with its own label so the
// activity feed can say who changed what.
const (
	settingSourceUser      = "user"
	settingSourceAssistant = "assistant"
)

func (d *Daemon) registerConfigMethods() {
	d.server.Handle("config.get", d.handleConfigGet)
	d.server.Handle("config.set", func(params json.RawMessage) (any, error) {
		return d.configSet(params, settingSourceUser)
	})
	d.server.Handle("config.reload", d.handleConfigReload)
	d.server.Handle("doctor.get", d.handleDoctorGet)
}

// handleConfigGet reports the editable settings with their running values and
// reload classes, the config file's fingerprint (for external-edit detection
// at config.set time), and secret *presence* — key values never travel over
// IPC, in either direction.
func (d *Daemon) handleConfigGet(json.RawMessage) (any, error) {
	// Redact defensively: the field list only ever reads registry settings,
	// but the running config that feeds it must not hold usable secrets.
	running := d.runningConfig().Redact()
	path := d.paths.ConfigFile()
	fp, err := config.FingerprintFile(path)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}

	fields := make([]map[string]any, 0, len(config.Settings()))
	for _, s := range config.Settings() {
		f := map[string]any{
			"key":    s.Key,
			"label":  s.Label,
			"type":   string(s.Type),
			"reload": string(s.Reload),
			"value":  s.Get(running),
		}
		switch {
		case s.Key == "ai.provider":
			f["enum"] = endpointNames(running)
		case len(s.Enum) > 0:
			f["enum"] = s.Enum
		}
		fields = append(fields, f)
	}

	secrets := make([]map[string]any, 0, len(running.AI.Endpoints))
	for _, name := range endpointNames(running) {
		ep := running.AI.Endpoints[name]
		secrets = append(secrets, map[string]any{
			"endpoint":   name,
			"env":        ep.APIKeyEnv,
			"env_set":    ep.APIKeyEnv != "" && os.Getenv(ep.APIKeyEnv) != "",
			"inline_key": ep.APIKey != "",
		})
	}

	return map[string]any{
		"path":        path,
		"fingerprint": fp,
		"fields":      fields,
		"secrets":     secrets,
	}, nil
}

type configSetParams struct {
	// Changes maps dotted setting keys to new values (native JSON types or
	// strings; Setting.Coerce accepts both).
	Changes map[string]any `json:"changes"`
	// Fingerprint is the file fingerprint from the client's config.get.
	// When present, a mismatch with the file on disk fails the set — the
	// file was edited externally, and hand edits are never clobbered.
	Fingerprint string `json:"fingerprint"`
}

// configSet validates and writes field changes into config.toml — preserving
// hand-edited content — then applies them to the running daemon per each
// setting's reload class. Nothing is written unless the whole resulting
// configuration validates. A named method with a source rather than a
// handler closure because the assistant's config.write_setting tool (issue
// #105) drives the very same function in-process — one settings write path,
// with only the source label differing.
func (d *Daemon) configSet(params json.RawMessage, source string) (map[string]any, error) {
	var p configSetParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "config.set params: %v", err)
		}
	}
	if len(p.Changes) == 0 {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "config.set: no changes given")
	}

	path := d.paths.ConfigFile()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	if p.Fingerprint != "" && p.Fingerprint != fp {
		return nil, &ipc.Error{
			Code: ipc.CodeConfigConflict,
			Message: "config.toml changed on disk since it was read; " +
				"reload the settings and reapply your change",
			Data: map[string]any{"fingerprint": fp},
		}
	}

	// Rebase onto the file, not the running config: keys this change does
	// not touch keep whatever the user hand-edited, even if the daemon has
	// not picked those edits up yet.
	fileCfg, err := config.ParseBytes(raw)
	if err != nil {
		return nil, &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: fmt.Sprintf("config.toml does not parse; fix it by hand first: %v", err),
		}
	}

	native := make(map[string]any, len(p.Changes))
	for key, value := range p.Changes {
		s, ok := config.SettingFor(key)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown setting %q", key)
		}
		nv, err := s.Coerce(value)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%s: %v", key, err)
		}
		native[key] = nv
		if err := s.Apply(&fileCfg, nv); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%s: %v", key, err)
		}
	}

	// Attached after the changes are applied, so the catalog belongs to the
	// engine the *candidate* selects: comparing a new Kokoro voice id against
	// the running Piper catalog would reject a perfectly good engine switch.
	// Rebuilt per call rather than cached on the daemon for the same reason —
	// config.set is a deliberate user action, not a hot path.
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := fileCfg.Validate(); err != nil {
		return nil, &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "the change was rejected by validation; nothing was written",
			Data:    map[string]any{"problems": validationProblems(err)},
		}
	}

	newRaw, err := config.RewriteTOML(raw, native)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "rewrite config: %v", err)
	}
	if err := config.WriteFileAtomic(path, newRaw); err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "write config: %v", err)
	}

	applied, reason := d.applyRuntime(fileCfg)
	newFP := config.Fingerprint(newRaw)
	d.publishConfigChanged(newFP)
	// One event per changed key, in stable order (issue #105's observability):
	// the activity feed's "settings equivalent" of config.entry_changed. Keys
	// only, never values — a value can be a whole system prompt, and the row
	// exists to say what moved and who moved it, not to republish content.
	keys := make([]string, 0, len(native))
	for key := range native {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		d.bus.Publish(session.Event{Type: "config.setting_changed", Data: map[string]any{
			"key": key, "source": source,
		}})
	}

	result := map[string]any{
		"fingerprint":   newFP,
		"applied":       applied,
		"needs_restart": d.restartPending(fileCfg),
	}
	if reason != "" {
		result["reason"] = reason
	}
	return result, nil
}

// handleConfigReload re-reads config.toml into the running daemon — the
// recovery path after an external edit, and the retry path after a set that
// could not apply mid-session. A file that fails validation changes nothing:
// the daemon never hot-swaps into a broken configuration.
func (d *Daemon) handleConfigReload(json.RawMessage) (any, error) {
	path := d.paths.ConfigFile()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	fileCfg, err := config.ParseBytes(raw)
	if err != nil {
		return nil, &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: fmt.Sprintf("config.toml does not parse; the running configuration is unchanged: %v", err),
		}
	}
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := fileCfg.Validate(); err != nil {
		return nil, &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "config.toml failed validation; the running configuration is unchanged",
			Data:    map[string]any{"problems": validationProblems(err)},
		}
	}
	applied, reason := d.applyRuntime(fileCfg)
	if !applied {
		return nil, ipc.Errorf(ipc.CodeConfigBusy, "%s", reason)
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	d.publishConfigChanged(fp)
	return map[string]any{
		"fingerprint":   fp,
		"needs_restart": d.restartPending(fileCfg),
	}, nil
}

// handleDoctorGet runs the settings-relevant readiness checks (offline and
// fast — no provider probe, no audio round trips) so the settings screen can
// show per-option readiness inline with the fix command.
func (d *Daemon) handleDoctorGet(json.RawMessage) (any, error) {
	results := doctor.SettingsChecks(d.runningConfig(), d.paths)
	checks := make([]map[string]any, 0, len(results))
	for _, r := range results {
		checks = append(checks, map[string]any{
			"status":  r.Status.String(),
			"name":    r.Name,
			"detail":  r.Detail,
			"fix":     r.Fix,
			"related": r.Related,
		})
	}
	return map[string]any{"checks": checks}, nil
}

// applyRuntime moves the daemon onto next, honouring reload classes:
// restart-class sections keep their running values (they were wired at
// construction), live-class settings always land, and idle-class settings
// swap the engine's collaborators — refused while a session is active, in
// which case the running configuration stays as it was, minus the live
// settings which are safe regardless.
func (d *Daemon) applyRuntime(next config.Config) (applied bool, reason string) {
	d.cfgMu.Lock()
	running := d.cfg
	d.cfgMu.Unlock()

	merged := next
	merged.Activation = running.Activation
	merged.Tools = running.Tools
	merged.Artifacts = running.Artifacts
	// Memory is restart-class with the rest of the tool registry: the store
	// and the memory tools are wired at construction (ADR 0025).
	merged.Memory = running.Memory
	// Vocabulary likewise (issue #129) — except speak_back, which only
	// shapes the injection block's stance sentence and is rebuilt with the
	// engine's collaborators below, so it lands idle-class.
	speakBack := merged.Vocabulary.SpeakBack
	merged.Vocabulary = running.Vocabulary
	merged.Vocabulary.SpeakBack = speakBack
	// Knowledge feeds are idle-class *once the service exists*: the tables
	// swap through Reconfigure below. With no feeds at boot there is no
	// service and no registered tool, so the section is pinned like memory —
	// the first feed takes a restart, exactly as the docs say (ADR 0031).
	if d.knowledge == nil {
		merged.Knowledge = running.Knowledge
	}
	merged.Log = running.Log

	if !idleClassChanged(running, merged) {
		// Only live-class settings moved; nothing to rebuild.
		d.cfgMu.Lock()
		d.cfg = merged
		d.cfgMu.Unlock()
		return true, ""
	}

	deps, workers, err := fillDeps(merged, d.paths, d.injected, d.vocabulary, d.log)
	if err != nil {
		return false, err.Error()
	}
	capture := newLayoutCapturer(d.paths, d.compositor, d.log)
	capture.committed = d.captureCommitted
	opts := engineOptions(merged, d.compositor, d.bus, d.memory, d.vocabulary, d.knowledge,
		d.conversations, d.windows, d.log)
	opts.Capture = capture
	// The focus runner is rebuilt around the same construction-wired service
	// (ADR 0041): the store instance survives every reload, exactly like the
	// memory book it is modelled on.
	opts.IntentRunner = &focus.IntentRunner{Service: d.focus, Log: d.log}
	if err := d.engine.Reconfigure(deps.Provider, deps.Transcriber, deps.Synthesizer,
		deps.Recorder, deps.Player, opts); err != nil {
		// The engine kept its old collaborators, so the workers just built are
		// unreachable — close them before they are dropped. Nothing has been
		// spawned yet (supervisors start lazily), which is what makes this
		// safe rather than a restart. Live settings still land, so the
		// notification switches never wait on an idle engine.
		workers.Close()
		d.cfgMu.Lock()
		d.cfg.UI = merged.UI
		d.cfgMu.Unlock()
		return false, err.Error()
	}
	// The engine swap succeeded, so the feed schedules follow the same
	// configuration (ADR 0031): the service keeps its cached values and its
	// tracked group, only the feed set and timers rebuild — the old loops
	// unwind into the same group shutdown drains, so a reload can never
	// orphan one (the #74 lesson).
	if d.knowledge != nil && !reflect.DeepEqual(running.Knowledge, merged.Knowledge) {
		d.knowledge.Reconfigure(feedSpecs(merged))
	}
	// The automation schedules follow the same tables (ADR 0032): a changed
	// [[routines]] or [[scripts]] set rebuilds the loops through Reconfigure —
	// old generations unwind into the same tracked group — and the
	// cannot-run-unattended warning re-fires against the new tables, so an
	// edit that schedules an ask-tier entry is warned about at the reload that
	// introduced it, not discovered at 2am.
	if !reflect.DeepEqual(running.Routines, merged.Routines) ||
		!reflect.DeepEqual(running.Scripts, merged.Scripts) {
		entries := merged.AutomationEntries()
		d.automations.Reconfigure(entries)
		d.warnUnattendableSchedules(merged, entries)
	}
	// The nickname collision check (#126) follows the engine to the rebuilt
	// grammar, so a nickname refused as "already a routine trigger" and the
	// routine that owns the phrase always come from the same config read.
	if d.router != nil {
		d.router.set(opts.Intents)
	}
	// The swap succeeded: the previous configuration's engine processes are
	// nobody's children now, so kill them. Without this a reload leaks a
	// whisper-server and a Python interpreter per reload.
	d.cfgMu.Lock()
	previous := d.warm
	d.cfg = merged
	d.warm = workers
	d.cfgMu.Unlock()
	previous.Close()
	d.log.Info("configuration applied", "component", "daemon",
		"provider", merged.AI.Provider, "model", merged.AI.Model, "tts", merged.TTS.Provider)
	return true, ""
}

// restartPending lists the restart-class settings whose file value differs
// from what the daemon booted with — the "restart jarvixd to finish" list.
func (d *Daemon) restartPending(next config.Config) []string {
	running := d.runningConfig()
	keys := []string{}
	for _, s := range config.Settings() {
		if s.Reload != config.ReloadRestart {
			continue
		}
		if !reflect.DeepEqual(s.Get(running), s.Get(next)) {
			keys = append(keys, s.Key)
		}
	}
	return keys
}

// idleClassChanged reports whether any idle-class setting differs between the
// running and candidate configurations. The structured tables the engine
// compiles — [[routines]], [[scripts]], and [[intents.custom]] — have no
// entry in the settings registry, so they are compared directly: without
// this, a reload after a hand edit or a layout capture (#62) would update the
// stored config but never rebuild the router that makes the phrases work.
func idleClassChanged(running, next config.Config) bool {
	for _, s := range config.Settings() {
		if s.Reload != config.ReloadIdle {
			continue
		}
		if !reflect.DeepEqual(s.Get(running), s.Get(next)) {
			return true
		}
	}
	return !reflect.DeepEqual(running.Routines, next.Routines) ||
		!reflect.DeepEqual(running.Scripts, next.Scripts) ||
		!reflect.DeepEqual(running.Intents.Custom, next.Intents.Custom) ||
		// [knowledge] is a structured table on the same terms as [[routines]]:
		// no settings-registry entry, compared directly so a hand edit plus
		// reload actually reschedules the feeds (ADR 0031). With no service
		// the section was pinned above, so this can never fire spuriously.
		!reflect.DeepEqual(running.Knowledge, next.Knowledge)
}

// publishConfigChanged tells every connected client (overlay, windows, CLIs)
// that configuration moved, so open settings screens can refresh.
func (d *Daemon) publishConfigChanged(fingerprint string) {
	d.bus.Publish(session.Event{Type: "config.changed",
		Data: map[string]any{"fingerprint": fingerprint}})
}

// endpointNames lists the configured AI endpoints in stable order — the
// dynamic enum for ai.provider.
func endpointNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.AI.Endpoints))
	for name := range cfg.AI.Endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validationProblems splits Config.Validate's aggregate error back into its
// per-key messages, so clients can place each next to the field it names.
func validationProblems(err error) []string {
	msg := err.Error()
	marker := "\n  - "
	if i := strings.Index(msg, marker); i >= 0 {
		return strings.Split(msg[i+len(marker):], marker)
	}
	return []string{msg}
}
