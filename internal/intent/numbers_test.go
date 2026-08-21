package intent

import (
	"strconv"
	"strings"
	"testing"
)

// TestNumberWordsRoundTripThroughRange is the exhaustive proof the NFR asks
// for: every value 0–150 must survive being spoken and read back, in both the
// word form and the digit form, because "volume thirty" and "volume 30" are
// the same request and only one of them can be wrong at a time.
func TestNumberWordsRoundTripThroughRange(t *testing.T) {
	for n := 0; n <= maxVolume; n++ {
		words := spokenWords(n)
		got, ok := parseNumber(words)
		if !ok || got != n {
			t.Errorf("%d spoken as %q parsed back as %d (ok=%v)", n, strings.Join(words, " "), got, ok)
		}
		if len(words) > maxSlotWords {
			t.Errorf("%d spells as %d words, more than a slot may consume (%d)", n, len(words), maxSlotWords)
		}
		digits, ok := parseNumber([]string{strconv.Itoa(n)})
		if !ok || digits != n {
			t.Errorf("%d as digits parsed as %d (ok=%v)", n, digits, ok)
		}
	}
}

func TestParseNumberVariants(t *testing.T) {
	tests := []struct {
		words string
		want  int
		ok    bool
	}{
		{"zero", 0, true},
		{"nought", 0, true},
		{"oh", 0, true},
		{"0", 0, true},
		{"seven", 7, true},
		{"nineteen", 19, true},
		{"twenty", 20, true},
		{"twenty five", 25, true},
		{"forty", 40, true},
		{"fourty", 40, true}, // the transcription misspelling
		{"ninety nine", 99, true},
		{"hundred", 100, true},
		{"a hundred", 100, true},
		{"one hundred", 100, true},
		{"one hundred and five", 105, true},
		{"one hundred five", 105, true},
		{"a hundred and fifty", 150, true},
		{"one hundred and forty five", 145, true},
		{"150", 150, true},
		{"999", 999, true},
		// Rejected: not numbers, or not the shapes English actually uses.
		{"", 0, false},
		{"a", 0, false},
		{"and", 0, false},
		{"five twenty", 0, false},
		{"twenty twenty", 0, false},
		{"twenty zero", 0, false},
		{"one hundred and", 0, false},
		{"hundred hundred", 0, false},
		{"thirty five six", 0, false},
		{"thirtyfive", 0, false},
		{"1000", 0, false},
		{"-1", 0, false},
		{"3.5", 0, false},
		{"0x10", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.words, func(t *testing.T) {
			got, ok := parseNumber(strings.Fields(tc.words))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %d)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("= %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSpokenNumber(t *testing.T) {
	tests := map[int]string{
		0: "zero", 1: "one", 11: "eleven", 20: "twenty", 21: "twenty-one",
		30: "thirty", 45: "forty-five", 99: "ninety-nine", 100: "one hundred",
		105: "one hundred and five", 150: "one hundred and fifty",
	}
	for n, want := range tests {
		if got := SpokenNumber(n); got != want {
			t.Errorf("SpokenNumber(%d) = %q, want %q", n, got, want)
		}
	}
	// Out of range falls back to digits rather than lying.
	if got := SpokenNumber(4096); got != "4096" {
		t.Errorf("SpokenNumber(4096) = %q", got)
	}
}
