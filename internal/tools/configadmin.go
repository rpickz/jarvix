package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// This file is the assistant's hands on its own configuration (issue #105,
// ADR 0036): six thin verbs over the daemon's EXISTING admin machinery — the
// entry pipeline behind the window's form dialogs (ADR 0033) and the settings
// registry behind the Settings tab (ADR 0015). The tools own what the *model*
// is told — the field-keyed problems it can act on, the read-before-edit
// discipline, the honesty steering in every result — while every rule about
// what may be written, and how, stays daemon-side where the forms already
// proved it: one write path, three clients (loader, forms, assistant).
//
// Two boundaries are drawn here and tested hard:
//
//   - The exclusion wall. The entry verbs address only the three families the
//     window's registry names (routines, scripts, knowledge feeds), and the
//     settings verbs address only the assistant's pruned view of the registry
//     — [ai], [tools.policy], [advisors], and [[intents.custom]] are not
//     denied, they are structurally absent, and an attempt is refused before
//     the permission gate with a spoken-ready reason (Refusing).
//   - The command-bearing floor. Everything these tools can write will later
//     RUN — a script's path, a feed's argv, a routine's launches — so the
//     write verbs sit on script.run's tier floor (policy.go) and their
//     confirmation cards show every command-bearing field VERBATIM, the
//     shell.run discipline applied at authoring time.

// Config tool names, exported so the policy's tier rules and the status
// surfaces can name them without guessing.
const (
	ConfigListEntriesToolName  = "config.list_entries"
	ConfigGetEntryToolName     = "config.get_entry"
	ConfigWriteEntryToolName   = "config.write_entry"
	ConfigDeleteEntryToolName  = "config.delete_entry"
	ConfigReadSettingsToolName = "config.read_settings"
	ConfigWriteSettingToolName = "config.write_setting"
)

// configEntryFamilies is the closed set of [[family]] tables the assistant
// may administer — exactly the families the window's entry registry edits
// (entry_admin.go), in the same spelling. Closed here as well as daemon-side
// because the *refusal* has to happen before the gate (Refusing), and the
// gate consults the tool, not the daemon. A daemon-side drift test pins this
// list to the registry, so a family added there is a reviewed decision here.
var configEntryFamilies = map[string]string{
	"routines":        "routine",
	"scripts":         "script",
	"knowledge.feeds": "feed",
}

// ConfigEntryFamilies lists the administrable families, sorted — exported so
// the daemon's drift test can pin this set to its entry registry.
func ConfigEntryFamilies() []string { return configFamilyNames() }

