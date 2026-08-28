package config

// Briefing configures the return briefing (#150, ADR 0050): what Jarvix says
// about the stretch of time you were not here. The feature reads only what
// Jarvix already participates in — AI sessions anchored to focus threads, its
// own activity, focus threads, reminders, and how many exchanges there were —
// and it is prepared lazily when you come back, never on a timer.
//
// The three switches are the whole policy surface. There is deliberately no
// per-source switch: a briefing that silently omits a source it could read is
// the dishonesty this feature is built to avoid, and a user who does not want
// to hear about a source turns that source off at its own configuration.
type Briefing struct {
	// Enabled is the one global off switch. False means nothing is prepared,
	// nothing is offered, and an explicit ask says the briefing is switched
	// off rather than composing one anyway.
	Enabled bool `toml:"enabled"`
	// AfterHours is how long away counts as an absence worth briefing. Eight
	// hours by default — a night, or a working day spent elsewhere — because
	// the feature exists for the re-entry cost of a long gap, and offering
	// after a lunch break would make the offer line noise.
	AfterHours int `toml:"after_hours"`
	// SpeakOnReturn speaks the whole briefing on the first answer after an
	// absence instead of offering it. Off by default: the default contract is
	// offer-not-ambush, and being read a report you did not ask for is
	// exactly the thing the default protects against. "Unprompted" here means
	// "without you asking for the briefing" — Jarvix still waits until you
	// are demonstrably back, because nothing on a clock ever prepares this.
	SpeakOnReturn bool `toml:"speak_on_return"`
}
