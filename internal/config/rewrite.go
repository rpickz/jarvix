package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// This file rewrites config.toml in place for the settings surface
// (config.set). The file stays authoritative and hand-editable, so the
// rewrite is surgical: only the changed key's value is touched — comments,
// unknown keys, ordering, and formatting elsewhere are preserved
// byte-for-byte. A managed section was rejected because TOML forbids
// defining the same table twice, and a full re-serialisation was rejected
// because it destroys the user's comments (ADR 0015).
//
// Safety over cleverness: after editing, the result must re-parse and every
// changed key must read back with its new value, otherwise the rewrite fails
// and nothing is written. A scanner bug can therefore cost a save, never the
// user's file.

// FingerprintMissing is the fingerprint of a config file that does not exist.
const FingerprintMissing = "missing"

// Fingerprint identifies file content for external-edit detection: config.get
// hands it to clients, config.set compares it against the file found on disk.
func Fingerprint(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

// FingerprintFile fingerprints the file at path ("missing" when absent).
func FingerprintFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return FingerprintMissing, nil
	}
	if err != nil {
		return "", err
	}
	return Fingerprint(data), nil
}

// ParseBytes parses a TOML document with defaults applied, like Load but from
// memory. Empty input yields the defaults.
func ParseBytes(data []byte) (Config, error) {
	return parse(data, Default())
}