// configFamilyNames lists the families for schemas and messages, sorted.
func configFamilyNames() []string {
	names := make([]string, 0, len(configEntryFamilies))
	for name := range configEntryFamilies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ConfigProblem is one field-keyed validation problem, in the exact
// {field, message} shape the daemon's entry pipeline produces (ADR 0033) —
// reused as model feedback rather than re-encoded, so what the form pins to
// an input the model reads as "fix this field".
type ConfigProblem struct {
	Field   string
	Message string
}

// ConfigEntrySummary is one entry in a family listing. One struct covers all
// three families; fields a family does not have stay zero.
type ConfigEntrySummary struct {
	Name        string
	Enabled     bool
	Phrases     []string
	Schedule    string
	Description string
	// Path is a script's executable; Command a feed's fixed argv.
	Path    string
	Command []string
}

// ConfigEntry is one whole entry as the file contains it, with the file
// fingerprint it was read under — the pair the write discipline needs.
type ConfigEntry struct {
	Family      string
	Name        string
	Entry       map[string]any
	Fingerprint string
}

// ConfigWriteReceipt reports one successful entry write or delete.
type ConfigWriteReceipt struct {
	Created bool
	// Applied is false when the entry was written but is not live on this
	// daemon (the first knowledge feed's restart boundary); Reason then says
	// why, in words safe to relay.
	Applied bool
	Reason  string
}

// ConfigSettingView is one registry setting as the assistant may see it —
// already pruned of the excluded space by the admin.
type ConfigSettingView struct {
	Key   string
	Label string
	Type  string
	Value any
	Enum  []string
	// Reload is the setting's reload class ("live", "idle", "restart").
	Reload string
	// Dangerous marks the always-confirm set (ADR 0036).
	Dangerous bool
}

// ConfigSettingReceipt reports one successful settings write.
type ConfigSettingReceipt struct {
	// Value is the value now in the file, read back after the write — what a
	// spoken confirmation may honestly claim.
	Value any
	// Applied is false when the change is written but not applied (a session
	// was active); Reason says why.
	Applied bool
	Reason  string
	// NeedsRestart is true for a restart-class setting whose running value
	// now differs from the file.
	NeedsRestart bool
}

// ConfigAdminError is the daemon's structured refusal of one admin call,
// preserved so the tools can turn each kind into the feedback the model can
// act on: validation problems become a fix-and-retry list, a stale
// fingerprint becomes the internal re-read-and-retry, absence becomes "no
// such entry".
type ConfigAdminError struct {
	// Invalid: the write was rejected by validation, nothing written;
	// Problems carries the field-keyed reasons.
	Invalid bool
	// Conflict: the file changed under the given fingerprint, nothing
	// written; Fingerprint carries the fresh one.
	Conflict bool
	// NotFound: no entry has the requested name.
	NotFound bool
	Message  string
	Problems []ConfigProblem
	// Fingerprint is the file's current fingerprint on a Conflict.
	Fingerprint string
}

// Error implements error.
func (e *ConfigAdminError) Error() string { return e.Message }

// ConfigAdmin is what the tools need from the daemon: the window's entry
// verbs and the settings surface, verbatim — the daemon-side bridge invokes
// the same handlers the window's IPC does, so these tools add zero write
// paths (issue #105's architecture requirement).
type ConfigAdmin interface {
	// ListEntries lists one family's entries in file order.
	ListEntries(family string) ([]ConfigEntrySummary, error)
	// GetEntry reads one whole entry plus the file fingerprint. A missing
	// entry is a *ConfigAdminError with NotFound.
	GetEntry(family, name string) (ConfigEntry, error)
	// UpsertEntry writes one whole entry through the validate-whole-then-
	// write pipeline. name addresses an existing entry (rename allowed via
	// the draft's own name); "" creates. fingerprint "" skips the conflict
	// check (creates only — an upsert of a new name cannot clobber).
	UpsertEntry(family, name string, entry map[string]any, fingerprint string) (ConfigWriteReceipt, error)
	// DeleteEntry removes one entry byte-preservingly through the same
	// pipeline.
	DeleteEntry(family, name, fingerprint string) (ConfigWriteReceipt, error)
	// Settings is the assistant's pruned view of the settings registry.
	Settings() []ConfigSettingView
	// WriteSetting validates and writes one registry setting.
	WriteSetting(key string, value any) (ConfigSettingReceipt, error)
	// ExcludedSetting reports the spoken-ready reason a key is structurally
	// outside the assistant's writable space; false for keys that are merely
	// unknown (those come back as correctable errors, never as the wall).
	ExcludedSetting(key string) (string, bool)
}

// ConfigToolsOptions configure the family.
type ConfigToolsOptions struct {
	Admin ConfigAdmin
	// Log records operations — families, names, keys, and outcomes only,
	// never entry bodies or setting values. Nil uses slog.Default().
	Log *slog.Logger
}

// ConfigTools bundles the six verbs, mirroring how the memory tools share one
// Book: one admin, one read cache, registered together or not at all.
type ConfigTools struct {
	admin ConfigAdmin
	log   *slog.Logger

	// mu guards reads: what the model last saw of each entry, keyed by
	// family + folded name. It exists for exactly two rules of the write
	// discipline: an edit must start from a read (a blind write replaces the
	// whole entry, silently dropping fields the model never mentioned), and
	// a file that moved between that read and the write must surface as a
	// conflict rather than clobber a hand edit (issue #105's acceptance
	// criteria). Fingerprints therefore never travel through the model — an
	// opaque token in a spoken exchange would be dropped or hallucinated —
	// the tools carry them here.
	mu    sync.Mutex
	reads map[string]entryRead
}

// entryRead is one remembered read: the entry as the model saw it and the
// file fingerprint it was read under.
type entryRead struct {
	entry       map[string]any
	fingerprint string
}

// NewConfigTools builds the family over one admin.
func NewConfigTools(opts ConfigToolsOptions) *ConfigTools {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &ConfigTools{admin: opts.Admin, log: log, reads: make(map[string]entryRead)}
}

// Tools returns the family in registration order: reads first, then writes,
// entries before settings — the order the descriptions reference each other.
func (c *ConfigTools) Tools() []Tool {
	return []Tool{
		&configListEntries{c},
		&configGetEntry{c},
		&configWriteEntry{c},
		&configDeleteEntry{c},
		&configReadSettings{c},
		&configWriteSetting{c},
	}
}

// Names lists the family's tool names, for the startup log.
func (c *ConfigTools) Names() []string {
	return []string{
		ConfigListEntriesToolName, ConfigGetEntryToolName,
		ConfigWriteEntryToolName, ConfigDeleteEntryToolName,
		ConfigReadSettingsToolName, ConfigWriteSettingToolName,
	}
}

// readKey folds an entry's identity the way the families match names:
// case-insensitively, whitespace-trimmed.
func readKey(family, name string) string {
	return family + "\x00" + strings.ToLower(strings.TrimSpace(name))
}

// rememberRead records what the model has seen of one entry.
func (c *ConfigTools) rememberRead(family, name string, e ConfigEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads[readKey(family, name)] = entryRead{entry: e.Entry, fingerprint: e.Fingerprint}
}

// lastRead reports what the model last saw of one entry, if anything.
func (c *ConfigTools) lastRead(family, name string) (entryRead, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.reads[readKey(family, name)]
	return r, ok
}

// forgetRead drops a remembered read (after a delete or a rename).
func (c *ConfigTools) forgetRead(family, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reads, readKey(family, name))
}

