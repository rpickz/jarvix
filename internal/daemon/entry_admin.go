package daemon

// This file is the window form dialog's daemon half (issue #99): the generic
// entry-administration surface that lets a client read, dry-run-validate,
// write, and delete one family entry of config.toml as a whole — the
// machinery behind the Automations tab's New/Edit/Delete forms, built with
// zero automations-specific logic so the knowledge form (#100) and the
// Providers section (#163) are registry rows here, not new surfaces.
//
// Three document shapes, one pipeline (ADR 0052, extended by ADR 0054). The
// array families (`[[routines]]`, `[[scripts]]`, `[[knowledge.feeds]]`,
// `[[intents.custom]]`) address an entry by a key inside it — `name` for all
// but the custom intents, whose identity is the phrase they match; the keyed
// families (`[ai.<name>]`, `[advisors.<name>]`) address it by its table key;
// and the scalar-map family (`[tts.lexicon]`) is one `key = "value"` line. A
// family declares which it is (entryFamilySpec.shape) and four one-line
// dispatch functions do the rest, so everything below — the fingerprint guard,
// the whole-document validation, the atomic write, the reload, the events —
// never learns which shape it is serving.
//
// Six verbs, one discipline (the settings discipline, ADR 0015, as
// automations.set_enabled already applies it):
//
//	config.get_entry      — the whole entry as the parser sees it, plus the
//	                        file's fingerprint. The form round-trips this map
//	                        and edits only the fields it shows, so keys the
//	                        form has no widget for (report, a step's size)
//	                        survive a save untouched.
//	config.validate_entry — a dry run: the draft is written into an in-memory
//	                        copy of the document and the WHOLE result is
//	                        validated with the loader's own rules — the real
//	                        intent router compiled for phrase grammar and
//	                        collisions, the real schedule parser, the real
//	                        path checks. Problems come back keyed to the form
//	                        field they belong to; nothing touches disk.
//	config.upsert_entry   — the same pipeline, and only when the whole
//	                        rewritten document validates is it written
//	                        atomically and picked up by the standard reload.
//	                        A validation failure returns the problems and
//	                        writes NOTHING — never a half-write.
//	config.delete_entry   — byte-preserving removal through the same
//	                        validate-whole-then-write pipeline, plus the
//	                        family's own in-use guard where it declares one.
//	config.list_entries   — every entry of one family, in the same shape, so
//	                        a listing screen for a new family needs no
//	                        listing code either (#163).
//	config.test_entry     — the family's live probe, where it declares one:
//	                        a real request against the entry as SAVED, on the
//	                        doctor's real-probe discipline (#114). A family
//	                        without one is refused, never answered.
//
// A credential a family declares (entryFamilySpec.secrets) is stripped from
// every read and written only through a separate instruction —
// entry_admin_secrets.go holds that rule and nothing else does.
//
// Every rule lives here or below (ADR 0013): the QML form renders fields,
// ships drafts, and pins returned problems to inputs — it decides nothing.
// Saving never executes anything: a script draft's path is stat-ed by the
// validator, never run, and the entry schema has no argv or environment for
// form text to reach (ADR 0030 — the whitelist below is the shape check).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/automation"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/routine"
	"github.com/rpickz/jarvix/internal/session"
)

// entryProblem is one field-level validation problem: which form field it
// belongs to and the loader's own message. An empty field is a whole-entry
// (or whole-document) problem the form shows in its general error area.
type entryProblem struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// entryKeyKind names the wire shape one entry key accepts. The closed set is
// the point: an entry can only carry keys its family declares, so a draft
// smuggling an `args` or `env` key onto a script is refused by shape before
// any validator runs (ADR 0030's "no code path" stance, restated for forms).
type entryKeyKind int

const (
	entryKeyString entryKeyKind = iota
	entryKeyBool
	entryKeyInt
	entryKeyStringList
	entryKeyIntPair
	entryKeyTables
)

// entryFamilyShape names how a family's entries sit in the document. It is
// the one thing the generic pipeline must branch on, so it is a declared
// property of the family rather than a guess made per call site (#163).
type entryFamilyShape int

const (
	// entryShapeArray is the [[family]] array of tables the surface shipped
	// with (#99): an entry is one element of the array, carries its identity
	// in its own `name` key, and is addressed case-insensitively — the rule
	// every array family already uses for uniqueness.
	entryShapeArray entryFamilyShape = iota
	// entryShapeKeyed is the [family.<name>] map of tables (#163): the entry
	// IS its table key. It is addressed EXACTLY, because TOML keys and the
	// Go maps the loader decodes them into are case-sensitive, and matching
	// "OpenAI" to "openai" would edit a different endpoint than the one
	// `ai.provider` resolves. The wire shape is unchanged — a draft still
	// carries `name` — but that key renders as the table header rather than
	// as a stored field, so one form drives both shapes.
	entryShapeKeyed
	// entryShapeScalarMap is the [family] table whose entries are single
	// `key = "value"` LINES (#164): the speech lexicon, where the written form
	// is the key and the spoken form is the value. It is addressed EXACTLY, for
	// the keyed shape's reason, and the wire key its one value travels in is
	// declared (valueKey) because a line has no keys of its own to name it
	// with. The wire shape is unchanged again — a draft carries `name` and that
	// one value — so the third shape costs the form nothing.
	entryShapeScalarMap
)

// entrySecretSpec declares one key of a family that holds a credential. It is
// the registry's whole vocabulary for secrets, and everything the credential
// rules require follows from it: the key is stripped from every read, refused
// in every draft, and written only through the separate `secrets` instruction
// (entrySecretWrite) — a channel with exactly one destination, the file.
type entrySecretSpec struct {
	// key is the TOML key holding the credential itself.
	key string
	// envKey names the sibling key that points at an environment variable to
	// read the credential from instead — the preferred indirection, because a
	// key that is never in the file is a key a backup cannot copy
	// (config.Endpoint.Key prefers it).
	envKey string
	// label is what the form calls this credential in its own words.
	label string
}

