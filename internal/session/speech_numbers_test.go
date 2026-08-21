package session

import (
	"strings"
	"testing"
)

// TestSpokenNumbersShapes pins one case per number shape an assistant
// actually produces. The assertions are on the normalised string, not on how
// it sounds — that is the whole point of expanding here rather than leaving
// it to the engine (issue #30).
func TestSpokenNumbersShapes(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		// The reported defect: Kokoro drops the point and says "nine two".
		"decimal keeps its point":  {"9.2 million", "nine point two million"},
		"decimal digit by digit":   {"3.14", "three point one four"},
		"decimal under one":        {"0.5", "zero point five"},
		"negative decimal":         {"-3.5", "minus three point five"},
		"percentage":               {"82.4%", "eighty two point four percent"},
		"whole percentage":         {"50%", "fifty percent"},
		"pounds and pence":         {"£3.50", "three pounds fifty"},
		"whole pounds":             {"£1.00", "one pound"},
		"pence only":               {"£0.99", "ninety nine pence"},
		"singular currency":        {"$1", "one dollar"},
		"euros":                    {"€10", "ten euros"},
		"currency with magnitude":  {"$9.2m", "nine point two million dollars"},
		"billions":                 {"£2bn", "two billion pounds"},
		"version with a v":         {"v1.5.2", "version one point five point two"},
		"two-part version":         {"v1.5", "version one point five"},
		"bare version":             {"1.5.2", "one point five point two"},
		"seconds":                  {"4.7s", "four point seven seconds"},
		"spelled-out seconds":      {"4.7 seconds", "four point seven seconds"},
		"milliseconds":             {"250ms", "two hundred and fifty milliseconds"},
		"hours":                    {"5h", "five hours"},
		"minutes":                  {"2 mins", "two minutes"},
		"gigabytes":                {"1.5GB", "one point five gigabytes"},
		"megabytes":                {"512MB", "five hundred and twelve megabytes"},
		"singular byte unit":       {"1KB", "one kilobyte"},
		"binary byte unit":         {"64MiB", "sixty four mebibytes"},
		"range":                    {"3-5", "three to five"},
		"range of large numbers":   {"8080-8090", "eight thousand and eighty to eight thousand and ninety"},
		"first":                    {"1st", "first"},
		"twenty first":             {"21st", "twenty first"},
		"forty second":             {"42nd", "forty second"},
		"hundredth":                {"100th", "one hundredth"},
		"british and in hundreds":  {"250ms", "two hundred and fifty milliseconds"},
		"british and in thousands": {"1.024s", "one point zero two four seconds"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := spokenNumbers(c.in); got != c.want {
				t.Errorf("spokenNumbers(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSpokenNumbersLeavesOtherNumbersAlone pins the other half of the
// contract: a number that belongs to something else, or that the engine
// already reads correctly, must survive untouched. Getting this wrong is
// worse than not expanding at all — it turns an address into prose.
func TestSpokenNumbersLeavesOtherNumbersAlone(t *testing.T) {
	for _, in := range []string{
		"port 8080",           // bare integers read correctly already
		"1995",                // a year must not become a mouthful
		"sail-8.5/app",        // an image tag inside a path
		"127.0.0.1:8080",      // an address with a port
		"2026-08-21",          // an ISO date, not a range
		"a 3-day sprint",      // a hyphenated compound, not a range
		"COVID-19",            // a name that ends in digits
		"H100",                // an identifier that starts with a letter
		"1,024 files",         // a thousands separator we do not parse
		"/var/log/syslog.1",   // a rotated log file
		"12345678901234567.5", // longer than the expander will parse
		"user_2fa",            // an identifier with an underscore
	} {
		t.Run(in, func(t *testing.T) {
			if got := spokenNumbers(in); got != in {
				t.Errorf("spokenNumbers(%q) = %q, want it unchanged", in, got)
			}
		})
	}
}

// TestSpeechTextGoldenAnswer is the end-to-end shape of the feature: a
// realistic answer, with the figures an assistant reports, read back as a
// person would say it.
func TestSpeechTextGoldenAnswer(t *testing.T) {
	in := "**Disk check:** the root volume is 82.4% full — 1.5GB free of 512GB. " +
		"The last backup took 4.7s and moved 9.2 million files.\n" +
		"- `postgresql` is on v1.5.2\n" +
		"- nginx restarts take 3-5 seconds\n" +
		"Storage costs £3.50 a month, or $9.2m across the fleet."
	want := "Disk check: the root volume is eighty two point four percent full — " +
		"one point five gigabytes free of five hundred and twelve gigabytes. " +
		"The last backup took four point seven seconds and moved nine point two million files. " +
		"post gres queue ell is on version one point five point two. " +
		"engine ex restarts take three to five seconds. " +
		"Storage costs three pounds fifty a month, or nine point two million dollars across the fleet."
	if got := speechText(in); got != want {
		t.Errorf("speechText golden answer:\n got %q\nwant %q", got, want)
	}
}

// A number the expander cannot make sense of must pass through rather than
// error: a bad figure should sound wrong at worst, never fail the session.
func TestSpokenNumbersSurvivesPathologicalInput(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("9", 400),
		"9." + strings.Repeat("9", 400),
		strings.Repeat("1.", 200) + "1",
		"£" + strings.Repeat("9", 40) + ".99",
		"%%%1..2..3%%%",
		"",
	} {
		if got := spokenNumbers(in); got == "" && in != "" {
			t.Errorf("spokenNumbers(%q) emptied the text", in)
		}
	}
}

func TestSpellCardinal(t *testing.T) {
	cases := map[uint64]string{
		0: "zero", 7: "seven", 13: "thirteen", 20: "twenty", 42: "forty two",
		100: "one hundred", 105: "one hundred and five", 250: "two hundred and fifty",
		1000: "one thousand", 1024: "one thousand and twenty four",
		1234:      "one thousand two hundred and thirty four",
		1000000:   "one million",
		9200000:   "nine million two hundred thousand",
		123456789: "one hundred and twenty three million four hundred and fifty six thousand seven hundred and eighty nine",
	}
	for n, want := range cases {
		if got := spellCardinal(n); got != want {
			t.Errorf("spellCardinal(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestSpellOrdinal(t *testing.T) {
	cases := map[uint64]string{
		1: "first", 2: "second", 3: "third", 4: "fourth", 5: "fifth", 8: "eighth",
		9: "ninth", 11: "eleventh", 12: "twelfth", 20: "twentieth", 21: "twenty first",
		42: "forty second", 100: "one hundredth", 1000: "one thousandth",
	}
	for n, want := range cases {
		if got := spellOrdinal(n); got != want {
			t.Errorf("spellOrdinal(%d) = %q, want %q", n, got, want)
		}
	}
}
