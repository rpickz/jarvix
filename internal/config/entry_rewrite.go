package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// This file is the generalised array-of-tables editor (issue #92, extended
// by #99): it edits [[family]] entries addressed by the entry's `name`, under
// the same contract as the surgical settings editor in rewrite.go (ADR 0015)
// and the [[routines]] upserter (#62): the file stays authoritative and
// hand-editable, so everything outside the part being written — comments,
// unknown keys, sibling entries, ordering, formatting — is preserved
// byte-for-byte, and the result must re-parse and read back exactly the
// intended edit or nothing is returned.
//
// It exists because #92 (knowledge feeds' `enabled` switch) and #93 (the same
// switch on [[routines]] and [[scripts]]) both need to flip one field on one
// entry, and a third copy of the block-location machinery would drift. The
// API is deliberately family-agnostic: `family` is the table-array name as it
// appears between the double brackets ("knowledge.feeds", "routines",
// "scripts"), `name` matches the entry's `name` key case-insensitively (the
// rule every family already uses for uniqueness), and `field` takes any
// scalar encodeTOMLValue can render. Which block is which is decided by the
// parser, never by a second grammar: entries are resolved against the decoded
// document and addressed by position, exactly as UpsertRoutineTOML does.
//
// Three granularities, one discipline. SetEntryField writes one scalar (#92,
// #93's enabled switch). UpsertEntryTOML and DeleteEntryTOML (#99, the window
// form dialog) write and remove one whole entry — sub-tables like
// [[routines.steps]] included — because a form saves a complete draft, not a
// field at a time. All three refuse to return a document that does not parse
// or does not read back as exactly the intended edit.

// SetEntryField returns doc with field set to value on the [[family]] entry
// whose `name` key equals name (case-insensitive, whitespace-trimmed). An
// existing field is replaced in place, keeping its inline comment; a missing
// one is appended to the entry's own body, before any sub-tables, matching
// the body's indentation. The document must already parse, and the named
// entry must exist — creation stays a hand edit (or #62's upserter).
func SetEntryField(doc []byte, family, name, field string, value any) ([]byte, error) {
	entries, err := entryNames(doc, family)
	if err != nil {
		return nil, err
	}
	index := -1
	for i, n := range entries {
		if strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(name)) {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("no [[%s]] entry is named %q", family, name)
	}

	encoded, err := encodeTOMLValue(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}

	lines := strings.Split(string(doc), "\n")
	start, end, err := entryBlockSpan(lines, family, index)
	if err != nil {
		return nil, err
	}
	out, err := setFieldInBlock(lines, start, end, family, field, encoded)
	if err != nil {
		return nil, err
	}

	// The guard that makes the surgical editor safe (rewrite.go's, restated
	// for entries): the result must parse as configuration, and the family
	// must read back as exactly the intended edit — the field landed on the
	// named entry, and no entry appeared, vanished, or was renamed. A scanner
	// bug costs the save, never the file.
	if _, err := ParseBytes([]byte(out)); err != nil {
		return nil, fmt.Errorf("rewrite produced an unparsable document (nothing was written): %w", err)
	}
	after, err := entryNames([]byte(out), family)
	if err != nil {
		return nil, fmt.Errorf("rewrite produced an unreadable document (nothing was written): %w", err)
	}
	if len(after) != len(entries) {
		return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	for i := range after {
		if after[i] != entries[i] {
			return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
		}
	}
	got, ok, err := entryFieldValue([]byte(out), family, index, field)
	if err != nil || !ok || !tomlValuesEqual(got, value) {
		return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	return []byte(out), nil
}

// setFieldInBlock performs the edit inside one entry's block: lines start
// through end, of which only the body — the keys before the first sub-table —
// may hold the field.
func setFieldInBlock(lines []string, start, end int, family, field, encoded string) (string, error) {
	bodyEnd := end
	for i := start + 1; i <= end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "[") {
			bodyEnd = i - 1
			break
		}
	}

	// scanDoc over just the body finds each key's exact value span — multi-line
	// values and inline comments included — without ever seeing another entry's
	// identically-named keys.
	body := lines[start+1 : bodyEnd+1]
	idx, err := scanDoc(body)
	if err != nil {
		return "", fmt.Errorf("[[%s]] entry: %w", family, err)
	}

	if span, ok := idx.keys[field]; ok {
		lineNo := start + 1 + span.startLine
		prefix := lines[lineNo][:span.valStart]
		replaced := prefix + encoded + span.suffix
		out := append([]string{}, lines[:lineNo]...)
		out = append(out, replaced)
		out = append(out, lines[start+1+span.endLine+1:]...)
		return strings.Join(out, "\n"), nil
	}

	// Field absent: append it after the last body key (or straight after the
	// header when the body is empty), matching that line's indentation so a
	// hand-formatted entry keeps reading as one.
	insertAfter := start
	indent := ""
	for i := bodyEnd; i > start; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		insertAfter = i
		indent = lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
		break
	}
	out := append([]string{}, lines[:insertAfter+1]...)
	out = append(out, indent+field+" = "+encoded)
	out = append(out, lines[insertAfter+1:]...)
	return strings.Join(out, "\n"), nil
}

