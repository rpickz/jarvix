package daemon

// This file is the window form dialog's daemon half (issue #99): the generic
// entry-administration surface that lets a client read, dry-run-validate,
// write, and delete one [[family]] entry of config.toml as a whole — the
// machinery behind the Automations tab's New/Edit/Delete forms, built with
// zero automations-specific logic so the knowledge/memory forms (#100) are a
// registry row here, not a new surface.
//
// Four verbs, one discipline (the settings discipline, ADR 0015, as
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
//	                        validate-whole-then-write pipeline.
//
// Every rule lives here or below (ADR 0013): the QML form renders fields,
// ships drafts, and pins returned problems to inputs — it decides nothing.
// Saving never executes anything: a script draft's path is stat-ed by the
// validator, never run, and the entry schema has no argv or environment for
// form text to reach (ADR 0030 — the whitelist below is the shape check).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/automation"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
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

// entryFamilySpec declares one editable [[family]]: its wire keys with their
// shapes, the rendering order (matching the documentation's worked examples),
// and the word activity rows use for one of its entries. This registry is the
// only family-specific code on the surface — #100 adds knowledge feeds and
// memory entries by adding rows, not verbs.
type entryFamilySpec struct {
	family   string
	kind     string
	keys     map[string]entryKeyKind
	keyOrder []string
	subKeys  map[string]map[string]entryKeyKind
	subOrder map[string][]string
}

// entryAdminFamilies lists the families the window may edit (#99: the
// Automations tab's two). The key set mirrors the config structs exactly —
// config.Routine/RoutineStep/Script — and a drift test pins that.
var entryAdminFamilies = map[string]entryFamilySpec{
	"routines": {
		family: "routines", kind: "routine",
		keys: map[string]entryKeyKind{
			"name": entryKeyString, "enabled": entryKeyBool,
			"phrases": entryKeyStringList, "schedule": entryKeyString,
			"announce": entryKeyBool, "steps": entryKeyTables,
		},
		keyOrder: []string{"name", "phrases", "schedule", "announce", "enabled", "steps"},
		subKeys: map[string]map[string]entryKeyKind{"steps": {
			"app": entryKeyString, "match": entryKeyString, "workspace": entryKeyInt,
			"float": entryKeyBool, "size": entryKeyIntPair, "position": entryKeyIntPair,
			"tile": entryKeyString,
		}},
		subOrder: map[string][]string{"steps": {"app", "match", "workspace", "float", "size", "position", "tile"}},
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
}

// entryFamily resolves a wire family name against the registry.
func entryFamily(family string) (entryFamilySpec, *ipc.Error) {
	if spec, ok := entryAdminFamilies[family]; ok {
		return spec, nil
	}
	return entryFamilySpec{}, ipc.Errorf(ipc.CodeInvalidParams,
		"family %q is not editable from the window; the editable families are %q and %q",
		family, "routines", "scripts")
}

// registerEntryAdminMethods adds the form dialog surface (#99).
func (d *Daemon) registerEntryAdminMethods() {
	// config.get_entry: one whole entry, straight from the file the form will
	// edit, paired with that file's fingerprint so the two can never be from
	// different versions of the document.
	d.server.Handle("config.get_entry", func(params json.RawMessage) (any, error) {
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
		entry, ok, err := config.EntryValue(raw, spec.family, p.Name)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "no [[%s]] entry is named %q", spec.family, p.Name)
		}
		fp := config.FingerprintMissing
		if raw != nil {
			fp = config.Fingerprint(raw)
		}
		return map[string]any{"fingerprint": fp, "family": spec.family, "entry": entry}, nil
	})

	// config.validate_entry: the dry run behind live field errors and the
	// next-fire preview. Same pipeline as the save, minus the write — so a
	// form that validated clean can still be refused at save time only by the
	// world moving (a fingerprint conflict), never by a rule it was not shown.
	d.server.Handle("config.validate_entry", func(params json.RawMessage) (any, error) {
		p, spec, draft, problems, ipcErr := decodeEntryParams("config.validate_entry", params)
		if ipcErr != nil {
			return nil, ipcErr
		}
		result := map[string]any{}
		if next, ok := entryNextFire(draft); ok {
			result["next_fire"] = next
		}
		if len(problems) == 0 {
			raw, err := os.ReadFile(d.paths.ConfigFile())
			if err != nil && !os.IsNotExist(err) {
				return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
			}
			newRaw, err := config.UpsertEntryTOML(raw, spec.family, p.Name, draft, spec.keyOrder, spec.subOrder)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
			}
			problems = d.entryDocProblems(newRaw, spec, draft)
		}
		result["valid"] = len(problems) == 0
		result["problems"] = problems
		return result, nil
	})

	// config.upsert_entry: the save. Fingerprint-guarded like
	// automations.set_enabled, validated whole before anything lands, written
	// atomically, applied by the standard reload — which is what recompiles
	// the grammar and rebuilds the schedules — and announced on the activity
	// feed naming the entry.
	d.server.Handle("config.upsert_entry", func(params json.RawMessage) (any, error) {
		p, spec, draft, problems, ipcErr := decodeEntryParams("config.upsert_entry", params)
		if ipcErr != nil {
			return nil, ipcErr
		}
		if len(problems) > 0 {
			return nil, entryProblemsError(problems)
		}
		return d.writeEntryChange(spec, p.Fingerprint, p.Name == "",
			func(raw []byte) ([]byte, error) {
				return config.UpsertEntryTOML(raw, spec.family, p.Name, draft, spec.keyOrder, spec.subOrder)
			}, draft)
	})

	// config.delete_entry: confirmed removal. The same whole-document
	// validation guards it — a delete that would leave the file invalid is
	// refused with the problems, nothing written.
	d.server.Handle("config.delete_entry", func(params json.RawMessage) (any, error) {
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
		result, err := d.writeEntryChange(spec, p.Fingerprint, false,
			func(raw []byte) ([]byte, error) {
				return config.DeleteEntryTOML(raw, spec.family, p.Name)
			}, nil)
		if err != nil {
			return nil, err
		}
		d.publishEntryChanged("deleted", spec, p.Name)
		return result, nil
	})
}

