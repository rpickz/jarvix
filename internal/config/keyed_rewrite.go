package config

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the map-shaped half of the entry editor (issue #163), sitting
// beside entry_rewrite.go's array-of-tables half under the same contract: the
// file stays authoritative and hand-editable, so everything outside the block
// being written — comments, unknown keys, sibling tables, ordering, formatting
// — survives byte-for-byte, and the result must re-parse and read back as
// exactly the intended edit or nothing is returned.
//
// The two families it exists for are the ones that decide which brains and
// helpers Jarvix uses, and neither is an array:
//
//	[ai.<name>]      — one OpenAI-compatible endpoint, keyed by the name
//	                   `ai.provider` selects it with. It shares the [ai] table
//	                   with the section's own scalars (provider, model, …), so
//	                   a caller passes the reserved key set and those are never
//	                   mistaken for endpoints.
//	[advisors.<name>] — one assistant CLI (ADR 0016). Nothing else lives in
//	                   [advisors], so its reserved set is empty.
//
// The identity difference from the array families is the whole reason this is
// a separate file rather than a flag on the old one: an array entry is
// addressed by a `name` KEY INSIDE the table and matched case-insensitively,
// while a keyed entry IS its table key. TOML keys are case-sensitive and so is
// the map the loader decodes them into, so addressing here is exact
// (whitespace-trimmed only): quietly matching "OpenAI" to "openai" would edit
// a different endpoint than the one `ai.provider` resolves, which is precisely
// the class of mistake a byte-preserving editor exists to make impossible.
//
// The wire shape is deliberately unchanged: callers still hand a draft whose
// `name` key carries the entry's identity, so one form and one registry
// vocabulary drive both shapes. Here `name` renders as the table HEADER rather
// than as a stored key — see renderKeyedEntryTOML.

// KeyedEntryNames lists the [family.<name>] tables in the document, sorted.
// reserved names — the scalars that share the family's table, like [ai]'s
// provider and model — are never entries however they decode.
func KeyedEntryNames(doc []byte, family string, reserved map[string]bool) ([]string, error) {
	entries, err := decodeKeyedEntries(doc, family, reserved)
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

// KeyedEntryValue reads one [family.<name>] table back as the parser sees it —
// the generic map a form round-trips, exactly like EntryValue does for the
// array families, so keys the form has no widget for survive a save.
func KeyedEntryValue(doc []byte, family, name string, reserved map[string]bool) (map[string]any, bool, error) {
	entries, err := decodeKeyedEntries(doc, family, reserved)
	if err != nil {
		return nil, false, err
	}
	entry, ok := entries[strings.TrimSpace(name)]
	if !ok {
		return nil, false, nil
	}
	return entry, true, nil
}

// UpsertKeyedEntryTOML returns doc with one [family.<name>] table written
// whole. A non-empty name replaces the table so named in place — the block
// keeps its position, and a draft whose `name` key differs is a rename, which
// rewrites the header. An empty name creates: the block lands after the last
// existing [family.*] table so the section stays together, or at the end of
// the document when the family has no tables yet.
//
// Whether a created name collides with an existing entry is deliberately not
// judged here — the caller validates the whole resulting document, where a
// duplicate table is the same parse error a hand edit would produce.
func UpsertKeyedEntryTOML(doc []byte, family, name string, entry map[string]any,
	keyOrder []string, reserved map[string]bool) ([]byte, error) {
	before, err := KeyedEntryNames(doc, family, reserved)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(name)
	replacing := target != ""
	if replacing && !containsString(before, target) {
		return nil, fmt.Errorf("no [%s.%s] table exists", family, target)
	}

	newName, _ := entry["name"].(string)
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, fmt.Errorf("name: the entry needs a name (it becomes the [%s.<name>] table)", family)
	}
	block, err := renderKeyedEntryTOML(family, newName, entry, keyOrder)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(doc), "\n")
	var out string
	if replacing {
		start, end, err := keyedBlockSpan(lines, family, target)
		if err != nil {
			return nil, err
		}
		replaced := append([]string{}, lines[:start]...)
		replaced = append(replaced, block...)
		replaced = append(replaced, lines[end+1:]...)
		out = strings.Join(replaced, "\n")
	} else if after, ok := lastKeyedBlockEnd(lines, family, before); ok {
		inserted := append([]string{}, lines[:after+1]...)
		inserted = append(inserted, "")
		inserted = append(inserted, block...)
		inserted = append(inserted, lines[after+1:]...)
		out = strings.Join(inserted, "\n")
	} else {
		out = strings.TrimRight(string(doc), "\n")
		if out != "" {
			out += "\n\n"
		}
		out += strings.Join(block, "\n") + "\n"
	}

	// The guard that makes the editor safe (UpsertEntryTOML's, restated for
	// keyed tables): the result must parse as configuration, the family must
	// read back with exactly the intended name set, and the written table must
	// read back as exactly the draft minus its name. A renderer bug costs the
	// save, never the file.
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
	if err := keyedReadBack([]byte(out), family, newName, entry, want, reserved); err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// DeleteKeyedEntryTOML returns doc with the [family.<name>] table removed: its
// header, its body, any tables nested under it, and any comment lines glued
// directly to its header — no blank line between — because those document the
// entry and would otherwise dangle over the next one. A comment separated by a
// blank line stays: it may head a whole section.
func DeleteKeyedEntryTOML(doc []byte, family, name string, reserved map[string]bool) ([]byte, error) {
	before, err := KeyedEntryNames(doc, family, reserved)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(name)
	if !containsString(before, target) {
		return nil, fmt.Errorf("no [%s.%s] table exists", family, target)
	}

	lines := strings.Split(string(doc), "\n")
	start, end, err := keyedBlockSpan(lines, family, target)
	if err != nil {
		return nil, err
	}
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}
	out := cutBlock(lines, start, end)

	want := make([]string, 0, len(before))
	for _, n := range before {
		if n != target {
			want = append(want, n)
		}
	}
	if err := keyedReadBack([]byte(out), family, "", nil, want, reserved); err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// keyedReadBack is the shared read-back guard: the rewritten document parses,
// the family holds exactly wantNames, and — for a write — the named table
// decodes as exactly the draft (its `name` key excluded, because that key is
// the header, not a stored field).
func keyedReadBack(out []byte, family, name string, entry map[string]any,
	wantNames []string, reserved map[string]bool) error {
	if _, err := ParseBytes(out); err != nil {
		return fmt.Errorf("rewrite produced an unparsable document (nothing was written): %w", err)
	}
	after, err := KeyedEntryNames(out, family, reserved)
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
	if entry == nil {
		return nil
	}
	got, ok, err := KeyedEntryValue(out, family, name, reserved)
	if err != nil || !ok {
		return fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	stored := make(map[string]any, len(entry))
	for k, v := range entry {
		if k != "name" {
			stored[k] = v
		}
	}
	if !entryMapEqual(got, stored) {
		return fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	return nil
}

// cutBlock removes lines[start:end] and tidies the separators the removal
// leaves behind — the same collapse DeleteEntryTOML performs, shared so the
// two shapes cannot drift on what a deleted last block leaves at EOF.
func cutBlock(lines []string, start, end int) string {
	cut := append([]string{}, lines[:start]...)
	rest := append([]string{}, lines[end+1:]...)
	for len(cut) > 0 && strings.TrimSpace(cut[len(cut)-1]) == "" &&
		len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
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
	return strings.Join(append(cut, rest...), "\n")
}

// keyedBlockSpan locates the [family.<name>] block: its header line through
// the last line belonging to it — tables nested under it included — minus
// trailing blank and comment lines, which are glued to whatever comes next.
func keyedBlockSpan(lines []string, family, name string) (start, end int, err error) {
	want := append(strings.Split(family, "."), name)
	start = -1
	for i, line := range lines {
		if path, array, ok := tableHeaderPath(line); ok && !array && pathEqual(path, want) {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, fmt.Errorf("[%s.%s] not found in the document", family, name)
	}
	// Any header ends the block — an array-of-tables one included, because a
	// following [[routines]] is emphatically not part of this endpoint even
	// though this editor never owns one.
	end = len(lines) - 1
	for i := start + 1; i < len(lines); i++ {
		path, _, ok := tableHeaderPath(lines[i])
		if !ok {
			continue
		}
		if !pathHasPrefix(path, want) {
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

// lastKeyedBlockEnd reports the last line belonging to any existing
// [family.*] table, so a created entry joins its section instead of landing
// at the end of the file behind unrelated tables. ok false means the family
// has no tables to join.
func lastKeyedBlockEnd(lines []string, family string, names []string) (int, bool) {
	last, found := -1, false
	for _, name := range names {
		_, end, err := keyedBlockSpan(lines, family, name)
		if err != nil {
			// The name decoded but its header did not: a dotted-key spelling
			// the block scanner cannot address. Appending at the end of the
			// document is the honest fallback — and the read-back guard is
			// what proves the result still says what was intended.
			continue
		}
		if end > last {
			last, found = end, true
		}
	}
	return last, found
}

// tableHeaderPath parses a table header line into its dotted path, reporting
// whether it is an array-of-tables header ([[x]]) rather than a single table
// ([x]). It accepts what TOML accepts — whitespace around the dots, quoted
// segments — so a hand-formatted [ai . "openai"] addresses the same table the
// loader decoded, and it recognises array headers as well because those END a
// keyed block even though this editor never owns one.
func tableHeaderPath(line string) (path []string, array bool, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return nil, false, false
	}
	array = strings.HasPrefix(trimmed, "[[")
	// Strip any trailing comment, then the closing bracket. A `#` inside a
	// quoted segment is not a comment, so quotes are tracked.
	body, ok := headerBody(trimmed, array)
	if !ok {
		return nil, array, false
	}
	var segment strings.Builder
	inQuote := byte(0)
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inQuote != 0 && c == inQuote:
			inQuote = 0
		case inQuote != 0:
			segment.WriteByte(c)
		case c == '"' || c == '\'':
			inQuote = c
		case c == '.':
			path = append(path, strings.TrimSpace(segment.String()))
			segment.Reset()
		default:
			segment.WriteByte(c)
		}
	}
	if inQuote != 0 {
		return nil, array, false
	}
	path = append(path, strings.TrimSpace(segment.String()))
	for _, p := range path {
		if p == "" {
			return nil, array, false
		}
	}
	return path, array, true
}

// headerBody returns the text between a header line's brackets.
func headerBody(trimmed string, array bool) (string, bool) {
	open := 1
	if array {
		open = 2
	}
	inQuote := byte(0)
	for i := open; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case inQuote != 0 && c == inQuote:
			inQuote = 0
		case inQuote != 0:
		case c == '"' || c == '\'':
			inQuote = c
		case c == ']':
			return trimmed[open:i], true
		}
	}
	return "", false
}

// pathEqual compares two table paths segment by segment.
func pathEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pathHasPrefix reports whether path is prefix or nested beneath it.
func pathHasPrefix(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	return pathEqual(path[:len(prefix)], prefix)
}

// containsString is the exact-match membership test keyed addressing uses.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// decodeKeyedEntries decodes the [family.<name>] tables generically, walking
// the dotted family name through plain tables. Absent segments mean no
// entries, not an error. Only map-valued children are entries, and reserved
// names never are — the parser decides which is which, never a second grammar.
func decodeKeyedEntries(doc []byte, family string, reserved map[string]bool) (map[string]map[string]any, error) {
	node, err := decodeNode(doc, family)
	if err != nil {
		return nil, err
	}
	table, ok := node.(map[string]any)
	if !ok {
		return map[string]map[string]any{}, nil
	}
	out := make(map[string]map[string]any, len(table))
	for name, value := range table {
		if reserved[name] {
			continue
		}
		if sub, ok := value.(map[string]any); ok {
			out[name] = sub
		}
	}
	return out, nil
}

// renderKeyedEntryTOML renders one whole table in the shape a hand-written
// section uses: the header, then the drafted keys in keyOrder. The draft's
// `name` is the header and is never written into the body — it is the entry's
// identity, not one of its fields, and a stored copy could disagree with it.
func renderKeyedEntryTOML(family, name string, entry map[string]any, keyOrder []string) ([]string, error) {
	if !plainTableKey(name) {
		return nil, fmt.Errorf("name: %q cannot be a table name; use letters, digits, dashes or underscores", name)
	}
	lines := []string{"[" + family + "." + name + "]"}
	for _, key := range orderedEntryKeys(entry, keyOrder) {
		if key == "name" {
			continue
		}
		if _, ok := entrySubTables(entry[key]); ok {
			return nil, fmt.Errorf("%s: a [%s.<name>] table holds no sub-tables", key, family)
		}
		encoded, err := encodeEntryValue(entry[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		lines = append(lines, key+" = "+encoded)
	}
	return lines, nil
}

// plainTableKey reports whether name can be written as a bare TOML key. The
// families that use this editor validate their names to the same set, so a
// refusal here is a caller that skipped validation — and rendering a quoted
// header for an exotic name would produce a table the loader's own map lookup
// (ai.provider, advisor names in a tool schema) could not address anyway.
func plainTableKey(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