// entryBlockSpan locates the index-th [[family]] block: its header line
// through the last line belonging to the entry — sub-tables like
// [[routines.steps]] included — minus trailing blank and comment lines. The
// generalisation of routineBlockSpan's rule: a header ends the block unless
// its table name is nested under the family. Trailing comments are excluded
// because a comment between two entries is glued to the one below it (the
// fixture convention throughout this repo), and a whole-entry replace or
// delete (#99) that swallowed it would eat the next entry's documentation.
func entryBlockSpan(lines []string, family string, index int) (start, end int, err error) {
	header := "[[" + family + "]]"
	seen := -1
	start = -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			seen++
			if seen == index {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return 0, 0, fmt.Errorf("%s[%d] not found in the document", family, index)
	}
	end = len(lines) - 1
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		if !strings.HasPrefix(strings.Trim(trimmed, "[]"), family+".") {
			end = i - 1
			break
		}
	}
	for end > start {
		trimmed := strings.TrimSpace(lines[end])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		end--
	}
	return start, end, nil
}

// entryNames lists the `name` key of every [[family]] entry, in document
// order — the parser's view, which is what block addressing trusts. The
// generic decode keeps this editor independent of any one family's struct.
func entryNames(doc []byte, family string) ([]string, error) {
	entries, err := decodeEntries(doc, family)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		if n, ok := e["name"].(string); ok {
			names[i] = n
		}
	}
	return names, nil
}

// EntryNames lists the `name` key of every [[family]] entry, in document
// order — the listing order an array family can promise, exported for the
// generic listing verb (#163) that serves both document shapes.
func EntryNames(doc []byte, family string) ([]string, error) {
	return entryNames(doc, family)
}

// entryFieldValue reads one field of the index-th [[family]] entry back from
// the document — the read-back half of the safety guard.
func entryFieldValue(doc []byte, family string, index int, field string) (any, bool, error) {
	entries, err := decodeEntries(doc, family)
	if err != nil {
		return nil, false, err
	}
	if index < 0 || index >= len(entries) {
		return nil, false, nil
	}
	v, ok := entries[index][field]
	return v, ok, nil
}

// decodeEntries decodes the [[family]] tables generically, walking the dotted
// family name through plain tables ("knowledge.feeds" → [knowledge] → feeds).
// Absent segments mean no entries, not an error.
func decodeEntries(doc []byte, family string) ([]map[string]any, error) {
	node, err := decodeNode(doc, family)
	if err != nil {
		return nil, err
	}
	switch list := node.(type) {
	case []map[string]any:
		return list, nil
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out, nil
	}
	return nil, nil
}