// decodeEntryParams reads the shared {family, name?, entry} params and
// sanitises the draft against the family's key whitelist. Shape problems come
// back as field-level entries so the form can pin "timeout_sec must be a
// whole number" to its input exactly like a validator message.
func decodeEntryParams(method string, params json.RawMessage) (
	p struct {
		Family      string          `json:"family"`
		Name        string          `json:"name"`
		Entry       json.RawMessage `json:"entry"`
		Fingerprint string          `json:"fingerprint"`
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
	rewrite func([]byte) ([]byte, error), draft map[string]any) (map[string]any, error) {
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
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
	}
	if problems := d.entryDocProblems(newRaw, spec, draft); len(problems) > 0 {
		return nil, entryProblemsError(problems)
	}
	fileCfg, err := config.ParseBytes(newRaw)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "rewrite config: %v", err)
	}
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := config.WriteFileAtomic(path, newRaw); err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "write config: %v", err)
	}

	applied, reason := d.applyRuntime(fileCfg)
	newFP := config.Fingerprint(newRaw)
	d.publishConfigChanged(newFP)
	if draft != nil {
		action := "edited"
		if created {
			action = "created"
		}
		name, _ := draft["name"].(string)
		d.publishEntryChanged(action, spec, name)
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

// publishEntryChanged announces one form save on the bus. The activity
// watcher renders it into the feed row the acceptance criteria require —
// create, edit, and delete each naming the entry — and any open window's
// listing refreshes off the config.changed event that accompanied it.
func (d *Daemon) publishEntryChanged(action string, spec entryFamilySpec, name string) {
	d.bus.Publish(session.Event{Type: "config.entry_changed", Data: map[string]any{
		"action": action, "family": spec.family, "kind": spec.kind, "name": name,
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
			problems = append(problems, entryProblem{Field: key, Message: fmt.Sprintf(
				"%q is not a [[%s]] key the window can write; remove it from the draft", key, spec.family)})
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
	name, _ := draft["name"].(string)
	index := entryIndexByName(newRaw, spec.family, name)
	var phrases []string
	if p, ok := draft["phrases"].([]string); ok {
		phrases = p
	}
	var out []entryProblem
	for _, msg := range validationProblems(err) {
		out = append(out, classifyEntryProblem(spec, index, name, phrases, msg))
	}
	return out
}

// entryIndexByName finds the draft's position in the rewritten document, for
// building the label prefix the validators use.
func entryIndexByName(doc []byte, family, name string) int {
	index, ok, err := config.EntryIndex(doc, family, name)
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
func classifyEntryProblem(spec entryFamilySpec, index int, name string, phrases []string, msg string) entryProblem {
	labelled := msg
	prefixed := false
	if index >= 0 {
		label := fmt.Sprintf("%s[%d]", spec.family, index)
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			label = fmt.Sprintf("%s[%d] (%q)", spec.family, index, trimmed)
		}
		if rest, ok := strings.CutPrefix(msg, label+": "); ok {
			labelled, prefixed = rest, true
		} else if rest, ok := strings.CutPrefix(msg, label+" "); ok {
			// The step labels: `routines[0] ("x") steps[1]: ...`.
			labelled, prefixed = rest, true
		}
	}
	if !prefixed {
		if phrase, ok := quotedPhrase(labelled); ok {
			if field, ok := phraseField(phrases, phrase); ok {
				return entryProblem{Field: field, Message: msg}
			}
		}
		return entryProblem{Message: msg}
	}

	if m := entryStepField.FindStringSubmatch(labelled); m != nil {
		rest := labelled[len(m[0]):]
		field := "steps[" + m[1] + "]"
		if sub, ok := spec.subKeys["steps"]; ok {
			token := strings.TrimSuffix(strings.SplitN(rest, " ", 2)[0], ":")
			if _, ok := sub[token]; ok {
				field += "." + token
			}
		}
		return entryProblem{Field: field, Message: rest}
	}
	if phrase, ok := quotedPhrase(labelled); ok {
		if field, ok := phraseField(phrases, phrase); ok {
			return entryProblem{Field: field, Message: labelled}
		}
		return entryProblem{Field: "phrases", Message: labelled}
	}
	switch {
	case strings.Contains(labelled, "no phrases"):
		return entryProblem{Field: "phrases", Message: labelled}
	case strings.Contains(labelled, "no steps"):
		return entryProblem{Field: "steps", Message: labelled}
	}
	token := strings.TrimSuffix(strings.SplitN(labelled, " ", 2)[0], ":")
	if token == "it" || token == "" {
		return entryProblem{Message: labelled}
	}
	if _, ok := spec.keys[token]; ok || token == "name" {
		return entryProblem{Field: token, Message: labelled}
	}
	return entryProblem{Message: labelled}
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
func phraseField(phrases []string, phrase string) (string, bool) {
	for i, p := range phrases {
		if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(phrase)) {
			return fmt.Sprintf("phrases[%d]", i), true
		}
	}
	return "", false
}