// refuseFamily is the entry half of the exclusion wall: a reason for any
// family outside the closed set, spoken-ready because it becomes both the
// tool.denied rule and the sentence the model relays. "" for the three
// administrable families and for absent/unparseable arguments — the latter
// are ordinary errors for Execute, not the wall.
func refuseFamily(input json.RawMessage) (string, bool) {
	var args struct {
		Family string `json:"family"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", false
	}
	family := strings.TrimSpace(args.Family)
	if family == "" {
		return "", false
	}
	if _, ok := configEntryFamilies[family]; ok {
		return "", false
	}
	return fmt.Sprintf("the assistant may only change routines, scripts, and knowledge feeds; "+
		"%q is off limits to it and no tools policy can open it", family), true
}

// entryJSON renders an entry compactly for a tool result. Map key order is
// stable (encoding/json sorts), which the equality check below relies on too.
func entryJSON(entry map[string]any) string {
	b, err := json.Marshal(entry)
	if err != nil {
		return fmt.Sprintf("%v", entry)
	}
	return string(b)
}

// sameEntry compares two entries structurally, via canonical JSON — the
// values cross a JSON boundary anyway, so JSON equality is exactly the
// equality that matters.
func sameEntry(a, b map[string]any) bool {
	return entryJSON(a) == entryJSON(b)
}

// entryString reads a string field from a loosely-typed entry.
func entryString(entry map[string]any, key string) string {
	s, _ := entry[key].(string)
	return strings.TrimSpace(s)
}

// entryStrings reads a string-list field from a loosely-typed entry,
// tolerating both []string (from Go callers) and []any (from JSON).
func entryStrings(entry map[string]any, key string) []string {
	switch v := entry[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// quoteAll renders list elements individually quoted — `"curl" "-s" "url"` —
// so a card can show an argv verbatim without hiding element boundaries the
// way plain joining would ("a b" versus "a", "b").
func quoteAll(list []string) string {
	quoted := make([]string, len(list))
	for i, s := range list {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, " ")
}

// quoteList renders phrases for a card: quoted, comma-separated.
func quoteList(list []string) string {
	quoted := make([]string, len(list))
	for i, s := range list {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

// entryCard renders the confirmation card for one entry draft (or one entry
// about to be deleted): the first line names the action, then name, phrases,
// and schedule, then EVERY command-bearing field verbatim — the script's
// path, the feed's argv element by element, each routine step's launch. This
// is the string published on tool.confirmation_required and shown by the
// window, so what the user approves is the entry, not the model's account of
// it (the shell.run discipline, ADR 0014).
func entryCard(action, family string, entry map[string]any) string {
	kind := configEntryFamilies[family]
	lines := []string{fmt.Sprintf("%s %s %q", action, kind, entryString(entry, "name"))}
	if phrases := entryStrings(entry, "phrases"); len(phrases) > 0 {
		lines = append(lines, "phrases: "+quoteList(phrases))
	}
	if schedule := entryString(entry, "schedule"); schedule != "" {
		lines = append(lines, "schedule: "+schedule)
	}
	switch family {
	case "scripts":
		if path := entryString(entry, "path"); path != "" {
			lines = append(lines, "runs file (verbatim): "+path)
		}
	case "knowledge.feeds":
		if command := entryStrings(entry, "command"); len(command) > 0 {
			lines = append(lines, "runs command (verbatim): "+quoteAll(command))
		}
	case "routines":
		steps, _ := entry["steps"].([]any)
		for i, raw := range steps {
			step, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			lines = append(lines, fmt.Sprintf("step %d (verbatim): launch %q", i+1, entryString(step, "app")))
		}
		// Go-typed callers (tests, the bridge's read-back) may carry steps as
		// []map[string]any; render those too rather than showing a routine
		// with no steps.
		if typed, ok := entry["steps"].([]map[string]any); ok {
			for i, step := range typed {
				lines = append(lines, fmt.Sprintf("step %d (verbatim): launch %q", i+1, entryString(step, "app")))
			}
		}
	}
	if enabled, ok := entry["enabled"].(bool); ok && !enabled {
		lines = append(lines, "enabled: false")
	}
	return strings.Join(lines, "\n")
}

// entrySummarySentence is the spoken half of the card: one sentence naming
// the entry and its command-bearing heart, capped for speech.
func entrySummarySentence(verb, family string, entry map[string]any) string {
	kind := configEntryFamilies[family]
	name := entryString(entry, "name")
	clause := ""
	switch family {
	case "scripts":
		if path := entryString(entry, "path"); path != "" {
			clause = ", which runs the file " + spokenCommand(path)
		}
	case "knowledge.feeds":
		if command := entryStrings(entry, "command"); len(command) > 0 {
			clause = ", which runs " + spokenCommand(strings.Join(command, " "))
		}
	case "routines":
		apps := routineApps(entry)
		if len(apps) > 0 {
			clause = ", which launches " + spokenCommand(strings.Join(apps, ", "))
		}
	}
	return fmt.Sprintf("I want to %s your %s %s%s. Should I go ahead?", verb, name, kind, clause)
}

// routineApps lists a routine draft's launched applications, in step order.
func routineApps(entry map[string]any) []string {
	var apps []string
	appendStep := func(step map[string]any) {
		if app := entryString(step, "app"); app != "" {
			apps = append(apps, app)
		}
	}
	if loose, ok := entry["steps"].([]any); ok {
		for _, raw := range loose {
			if step, ok := raw.(map[string]any); ok {
				appendStep(step)
			}
		}
	}
	if typed, ok := entry["steps"].([]map[string]any); ok {
		for _, step := range typed {
			appendStep(step)
		}
	}
	return apps
}

// problemsText renders the daemon's field-keyed problems as feedback the
// model can act on — requirement 4's shape: the problems, then exactly two
// legal continuations (correct and retry, or report the real error), stated
// so "claim success anyway" is never one of them.
func problemsText(what string, problems []ConfigProblem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The %s was rejected by validation; NOTHING was written. Problems:\n", what)
	for _, p := range problems {
		if p.Field != "" {
			fmt.Fprintf(&b, "- %s: %s\n", p.Field, p.Message)
		} else {
			fmt.Fprintf(&b, "- %s\n", p.Message)
		}
	}
	b.WriteString("Fix exactly what each problem names and call the tool again. " +
		"If you cannot fix it, tell the user the actual problem in plain words — " +
		"never say the change was made.")
	return b.String()
}

// appliedText words a receipt's applied/reason honestly for the model.
func appliedText(applied bool, reason string) string {
	if applied {
		return "The change is applied."
	}
	if reason != "" {
		return "The change is saved but NOT yet in effect: " + reason + ". Tell the user so."
	}
	return "The change is saved but not yet in effect. Tell the user so."
}

// ------------------------------------------------------ config.list_entries

type configListEntries struct{ c *ConfigTools }

// Name implements Tool.
func (t *configListEntries) Name() string { return ConfigListEntriesToolName }

// Description implements Tool.
func (t *configListEntries) Description() string {
	return "List the user's configured entries in one family: routines (spoken desktop setups), " +
		"scripts (executables behind a spoken phrase), or knowledge.feeds (commands fetched on a " +
		"schedule). Shows each entry's name, phrases, schedule, and whether it is enabled. Use it " +
		"to find the exact entry name before reading, changing, or removing one."
}

// Schema implements Tool.
func (t *configListEntries) Schema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"family": {"type": "string", "enum": [%s], "description": "Which configuration family to list"}
		},
		"required": ["family"]
	}`, quotedFamilyEnum()))
}