// entryNote is something true about a saved draft that is not a problem: a
// consequence the user must SEE before they save, stated in their words and
// keyed to the field that causes it. The advisor tier is the case that
// demands it (ADR 0016) — a hand-written argv silently drops an advisor from
// allow to ask, and a form that did not say so would let someone loosen or
// tighten a permission gate without noticing.
type entryNote struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// entryFamilySpec declares one editable family: its shape, its wire keys with
// their value kinds, the rendering order (matching the documentation's worked
// examples), and the word activity rows use for one of its entries. This
// registry is the only family-specific code on the surface — #100 added
// knowledge feeds as a row here and #163 added the two map-shaped families,
// exactly as ADR 0033 planned. (Memory entries are NOT a row: memory.toml is
// not config.toml, and the memory book's own write path serves the window
// instead — memory_admin.go.)
type entryFamilySpec struct {
	family   string
	kind     string
	shape    entryFamilyShape
	keys     map[string]entryKeyKind
	keyOrder []string
	subKeys  map[string]map[string]entryKeyKind
	subOrder map[string][]string
	// idKey names the key carrying an ARRAY family's identity, "" meaning the
	// "name" every family but one uses. `[[intents.custom]]` is that one: its
	// identity is the phrase it matches (#164), and inventing a `name` key for
	// it would change a file format the published examples already use. The
	// keyed and scalar-map shapes are addressed by their TOML key instead, so
	// this says nothing about them — a draft still carries `name` on the wire
	// for every shape, which is what keeps one form driving all three.
	idKey string
	// valueKey names the wire key a SCALAR-MAP entry's single value travels in
	// ("spoken", for the lexicon). Empty for every other shape.
	valueKey string
	// phraseKeys names the keys holding this family's trigger phrases, so a
	// collision reported by ANOTHER entry — those quote the phrase and carry
	// their own label — still lands on the input the user has to change. A
	// string-list key contributes `key[i]` fields, a string key the key itself.
	// Empty means "phrases", the list every phrase-carrying family used before
	// #164 gave custom intents a single `match`.
	phraseKeys []string
	// reserved names the keys that share a keyed family's own table without
	// being entries — [ai]'s provider, model, system_prompt and friends. Only
	// the parser decides what is an entry; this set decides what an entry is
	// not. Empty for every other family.
	reserved map[string]bool
	// secrets lists this family's credential keys. Empty for a family that
	// holds none, which is what keeps the machinery inert rather than
	// conditional everywhere.
	secrets []entrySecretSpec
	// pending, when set, reports why a validly written entry cannot go live
	// on this daemon ("" when it can). It exists for the families with a
	// restart-class boundary: with zero feeds at boot there is no knowledge
	// service to adopt the first one (ADR 0031), and the advisor tool is
	// wired once at construction (ADR 0016), so the save's reply must say so
	// rather than let the window show a saved entry that never runs. A
	// registry field, not a family-specific branch in the flow — the pressure
	// valve ADR 0033 prescribed.
	pending func(d *Daemon) string
	// guardDelete, when set, reports why one entry of this family cannot be
	// removed ("" when it can), judged against the document as it stands. It
	// exists because whole-document validation cannot make every in-use
	// check: deleting the [ai.<name>] table of a PRESET endpoint still
	// validates, because the preset's defaults survive the file — while
	// leaving the user's chosen provider pointing at something they can no
	// longer see or edit.
	guardDelete func(cfg config.Config, name string) string
	// notes, when set, states what the draft's configuration EARNS, so the
	// form can show it beside the field that decides it.
	notes func(name string, draft map[string]any) []entryNote
	// preview, when set, computes what the SAVED draft would produce, for a
	// form that has something to show beside its fields. The routines family
	// is the case that demands it (#181, ADR 0059): a routine is a
	// description of a desktop, and the only other way to find out whether it
	// is the description you meant is to run it and watch six windows land.
	//
	// It is handed the rewritten document rather than the draft map, so the
	// picture is drawn from the values a LOAD would read — one conversion,
	// not a parallel one that could come to disagree — plus the problems
	// found so far, so an arrangement the loader already refused is not drawn
	// around. Nil result means "nothing to show", which is what an
	// unparsable draft and a family without a preview both are.
	preview func(d *Daemon, doc []byte, name string, problems []entryProblem) any
	// nameProblems lists substrings that mark a whole-document problem as
	// belonging on the name field even though it carries no `family.name.key`
	// label — the validators that word a bad name as a sentence about the
	// name rather than about a key of the entry.
	nameProblems []string
	// probe, when set, is this family's live Test action: a minimal real
	// request against the entry as the file configures it, on the doctor's
	// real-probe discipline (#114). Nil means config.test_entry refuses the
	// family rather than inventing a result for it.
	probe func(ctx context.Context, cfg config.Config, name string) map[string]any
	// assistantReason, when non-empty, is why the assistant's own config
	// tools may not reach this family at all — the entry half of #109's
	// exclusion wall, worded for speaking. A family carrying it is not
	// denied to the model, it is absent from the surface the model operates
	// on (config_admin_tools.go), exactly as an excluded SETTING is.
	assistantReason string
}

// entryAdminFamilies lists the families the window may edit (#99: the
// Automations tab's two; #100: knowledge feeds). The key sets mirror the
// config structs exactly — config.Routine/RoutineStep/Script/KnowledgeFeed —
// and a drift test pins that.
var entryAdminFamilies = map[string]entryFamilySpec{
	"routines": {
		family: "routines", kind: "routine",
		keys: map[string]entryKeyKind{
			"name": entryKeyString, "enabled": entryKeyBool,
			"phrases": entryKeyStringList, "schedule": entryKeyString,
			"announce": entryKeyBool, "steps": entryKeyTables,
		},
		keyOrder: []string{"name", "phrases", "schedule", "announce", "enabled", "steps"},
		// The step keys are the launching half plus the window-placement
		// vocabulary (ADR 0056), which is why they read as one list rather
		// than two: a step IS a launch and a placement. The three superseded
		// spellings (float, size, tile) stay declared because the form must
		// be able to save an entry a hand edit gave them — dropping them from
		// the whitelist would make a working routine unsavable — and the
		// renderer writes only the current vocabulary, so an entry the window
		// touches comes back migrated.
		subKeys: map[string]map[string]entryKeyKind{"steps": {
			"app": entryKeyString, "desktop_entry": entryKeyString,
			"args": entryKeyStringList, "identity": entryKeyString,
			"match": entryKeyString, "launch": entryKeyString,
			"workspace": entryKeyInt,
			"monitor":   entryKeyString, "mode": entryKeyString,
			"width": entryKeyString, "height": entryKeyString,
			"position": entryKeyIntPair, "place_next": entryKeyString,
			"master": entryKeyBool, "focus": entryKeyString,
			"float": entryKeyBool, "size": entryKeyIntPair, "tile": entryKeyString,
		}},
		subOrder: map[string][]string{"steps": {
			"app", "desktop_entry", "args", "identity", "match", "launch",
			"workspace", "monitor", "mode", "width", "height",
			"position", "place_next", "master", "focus", "float", "size", "tile",
		}},
		notes:   routineInstallNotes,
		preview: routinePreview,
	},
	"scripts": {
		family: "scripts", kind: "script",
		keys: map[string]entryKeyKind{
			"name": entryKeyString, "enabled": entryKeyBool,
			"phrases": entryKeyStringList, "path": entryKeyString,
			"timeout_sec": entryKeyInt, "report": entryKeyString,
			"schedule": entryKeyString, "announce": entryKeyBool,
		},
		keyOrder: []string{"name", "phrases", "path", "timeout_sec", "report", "schedule", "announce", "enabled"},
	},
	// The Knowledge tab's feeds (#100, ADR 0031). The command is a string
	// list on the wire like phrases — the fixed argv, never a shell line —
	// and writing it never runs it: the pipeline below has no exec, and the
	// whole-document validation only inspects the value. Fetching stays
	// behind the existing knowledge.refresh gate, untouched by how the entry
	// got into the file.
	"knowledge.feeds": {
		family: "knowledge.feeds", kind: "feed",
		keys: map[string]entryKeyKind{
			"name": entryKeyString, "enabled": entryKeyBool,
			"description": entryKeyString, "command": entryKeyStringList,
			"mode": entryKeyString, "interval_sec": entryKeyInt,
			"ttl_sec": entryKeyInt, "timeout_sec": entryKeyInt,
			"inject": entryKeyBool,
		},
		keyOrder: []string{"name", "description", "command", "mode",
			"interval_sec", "ttl_sec", "timeout_sec", "inject", "enabled"},
		// With zero feeds at boot there is no service, no registered tool,
		// and applyRuntime pins the [knowledge] section (settings.go) — the
		// first feed takes a restart (ADR 0031), and the save must say so.
		pending: func(d *Daemon) string {
			if d.knowledge == nil {
				return "the first knowledge feed needs a daemon restart to start fetching " +
					"(restart jarvixd; every later feed change applies on the standard reload)"
			}
			return ""
		},
	},
	// The two map-shaped families (#163, ADR 0052), declared in
	// entry_admin_providers.go because what they add beyond a key list —
	// a credential, a live probe, an in-use guard, an earned permission tier
	// — is family knowledge, and this map is the index, not the encyclopaedia.
	"ai":       aiEndpointFamily,
	"advisors": advisorFamily,
	// The last two config-file holdouts (#164, ADR 0054), declared in
	// entry_admin_holdouts.go: the phrases the user invents and the words the
	// voice says wrongly. The first is the only array family whose identity is
	// not a `name`; the second is the first of the third document shape.
	"intents.custom": customIntentFamily,
	"tts.lexicon":    lexiconFamily,
}