// RewriteTOML returns doc with each dotted key in changes set to its new
// (native-typed) value: existing keys are replaced in place, missing keys are
// appended to their table, and missing tables are appended to the document.
// Values must be coerced (Setting.Coerce) before calling.
func RewriteTOML(doc []byte, changes map[string]any) ([]byte, error) {
	// Deterministic order so repeated rewrites are byte-identical.
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := string(doc)
	for _, key := range keys {
		// A map is a TOML *table*, not a value on a line, so it is edited as
		// a table — see rewriteTable.
		if table, ok := changes[key].(map[string]string); ok {
			var err error
			if out, err = rewriteTable(out, key, table); err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			continue
		}
		encoded, err := encodeTOMLValue(changes[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out, err = rewriteOne(out, key, encoded)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}

	// The guard that makes the surgical editor safe: the result must parse,
	// and every change must read back. Failure aborts the save.
	parsed, err := parse([]byte(out), Default())
	if err != nil {
		return nil, fmt.Errorf("rewrite produced an unparsable document (nothing was written): %w", err)
	}
	for _, key := range keys {
		s, ok := SettingFor(key)
		if !ok {
			return nil, fmt.Errorf("%s: not an editable setting", key)
		}
		if !settingValuesEqual(s.Get(parsed), changes[key]) {
			return nil, fmt.Errorf("%s: rewrite did not take effect (nothing was written)", key)
		}
	}
	return []byte(out), nil
}

// WriteFileAtomic writes data to path via a same-directory temp file and
// rename, mode 0600, so a crash mid-write can never leave a truncated config.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restrict temp config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// settingValuesEqual compares a parsed-back value with the intended one.
// Empty string lists compare equal regardless of nil-ness.
func settingValuesEqual(got, want any) bool {
	if g, ok := got.([]string); ok {
		if w, ok := want.([]string); ok && len(g) == 0 && len(w) == 0 {
			return true
		}
	}
	return reflect.DeepEqual(got, want)
}

// ------------------------------------------------------------ the editor

// keySpan locates one key's value in the document's lines.
type keySpan struct {
	startLine int    // line holding "key ="
	endLine   int    // last line of the value (== startLine for one-liners)
	valStart  int    // byte offset of the value on startLine
	suffix    string // endLine content after the value (inline comment etc.)
}

// docIndex is a scan of the document: where each table and dotted key lives.
type docIndex struct {
	lines  []string
	tables map[string]int     // table name → header line
	last   map[string]int     // table name → last content line of the table
	keys   map[string]keySpan // dotted key → value location
}

// rewriteOne sets a single dotted key to an encoded value, re-scanning the
// document each time (config files are tiny; simplicity wins).
func rewriteOne(doc, key, encoded string) (string, error) {
	// Preserve a trailing-newline-less document's shape; work line-wise.
	lines := strings.Split(doc, "\n")
	idx, err := scanDoc(lines)
	if err != nil {
		return "", err
	}

	if span, ok := idx.keys[key]; ok {
		prefix := lines[span.startLine][:span.valStart]
		replaced := prefix + encoded + span.suffix
		out := append([]string{}, lines[:span.startLine]...)
		out = append(out, replaced)
		out = append(out, lines[span.endLine+1:]...)
		return strings.Join(out, "\n"), nil
	}

	// Key absent: find the deepest existing table that prefixes the key.
	parts := strings.Split(key, ".")
	for cut := len(parts) - 1; cut >= 1; cut-- {
		table := strings.Join(parts[:cut], ".")
		if _, ok := idx.tables[table]; !ok {
			continue
		}
		bare := strings.Join(parts[cut:], ".")
		insertAt := idx.last[table] + 1
		out := append([]string{}, lines[:insertAt]...)
		out = append(out, bare+" = "+encoded)
		out = append(out, lines[insertAt:]...)
		return strings.Join(out, "\n"), nil
	}

	// No table to hold it: append one at the end of the document.
	table := strings.Join(parts[:len(parts)-1], ".")
	bare := parts[len(parts)-1]
	out := strings.TrimRight(doc, "\n")
	if out != "" {
		out += "\n\n"
	}
	return out + "[" + table + "]\n" + bare + " = " + encoded + "\n", nil
}

// rewriteTable sets a whole table ([tts.lexicon]) to the given entries. A
// table-valued setting cannot go through rewriteOne: TOML forbids defining
// the same table twice, so writing `lexicon = { … }` under [tts] next to an
// existing [tts.lexicon] section would produce a document that does not
// parse. The table's own body is replaced instead, which is also the shape a
// hand-editor wrote and expects to keep reading.
//
// The body — every line from the header to the table's last key — is
// replaced wholesale, so comments *inside* the table do not survive being
// rewritten from the settings surface. Comments elsewhere, including the ones
// above the header, are untouched.
func rewriteTable(doc, key string, entries map[string]string) (string, error) {
	lines := strings.Split(doc, "\n")
	idx, err := scanDoc(lines)
	if err != nil {
		return "", err
	}
	// An inline table already on a line ("lexicon = { … }") stays inline.
	if _, ok := idx.keys[key]; ok {
		return rewriteOne(doc, key, encodeTOMLInlineTable(entries))
	}

	body := encodeTOMLTableBody(entries)
	if header, ok := idx.tables[key]; ok {
		out := append([]string{}, lines[:header+1]...)
		out = append(out, body...)
		out = append(out, lines[idx.last[key]+1:]...)
		return strings.Join(out, "\n"), nil
	}

	out := strings.TrimRight(doc, "\n")
	if out != "" {
		out += "\n\n"
	}
	out += "[" + key + "]\n"
	for _, line := range body {
		out += line + "\n"
	}
	return out, nil
}

// encodeTOMLTableBody renders the entries of a table, one key per line, in a
// deterministic order so repeated rewrites are byte-identical.
func encodeTOMLTableBody(entries map[string]string) []string {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	body := make([]string, 0, len(keys))
	for _, k := range keys {
		body = append(body, encodeTOMLKey(k)+" = "+encodeTOMLString(entries[k]))
	}
	return body
}

// encodeTOMLInlineTable renders entries as `{ a = "b", c = "d" }`.
func encodeTOMLInlineTable(entries map[string]string) string {
	body := encodeTOMLTableBody(entries)
	if len(body) == 0 {
		return "{}"
	}
	return "{ " + strings.Join(body, ", ") + " }"
}

// encodeTOMLKey renders a table key, quoting anything that is not a bare key.
func encodeTOMLKey(k string) string {
	for i := 0; i < len(k); i++ {
		c := k[i]
		bare := c == '_' || c == '-' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !bare {
			return encodeTOMLString(k)
		}
	}
	if k == "" {
		return `""`
	}
	return k
}

// scanDoc walks the document tracking the current table and the extent of
// every key's value, including multi-line strings and arrays. It only needs
// to be right for documents that already parse — callers parse first — and a
// re-parse guard backstops it besides.
func scanDoc(lines []string) (docIndex, error) {
	idx := docIndex{
		lines:  lines,
		tables: make(map[string]int),
		last:   make(map[string]int),
		keys:   make(map[string]keySpan),
	}
	table := ""
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			name := strings.TrimSpace(strings.Trim(trimmed, "[]"))
			table = name
			if _, seen := idx.tables[name]; !seen {
				idx.tables[name] = i
			}
			idx.last[name] = i
			continue
		}
		eq := strings.Index(lines[i], "=")
		if eq < 0 {
			// A continuation would have been consumed by scanValue; anything
			// else is a document this editor does not understand.
			return idx, fmt.Errorf("line %d: expected `key = value`", i+1)
		}
		key := strings.TrimSpace(lines[i][:eq])
		full := key
		if table != "" {
			full = table + "." + key
		}
		span, err := scanValue(lines, i, eq+1)
		if err != nil {
			return idx, err
		}
		idx.keys[full] = span
		if table != "" {
			idx.last[table] = span.endLine
		}
		i = span.endLine
	}
	return idx, nil
}