func quotedFamilyEnum() string {
	names := configFamilyNames()
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

// Execute implements Tool.
func (t *configListEntries) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Family string `json:"family"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid config.list_entries arguments: %w", err)
	}
	family := strings.TrimSpace(args.Family)
	if _, ok := configEntryFamilies[family]; !ok {
		return fmt.Sprintf("No family is named %q. The families are: %s.",
			family, strings.Join(configFamilyNames(), ", ")), nil
	}
	entries, err := t.c.admin.ListEntries(family)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	t.c.log.Info("config entries listed", "component", "tools",
		"tool", t.Name(), "family", family, "entries", len(entries))
	if len(entries) == 0 {
		return fmt.Sprintf("No [[%s]] entries exist yet. You can create one with config.write_entry "+
			"if the user asks for it.", family), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The [[%s]] entries are:\n", family)
	for _, e := range entries {
		parts := []string{}
		if len(e.Phrases) > 0 {
			parts = append(parts, "phrases "+quoteList(e.Phrases))
		}
		if e.Schedule != "" {
			parts = append(parts, "schedule "+e.Schedule)
		}
		if e.Path != "" {
			parts = append(parts, "runs "+e.Path)
		}
		if len(e.Command) > 0 {
			parts = append(parts, "runs "+quoteAll(e.Command))
		}
		if e.Description != "" {
			parts = append(parts, e.Description)
		}
		if !e.Enabled {
			parts = append(parts, "disabled")
		}
		fmt.Fprintf(&b, "- %s: %s\n", e.Name, strings.Join(parts, "; "))
	}
	b.WriteString("Answer the user in plain words; never read this list out verbatim.")
	return b.String(), nil
}

// --------------------------------------------------------- config.get_entry

type configGetEntry struct{ c *ConfigTools }

// Name implements Tool.
func (t *configGetEntry) Name() string { return ConfigGetEntryToolName }

// Description implements Tool.
func (t *configGetEntry) Description() string {
	return "Read one whole routines, scripts, or knowledge.feeds entry from the user's " +
		"configuration, exactly as the file contains it. You MUST read an entry with this " +
		"before editing it: config.write_entry replaces the whole entry, so an edit has to " +
		"start from what is really there or unmentioned fields would be lost."
}

// Schema implements Tool.
func (t *configGetEntry) Schema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"family": {"type": "string", "enum": [%s], "description": "The entry's configuration family"},
			"name": {"type": "string", "description": "The entry's exact name (config.list_entries shows them)"}
		},
		"required": ["family", "name"]
	}`, quotedFamilyEnum()))
}

// Execute implements Tool.
func (t *configGetEntry) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Family string `json:"family"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid config.get_entry arguments: %w", err)
	}
	family := strings.TrimSpace(args.Family)
	if _, ok := configEntryFamilies[family]; !ok {
		return fmt.Sprintf("No family is named %q. The families are: %s.",
			family, strings.Join(configFamilyNames(), ", ")), nil
	}
	entry, err := t.c.admin.GetEntry(family, args.Name)
	if err != nil {
		var adminErr *ConfigAdminError
		if asConfigAdminError(err, &adminErr) && adminErr.NotFound {
			return fmt.Sprintf("No [[%s]] entry is named %q. Call config.list_entries to see "+
				"the names that exist.", family, strings.TrimSpace(args.Name)), nil
		}
		return fmt.Sprintf("error: %v", err), nil
	}
	t.c.rememberRead(family, entry.Name, entry)
	t.c.log.Info("config entry read", "component", "tools",
		"tool", t.Name(), "family", family, "name", entry.Name)
	return fmt.Sprintf("The [[%s]] entry %q reads:\n%s\n"+
		"To change it, call config.write_entry with the WHOLE edited entry (name set to %q) — "+
		"start from exactly this, because the write replaces the entry and drops anything "+
		"you leave out.", family, entry.Name, entryJSON(entry.Entry), entry.Name), nil
}

// ------------------------------------------------------- config.write_entry

type configWriteEntry struct{ c *ConfigTools }

// Name implements Tool.
func (t *configWriteEntry) Name() string { return ConfigWriteEntryToolName }

// Description implements Tool. The field vocabulary is stated here because a
// draft is the model's work: the daemon validates it, but a model that has
// never seen the key names would burn a round learning each one from a
// problem message.
func (t *configWriteEntry) Description() string {
	return "Create or change one routines, scripts, or knowledge.feeds entry in the user's " +
		"configuration, when the user asks for it. Set name to the existing entry you are " +
		"replacing (after reading it with config.get_entry); omit name to create a new entry. " +
		"The daemon validates the draft with the loader's own rules and nothing is written when " +
		"validation fails — fix the returned problems and retry. The user is asked to confirm " +
		"before anything is written. Fields — routines: name, phrases, schedule, announce, " +
		"enabled, steps (list of {app, match, workspace, float, size, position, tile}); scripts: " +
		"name, phrases, path (an executable file, run with no arguments), timeout_sec, report, " +
		"schedule, announce, enabled; knowledge.feeds: name, description, command (a fixed argv " +
		"list like [\"curl\", \"-s\", \"URL\"], never a shell line), mode, interval_sec, ttl_sec, " +
		"timeout_sec, inject, enabled."
}

// Schema implements Tool.
func (t *configWriteEntry) Schema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"family": {"type": "string", "enum": [%s], "description": "The entry's configuration family"},
			"name": {"type": "string", "description": "The existing entry to replace, exactly as read with config.get_entry; omit to create a new entry"},
			"entry": {"type": "object", "description": "The whole entry to write, its name field included"}
		},
		"required": ["family", "entry"]
	}`, quotedFamilyEnum()))
}

