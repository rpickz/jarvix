package ai

import "testing"

// The routing table is the whole of the decision, so it is tested as a table:
// every combination of default, pin, per-turn ask and tool attachment that the
// engine can hand it, with the answer written out rather than computed.

// allTiers is the fully-configured world: every tier has a binding.
func allTiers() map[Tier]bool {
	return map[Tier]bool{TierInstant: true, TierMedium: true, TierDeep: true}
}

// mediumOnly is the world of a config with an [ai.tiers] table that names no
// instant and no deep — the only tier that always exists is medium, because an
// absent medium is the [ai] brain.
func mediumOnly() map[Tier]bool {
	return map[Tier]bool{TierMedium: true}
}

func TestRoutingTable(t *testing.T) {
	cases := map[string]struct {
		in         RouteInput
		wantTier   Tier
		wantReason RouteReason
		wantWanted Tier
	}{
		"nothing asked lands on medium": {
			in:       RouteInput{Available: allTiers()},
			wantTier: TierMedium, wantReason: ReasonDefault,
		},
		"the configured default is honoured": {
			in:       RouteInput{Available: allTiers(), Default: TierInstant},
			wantTier: TierInstant, wantReason: ReasonDefault,
		},
		"an unknown default is medium": {
			in:       RouteInput{Available: allTiers(), Default: Tier("enormous")},
			wantTier: TierMedium, wantReason: ReasonDefault,
		},
		"a pin outranks the default": {
			in:       RouteInput{Available: allTiers(), Default: TierMedium, Pinned: TierDeep},
			wantTier: TierDeep, wantReason: ReasonPinned,
		},
		"a per-turn ask outranks the pin": {
			in:       RouteInput{Available: allTiers(), Pinned: TierInstant, Asked: TierDeep},
			wantTier: TierDeep, wantReason: ReasonAsked,
		},
		"an unavailable deep names what it could not reach": {
			in:       RouteInput{Available: mediumOnly(), Asked: TierDeep},
			wantTier: TierMedium, wantReason: ReasonUnavailable, wantWanted: TierDeep,
		},
		"an unavailable pin names it too": {
			in:       RouteInput{Available: mediumOnly(), Pinned: TierInstant},
			wantTier: TierMedium, wantReason: ReasonUnavailable, wantWanted: TierInstant,
		},
		"an unavailable default is not a refusal anybody asked for": {
			// Nothing was asked for, so nothing was refused: a default that
			// cannot be served is a configuration problem the doctor reports,
			// not a sentence to interrupt an answer with.
			in:       RouteInput{Available: mediumOnly(), Default: TierDeep},
			wantTier: TierMedium, wantReason: ReasonDefault,
		},
		"tools refuse instant even as the default": {
			in: RouteInput{Available: allTiers(), Default: TierInstant,
				ToolsAttached: true},
			wantTier: TierMedium, wantReason: ReasonToolsRefuseInstant, wantWanted: TierInstant,
		},
		"tools refuse instant even when pinned": {
			in: RouteInput{Available: allTiers(), Pinned: TierInstant,
				ToolsAttached: true},
			wantTier: TierMedium, wantReason: ReasonToolsRefuseInstant, wantWanted: TierInstant,
		},
		"tools refuse instant even when asked for outright": {
			in: RouteInput{Available: allTiers(), Asked: TierInstant,
				ToolsAttached: true},
			wantTier: TierMedium, wantReason: ReasonToolsRefuseInstant, wantWanted: TierInstant,
		},
		"tools leave deep alone": {
			in: RouteInput{Available: allTiers(), Asked: TierDeep,
				ToolsAttached: true},
			wantTier: TierDeep, wantReason: ReasonAsked,
		},
		"instant serves a turn with no tools": {
			in:       RouteInput{Available: allTiers(), Pinned: TierInstant},
			wantTier: TierInstant, wantReason: ReasonPinned,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Decide(tc.in)
			if got.Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", got.Tier, tc.wantTier)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Wanted != tc.wantWanted {
				t.Errorf("wanted = %q, want %q", got.Wanted, tc.wantWanted)
			}
		})
	}
}

// The tool rule is the one this feature cannot get wrong, so it gets its own
// exhaustive proof rather than riding on the table above: no combination of
// inputs whatsoever may route a tool-carrying turn to the instant tier.
//
// #71 is why. A model too small for what it was holding narrated actions it
// had never performed; a small model with tools in its hands is that incident
// with the safety catch off.
func TestNoToolCarryingTurnIsEverServedByInstant(t *testing.T) {
	worlds := []map[Tier]bool{allTiers(), mediumOnly(), {TierInstant: true, TierMedium: true}}
	tiers := []Tier{TierNone, TierInstant, TierMedium, TierDeep, Tier("nonsense")}
	for _, available := range worlds {
		for _, def := range tiers {
			for _, pinned := range tiers {
				for _, asked := range tiers {
					in := RouteInput{
						Available: available, Default: def, Pinned: pinned,
						Asked: asked, ToolsAttached: true,
					}
					if got := Decide(in); got.Tier == TierInstant {
						t.Fatalf("Decide(%+v) routed a tool-carrying turn to instant", in)
					}
				}
			}
		}
	}
}

// And the mirror image, so the rule is a rule about tools rather than a rule
// that quietly disabled the instant tier: with no tools attached, the same
// inputs reach it.
func TestInstantIsReachableWithoutTools(t *testing.T) {
	in := RouteInput{Available: allTiers(), Pinned: TierInstant}
	if got := Decide(in); got.Tier != TierInstant {
		t.Fatalf("tier = %q, want instant — the tool rule must not disable the tier", got.Tier)
	}
}

func TestParseTier(t *testing.T) {
	for _, tier := range TierOrder() {
		got, ok := ParseTier(string(tier))
		if !ok || got != tier {
			t.Errorf("ParseTier(%q) = %q, %v", tier, got, ok)
		}
	}
	for _, bad := range []string{"", "Instant", "fast", "deeper", "medium "} {
		if got, ok := ParseTier(bad); ok {
			t.Errorf("ParseTier(%q) = %q, true — unknown text must not become a tier", bad, got)
		}
	}
}

// The zero value must be answerable: an engine that has not been given tiers
// still calls nothing here, but a bug that did must not panic mid-turn.
func TestDecideIsTotal(t *testing.T) {
	if got := Decide(RouteInput{}); got.Tier != TierMedium {
		t.Errorf("Decide(zero).Tier = %q, want medium", got.Tier)
	}
}
