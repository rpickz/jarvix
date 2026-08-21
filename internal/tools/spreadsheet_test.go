package tools

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The quoting matrix: every RFC 4180 corner the model is likely to emit
// must validate, because rejecting good CSV burns the model's one retry.
func TestSpreadsheetValidatesQuotingMatrix(t *testing.T) {
	r := &SpreadsheetRenderer{}
	for name, source := range map[string]string{
		"plain cells":         "a,b,c\n1,2,3\n",
		"embedded commas":     "name,notes\n\"Smith, Jane\",ok\n",
		"embedded quotes":     "quote\n\"she said \"\"hi\"\"\"\n",
		"embedded newlines":   "note\n\"line one\nline two\"\n",
		"empty cells":         "a,b\n,\n",
		"single column":       "only\n1\n2\n",
		"no trailing newline": "a,b\n1,2",
		"unicode":             "città,naïve\n→,✓\n",
	} {
		if err := r.ValidateSource(source); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

// Broken CSV must fail with an error the model can act on — the line number
// and the nature of the fault, not a bare "invalid".
func TestSpreadsheetValidationErrorsAreSpecific(t *testing.T) {
	r := &SpreadsheetRenderer{}
	for name, tc := range map[string]struct {
		source string
		want   string
	}{
		"ragged row":         {"a,b,c\n1,2\n", "line 2"},
		"bare quote":         {"a,b\nx\"y,2\n", "bare \" in non-quoted-field"},
		"unterminated quote": {"a,b\n\"open,2\n", "\" in quoted-field"},
		"no rows":            {"\n\n", "no rows"},
	} {
		err := r.ValidateSource(tc.source)
		if err == nil {
			t.Errorf("%s: must be rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name the fault (%q)", name, err, tc.want)
		}
	}
}

// The golden file lands byte-for-byte and, read back through encoding/csv,
// yields exactly the cells it encodes — the round-trip the AC demands.
func TestSpreadsheetGoldenRoundTrip(t *testing.T) {
	golden, err := os.ReadFile("testdata/spreadsheet_quoting.csv")
	if err != nil {
		t.Fatal(err)
	}
	got := goldenArtifact(t, &SpreadsheetRenderer{}, "quoting matrix", string(golden))
	if !bytes.Equal(got, golden) {
		t.Errorf("CSV altered on save:\ngot:  %q\nwant: %q", got, golden)
	}
	records, err := csv.NewReader(bytes.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("saved CSV does not parse: %v", err)
	}
	want := [][]string{
		{"name", "notes", "amount"},
		{"plain", "simple cell", "10"},
		{"comma, inside", `says "hello" twice`, "20"},
		{"multi\nline cell", "trailing spaces  ", "30"},
		{"empty", "", "40"},
	}
	if !reflect.DeepEqual(records, want) {
		t.Errorf("cells = %q, want %q", records, want)
	}
}

// A 500-row table (the AC's size bar) must validate, save, and read back
// complete — no truncation anywhere in the pipeline.
func TestSpreadsheet500RowsSurviveIntact(t *testing.T) {
	var b strings.Builder
	b.WriteString("id,name,notes\n")
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&b, "%d,\"row, %d\",\"note \"\"%d\"\"\"\n", i, i, i)
	}
	got := goldenArtifact(t, &SpreadsheetRenderer{}, "big table", b.String())
	records, err := csv.NewReader(bytes.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("saved CSV does not parse: %v", err)
	}
	if len(records) != 501 {
		t.Fatalf("rows = %d, want 501 (header + 500)", len(records))
	}
	if last := records[500]; last[0] != "500" || last[1] != "row, 500" || last[2] != `note "500"` {
		t.Errorf("last row = %q", last)
	}
}