// writeEntryArgs is the model's input, parsed once for Refuse, Confirmation,
// and Execute so all three judge the same call.
type writeEntryArgs struct {
	Family string         `json:"family"`
	Name   string         `json:"name"`
	Entry  map[string]any `json:"entry"`
}

// Refuse implements Refusing: the family wall, before the gate.
func (t *configWriteEntry) Refuse(input json.RawMessage) (string, bool) {
	return refuseFamily(input)
}

// Confirmation implements Confirmable: the card is built from the draft — the
// exact entry Execute will hand to the write pipeline — never from the
// model's narration of it. Name, phrases, schedule, and every command-bearing
// field, verbatim (entryCard).
func (t *configWriteEntry) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	var args writeEntryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", "", false
	}
	family := strings.TrimSpace(args.Family)
	if _, known := configEntryFamilies[family]; !known || len(args.Entry) == 0 {
		return "", "", false
	}
	if entryString(args.Entry, "name") == "" && strings.TrimSpace(args.Name) == "" {
		return "", "", false
	}
	action := "create"
	if strings.TrimSpace(args.Name) != "" {
		action = "edit"
	}
	return entryCard(action, family, args.Entry),
		entrySummarySentence("save", family, args.Entry), true
}

// Execute implements Tool. The write path is the daemon's (ConfigAdmin —
// fingerprint-guarded, validated whole, atomic, byte-preserving); this method
// owns the model-facing discipline around it: the read-before-edit rule, the
// conflict handling (retry once internally when the entry itself is
// untouched, surface it when the model's view is stale), the field-keyed
// problem feedback, and a success wording drawn from the entry AS WRITTEN,
// re-read from the file, never from the request.
func (t *configWriteEntry) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args writeEntryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid config.write_entry arguments: %w", err)
	}
	family := strings.TrimSpace(args.Family)
	// The wall again, Execute-side: a registry running without a policy (or a
	// future caller that skips Check) still cannot reach an excluded family —
	// nothing below runs.
	if reason, refused := refuseFamily(input); refused {
		return "Refused: " + reason + ". Tell the user in one short sentence.", nil
	}
	if _, known := configEntryFamilies[family]; !known {
		return fmt.Sprintf("No family is named %q. The families are: %s.",
			family, strings.Join(configFamilyNames(), ", ")), nil
	}
	if len(args.Entry) == 0 {
		return "The entry is missing: call config.write_entry with the whole entry to write.", nil
	}
	draftName := entryString(args.Entry, "name")
	addressed := strings.TrimSpace(args.Name)
	if draftName == "" && addressed == "" {
		return "The entry has no name. Give it one in the entry's name field.", nil
	}

	fingerprint := ""
	var before ConfigEntry
	edit := addressed != ""
	if edit {
		current, err := t.c.admin.GetEntry(family, addressed)
		var adminErr *ConfigAdminError
		if asConfigAdminError(err, &adminErr) && adminErr.NotFound {
			return fmt.Sprintf("No [[%s]] entry is named %q, so there is nothing to edit. "+
				"Call config.list_entries to see the names, or omit name to create a new entry.",
				family, addressed), nil
		}
		if err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		cached, seen := t.c.lastRead(family, addressed)
		if !seen {
			// The read-before-edit rule, enforced rather than hoped for — and
			// enforced usefully: the refusal IS the read, so the model's next
			// call can be the corrected write instead of a detour.
			t.c.rememberRead(family, addressed, current)
			return fmt.Sprintf("Not written: you have not read this entry yet, and a write "+
				"replaces it whole. It currently reads:\n%s\nCheck your edit against this — "+
				"keep every field you do not mean to change — and call config.write_entry again.",
				entryJSON(current.Entry)), nil
		}
		if !sameEntry(cached.entry, current.Entry) {
			t.c.rememberRead(family, addressed, current)
			return fmt.Sprintf("Not written: config.toml changed since you read this entry "+
				"(a hand edit, or another save). The entry now reads:\n%s\nRe-check your edit "+
				"against this and call config.write_entry again if it is still right.",
				entryJSON(current.Entry)), nil
		}
		before = current
		fingerprint = current.Fingerprint
	}

	receipt, err := t.c.admin.UpsertEntry(family, addressed, args.Entry, fingerprint)
	var adminErr *ConfigAdminError
	if asConfigAdminError(err, &adminErr) && adminErr.Conflict && edit {
		// The file moved between our own read and the write. Re-read once: if
		// the entry itself is untouched — the change was elsewhere in the
		// file — retry under the fresh fingerprint; if the entry moved, the
		// model's view is stale and the conflict surfaces (issue #105's
		// hand-edit criterion), current content included so "re-read before
		// retrying" costs no extra round.
		current, rerr := t.c.admin.GetEntry(family, addressed)
		if rerr == nil && sameEntry(current.Entry, before.Entry) {
			t.c.log.Info("config entry write retried after fingerprint conflict",
				"component", "tools", "tool", t.Name(), "family", family, "name", addressed)
			receipt, err = t.c.admin.UpsertEntry(family, addressed, args.Entry, current.Fingerprint)
			adminErr = nil
			_ = asConfigAdminError(err, &adminErr)
		} else {
			if rerr == nil {
				t.c.rememberRead(family, addressed, current)
				return fmt.Sprintf("Not written: config.toml changed underneath this edit. "+
					"The entry now reads:\n%s\nRe-check your edit against this and call "+
					"config.write_entry again if it is still right.", entryJSON(current.Entry)), nil
			}
			return "Not written: config.toml changed underneath this edit and the entry could " +
				"not be re-read. Tell the user, and try config.get_entry before editing again.", nil
		}
	}
	if adminErr != nil {
		switch {
		case adminErr.Invalid:
			return problemsText(configEntryFamilies[family]+" draft", adminErr.Problems), nil
		case adminErr.Conflict:
			return "Not written: config.toml is changing underneath this edit. Read the entry " +
				"again with config.get_entry before retrying.", nil
		}
		return fmt.Sprintf("error: %v", adminErr.Message), nil
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	// Success. The spoken confirmation must state what actually changed, so
	// the wording is drawn from the entry as the file now contains it — a
	// re-read, not an echo of the request.
	writtenName := draftName
	if writtenName == "" {
		writtenName = addressed
	}
	if edit && !strings.EqualFold(writtenName, addressed) {
		t.c.forgetRead(family, addressed)
	}
	verb := "updated"
	if receipt.Created {
		verb = "created"
	}
	t.c.log.Info("config entry written", "component", "tools", "tool", t.Name(),
		"family", family, "name", writtenName, "created", receipt.Created, "applied", receipt.Applied)
	written, rerr := t.c.admin.GetEntry(family, writtenName)
	if rerr != nil {
		// Written but unreadable back — report the write honestly without
		// inventing its content.
		return fmt.Sprintf("The %s %q was %s and saved. %s Confirm to the user in one short "+
			"sentence.", configEntryFamilies[family], writtenName, verb,
			appliedText(receipt.Applied, receipt.Reason)), nil
	}
	t.c.rememberRead(family, writtenName, written)
	return fmt.Sprintf("Saved: the %s %q was %s. It now reads:\n%s\n%s\n"+
		"Confirm to the user in one short sentence what actually changed, using these saved "+
		"values — never your request.", configEntryFamilies[family], writtenName, verb,
		entryJSON(written.Entry), appliedText(receipt.Applied, receipt.Reason)), nil
}

