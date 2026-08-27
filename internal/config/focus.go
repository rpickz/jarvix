package config

// Focus configures focus threads (#123, ADR 0041): the named pieces of work
// behind "new thread …", "switch to the … thread", and timeboxed focus
// sessions. The feature itself has no enable switch — threads only exist
// when the user creates one, so an empty store already is "off" — and the
// thread data lives in its own hand-editable state file (focus.toml), not
// here: configuration carries policy, state carries work.
type Focus struct {
	// MidpointCheckin speaks a halfway line during a timeboxed focus session
	// ("Halfway — thirteen minutes left on the refactor."). Off by default,
	// deliberately: a timebox is a promise of quiet, and anything that
	// speaks into the middle of it is opted into, never inherited.
	MidpointCheckin bool `toml:"midpoint_checkin"`
}