// assistantEntryFamily resolves a family name for the ASSISTANT's config
// tools. It is entryFamily minus the families the model may not administer at
// all (#109's exclusion wall, the entry half): [ai] is the brain and the
// credentials that buy it, and [advisors] are commands the daemon executes
// with the user's own authentication and whose tiers feed the permission gate.
// Both are refused structurally — a family the tool surface does not have —
// with the spoken-ready reason the registry declares, exactly as an excluded
// SETTING is refused by AssistantExcludedSettingReason.
func assistantEntryFamily(family string) (entryFamilySpec, *ipc.Error) {
	spec, ipcErr := entryFamily(family)
	if ipcErr != nil {
		return spec, ipcErr
	}
	if spec.assistantReason != "" {
		return entryFamilySpec{}, ipc.Errorf(ipc.CodeInvalidParams, "%s", spec.assistantReason)
	}
	return spec, nil
}

// assistantEntryFamilies lists the families the assistant MAY administer,
// sorted — the set the tool layer's own closed list is pinned to.
func assistantEntryFamilies() []string {
	names := make([]string, 0, len(entryAdminFamilies))
	for name, spec := range entryAdminFamilies {
		if spec.assistantReason == "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// tableLabel is how this family's tables are written in config.toml, for
// messages that name the shape rather than the entry: `[[routines]]` for an
// array family, `[ai.<name>]` for a keyed one.
func (s entryFamilySpec) tableLabel() string {
	if s.shape == entryShapeKeyed {
		return "[" + s.family + ".<name>]"
	}
	return "[[" + s.family + "]]"
}

// identity is the key an entry of this family carries its name in — "name"
// unless the family declares otherwise. An accessor rather than a default
// filled in at registration, so a row that omits it cannot be half-initialised
// by whichever code path happened to read it first.
func (s entryFamilySpec) identity() string {
	if s.idKey != "" {
		return s.idKey
	}
	return "name"
}

// entryName reads a draft's identity: the value of its identity key.
func (s entryFamilySpec) entryName(draft map[string]any) string {
	name, _ := draft[s.identity()].(string)
	return name
}

// phraseFields maps each trigger phrase in a draft to the form field holding
// it. See phraseKeys for why this is declared rather than hard-coded.
func (s entryFamilySpec) phraseFields(draft map[string]any) map[string]string {
	keys := s.phraseKeys
	if len(keys) == 0 {
		keys = []string{"phrases"}
	}
	out := map[string]string{}
	for _, key := range keys {
		switch v := draft[key].(type) {
		case string:
			out[key] = v
		case []string:
			for i, phrase := range v {
				out[fmt.Sprintf("%s[%d]", key, i)] = phrase
			}
		}
	}
	return out
}

// secretFor returns the declaration of key as a credential, or nil when the
// family holds none by that name.
func (s entryFamilySpec) secretFor(key string) *entrySecretSpec {
	for i := range s.secrets {
		if s.secrets[i].key == key {
			return &s.secrets[i]
		}
	}
	return nil
}

// entryFamily resolves a wire family name against the registry.
func entryFamily(family string) (entryFamilySpec, *ipc.Error) {
	if spec, ok := entryAdminFamilies[family]; ok {
		return spec, nil
	}
	names := make([]string, 0, len(entryAdminFamilies))
	for name := range entryAdminFamilies {
		names = append(names, strconv.Quote(name))
	}
	sort.Strings(names)
	return entryFamilySpec{}, ipc.Errorf(ipc.CodeInvalidParams,
		"family %q is not editable from the window; the editable families are %s",
		family, strings.Join(names, ", "))
}

// The shape dispatch. Four one-line functions are the ENTIRE cost of a second
// document shape on this surface: every handler below reads, rewrites, and
// removes an entry through these, so the pipeline (fingerprint guard,
// whole-document validation, atomic write, reload, events) is one piece of
// code that never learns which shape it is serving.

// entryReadValue reads one entry back as the parser sees it. For a keyed
// family the table key is folded in as `name`, so both shapes hand the form
// the same map: identity in `name`, everything else as written.
func entryReadValue(spec entryFamilySpec, raw []byte, name string) (map[string]any, bool, error) {
	if spec.shape == entryShapeScalarMap {
		return config.ScalarMapEntryValue(raw, spec.family, spec.valueKey, name, spec.reserved)
	}
	if spec.shape == entryShapeKeyed {
		entry, ok, err := config.KeyedEntryValue(raw, spec.family, name, spec.reserved)
		if !ok || err != nil {
			return nil, ok, err
		}
		out := make(map[string]any, len(entry)+1)
		for k, v := range entry {
			out[k] = v
		}
		out["name"] = strings.TrimSpace(name)
		return out, true, nil
	}
	return config.EntryValue(raw, spec.family, spec.identity(), name)
}

// entryRewriteUpsert writes one whole entry into the document, byte-preserving
// everything outside its block.
func entryRewriteUpsert(spec entryFamilySpec, raw []byte, name string, draft map[string]any) ([]byte, error) {
	switch spec.shape {
	case entryShapeScalarMap:
		return config.UpsertScalarMapEntryTOML(raw, spec.family, spec.valueKey, name, draft, spec.reserved)
	case entryShapeKeyed:
		return config.UpsertKeyedEntryTOML(raw, spec.family, name, draft, spec.keyOrder, spec.reserved)
	}
	return config.UpsertEntryTOML(raw, spec.family, spec.identity(), name, draft, spec.keyOrder, spec.subOrder)
}

// entryRewriteDelete removes one entry from the document.
func entryRewriteDelete(spec entryFamilySpec, raw []byte, name string) ([]byte, error) {
	switch spec.shape {
	case entryShapeScalarMap:
		return config.DeleteScalarMapEntryTOML(raw, spec.family, name, spec.reserved)
	case entryShapeKeyed:
		return config.DeleteKeyedEntryTOML(raw, spec.family, name, spec.reserved)
	}
	return config.DeleteEntryTOML(raw, spec.family, spec.identity(), name)
}

// entryShapeProblems reports what a draft's IDENTITY makes impossible, for the
// two shapes whose identity is a TOML key rather than a value inside the entry.
//
// It exists because the shapes fail differently. An array family's empty or
// duplicated name is caught by the loader's own validators, which word it and
// name the entry. A keyed duplicate is a duplicate TOML TABLE and a scalar-map
// duplicate a duplicate KEY — the rewritten document simply does not parse, and
// the read-back guard would refuse the save with "unparsable document", which
// is true and useless. An empty one never reaches validation at all, because
// there is no table to write it into. The user typed a name; the form should
// say what is wrong with it, on the name field.
func entryShapeProblems(spec entryFamilySpec, raw []byte, target string, draft map[string]any) []entryProblem {
	if spec.shape == entryShapeArray {
		return nil
	}
	name, _ := draft["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		if spec.shape == entryShapeScalarMap {
			return []entryProblem{{Field: "name", Message: fmt.Sprintf(
				"the written form is empty; it is the word to respell and becomes the key in "+
					"[%s]", spec.family)}}
		}
		return []entryProblem{{Field: "name", Message: fmt.Sprintf(
			"the name is empty; it becomes the [%s.<name>] table this entry is written as",
			spec.family)}}
	}
	if name == strings.TrimSpace(target) {
		return nil
	}
	names, err := entryNamesOf(spec, raw)
	if err != nil {
		return nil // the document does not parse; the pipeline says so with its own words
	}
	for _, existing := range names {
		if existing != name {
			continue
		}
		if spec.shape == entryShapeScalarMap {
			return []entryProblem{{Field: "name", Message: fmt.Sprintf(
				"[%s] already has an entry for %q; choose another written form, or open that "+
					"one to edit it", spec.family, name)}}
		}
		return []entryProblem{{Field: "name", Message: fmt.Sprintf(
			"there is already a [%s.%s] table; choose another name, or open that one to edit it",
			spec.family, name)}}
	}
	return nil
}

// entryLabel is how this family's validators label one entry in a
// whole-document problem — the prefix classifyEntryProblem strips to find the
// field a message belongs to. Arrays label by position (`routines[2]`), keyed
// tables by their key (`ai.openai`), because that is what each family's
// validator already writes.
func entryLabel(spec entryFamilySpec, index int, name string) (string, bool) {
	if spec.shape != entryShapeArray {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return "", false
		}
		return spec.family + "." + trimmed, true
	}
	if index < 0 {
		return "", false
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return fmt.Sprintf("%s[%d] (%q)", spec.family, index, trimmed), true
	}
	return fmt.Sprintf("%s[%d]", spec.family, index), true
}