// ------------------------------------------------------ config.delete_entry

type configDeleteEntry struct{ c *ConfigTools }

// Name implements Tool.
func (t *configDeleteEntry) Name() string { return ConfigDeleteEntryToolName }

// Description implements Tool.
func (t *configDeleteEntry) Description() string {
	return "Remove one routines, scripts, or knowledge.feeds entry from the user's " +
		"configuration, when the user asks for it. The user is asked to confirm the named " +
		"entry first; the removal preserves everything else in the file byte-for-byte. Give " +
		"the family and the exact entry name (config.list_entries shows them)."
}

// Schema implements Tool.
func (t *configDeleteEntry) Schema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"family": {"type": "string", "enum": [%s], "description": "The entry's configuration family"},
			"name": {"type": "string", "description": "The exact name of the entry to remove"}
		},
		"required": ["family", "name"]
	}`, quotedFamilyEnum()))
}

// Refuse implements Refusing: the family wall, before the gate.
func (t *configDeleteEntry) Refuse(input json.RawMessage) (string, bool) {
	return refuseFamily(input)
}

// Confirmation implements Confirmable: the question names the entry actually
// about to be removed — resolved from the file, like memory.forget resolves
// from the store — command-bearing fields included, so what disappears is on
// the card, not in the model's words.
func (t *configDeleteEntry) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	var args struct {
		Family string `json:"family"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", "", false
	}
	family := strings.TrimSpace(args.Family)
	if _, known := configEntryFamilies[family]; !known {
		return "", "", false
	}
	entry, err := t.c.admin.GetEntry(family, args.Name)
	if err != nil {
		return "", "", false
	}
	return entryCard("delete", family, entry.Entry),
		fmt.Sprintf("I want to permanently remove your %s %s. Should I go ahead?",
			entry.Name, configEntryFamilies[family]), true
}

