package tools

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SpreadsheetRenderer is format "spreadsheet": CSV the model writes, saved
// verbatim as a .csv file and opened in the user's spreadsheet application.
// CSV is the v1 spreadsheet contract (see issue #6) — every spreadsheet app
// opens it, and no formula engine is dragged in. A passthrough with a
// validator: the source is parsed in full before anything is written,
// because a CSV with broken quoting or ragged rows opens as scrambled cells
// — worse than no file, and invisible until the user looks.
type SpreadsheetRenderer struct{ passthrough }

// Format implements Renderer.
func (*SpreadsheetRenderer) Format() string { return "spreadsheet" }

// SourceExt implements Renderer.
func (*SpreadsheetRenderer) SourceExt() string { return ".csv" }

// OutputExt implements Renderer. Same as SourceExt: the saved CSV is the
// artifact.
func (*SpreadsheetRenderer) OutputExt() string { return ".csv" }

// ValidateSource implements SourceValidator by parsing the whole document
// with encoding/csv in strict mode (no lazy quotes). Anything that parses
// here round-trips: quoted cells with embedded commas, quotes, and newlines
// come back as the same cells in any RFC 4180 reader. The parser's own
// errors carry line numbers ("record on line 7: wrong number of fields"),
// which is exactly the specificity the model's retry round needs.
func (*SpreadsheetRenderer) ValidateSource(source string) error {
	reader := csv.NewReader(strings.NewReader(source))
	// The default FieldsPerRecord (0) locks the column count to the first
	// row, so ragged rows fail with the offending line number instead of
	// silently shifting cells in the spreadsheet app.
	rows := 0
	for {
		_, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("CSV does not parse: %v. Every row must have the same number "+
				"of columns; wrap any cell containing commas, quotes, or newlines in double "+
				"quotes and double any embedded quotes", err)
		}
		rows++
	}
	if rows == 0 {
		return fmt.Errorf("CSV has no rows")
	}
	return nil
}
