package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// This file writes one [[routines]] entry into config.toml for layout
// capture (#62), under the same contract as the surgical settings editor in
// rewrite.go (ADR 0015): the file stays authoritative and hand-editable, so
// everything outside the one entry being written — comments, unknown keys,
// ordering, formatting — is preserved byte-for-byte, and the result must
// re-parse and read back exactly what was asked for or nothing is returned.
// Array-of-tables need their own editor because rewriteOne addresses dotted
// keys, and every [[routines]] block shares the same dotted names; blocks
// are addressed by position instead, with the parser — not a second grammar
// — deciding which block is which.

// UpsertRoutineTOML returns doc with the [[routines]] entry named entry.Name
// replaced in place (matched case-insensitively) or, when no entry has that
// name, appended at the end of the document. provenance becomes a comment
// line above the entry's header ("captured 2026-08-21"); notes[i], when not
// empty, becomes a comment line above step i's table — the TODO a
// placeholder step carries. Replacing consumes only a directly preceding
// provenance comment this writer itself wrote; every hand-written comment
// stays.
func UpsertRoutineTOML(doc []byte, entry Routine, provenance string, notes []string) ([]byte, error) {
	original, err := ParseBytes(doc)
	if err != nil {
		return nil, fmt.Errorf("config.toml does not parse; fix it by hand first: %w", err)
	}
	// Normalise before rendering. The renderer writes only the current
	// placement vocabulary (ADR 0056), so an entry handed to it with a
	// superseded spelling — `float = true`, `size = [w, h]`, `tile` — would
	// otherwise render as something the read-back check does not recognise
	// and the save would be refused. Translating it here is what makes the
	// promise real: an entry the window or the capture writer touches comes
	// back in the current vocabulary, and nobody is asked to migrate a file.
	entry = normaliseRoutine(entry)
	replaceAt := -1
	for i, r := range original.Routines {
		if strings.EqualFold(strings.TrimSpace(r.Name), strings.TrimSpace(entry.Name)) {
			replaceAt = i
			break
		}
	}

	block := renderRoutineTOML(entry, provenance, notes)
	var out string
	if replaceAt < 0 {
		out = strings.TrimRight(string(doc), "\n")
		if out != "" {
			out += "\n\n"
		}
		out += strings.Join(block, "\n") + "\n"
	} else {
		lines := strings.Split(string(doc), "\n")
		start, end, err := routineBlockSpan(lines, replaceAt)
		if err != nil {
			return nil, err
		}
		replaced := append([]string{}, lines[:start]...)
		replaced = append(replaced, block...)
		replaced = append(replaced, lines[end+1:]...)
		out = strings.Join(replaced, "\n")
	}

	// The guard that makes this editor safe (rewrite.go's, restated for
	// array-of-tables): the result must parse, and the whole routines list
	// must read back as exactly the intended edit — the entry landed, and no
	// other entry moved. A scanner bug costs the save, never the file.
	parsed, err := ParseBytes([]byte(out))
	if err != nil {
		return nil, fmt.Errorf("rewrite produced an unparsable document (nothing was written): %w", err)
	}
	want := append([]Routine{}, original.Routines...)
	if replaceAt < 0 {
		want = append(want, entry)
	} else {
		want[replaceAt] = entry
	}
	if !routinesEqual(parsed.Routines, want) {
		return nil, fmt.Errorf("rewrite did not take effect (nothing was written)")
	}
	return []byte(out), nil
}

// routineBlockSpan locates the index-th [[routines]] block: its header line
// through its last [[routines.steps]] content, minus trailing blank lines,
// extended upward over a provenance comment this writer wrote earlier so a
// replace refreshes it instead of stacking a second one.
func routineBlockSpan(lines []string, index int) (start, end int, err error) {
	seen := -1
	start = -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "[[routines]]" {
			seen++
			if seen == index {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return 0, 0, fmt.Errorf("routines[%d] not found in the document", index)
	}
	end = len(lines) - 1
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "[") && strings.Trim(trimmed, "[]") != "routines.steps" {
			end = i - 1
			break
		}
	}
	for end > start && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "# captured") {
		start--
	}
	return start, end, nil
}