// Execute implements Tool. Same conflict discipline as the write: delete
// under the fingerprint of our own read, retry once when the file moved but
// the entry did not, surface it when the entry itself changed.
func (t *configDeleteEntry) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Family string `json:"family"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid config.delete_entry arguments: %w", err)
	}
	family := strings.TrimSpace(args.Family)
	if reason, refused := refuseFamily(input); refused {
		return "Refused: " + reason + ". Tell the user in one short sentence.", nil
	}
	if _, known := configEntryFamilies[family]; !known {
		return fmt.Sprintf("No family is named %q. The families are: %s.",
			family, strings.Join(configFamilyNames(), ", ")), nil
	}
	before, err := t.c.admin.GetEntry(family, args.Name)
	var adminErr *ConfigAdminError
	if asConfigAdminError(err, &adminErr) && adminErr.NotFound {
		return fmt.Sprintf("No [[%s]] entry is named %q; nothing was removed. Call "+
			"config.list_entries to see the names that exist.", family, strings.TrimSpace(args.Name)), nil
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	receipt, err := t.c.admin.DeleteEntry(family, before.Name, before.Fingerprint)
	adminErr = nil
	if asConfigAdminError(err, &adminErr) && adminErr.Conflict {
		current, rerr := t.c.admin.GetEntry(family, before.Name)
		if rerr == nil && sameEntry(current.Entry, before.Entry) {
			t.c.log.Info("config entry delete retried after fingerprint conflict",
				"component", "tools", "tool", t.Name(), "family", family, "name", before.Name)
			receipt, err = t.c.admin.DeleteEntry(family, before.Name, current.Fingerprint)
			adminErr = nil
			_ = asConfigAdminError(err, &adminErr)
		} else {
			return fmt.Sprintf("Not removed: config.toml changed underneath this delete. Read "+
				"the entry again with config.get_entry and ask the user before retrying. "+
				"Nothing was removed from the %s entries.", family), nil
		}
	}
	if adminErr != nil {
		if adminErr.Invalid {
			return problemsText("removal", adminErr.Problems), nil
		}
		return fmt.Sprintf("error: %v", adminErr.Message), nil
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	t.c.forgetRead(family, before.Name)
	t.c.log.Info("config entry deleted", "component", "tools", "tool", t.Name(),
		"family", family, "name", before.Name, "applied", receipt.Applied)
	return fmt.Sprintf("Deleted: the %s %q was removed from the configuration; nothing else "+
		"changed. %s Confirm to the user in one short sentence.",
		configEntryFamilies[family], before.Name, appliedText(receipt.Applied, receipt.Reason)), nil
}

// ----------------------------------------------------- config.read_settings

type configReadSettings struct{ c *ConfigTools }

// Name implements Tool.
func (t *configReadSettings) Name() string { return ConfigReadSettingsToolName }

// Description implements Tool.
func (t *configReadSettings) Description() string {
	return "List the settings you can change for the user — each key with its current value, " +
		"what values it accepts, and whether a change needs a restart. Optionally filter by a " +
		"key prefix such as \"tts.\" or \"assistant.\". These are the ONLY settings you can " +
		"change: the tool permission policy, advisors, and AI provider settings never appear " +
		"here and can never be changed by you."
}

// Schema implements Tool.
func (t *configReadSettings) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"prefix": {"type": "string", "description": "Only settings whose key starts with this, e.g. \"tts.\""}
		}
	}`)
}

// Execute implements Tool.
func (t *configReadSettings) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Prefix string `json:"prefix"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return "", fmt.Errorf("invalid config.read_settings arguments: %w", err)
		}
	}
	prefix := strings.TrimSpace(args.Prefix)
	settings := t.c.admin.Settings()
	var b strings.Builder
	matched := 0
	for _, s := range settings {
		if prefix != "" && !strings.HasPrefix(s.Key, prefix) {
			continue
		}
		matched++
		value, err := json.Marshal(s.Value)
		if err != nil {
			value = []byte(fmt.Sprintf("%v", s.Value))
		}
		fmt.Fprintf(&b, "- %s = %s (%s", s.Key, value, s.Label)
		if len(s.Enum) > 0 {
			fmt.Fprintf(&b, "; one of %s", strings.Join(s.Enum, "|"))
		}
		if s.Reload == "restart" {
			b.WriteString("; needs a daemon restart to apply")
		}
		if s.Dangerous {
			b.WriteString("; always confirmed with the user")
		}
		b.WriteString(")\n")
	}
	t.c.log.Info("config settings listed", "component", "tools",
		"tool", t.Name(), "prefix", prefix, "settings", matched)
	if matched == 0 {
		return fmt.Sprintf("No changeable setting starts with %q. Call config.read_settings "+
			"without a prefix to see them all; settings not listed cannot be changed.", prefix), nil
	}
	b.WriteString("Change one with config.write_setting. Settings not listed here cannot be " +
		"changed by you. Answer the user in plain words; never read keys or this list aloud.")
	return b.String(), nil
}

// ----------------------------------------------------- config.write_setting

type configWriteSetting struct{ c *ConfigTools }

// Name implements Tool.
func (t *configWriteSetting) Name() string { return ConfigWriteSettingToolName }

// Description implements Tool.
func (t *configWriteSetting) Description() string {
	return "Change one setting from config.read_settings to a new value, when the user asks — " +
		"\"talk faster\" is tts.kokoro.speed, \"change your name\" is assistant.name, and so " +
		"on. The daemon validates the whole configuration first; nothing is written when it " +
		"fails, and risky settings ask the user before changing. The result states the value " +
		"actually saved and whether it is live yet — confirm with that, and say so plainly " +
		"when a restart is still needed. You cannot invent settings, and the tool permission " +
		"policy, advisors, and AI provider settings are not changeable."
}

// Schema implements Tool.
func (t *configWriteSetting) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"key": {"type": "string", "description": "The setting's dotted key, from config.read_settings"},
			"value": {"description": "The new value, in the setting's own type (lists as JSON arrays)"}
		},
		"required": ["key", "value"]
	}`)
}