// Entry-change sources: who drove one write through this surface. On the
// config.entry_changed event and in the activity row's wording, because "the
// window saved this" and "the assistant saved this" are different facts to
// audit (issue #105) even though the pipeline underneath is one and the same.
const (
	entrySourceWindow    = "window"
	entrySourceAssistant = "assistant"
)

// registerEntryAdminMethods adds the form dialog surface (#99). The handlers
// are named methods rather than closures because the assistant's config tools
// (issue #105) invoke the very same functions in-process — one write path,
// with only the source label differing.
func (d *Daemon) registerEntryAdminMethods() {
	d.server.Handle("config.get_entry", d.entryAdminGet)
	d.server.Handle("config.validate_entry", d.entryAdminValidate)
	d.server.Handle("config.upsert_entry", func(params json.RawMessage) (any, error) {
		return d.entryAdminUpsert(params, entrySourceWindow)
	})
	d.server.Handle("config.delete_entry", func(params json.RawMessage) (any, error) {
		return d.entryAdminDelete(params, entrySourceWindow)
	})
	d.server.Handle("config.test_entry", d.entryAdminTest)
	d.server.Handle("config.list_entries", d.entryAdminList)
}

// entryAdminList serves config.list_entries: every entry of one family, from
// the file, in the same secret-stripped shape config.get_entry returns one.
//
// It is registry-driven rather than summarised per family (the shape the
// assistant's bridge builds, which needs typed fields for a spoken sentence):
// a listing screen renders whichever keys it has widgets for, so handing it
// the whole entry means a family added to the registry needs no listing code
// at all — the same claim the form already makes about the four write verbs.
func (d *Daemon) entryAdminList(params json.RawMessage) (any, error) {
	var p struct {
		Family string `json:"family"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "config.list_entries params: %v", err)
		}
	}
	spec, ipcErr := entryFamily(p.Family)
	if ipcErr != nil {
		return nil, ipcErr
	}
	raw, err := os.ReadFile(d.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	names, err := entryNamesOf(spec, raw)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
	}
	entries := make([]map[string]any, 0, len(names))
	for _, name := range names {
		entry, ok, err := entryReadValue(spec, raw, name)
		if err != nil || !ok {
			continue
		}
		row := map[string]any{"entry": stripEntrySecrets(spec, entry)}
		if states := entrySecretStates(spec, entry); len(states) > 0 {
			row["secrets"] = states
		}
		if spec.notes != nil {
			row["notes"] = spec.notes(name, entry)
		}
		entries = append(entries, row)
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	result := map[string]any{
		"fingerprint": fp, "family": spec.family, "kind": spec.kind,
		"entries": entries,
	}
	// The one fact a Providers listing needs that no entry carries: which
	// endpoint `ai.provider` selects, so the row can say "in use" and the
	// delete button can explain itself before it is pressed.
	if spec.family == "ai" {
		result["in_use"] = d.runningConfig().AI.Provider
	}
	return result, nil
}

// entryNamesOf lists one family's entry names in document order (arrays) or
// sorted (keyed tables) — the order each shape can promise.
func entryNamesOf(spec entryFamilySpec, raw []byte) ([]string, error) {
	switch spec.shape {
	case entryShapeScalarMap:
		return config.ScalarMapEntryNames(raw, spec.family, spec.reserved)
	case entryShapeKeyed:
		return config.KeyedEntryNames(raw, spec.family, spec.reserved)
	}
	return config.EntryNames(raw, spec.family, spec.identity())
}

// entryAdminGet serves config.get_entry: one whole entry, straight from the
// file the form will edit, paired with that file's fingerprint so the two can
// never be from different versions of the document.
func (d *Daemon) entryAdminGet(params json.RawMessage) (any, error) {
	var p struct {
		Family string `json:"family"`
		Name   string `json:"name"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "config.get_entry params: %v", err)
		}
	}
	spec, ipcErr := entryFamily(p.Family)
	if ipcErr != nil {
		return nil, ipcErr
	}
	raw, err := os.ReadFile(d.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	entry, ok, err := entryReadValue(spec, raw, p.Name)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
	}
	if !ok {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "no %s entry is named %q", spec.kind, p.Name)
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	// The credential leaves the entry here and goes no further: what the wire
	// carries about it is presence, not value (entrySecretStates).
	secrets := entrySecretStates(spec, entry)
	result := map[string]any{
		"fingerprint": fp, "family": spec.family,
		"entry": stripEntrySecrets(spec, entry),
	}
	if len(secrets) > 0 {
		result["secrets"] = secrets
	}
	if spec.notes != nil {
		result["notes"] = spec.notes(spec.entryName(entry), entry)
	}
	return result, nil
}

