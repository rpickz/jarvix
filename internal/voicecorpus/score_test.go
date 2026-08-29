package voicecorpus

import "testing"

func TestScoreMeasuresWhatSurvivedNotHowItWasWritten(t *testing.T) {
	cases := []struct {
		name       string
		say        string
		transcript string
		want       float64
	}{
		{"punctuation and case are whisper's business", "workspace four", " Workspace four.", 1},
		{"an inserted filler is not a recognition failure", "volume up", "um, volume up", 1},
		{"a lost word costs its share", "call this window builds", "call this window bills", 0.75},
		{"nothing heard", "mute", "", 0},
		{"nothing asked", "", "anything at all", 1},
		{"repetition is counted, not collapsed", "no no", "no", 0.5},
		{"an apostrophe stays inside its word", "no don't", "No, don't.", 1},
		{"a digit is not reconciled with its number word", "workspace four", "workspace 4", 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Score(c.say, c.transcript); got != c.want {
				t.Errorf("Score(%q, %q) = %.2f, want %.2f", c.say, c.transcript, got, c.want)
			}
		})
	}
}

func TestContainsWordMatchesWholeWordsOnly(t *testing.T) {
	cases := []struct {
		text, word string
		want       bool
	}{
		{"How many quid did I spend?", "quid", true},
		{"how many quids did i spend", "quid", false},
		{"later, reply to Dan", "dan", true},
		{"nothing like it", "dan", false},
		{"anything", "two words", false}, // refused rather than substring-matched
	}
	for _, c := range cases {
		if got := containsWord(c.text, c.word); got != c.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", c.text, c.word, got, c.want)
		}
	}
}