// String/nesting states for scanValue.
type scanState int

const (
	stNone scanState = iota
	stBasic
	stLiteral
	stMLBasic
	stMLLiteral
)

// scanValue finds the extent of the value starting at lines[start][from:]:
// the first value byte, the line it ends on, and any trailing inline comment.
// Comments inside arrays are skipped; a '#' at nesting depth zero ends the
// value.
func scanValue(lines []string, start, from int) (keySpan, error) {
	span := keySpan{startLine: start}
	state := stNone
	depth := 0
	lastLine, lastCol := -1, -1 // last byte that belongs to the value

	for li := start; li < len(lines); li++ {
		line := lines[li]
		col := 0
		if li == start {
			col = from
			// The value itself must begin on the key's line (TOML rule).
			for col < len(line) && (line[col] == ' ' || line[col] == '\t') {
				col++
			}
			span.valStart = col
		}
		for col < len(line) {
			c := line[col]
			switch state {
			case stBasic:
				switch c {
				case '\\':
					col++ // skip the escaped byte
				case '"':
					state = stNone
				}
			case stLiteral:
				if c == '\'' {
					state = stNone
				}
			case stMLBasic:
				switch c {
				case '\\':
					col++
				case '"':
					if strings.HasPrefix(line[col:], `"""`) {
						state = stNone
						col += 2
					}
				}
			case stMLLiteral:
				if c == '\'' && strings.HasPrefix(line[col:], "'''") {
					state = stNone
					col += 2
				}
			case stNone:
				switch c {
				case '#':
					if depth == 0 {
						// Inline comment after the value.
						if lastLine != li {
							return span, fmt.Errorf("line %d: comment before value", li+1)
						}
						span.endLine = li
						span.suffix = line[lastCol+1:]
						return span, nil
					}
					col = len(line) // comment inside an array: skip the rest
					continue
				case '"':
					if strings.HasPrefix(line[col:], `"""`) {
						state = stMLBasic
						col += 2
					} else {
						state = stBasic
					}
				case '\'':
					if strings.HasPrefix(line[col:], "'''") {
						state = stMLLiteral
						col += 2
					} else {
						state = stLiteral
					}
				case '[', '{':
					depth++
				case ']', '}':
					depth--
				}
			}
			if state != stNone || (c != ' ' && c != '\t') {
				lastLine, lastCol = li, col
			}
			col++
		}
		if state == stNone && depth == 0 {
			if lastLine != li {
				return span, fmt.Errorf("line %d: no value found", start+1)
			}
			span.endLine = li
			span.suffix = line[lastCol+1:]
			return span, nil
		}
	}
	return span, fmt.Errorf("line %d: value never ends", start+1)
}

// ---------------------------------------------------------- serialisation

// encodeTOMLValue renders a native settings value as a TOML literal.
func encodeTOMLValue(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return encodeTOMLString(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case float64:
		s := strconv.FormatFloat(t, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") || strings.Contains(s, "Inf") || strings.Contains(s, "NaN") {
			if strings.ContainsAny(s, "IN") {
				return "", fmt.Errorf("cannot encode %v as a TOML float", t)
			}
			s += ".0" // TOML floats need a fractional part or exponent
		}
		return s, nil
	case []string:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = encodeTOMLString(e)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	}
	return "", fmt.Errorf("cannot encode a %T as TOML", v)
}

// encodeTOMLString renders a TOML basic string. Go's strconv.Quote is close
// but emits escapes TOML lacks (\a, \v, \xNN), so this stays hand-rolled.
func encodeTOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