// normaliseRoutine rewrites an entry's placement keys into the current
// vocabulary, by the same round trip through routine.Definition the loader
// and the capture writer already use — so there is one translation and not a
// second copy of it here.
func normaliseRoutine(entry Routine) Routine {
	converted := Config{Routines: []Routine{entry}}.RoutineDefinitions()
	if len(converted) != 1 {
		return entry
	}
	normalised := RoutineFromDefinition(converted[0])
	// RoutineFromDefinition carries only what a Definition holds; the keys
	// that are the entry's own — its identity and its triggers — are copied
	// back so normalising cannot quietly drop a schedule.
	normalised.Enabled = entry.Enabled
	normalised.Schedule = entry.Schedule
	normalised.Announce = entry.Announce
	return normalised
}

// renderRoutineTOML renders one entry in the same shape the documentation's
// worked examples use — steps indented two spaces under their routine — so a
// captured entry reads like a hand-written one.
func renderRoutineTOML(entry Routine, provenance string, notes []string) []string {
	var lines []string
	if provenance != "" {
		lines = append(lines, "# "+provenance)
	}
	lines = append(lines, "[[routines]]",
		"name = "+encodeTOMLString(entry.Name),
		"phrases = "+encodeTOMLStrings(entry.Phrases))
	for i, s := range entry.Steps {
		lines = append(lines, "")
		if i < len(notes) && notes[i] != "" {
			lines = append(lines, "  # "+notes[i])
		}
		lines = append(lines, "  [[routines.steps]]",
			"  app = "+encodeTOMLString(s.App))
		if s.Match != "" {
			lines = append(lines, "  match = "+encodeTOMLString(s.Match))
		}
		lines = append(lines, "  workspace = "+strconv.Itoa(s.Workspace))
		// The placement keys, in the vocabulary's own presentation order and
		// only where the step said something. The superseded spellings
		// (float, size, tile) are deliberately never written: an entry the
		// window or the capture writer touches comes back in the current
		// vocabulary, which is how a file migrates without anyone being told
		// to migrate it.
		if s.Monitor != "" {
			lines = append(lines, "  monitor = "+encodeTOMLString(s.Monitor))
		}
		if s.Mode != "" {
			lines = append(lines, "  mode = "+encodeTOMLString(s.Mode))
		}
		if s.Width != "" {
			lines = append(lines, "  width = "+encodeTOMLString(s.Width))
		}
		if s.Height != "" {
			lines = append(lines, "  height = "+encodeTOMLString(s.Height))
		}
		if len(s.Position) == 2 {
			lines = append(lines, fmt.Sprintf("  position = [%d, %d]", s.Position[0], s.Position[1]))
		}
		if s.PlaceNext != "" {
			lines = append(lines, "  place_next = "+encodeTOMLString(s.PlaceNext))
		}
		if s.Master {
			lines = append(lines, "  master = true")
		}
		if s.Focus != "" {
			lines = append(lines, "  focus = "+encodeTOMLString(s.Focus))
		}
	}
	return lines
}

// encodeTOMLStrings renders a string array literal.
func encodeTOMLStrings(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = encodeTOMLString(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// routinesEqual compares parsed-back routines with the intended ones,
// treating nil and empty slices as the same — TOML has no way to say which
// was meant, so the distinction cannot be load-bearing.
func routinesEqual(got, want []Routine) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Name != want[i].Name || !stringSlicesEqual(got[i].Phrases, want[i].Phrases) {
			return false
		}
		if len(got[i].Steps) != len(want[i].Steps) {
			return false
		}
		for j := range got[i].Steps {
			g, w := got[i].Steps[j], want[i].Steps[j]
			if g.App != w.App || g.Match != w.Match || g.Workspace != w.Workspace ||
				g.Float != w.Float || g.Tile != w.Tile ||
				g.Monitor != w.Monitor || g.Mode != w.Mode ||
				g.Width != w.Width || g.Height != w.Height ||
				g.PlaceNext != w.PlaceNext || g.Master != w.Master || g.Focus != w.Focus ||
				!intSlicesEqual(g.Size, w.Size) || !intSlicesEqual(g.Position, w.Position) {
				return false
			}
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || reflect.DeepEqual(a, b)
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || reflect.DeepEqual(a, b)
}