// entryAdminValidate serves config.validate_entry: the dry run behind live
// field errors and the next-fire preview. Same pipeline as the save, minus
// the write — so a form that validated clean can still be refused at save
// time only by the world moving (a fingerprint conflict), never by a rule it
// was not shown.
func (d *Daemon) entryAdminValidate(params json.RawMessage) (any, error) {
	p, spec, draft, problems, ipcErr := decodeEntryParams("config.validate_entry", params)
	if ipcErr != nil {
		return nil, ipcErr
	}
	result := map[string]any{}
	if next, ok := entryNextFire(draft); ok {
		result["next_fire"] = next
	}
	raw, err := os.ReadFile(d.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	// The credentials are folded into the draft before anything looks at it,
	// so the dry run validates the document that a save would produce — and
	// the scrubber below is what guarantees the values cannot come back out
	// of it, whatever any validator chooses to quote.
	scrub, secretProblems := d.applyEntrySecrets(spec, raw, p.Name, draft, p.Secrets)
	problems = append(problems, secretProblems...)
	problems = append(problems, entryShapeProblems(spec, raw, p.Name, draft)...)
	// newRaw is the document the save would write. It is kept beyond the
	// validation because the preview is drawn from it (#181): the picture must
	// come from the values a LOAD would read, not from a second reading of the
	// draft map, and it is worth drawing even when a problem was found — a
	// workspace whose numbers are fine still shows, and one whose numbers are
	// not says so where the problem is.
	var newRaw []byte
	if len(problems) == 0 {
		var err error
		newRaw, err = entryRewriteUpsert(spec, raw, p.Name, draft)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%s", scrub(err.Error()))
		}
		problems = d.entryDocProblems(newRaw, spec, draft)
	}
	if spec.notes != nil {
		result["notes"] = spec.notes(spec.entryName(draft), draft)
	}
	if spec.preview != nil {
		if preview := spec.preview(d, newRaw, spec.entryName(draft), problems); preview != nil {
			result["preview"] = preview
		}
	}
	result["valid"] = len(problems) == 0
	result["problems"] = scrubProblems(scrub, problems)
	return result, nil
}

// entryAdminUpsert serves config.upsert_entry: the save. Fingerprint-guarded
// like automations.set_enabled, validated whole before anything lands,
// written atomically, applied by the standard reload — which is what
// recompiles the grammar and rebuilds the schedules — and announced on the
// activity feed naming the entry and who saved it.
func (d *Daemon) entryAdminUpsert(params json.RawMessage, source string) (map[string]any, error) {
	p, spec, draft, problems, ipcErr := decodeEntryParams("config.upsert_entry", params)
	if ipcErr != nil {
		return nil, ipcErr
	}
	raw, err := os.ReadFile(d.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	scrub, secretProblems := d.applyEntrySecrets(spec, raw, p.Name, draft, p.Secrets)
	problems = append(problems, secretProblems...)
	problems = append(problems, entryShapeProblems(spec, raw, p.Name, draft)...)
	if len(problems) > 0 {
		return nil, entryProblemsError(scrubProblems(scrub, problems))
	}
	return d.writeEntryChange(spec, p.Fingerprint, p.Name == "",
		func(raw []byte) ([]byte, error) {
			return entryRewriteUpsert(spec, raw, p.Name, draft)
		}, draft, source, scrub)
}

// entryAdminDelete serves config.delete_entry: confirmed removal. The same
// whole-document validation guards it — a delete that would leave the file
// invalid is refused with the problems, nothing written.
func (d *Daemon) entryAdminDelete(params json.RawMessage, source string) (map[string]any, error) {
	var p struct {
		Family      string `json:"family"`
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "config.delete_entry params: %v", err)
		}
	}
	spec, ipcErr := entryFamily(p.Family)
	if ipcErr != nil {
		return nil, ipcErr
	}
	// The in-use guard runs before anything is rewritten, against the file as
	// it stands, so a refusal is a refusal with a reason rather than a
	// document that failed to validate for a reason nobody would connect to
	// the delete button they pressed.
	if spec.guardDelete != nil {
		raw, err := os.ReadFile(d.paths.ConfigFile())
		if err != nil && !os.IsNotExist(err) {
			return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
		}
		cfg, err := config.ParseBytes(raw)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		if reason := spec.guardDelete(cfg, p.Name); reason != "" {
			return nil, entryProblemsError([]entryProblem{{Message: reason}})
		}
	}
	result, err := d.writeEntryChange(spec, p.Fingerprint, false,
		func(raw []byte) ([]byte, error) {
			return entryRewriteDelete(spec, raw, p.Name)
		}, nil, source, nil)
	if err != nil {
		return nil, err
	}
	d.publishEntryChanged("deleted", spec, p.Name, source)
	return result, nil
}

// decodeEntryParams reads the shared {family, name?, entry} params and
// sanitises the draft against the family's key whitelist. Shape problems come
// back as field-level entries so the form can pin "timeout_sec must be a
// whole number" to its input exactly like a validator message.
func decodeEntryParams(method string, params json.RawMessage) (
	p struct {
		Family      string                      `json:"family"`
		Name        string                      `json:"name"`
		Entry       json.RawMessage             `json:"entry"`
		Fingerprint string                      `json:"fingerprint"`
		Secrets     map[string]entrySecretWrite `json:"secrets"`
	}, spec entryFamilySpec, draft map[string]any, problems []entryProblem, ipcErr *ipc.Error) {
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return p, spec, nil, nil, ipc.Errorf(ipc.CodeInvalidParams, "%s params: %v", method, err)
		}
	}
	spec, ipcErr = entryFamily(p.Family)
	if ipcErr != nil {
		return p, spec, nil, nil, ipcErr
	}
	if len(p.Entry) == 0 {
		return p, spec, nil, nil, ipc.Errorf(ipc.CodeInvalidParams, "%s: entry is required", method)
	}
	dec := json.NewDecoder(bytes.NewReader(p.Entry))
	dec.UseNumber()
	var loose map[string]any
	if err := dec.Decode(&loose); err != nil {
		return p, spec, nil, nil, ipc.Errorf(ipc.CodeInvalidParams, "%s: entry must be an object: %v", method, err)
	}
	draft, problems = sanitizeEntry(spec, loose)
	return p, spec, draft, problems, nil
}

