package config

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// This file is the generalised array-of-tables editor (issue #92): it sets
// one scalar field on one [[family]] entry, addressed by the entry's `name`,
// under the same contract as the surgical settings editor in rewrite.go (ADR
// 0015) and the [[routines]] upserter (#62): the file stays authoritative and
// hand-editable, so everything outside the one line being written — comments,
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
// [[routines.steps]] included — minus trailing blank lines. The
// generalisation of routineBlockSpan's rule: a header ends the block unless
// its table name is nested under the family.
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
	for end > start && strings.TrimSpace(lines[end]) == "" {
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
