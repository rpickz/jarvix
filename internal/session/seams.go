package session

// This file exports, without changing them, the three decisions the voice
// corpus harness asserts a real recording against (issue #143).
//
// The corpus exists because every STT-adjacent behaviour in this repository is
// tested with *faked* transcripts: the tests prove what happens after whisper,
// never that whisper and the bias prompt produce those transcripts from real
// speech. Closing that loop means running a WAV through the real engine and
// then asking the real downstream code what it made of the result — and "the
// real downstream code" is the wording that matters. A harness with its own
// copy of the alias comparison or its own yes/no vocabulary would prove that
// the copy still works, which is worth nothing: the whole point is that a
// change to `assistant.aliases`, or to the affirmative word list, shows up as
// a recording that stopped being understood.
//
// So the harness reaches these three functions, and only these three. They are
// thin exports rather than renamed originals so that the engine's own call
// sites and their tests stay as they were; the alternative — exporting the
// originals — would spread a rename across a file the corpus has no business
// touching, for no gain in honesty.
//
// Everything here is pure: a string in, a decision out, no engine, no session,
// no I/O. That is what makes them safe to call from a test binary that has no
// daemon behind it.

// StripWakeWord removes the assistant's summons from the front of a
// transcript, exactly as the engine does before routing a hands-free
// utterance. See stripWakeWord for the rules and their reasoning.
func StripWakeWord(transcript, word string, aliases []string) string {
	return stripWakeWord(transcript, word, aliases)
}

// WakeWordLeads reports whether the transcript opens with the assistant's
// name, or with one of the spellings configured as an alias for it.
//
// This is the question StripWakeWord's return value cannot answer on its own:
// a transcript that is *only* the name comes back unchanged, deliberately (an
// empty transcript would be worse), and so does a transcript that never
// mentioned the name. For the corpus recording that is just the spoken word
// "Jarvix", telling those two apart is the entire test.
func WakeWordLeads(transcript, word string, aliases []string) bool {
	_, _, ok := wakeWordPrefix(transcript, word, aliases)
	return ok
}

// IsAffirmative interprets a spoken confirmation reply — the gate that decides
// whether a heard "yes" runs a tool. See the vocabulary above isAffirmative
// for why the matching is as strict as it is.
func IsAffirmative(text string) bool {
	return isAffirmative(text)
}