// decodeNode decodes the document and walks the dotted family name down to
// the node that holds the family ("knowledge.feeds" → [knowledge] → feeds,
// "ai" → [ai]). An absent segment yields a nil node, not an error: a family
// with no entries yet is an ordinary state, not a broken file. Shared with the
// keyed editor (keyed_rewrite.go) so both shapes resolve a family name through
// exactly one piece of code.
func decodeNode(doc []byte, family string) (any, error) {
	var raw map[string]any
	if _, err := toml.Decode(string(doc), &raw); err != nil {
		return nil, fmt.Errorf("config.toml does not parse; fix it by hand first: %w", err)
	}
	node := any(raw)
	for _, part := range strings.Split(family, ".") {
		table, ok := node.(map[string]any)
		if !ok {
			return nil, nil
		}
		node = table[part]
	}
	return node, nil
}

// UpsertEntryTOML returns doc with one [[family]] entry written whole. A
// non-empty name replaces the entry so named in place — the block keeps its
// position, and a draft whose `name` key differs is a rename. An empty name
// appends the entry at the end of the document (creation); whether the new
// name collides with an existing entry is deliberately not judged here — the
// caller validates the whole resulting document, where a duplicate fails with
// the same error a config load gives.
//
// The rendered block contains exactly the keys of entry: scalar keys first in
// keyOrder (TOML demands it — a key after a sub-table header would belong to
// the sub-table), then each array-of-tables key ([]map values) as indented
// [[family.key]] blocks whose keys follow subOrder. Replacing renders the
// block fresh, so comments *inside* the replaced entry go with it — the
// UpsertRoutineTOML precedent — while every byte outside the block, glued
// header comments included, survives.
func UpsertEntryTOML(doc []byte, family, name string, entry map[string]any,
	keyOrder []string, subOrder map[string][]string) ([]byte, error) {
	before, err := entryNames(doc, family)
	if err != nil {
		return nil, err
	}
	replaceAt := -1
	if strings.TrimSpace(name) != "" {
		for i, n := range before {
			if strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(name)) {
				replaceAt = i
				break
			}
		}
		if replaceAt < 0 {
			return nil, fmt.Errorf("no [[%s]] entry is named %q", family, name)
		}
	}

	block, err := renderEntryTOML(family, entry, keyOrder, subOrder)
	if err != nil {
		return nil, err
	}
	var out string
	if replaceAt < 0 {
		out = strings.TrimRight(string(doc), "\n")
		if out != "" {
			out += "\n\n"
		}
		out += strings.Join(block, "\n") + "\n"
	} else {
		lines := strings.Split(string(doc), "\n")
		start, end, err := entryBlockSpan(lines, family, replaceAt)
		if err != nil {
			return nil, err
		}
		replaced := append([]string{}, lines[:start]...)
		replaced = append(replaced, block...)
		replaced = append(replaced, lines[end+1:]...)
		out = strings.Join(replaced, "\n")
	}

	// The guard that makes the editor safe (SetEntryField's, restated for
	// whole entries): the result must parse as configuration, the family must
	// read back with no sibling moved or renamed, and the written entry must
	// read back as exactly the draft. A renderer bug costs the save, never
	// the file.
	if _, err := ParseBytes([]byte(out)); err != nil {
		return nil, fmt.Errorf("rewrite produced an unparsable document (nothing was written): %w", err)
	}
	after, err := entryNames([]byte(out), family)
	if err != nil {
		return nil, fmt.Errorf("rewrite produced an unreadable document (nothing was written): %w", err)
	}
	wantNames := append([]string{}, before...)
	newName, _ := entry["name"].(string)
	index := replaceAt
	if replaceAt < 0 {
		wantNames = append(wantNames, newName)
		index = len(before)
	} else {
		wantNames[replaceAt] = newName
	}
	if len(after) != len(wantNames) {
		return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	for i := range after {
		if after[i] != wantNames[i] {
			return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
		}
	}
	entries, err := decodeEntries([]byte(out), family)
	if err != nil || index >= len(entries) || !entryMapEqual(entries[index], entry) {
		return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	return []byte(out), nil
}

// DeleteEntryTOML returns doc with the [[family]] entry named name removed:
// its header, body, and sub-tables, plus any comment lines glued directly to
// its header — no blank line between — because those document the entry and
// would otherwise dangle over the next one. A comment separated by a blank
// line stays: it may head a whole section. The blank line that separated the
// removed block from its neighbour is collapsed so the document never
// accumulates double blanks; everything else is byte-preserved.
func DeleteEntryTOML(doc []byte, family, name string) ([]byte, error) {
	before, err := entryNames(doc, family)
	if err != nil {
		return nil, err
	}
	index := -1
	for i, n := range before {
		if strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(name)) {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("no [[%s]] entry is named %q", family, name)
	}

	lines := strings.Split(string(doc), "\n")
	start, end, err := entryBlockSpan(lines, family, index)
	if err != nil {
		return nil, err
	}
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}

	cut := append([]string{}, lines[:start]...)
	rest := append([]string{}, lines[end+1:]...)
	// Collapse the separator: with the block gone, a blank on both sides of
	// the cut would read as a double blank line.
	for len(cut) > 0 && strings.TrimSpace(cut[len(cut)-1]) == "" &&
		len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	// A deleted last entry must not leave trailing blank lines before EOF.
	hasContent := false
	for _, l := range rest {
		if strings.TrimSpace(l) != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		for len(cut) > 0 && strings.TrimSpace(cut[len(cut)-1]) == "" {
			cut = cut[:len(cut)-1]
		}
		rest = []string{""}
	}
	out := strings.Join(append(cut, rest...), "\n")

	// The read-back guard: the result parses, and the family lost exactly the
	// named entry — no sibling went with it.
	if _, err := ParseBytes([]byte(out)); err != nil {
		return nil, fmt.Errorf("rewrite produced an unparsable document (nothing was written): %w", err)
	}
	after, err := entryNames([]byte(out), family)
	if err != nil {
		return nil, fmt.Errorf("rewrite produced an unreadable document (nothing was written): %w", err)
	}
	want := append(append([]string{}, before[:index]...), before[index+1:]...)
	if len(after) != len(want) {
		return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	for i := range after {
		if after[i] != want[i] {
			return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
		}
	}
	return []byte(out), nil
}

// EntryValue reads one [[family]] entry back as the parser sees it — the
// generic map a form round-trips (#99): the daemon serves it, the form edits
// the fields it shows, and every key it does not show survives the save
// untouched because the whole map comes back.
func EntryValue(doc []byte, family, name string) (map[string]any, bool, error) {
	entries, err := decodeEntries(doc, family)
	if err != nil {
		return nil, false, err
	}
	for _, e := range entries {
		if n, ok := e["name"].(string); ok &&
			strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(name)) {
			return e, true, nil
		}
	}
	return nil, false, nil
}

