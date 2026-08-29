package intent

import "testing"

// The mutation report's survivor in this package (issue #172,
// docs/mutation.md), killed by the example the properties could not supply.
//
// A property says "whatever parses, resolves to the future and reads back as
// itself". It is silent about which characters the parser thinks are digits,
// because a parser that rejected '9' would simply parse fewer things and every
// law would still hold. Mutation testing is what asks that question.

// TestNineIsADigit kills CONDITIONALS_BOUNDARY at when.go:496 (`r > '9'` in
// isDigits).
//
// isDigits guards the two places a numeral reaches the clock: the fused "9pm"
// token, and the two-digit minutes that "15:09" normalises to. Move the bound
// by one and the parser stops believing in nines — "at 9pm" and "at 15:09"
// become misses and fall through to the model, silently, for one ninth of the
// clock.
func TestNineIsADigit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want When
	}{
		{"at 9pm", When{Hour: 21}},
		{"at 9am", When{Hour: 9}},
		{"at 19:09", When{Hour: 19, Minute: 9}},
		{"at 15:59", When{Hour: 15, Minute: 59}},
		{"at 09:09", When{Hour: 9, Minute: 9, Ambiguous: true}},
	} {
		got, ok := ParseWhen(tc.in)
		if !ok {
			t.Errorf("%q did not parse; every one of its characters is a digit", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("%q parsed as %+v, want %+v", tc.in, got, tc.want)
		}
	}
	// The other side of the bound: a character just past '9' is not a digit,
	// so a rule that widened rather than narrowed also fails here.
	for _, in := range []string{"at :pm", "at 15:0:"} {
		if w, ok := ParseWhen(in); ok {
			t.Errorf("%q parsed as %+v", in, w)
		}
	}
}