// writeEntryChange is the shared save pipeline: fingerprint check, rewrite,
// whole-document validation, atomic write, standard reload, events. rewrite
// is the byte-preserving editor call; draft is nil for a delete. On any
// refusal the file is untouched — the mutation tests pin it.
func (d *Daemon) writeEntryChange(spec entryFamilySpec, fingerprint string, created bool,
	rewrite func([]byte) ([]byte, error), draft map[string]any, source string,
	scrub func(string) string) (map[string]any, error) {
	if scrub == nil {
		scrub = func(s string) string { return s }
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
	if fingerprint != "" && fingerprint != fp {
		return nil, &ipc.Error{
			Code: ipc.CodeConfigConflict,
			Message: "config.toml changed outside the window since the form was opened; " +
				"nothing was saved — close and reopen the form to edit the current file",
			Data: map[string]any{"fingerprint": fp},
		}
	}

	newRaw, err := rewrite(raw)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%s", scrub(err.Error()))
	}
	if problems := d.entryDocProblems(newRaw, spec, draft); len(problems) > 0 {
		return nil, entryProblemsError(scrubProblems(scrub, problems))
	}
	fileCfg, err := config.ParseBytes(newRaw)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "rewrite config: %s", scrub(err.Error()))
	}
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := config.WriteFileAtomic(path, newRaw); err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "write config: %v", err)
	}

	applied, reason := d.applyRuntime(fileCfg)
	reason = scrub(reason)
	// A family behind a restart-class boundary (the first knowledge feed)
	// was written and "applied" only in the sense that nothing else changed;
	// the entry itself is not live, and honesty beats a row that pretends the
	// scheduler already knows it (the applied=false contract the window
	// already words).
	if applied && spec.pending != nil {
		if note := spec.pending(d); note != "" {
			applied, reason = false, note
		}
	}
	newFP := config.Fingerprint(newRaw)
	d.publishConfigChanged(newFP)
	if draft != nil {
		action := "edited"
		if created {
			action = "created"
		}
		d.publishEntryChanged(action, spec, spec.entryName(draft), source)
	}
	result := map[string]any{
		"fingerprint": newFP,
		"applied":     applied,
	}
	if draft != nil {
		result["created"] = created
	}
	if reason != "" {
		result["reason"] = reason
	}
	return result, nil
}

// publishEntryChanged announces one save on the bus. The activity watcher
// renders it into the feed row the acceptance criteria require — create,
// edit, and delete each naming the entry and its source (the window's form
// or the assistant's tool) — and any open window's listing refreshes off the
// config.changed event that accompanied it.
func (d *Daemon) publishEntryChanged(action string, spec entryFamilySpec, name, source string) {
	d.bus.Publish(session.Event{Type: "config.entry_changed", Data: map[string]any{
		"action": action, "family": spec.family, "kind": spec.kind, "name": name,
		"source": source,
	}})
}

// entryProblemsError wraps field-level problems in the standard validation
// refusal: the same code automations.set_enabled uses, with structured
// problems in place of its flat strings.
func entryProblemsError(problems []entryProblem) *ipc.Error {
	return &ipc.Error{
		Code:    ipc.CodeConfigInvalid,
		Message: "the entry was rejected by validation; nothing was written",
		Data:    map[string]any{"problems": problems},
	}
}

// sanitizeEntry checks a decoded draft against the family's key whitelist and
// converts it to the typed map the TOML renderer takes. Problems are keyed to
// the offending field. An empty sub-table list is dropped rather than kept:
// TOML cannot express "steps = []", and the whole-document validator is the
// one that says "it has no steps" with the loader's wording.
func sanitizeEntry(spec entryFamilySpec, loose map[string]any) (map[string]any, []entryProblem) {
	draft := make(map[string]any, len(loose))
	var problems []entryProblem
	for key, value := range loose {
		kind, ok := spec.keys[key]
		if !ok {
			if spec.secretFor(key) != nil {
				// A credential in the draft is refused by SHAPE, not by
				// policy: the entry map is echoed to the form, logged on the
				// way through, and quoted in problems, and a secret must
				// never travel in something with that many destinations. It
				// has its own write-only channel (entrySecretWrite) and this
				// message says so — without repeating what was sent.
				problems = append(problems, entryProblem{Field: key, Message: fmt.Sprintf(
					"%q is never sent inside the entry; set it through the credential field, "+
						"which is write-only", key)})
				continue
			}
			problems = append(problems, entryProblem{Field: key, Message: fmt.Sprintf(
				"%q is not a %s key the window can write; remove it from the draft",
				key, spec.tableLabel())})
			continue
		}
		if kind == entryKeyTables {
			tables, tableProblems := sanitizeEntryTables(spec, key, value)
			problems = append(problems, tableProblems...)
			if len(tables) > 0 && len(tableProblems) == 0 {
				draft[key] = tables
			}
			continue
		}
		converted, problem := coerceEntryValue(kind, value)
		if problem != "" {
			problems = append(problems, entryProblem{Field: key, Message: problem})
			continue
		}
		draft[key] = converted
	}
	return draft, problems
}

// sanitizeEntryTables converts one array-of-tables value ([[routines.steps]])
// element-wise, keying problems as "steps[2].workspace".
func sanitizeEntryTables(spec entryFamilySpec, key string, value any) ([]map[string]any, []entryProblem) {
	list, ok := value.([]any)
	if !ok {
		return nil, []entryProblem{{Field: key, Message: fmt.Sprintf("%s must be a list of tables", key)}}
	}
	allowed := spec.subKeys[key]
	var problems []entryProblem
	tables := make([]map[string]any, 0, len(list))
	for i, raw := range list {
		table, ok := raw.(map[string]any)
		if !ok {
			problems = append(problems, entryProblem{Field: fmt.Sprintf("%s[%d]", key, i),
				Message: fmt.Sprintf("%s[%d] must be a table", key, i)})
			continue
		}
		converted := make(map[string]any, len(table))
		for sk, sv := range table {
			kind, ok := allowed[sk]
			field := fmt.Sprintf("%s[%d].%s", key, i, sk)
			if !ok {
				problems = append(problems, entryProblem{Field: field, Message: fmt.Sprintf(
					"%q is not a %s key the window can write; remove it from the draft", sk, key)})
				continue
			}
			value, problem := coerceEntryValue(kind, sv)
			if problem != "" {
				problems = append(problems, entryProblem{Field: field, Message: problem})
				continue
			}
			converted[sk] = value
		}
		tables = append(tables, converted)
	}
	return tables, problems
}