// EntryIndex reports the position of the [[family]] entry named name in
// document order — the index the loader's validators use in their labels
// ("routines[2]"), which is what lets a caller match whole-document problems
// back to one entry (#99).
func EntryIndex(doc []byte, family, name string) (int, bool, error) {
	names, err := entryNames(doc, family)
	if err != nil {
		return 0, false, err
	}
	for i, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(name)) {
			return i, true, nil
		}
	}
	return 0, false, nil
}

// renderEntryTOML renders one whole entry in the shape the documentation's
// worked examples use — scalar keys on the header's level, sub-tables
// indented two spaces — so a written entry reads like a hand-written one.
// Only keys present in the map are rendered; keys outside keyOrder (a caller
// bug, not a user state) still render, after the ordered ones, sorted, so the
// read-back guard judges the real content rather than a silent omission.
func renderEntryTOML(family string, entry map[string]any,
	keyOrder []string, subOrder map[string][]string) ([]string, error) {
	lines := []string{"[[" + family + "]]"}
	scalars, tables := []string{}, []string{}
	for _, key := range orderedEntryKeys(entry, keyOrder) {
		if _, ok := entrySubTables(entry[key]); ok {
			tables = append(tables, key)
		} else {
			scalars = append(scalars, key)
		}
	}
	for _, key := range scalars {
		encoded, err := encodeEntryValue(entry[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		lines = append(lines, key+" = "+encoded)
	}
	for _, key := range tables {
		subs, _ := entrySubTables(entry[key])
		for _, sub := range subs {
			lines = append(lines, "", "  [["+family+"."+key+"]]")
			for _, sk := range orderedEntryKeys(sub, subOrder[key]) {
				encoded, err := encodeEntryValue(sub[sk])
				if err != nil {
					return nil, fmt.Errorf("%s.%s: %w", key, sk, err)
				}
				lines = append(lines, "  "+sk+" = "+encoded)
			}
		}
	}
	return lines, nil
}

// orderedEntryKeys lists the keys of m that exist, in the caller's order,
// with any stragglers sorted at the end for determinism.
func orderedEntryKeys(m map[string]any, order []string) []string {
	var keys []string
	seen := make(map[string]bool, len(order))
	for _, k := range order {
		if _, ok := m[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// entrySubTables recognises a value that renders as [[family.key]] blocks:
// a list of maps, in either shape a caller holds one (typed after the
// daemon's sanitiser, []any straight from a TOML decode).
func entrySubTables(v any) ([]map[string]any, bool) {
	switch list := v.(type) {
	case []map[string]any:
		return list, true
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, e := range list {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, m)
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	return nil, false
}

// encodeEntryValue renders one entry value as a TOML literal: the scalar set
// encodeTOMLValue takes, plus the widened and composite shapes a whole-entry
// draft carries (int64 from JSON numbers, []int for size/position pairs,
// []any element-wise).
func encodeEntryValue(v any) (string, error) {
	switch t := v.(type) {
	case int64:
		return strconv.FormatInt(t, 10), nil
	case []int:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = strconv.Itoa(e)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			encoded, err := encodeEntryValue(e)
			if err != nil {
				return "", err
			}
			parts[i] = encoded
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	}
	return encodeTOMLValue(v)
}

// entryMapEqual compares a TOML-decoded entry with the intended draft, both
// ways: every drafted key read back equal, and no key the draft did not
// carry. The value comparison bridges the decoder's widened types.
func entryMapEqual(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || !entryValueEqual(g, w) {
			return false
		}
	}
	return true
}

// entryValueEqual compares one intended value with its decoded read-back,
// recursing through lists and sub-tables.
func entryValueEqual(got, want any) bool {
	switch w := want.(type) {
	case int:
		g, ok := got.(int64)
		return ok && g == int64(w)
	case int64:
		g, ok := got.(int64)
		return ok && g == w
	case []string:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !entryValueEqual(g[i], w[i]) {
				return false
			}
		}
		return true
	case []int:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !entryValueEqual(g[i], w[i]) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !entryValueEqual(g[i], w[i]) {
				return false
			}
		}
		return true
	case []map[string]any:
		g, ok := entrySubTables(got)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !entryMapEqual(g[i], w[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		return ok && entryMapEqual(g, w)
	default:
		return got == want
	}
}

// tomlValuesEqual compares an intended native value with its TOML-decoded
// read-back, bridging the decoder's widened types (int64 for ints, []any for
// arrays).
func tomlValuesEqual(got, want any) bool {
	switch w := want.(type) {
	case int:
		g, ok := got.(int64)
		return ok && g == int64(w)
	case []string:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			s, ok := g[i].(string)
			if !ok || s != w[i] {
				return false
			}
		}
		return true
	default:
		return got == want
	}
}
