package config

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the third document shape the entry editor serves (issue #164),
// beside entry_rewrite.go's arrays of tables and keyed_rewrite.go's maps of
// tables, under the same contract all three keep: the file stays authoritative
// and hand-editable, so everything outside the part being written — comments,
// unknown keys, sibling entries, ordering, formatting — survives byte-for-byte,
// and the result must re-parse and read back as exactly the intended edit or
// nothing is returned.
//
// The family it exists for is the speech pronunciation lexicon:
//
//	[tts.lexicon]
//	Kubernetes = "koo ber net eez"
//	k9s = "kay nine ess"
//
// An entry here is not a table at all. It is ONE LINE — a key and a string —
// which is why neither of the other two editors can address it: the array
// editor looks for `[[family]]` headers and the keyed editor for
// `[family.<name>]` ones, and a lexicon has neither. ADR 0052 anticipated
// exactly this ("a third document shape is a `shape` value and four dispatch
// cases, not a new surface"), and this file is the four dispatch cases' worth
// of editing that claim rests on.
//
// Two decisions are worth stating because they differ from the other shapes:
//
//   - Addressing is EXACT, like the keyed shape and unlike the arrays. The
//     written form is a TOML key, and TOML keys are case-sensitive; more to the
//     point the lexicon matches text case-insensitively at speech time but
//     stores what the user typed, so quietly folding "GIF" onto "gif" would
//     edit an entry the user can see is a different one.
//   - An in-place edit KEEPS the line's inline comment. For every other family
//     a replaced block renders fresh and its inner comments go with it, which
//     is right when the block has room for comments of its own. Here the block
//     is a single line, so its trailing `# piper says koo-ber-net-es` is the
//     only place an entry can be documented at all, and swallowing it on every
//     save would quietly delete the user's notes one edit at a time.

// ScalarMapEntryNames lists the keys of the [family] table that hold a plain
// string, sorted. reserved names — keys that share the table without being
// entries — are never entries however they decode, the same guard the keyed
// shape applies to [ai]'s own scalars.
func ScalarMapEntryNames(doc []byte, family string, reserved map[string]bool) ([]string, error) {
	entries, err := decodeScalarMapEntries(doc, family, reserved)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ScalarMapEntryValue reads one entry of the [family] table as the parser sees
// it: {name, valueKey}. Unlike the other two shapes there is nothing else in
// the entry to round-trip — a line holds one value — so the map is built here
// rather than decoded, and valueKey names the wire key the caller's form uses
// for it.
func ScalarMapEntryValue(doc []byte, family, valueKey, name string,
	reserved map[string]bool) (map[string]any, bool, error) {
	entries, err := decodeScalarMapEntries(doc, family, reserved)
	if err != nil {
		return nil, false, err
	}
	value, ok := entries[strings.TrimSpace(name)]
	if !ok {
		return nil, false, nil
	}
	return map[string]any{"name": strings.TrimSpace(name), valueKey: value}, true, nil
}

// UpsertScalarMapEntryTOML returns doc with one `name = "value"` line of the
// [family] table written. A non-empty name replaces the line so named in place
// — it keeps its position and its inline comment, and a draft whose `name`
// differs is a rename. An empty name creates: the line lands after the table's
// last existing entry so the section stays together, or in a new [family]
// table at the end of the document when there is none yet.
func UpsertScalarMapEntryTOML(doc []byte, family, valueKey, name string, entry map[string]any,
	reserved map[string]bool) ([]byte, error) {
	before, err := ScalarMapEntryNames(doc, family, reserved)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(name)
	replacing := target != ""
	if replacing && !containsString(before, target) {
		return nil, fmt.Errorf("no %s entry is named %q", "["+family+"]", target)
	}

	newName, _ := entry["name"].(string)
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, fmt.Errorf("name: the entry needs a written form (it becomes the key in [%s])", family)
	}
	value, ok := entry[valueKey].(string)
	if !ok {
		return nil, fmt.Errorf("%s: the entry needs a value", valueKey)
	}
	rendered := encodeTOMLKey(newName) + " = " + encodeTOMLString(value)

	lines := strings.Split(string(doc), "\n")
	var out string
	switch {
	case replacing:
		at, err := scalarMapEntryLine(lines, family, target)
		if err != nil {
			return nil, err
		}
		// The inline comment survives the edit — see the file comment: a
		// one-line entry has nowhere else to carry the user's note about it.
		replaced := append([]string{}, lines[:at]...)
		replaced = append(replaced, rendered+scalarMapInlineComment(lines[at]))
		replaced = append(replaced, lines[at+1:]...)
		out = strings.Join(replaced, "\n")
	default:
		after, ok := scalarMapTableEnd(lines, family)
		if !ok {
			// No [family] table yet: append one, the same fallback rewriteOne
			// takes for an absent table.
			out = strings.TrimRight(string(doc), "\n")
			if out != "" {
				out += "\n\n"
			}
			out += "[" + family + "]\n" + rendered + "\n"
			break
		}
		inserted := append([]string{}, lines[:after+1]...)
		inserted = append(inserted, rendered)
		inserted = append(inserted, lines[after+1:]...)
		out = strings.Join(inserted, "\n")
	}

	// The guard that makes the editor safe (UpsertKeyedEntryTOML's, restated
	// for a shape whose entry is a line): the result must parse as
	// configuration, the table must read back with exactly the intended key
	// set, and the written entry must read back as exactly the drafted value.
	want := append([]string{}, before...)
	if replacing {
		for i, n := range want {
			if n == target {
				want[i] = newName
			}
		}
	} else {
		want = append(want, newName)
	}
	sort.Strings(want)
	if err := scalarMapReadBack([]byte(out), family, newName, value, want, reserved); err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// DeleteScalarMapEntryTOML returns doc with one entry of the [family] table
// removed: its line, plus any comment lines glued directly above it — no blank
// line between — because those document the entry and would otherwise dangle
// over the next one. A comment separated by a blank line stays: it may head the
// whole table.
func DeleteScalarMapEntryTOML(doc []byte, family, name string, reserved map[string]bool) ([]byte, error) {
	before, err := ScalarMapEntryNames(doc, family, reserved)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(name)
	if !containsString(before, target) {
		return nil, fmt.Errorf("no %s entry is named %q", "["+family+"]", target)
	}

	lines := strings.Split(string(doc), "\n")
	at, err := scalarMapEntryLine(lines, family, target)
	if err != nil {
		return nil, err
	}
	start := at
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}
	out := cutBlock(lines, start, at)

	want := make([]string, 0, len(before))
	for _, n := range before {
		if n != target {
			want = append(want, n)
		}
	}
	if err := scalarMapReadBack([]byte(out), family, "", "", want, reserved); err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// scalarMapReadBack is the shared read-back guard: the rewritten document
// parses, the table holds exactly wantNames, and — for a write — the named key
// decodes as exactly the drafted value.
func scalarMapReadBack(out []byte, family, name, value string,
	wantNames []string, reserved map[string]bool) error {
	if _, err := ParseBytes(out); err != nil {
		return fmt.Errorf("rewrite produced an unparsable document (nothing was written): %w", err)
	}
	after, err := ScalarMapEntryNames(out, family, reserved)
	if err != nil {
		return fmt.Errorf("rewrite produced an unreadable document (nothing was written): %w", err)
	}
	if len(after) != len(wantNames) {
		return fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	for i := range after {
		if after[i] != wantNames[i] {
			return fmt.Errorf("rewrite did not take effect (nothing was written)")
		}
	}
	if name == "" {
		return nil
	}
	entries, err := decodeScalarMapEntries(out, family, reserved)
	if err != nil || entries[name] != value {
		return fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	return nil
}

// scalarMapEntryLine locates the line holding one entry of the [family] table.
// It walks the document's headers so a key of the same name under a DIFFERENT
// table is never mistaken for this one — the mistake a plain text search for
// `Kubernetes =` would make in any file that also has an [stt] section.
func scalarMapEntryLine(lines []string, family, name string) (int, error) {
	want := strings.Split(family, ".")
	inTable := false
	for i, line := range lines {
		if path, array, ok := tableHeaderPath(line); ok {
			inTable = !array && pathEqual(path, want)
			continue
		}
		if !inTable {
			continue
		}
		if key, _, ok := tomlLineKey(line); ok && key == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("[%s] has no line for %q", family, name)
}

// scalarMapTableEnd reports the last line belonging to the [family] table, so a
// created entry joins its section rather than landing at the end of the file.
// ok false means the table is not written as a header anywhere — either absent
// entirely, or spelled as a dotted key the line editor cannot address, in which
// case appending a fresh table is the honest fallback and the read-back guard
// is what proves the result still says what was intended.
func scalarMapTableEnd(lines []string, family string) (int, bool) {
	want := strings.Split(family, ".")
	start := -1
	for i, line := range lines {
		if path, array, ok := tableHeaderPath(line); ok && !array && pathEqual(path, want) {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	end := start
	for i := start + 1; i < len(lines); i++ {
		if _, _, ok := tableHeaderPath(lines[i]); ok {
			break
		}
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			// A comment glued to whatever follows it belongs to the next entry,
			// so an insertion goes ABOVE it rather than between a comment and
			// the line it documents.
			continue
		}
		end = i
	}
	return end, true
}

// tomlLineKey reads the key a `key = value` line assigns to, bare or quoted,
// and the offset of the `=` that follows it; false for anything else (a
// comment, a blank, a header). It exists because a lexicon key may be quoted —
// `"New York" = "new york"` — and the stored name is the DECODED key, which is
// what the parser hands back and what a form addresses the entry by. The
// offset is returned rather than searched for again because a quoted key may
// itself contain the `=`.
func tomlLineKey(line string) (key string, eq int, ok bool) {
	lead := len(line) - len(strings.TrimLeft(line, " \t"))
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
		return "", 0, false
	}
	if trimmed[0] == '"' || trimmed[0] == '\'' {
		quote := trimmed[0]
		var b strings.Builder
		for i := 1; i < len(trimmed); i++ {
			c := trimmed[i]
			if quote == '"' && c == '\\' && i+1 < len(trimmed) {
				// Only the escapes a key realistically carries; anything more
				// exotic falls out as "not a key this editor addresses", and the
				// caller's read-back guard is what keeps that safe.
				i++
				switch trimmed[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(trimmed[i])
				}
				continue
			}
			if c == quote {
				rest := trimmed[i+1:]
				at := strings.Index(rest, "=")
				if at < 0 || strings.TrimSpace(rest[:at]) != "" {
					return "", 0, false
				}
				return b.String(), lead + i + 1 + at, true
			}
			b.WriteByte(c)
		}
		return "", 0, false
	}
	at := strings.Index(trimmed, "=")
	if at < 0 {
		return "", 0, false
	}
	key = strings.TrimSpace(trimmed[:at])
	if key == "" || strings.ContainsAny(key, " \t\"'.") {
		// A dotted key (`lexicon.gif = …`) is a different addressing scheme and
		// is deliberately not edited by the line editor: the read-back guard
		// would catch a wrong edit, but refusing to find it at all is better.
		return "", 0, false
	}
	return key, lead + at, true
}

// scalarMapInlineComment returns a line's trailing comment, with the whitespace
// that preceded it, or "" when it has none.
func scalarMapInlineComment(line string) string {
	_, eq, ok := tomlLineKey(line)
	if !ok {
		return ""
	}
	span, err := scanValue([]string{line}, 0, eq+1)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(span.suffix) == "" {
		return ""
	}
	return span.suffix
}

// decodeScalarMapEntries decodes the string-valued keys of the [family] table.
// Absent segments mean no entries, not an error, and only STRING values are
// entries — a sub-table or a number under the same header is somebody else's
// key and is left exactly where it is.
func decodeScalarMapEntries(doc []byte, family string, reserved map[string]bool) (map[string]string, error) {
	node, err := decodeNode(doc, family)
	if err != nil {
		return nil, err
	}
	table, ok := node.(map[string]any)
	if !ok {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(table))
	for name, value := range table {
		if reserved[name] {
			continue
		}
		if s, ok := value.(string); ok {
			out[name] = s
		}
	}
	return out, nil
}