// coerceEntryValue converts one JSON-decoded value to the shape its key
// declares. Messages are actionable and generic — they are shown under the
// field verbatim.
func coerceEntryValue(kind entryKeyKind, value any) (any, string) {
	switch kind {
	case entryKeyString:
		s, ok := value.(string)
		if !ok {
			return nil, "must be text"
		}
		return s, ""
	case entryKeyBool:
		b, ok := value.(bool)
		if !ok {
			return nil, "must be true or false"
		}
		return b, ""
	case entryKeyInt:
		n, ok := value.(json.Number)
		if !ok {
			return nil, "must be a whole number"
		}
		i, err := n.Int64()
		if err != nil {
			return nil, "must be a whole number"
		}
		return i, ""
	case entryKeyStringList:
		list, ok := value.([]any)
		if !ok {
			return nil, "must be a list of text values"
		}
		out := make([]string, len(list))
		for i, e := range list {
			s, ok := e.(string)
			if !ok {
				return nil, "must be a list of text values"
			}
			out[i] = s
		}
		return out, ""
	case entryKeyIntPair:
		list, ok := value.([]any)
		if !ok || len(list) != 2 {
			return nil, "must be a pair of whole numbers, like [1280, 720]"
		}
		out := make([]int, 2)
		for i, e := range list {
			n, ok := e.(json.Number)
			if !ok {
				return nil, "must be a pair of whole numbers, like [1280, 720]"
			}
			v, err := n.Int64()
			if err != nil {
				return nil, "must be a pair of whole numbers, like [1280, 720]"
			}
			out[i] = int(v)
		}
		return out, ""
	}
	return nil, "unsupported value"
}

// entryNextFire computes the schedule preview: the next moment the draft's
// schedule would fire, from the same arithmetic the scheduler runs (ADR
// 0032). Only a parseable schedule has one; its parse problems surface
// through the whole-document validation instead.
func entryNextFire(draft map[string]any) (string, bool) {
	schedule, _ := draft["schedule"].(string)
	if strings.TrimSpace(schedule) == "" {
		return "", false
	}
	spec, err := automation.ParseSpec(schedule)
	if err != nil {
		return "", false
	}
	return spec.Next(time.Now()).Format(time.RFC3339), true
}

// entryDocProblems validates a rewritten document whole — the loader's own
// Validate, no second copy of any rule — and keys each problem to the form
// field it names. draft nil (a delete) keeps every problem whole-entry.
func (d *Daemon) entryDocProblems(newRaw []byte, spec entryFamilySpec, draft map[string]any) []entryProblem {
	cfg, err := config.ParseBytes(newRaw)
	if err != nil {
		return []entryProblem{{Message: err.Error()}}
	}
	cfg.Voices = cfg.InstalledVoices(d.paths)
	err = cfg.Validate()
	if err == nil {
		return nil
	}
	if draft == nil {
		var out []entryProblem
		for _, msg := range validationProblems(err) {
			out = append(out, entryProblem{Message: msg})
		}
		return out
	}
	name := spec.entryName(draft)
	index := -1
	if spec.shape == entryShapeArray {
		index = entryIndexByName(newRaw, spec, name)
	}
	phrases := spec.phraseFields(draft)
	var out []entryProblem
	for _, msg := range validationProblems(err) {
		out = append(out, classifyEntryProblem(spec, index, name, phrases, msg))
	}
	return out
}

// routineInstallNotes says which of a routine's steps this machine cannot
// launch RIGHT NOW — and says it as a note rather than a problem (#175).
//
// The distinction is the whole of this function. "Is this step well formed?"
// is a fact about the entry and is a refusal; "is that program installed
// here?" is a fact about the machine at this moment, and refusing the save
// over it would make config.toml unwritable exactly where it most needs
// editing: a new laptop, a machine being set up, an application the user is
// about to install. A person must be able to author the routine first and
// install the program afterwards, and one authored on a desktop must remain
// editable from a machine that has none of it.
//
// So the save succeeds and the form shows a caution the user can save
// through. The enforcement point is the RUN, which resolves the same way from
// the same code and reports "discord is not installed" by name, skipping the
// step rather than waiting eight seconds for a window — which is where the
// acceptance criterion this ticket was written for actually bites.
//
// A missing DESKTOP ENTRY stays a refusal, and stays in whole-document
// validation rather than here: an entry id is resolved out of the machine's
// own applications index, nothing installs one under a name the user invented,
// and there is no "not yet" reading of it — it is a typo.
func routineInstallNotes(name string, draft map[string]any) []entryNote {
	steps := entryDraftTables(draft, "steps")
	if len(steps) == 0 {
		return nil
	}
	def := routine.Definition{Name: name}
	for _, raw := range steps {
		def.Steps = append(def.Steps, routine.Step{
			App:          entryDraftString(raw, "app"),
			DesktopEntry: entryDraftString(raw, "desktop_entry"),
			Args:         entryDraftStrings(raw, "args"),
			Identity:     entryDraftString(raw, "identity"),
		})
	}
	var out []entryNote
	for _, p := range routine.InstallProblems(def, routine.MachineResolver([]routine.Definition{def})) {
		out = append(out, entryNote{
			Field: fmt.Sprintf("steps[%d].%s", p.Step, p.Field),
			Message: fmt.Sprintf("%s on this computer right now. The routine saves either way; "+
				"until it is installed, this step is skipped when the routine runs and the "+
				"summary says so.", p.Message),
		})
	}
	return out
}

// entryDraftTables reads a list of sub-tables out of a loosely-typed draft.
// Both shapes the pipeline produces are accepted: the sanitiser's own
// []map[string]any, and the []any a decoded JSON entry arrives as.
func entryDraftTables(draft map[string]any, key string) []map[string]any {
	switch value := draft[key].(type) {
	case []map[string]any:
		return value
	case []any:
		out := make([]map[string]any, 0, len(value))
		for _, item := range value {
			if table, ok := item.(map[string]any); ok {
				out = append(out, table)
			}
		}
		return out
	}
	return nil
}