// writeSettingArgs parses once for Refuse, Escalate, Confirmation, Execute.
type writeSettingArgs struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// decodedValue returns the value as a native Go value (UseNumber-free: the
// daemon's Coerce accepts JSON's float64 and strings alike).
func (a writeSettingArgs) decodedValue() (any, error) {
	if len(a.Value) == 0 {
		return nil, fmt.Errorf("value is required")
	}
	var v any
	if err := json.Unmarshal(a.Value, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Refuse implements Refusing: the settings half of the exclusion wall,
// before the gate. Only structurally excluded keys refuse; unknown keys are
// correctable mistakes and go through Execute's error text instead.
func (t *configWriteSetting) Refuse(input json.RawMessage) (string, bool) {
	var args writeSettingArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", false
	}
	return t.c.admin.ExcludedSetting(args.Key)
}

// Escalate implements Escalating: the always-confirm floor for the
// registry-flagged dangerous settings (ADR 0036). Only ever allow → ask —
// the one direction trusting a tool's own judgement is safe — so a global
// (or even explicit) allow cannot silence a change to what the assistant may
// do; deny still wins, since Escalate is only consulted on allow.
func (t *configWriteSetting) Escalate(input json.RawMessage) (rule string, ok bool) {
	var args writeSettingArgs
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Key) == "" {
		return "the setting could not be identified", true
	}
	key := strings.TrimSpace(args.Key)
	for _, s := range t.c.admin.Settings() {
		if s.Key != key {
			continue
		}
		if s.Dangerous {
			return fmt.Sprintf("setting %q always requires confirmation", key), true
		}
		return "", false
	}
	// Unknown (or excluded — Refuse has already won by the time escalation
	// runs, so this is only unknown): asking is the safe failure mode, the
	// unparseable-arguments precedent.
	return fmt.Sprintf("setting %q is not recognised", key), true
}

// Confirmation implements Confirmable: the exact key and value about to be
// written, verbatim on the card.
func (t *configWriteSetting) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	var args writeSettingArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", "", false
	}
	key := strings.TrimSpace(args.Key)
	value, err := args.decodedValue()
	if key == "" || err != nil {
		return "", "", false
	}
	rendered, merr := json.Marshal(value)
	if merr != nil {
		return "", "", false
	}
	label := key
	for _, s := range t.c.admin.Settings() {
		if s.Key == key {
			label = s.Label
			break
		}
	}
	return fmt.Sprintf("set %s = %s", key, rendered),
		fmt.Sprintf("I want to change the setting %q to %s. Should I go ahead?",
			label, spokenCommand(string(rendered))), true
}

// Execute implements Tool.
func (t *configWriteSetting) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args writeSettingArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid config.write_setting arguments: %w", err)
	}
	key := strings.TrimSpace(args.Key)
	if key == "" {
		return "The setting's key is missing. Call config.read_settings to see the keys.", nil
	}
	// The wall again, Execute-side (see configWriteEntry.Execute).
	if reason, excluded := t.c.admin.ExcludedSetting(key); excluded {
		return "Refused: " + reason + ". Tell the user in one short sentence; do not retry.", nil
	}
	known := false
	for _, s := range t.c.admin.Settings() {
		if s.Key == key {
			known = true
			break
		}
	}
	if !known {
		return fmt.Sprintf("No setting is named %q. Call config.read_settings to see every "+
			"setting you can change; you cannot invent new ones.", key), nil
	}
	value, err := args.decodedValue()
	if err != nil {
		return fmt.Sprintf("The value could not be read: %v. Send it in the setting's own type.", err), nil
	}
	receipt, err := t.c.admin.WriteSetting(key, value)
	if err != nil {
		var adminErr *ConfigAdminError
		if asConfigAdminError(err, &adminErr) && adminErr.Invalid {
			return problemsText("setting change", adminErr.Problems), nil
		}
		return fmt.Sprintf("error: %v", err), nil
	}
	saved, merr := json.Marshal(receipt.Value)
	if merr != nil {
		saved = []byte(fmt.Sprintf("%v", receipt.Value))
	}
	t.c.log.Info("config setting written", "component", "tools", "tool", t.Name(),
		"key", key, "applied", receipt.Applied, "needs_restart", receipt.NeedsRestart)
	status := appliedText(receipt.Applied, receipt.Reason)
	if receipt.NeedsRestart {
		status = "The value is saved, but the daemon must be restarted before it takes " +
			"effect — tell the user so, plainly."
	}
	return fmt.Sprintf("Saved: %s is now %s. %s Confirm to the user in one short sentence, "+
		"using this saved value — never your request.", key, saved, status), nil
}

// asConfigAdminError unwraps err into target when it is a *ConfigAdminError —
// errors.As with the nil-guard the call sites' `err != nil` checks would
// otherwise each repeat.
func asConfigAdminError(err error, target **ConfigAdminError) bool {
	return err != nil && errors.As(err, target)
}