// entryDraftStrings reads a string list out of a loosely-typed draft.
func entryDraftStrings(draft map[string]any, key string) []string {
	switch value := draft[key].(type) {
	case []string:
		return append([]string(nil), value...)
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// entryIndexByName finds the draft's position in the rewritten document, for
// building the label prefix the validators use.
func entryIndexByName(doc []byte, spec entryFamilySpec, name string) int {
	index, ok, err := config.EntryIndex(doc, spec.family, spec.identity(), name)
	if err != nil || !ok {
		return -1
	}
	return index
}

// entryStepField matches a step-scoped problem tail: "steps[2]: app is
// empty; ...".
var entryStepField = regexp.MustCompile(`^steps\[(\d+)\]: `)

// classifyEntryProblem keys one whole-document problem to the form field it
// belongs to. The validators' labels are the contract: every family labels
// its entries `family[i] ("name")` (or `family[i]` when the name is empty)
// and its steps `... steps[j]`, and the intent router quotes the colliding
// phrase — so a prefixed problem is stripped to its message and matched on
// its leading token, and an unprefixed collision (the OTHER entry complains
// when the draft steals its phrase, because it compiles later) still lands on
// the draft's phrase field when the quoted phrase is one of the draft's. A
// problem this function cannot place keeps field "" and shows in the form's
// general area — never dropped.
func classifyEntryProblem(spec entryFamilySpec, index int, name string,
	phrases map[string]string, msg string) entryProblem {
	labelled := msg
	prefixed := false
	if label, ok := entryLabel(spec, index, name); ok {
		if spec.shape != entryShapeArray {
			// A keyed family's validators write `family.name.key …` — the
			// dotted path a user would type — so the label and the field are
			// separated by a dot, not a colon or a bracket.
			if rest, ok := strings.CutPrefix(msg, label+"."); ok {
				token := strings.TrimSuffix(strings.SplitN(rest, " ", 2)[0], ":")
				if _, known := spec.keys[token]; known {
					return entryProblem{Field: token, Message: msg}
				}
				return entryProblem{Message: msg}
			}
		}
		if rest, ok := strings.CutPrefix(msg, label+": "); ok {
			labelled, prefixed = rest, true
		} else if rest, ok := strings.CutPrefix(msg, label+" "); ok {
			// The step labels: `routines[0] ("x") steps[1]: ...`.
			labelled, prefixed = rest, true
		}
	}
	// The bare positional label, for an array family whose validators do not
	// quote the entry's identity in it. `[[intents.custom]]` is the case (#164):
	// the router writes `intents.custom[2]: match "…" is already …`, because a
	// custom intent's identity IS the phrase the message already quotes, and
	// repeating it in the prefix would say it twice.
	//
	// The position in the label is not trusted, and deliberately so: when a
	// draft's identity DUPLICATES an existing entry's, the index we resolved for
	// it is the existing entry's — that is what the collision means — so the
	// label carries a different number than the message does. What is trusted
	// instead is that the message quotes the draft's own identity, which is the
	// same evidence the phrase-collision branch below already relies on.
	if !prefixed && spec.shape == entryShapeArray && strings.TrimSpace(name) != "" {
		if rest, ok := cutPositionalLabel(msg, spec.family); ok &&
			strings.Contains(rest, strconv.Quote(strings.TrimSpace(name))) {
			labelled, prefixed = rest, true
		}
	}
	// Some validators word a bad NAME as a sentence about the name rather
	// than about a key of the entry, so they carry no label to strip. The
	// registry names those wordings; the field where the fix happens is the
	// name either way.
	for _, hint := range spec.nameProblems {
		if strings.Contains(msg, hint) && strings.Contains(msg, strconv.Quote(strings.TrimSpace(name))) {
			return entryProblem{Field: spec.identity(), Message: msg}
		}
	}
	if !prefixed {
		if phrase, ok := quotedPhrase(labelled); ok {
			if field, ok := phraseField(phrases, phrase); ok {
				return entryProblem{Field: field, Message: msg}
			}
		}
		if strings.Contains(msg, "duplicate feed name") {
			// The asymmetric duplicate (#100's twin of the phrase collision):
			// a rename that steals a later feed's name makes the OTHER entry
			// carry the label, but the field where the fix happens is still
			// the draft's own name.
			return entryProblem{Field: spec.identity(), Message: msg}
		}
		return entryProblem{Message: msg}
	}

	if m := entryStepField.FindStringSubmatch(labelled); m != nil {
		rest := labelled[len(m[0]):]
		field := "steps[" + m[1] + "]"
		if sub, ok := spec.subKeys["steps"]; ok {
			token := strings.TrimSuffix(strings.SplitN(rest, " ", 2)[0], ":")
			// A step key may be a LIST, and a problem may name one element of
			// it — `args[1] contains a null byte`. The key is the token with
			// its subscript removed; the field keeps the subscript, so the
			// form pins the message to the row it means, exactly as a
			// `phrases[1]` problem already lands on the phrase it means.
			key := token
			if open := strings.IndexByte(token, '['); open > 0 && strings.HasSuffix(token, "]") {
				key = token[:open]
			}
			if _, ok := sub[key]; ok {
				field += "." + token
			}
		}
		return entryProblem{Field: field, Message: rest}
	}
	if phrase, ok := quotedPhrase(labelled); ok {
		if field, ok := phraseField(phrases, phrase); ok {
			return entryProblem{Field: field, Message: labelled}
		}
		return entryProblem{Field: defaultPhraseField(spec), Message: labelled}
	}
	switch {
	case strings.Contains(labelled, "no phrases"):
		return entryProblem{Field: defaultPhraseField(spec), Message: labelled}
	case strings.Contains(labelled, "no steps"):
		return entryProblem{Field: "steps", Message: labelled}
	case strings.Contains(labelled, "duplicate feed name"):
		// The knowledge validator's collision case (#100) words the clash
		// without leading with the key — "duplicate feed name; each feed
		// needs its own" — so the token match below would miss it. The name
		// field is where the fix happens.
		return entryProblem{Field: spec.identity(), Message: labelled}
	}
	token := strings.TrimSuffix(strings.SplitN(labelled, " ", 2)[0], ":")
	if token == "it" || token == "" {
		return entryProblem{Message: labelled}
	}
	if _, ok := spec.keys[token]; ok || token == spec.identity() {
		return entryProblem{Field: token, Message: labelled}
	}
	return entryProblem{Message: labelled}
}

// defaultPhraseField is where a phrase problem lands when the quoted phrase is
// not one of the draft's own — the family's first phrase-bearing key.
func defaultPhraseField(spec entryFamilySpec) string {
	if len(spec.phraseKeys) > 0 {
		return spec.phraseKeys[0]
	}
	return "phrases"
}

// positionalLabel matches a validator's bare positional prefix — `routines[2]:`
// — for a family named in the pattern's first group.
var positionalLabel = regexp.MustCompile(`^([a-z.]+)\[\d+\]: `)

// cutPositionalLabel strips `family[N]: ` from a message, reporting whether it
// was there and belonged to this family.
func cutPositionalLabel(msg, family string) (string, bool) {
	m := positionalLabel.FindStringSubmatch(msg)
	if m == nil || m[1] != family {
		return msg, false
	}
	return msg[len(m[0]):], true
}

// quotedPhrase extracts the phrase a validator quoted: `phrase "wind down"`.
func quotedPhrase(msg string) (string, bool) {
	const marker = `phrase "`
	i := strings.Index(msg, marker)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// phraseField locates a quoted phrase in the draft's phrase list.
func phraseField(phrases map[string]string, phrase string) (string, bool) {
	fields := make([]string, 0, len(phrases))
	for field, p := range phrases {
		if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(phrase)) {
			fields = append(fields, field)
		}
	}
	if len(fields) == 0 {
		return "", false
	}
	// A draft repeating one phrase in two fields is a user state, not a bug;
	// sorting keeps the reported field deterministic rather than map-ordered.
	sort.Strings(fields)
	return fields[0], true
}
