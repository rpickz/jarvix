package daemon

import (
	"encoding/json"

	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/routine"
)

// registerPlacementMethods adds `placement.vocabulary` (issue #181): every
// closed set the routine editor's controls offer, with the words to offer
// them in.
//
// It exists so the window can render a dropdown without knowing what is in
// it. The vocabulary is declared once (ADR 0056) and this verb serves it: a
// mode added there appears in the editor on the next reply, and a mode
// REMOVED disappears from it, which is the property a hard-coded QML list
// cannot have. The same argument monitors.list already makes for the reserved
// words, applied to the rest of the vocabulary.
//
// Each list is served as ready-made options — a value to write and a label to
// show — rather than as raw names, because composing "tiled — Tiled into the
// workspace's layout…" in QML would be composing a sentence about placement
// in the window, which is the thing ADR 0013 is for. The "not said" option is
// served too, with its own words, for the same reason: a step that names no
// mode is a legitimate answer and the label for it is a fact about the
// vocabulary, not a UI flourish.
func (d *Daemon) registerPlacementMethods() {
	d.server.Handle("placement.vocabulary", func(json.RawMessage) (any, error) {
		return map[string]any{
			"modes":       modeOptions(),
			"place_next":  placeNextOptions(),
			"focus":       focusOptions(),
			"launch":      launchOptions(),
			"unsupported": unsupportedOptions(),
			"workspace": map[string]any{
				"min": placement.MinWorkspace, "max": placement.MaxWorkspace,
			},
		}, nil
	})
}

// placementOption is one choice a picker offers: the value written into
// config.toml, and the words the control shows for it.
type placementOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// modeOptions lists how a window may sit, each with the vocabulary's own
// one-sentence summary — the text ModeSpec.Summary was written for.
func modeOptions() []placementOption {
	out := []placementOption{{Value: "",
		Label: "leave it to the layout — the compositor decides"}}
	for _, spec := range placement.Modes() {
		out = append(out, placementOption{
			Value: string(spec.Name), Label: string(spec.Name) + " — " + spec.Summary})
	}
	return out
}

// placeNextOptions lists where the window AFTER this one goes. The empty
// option leads because most steps do not arrange anything, and its label says
// what "nothing" means rather than leaving it blank.
func placeNextOptions() []placementOption {
	out := []placementOption{{Value: "",
		Label: "wherever the layout puts it"}}
	for _, value := range placement.PlaceNextValues() {
		out = append(out, placementOption{Value: value, Label: "to the " + value + " of this one"})
	}
	return out
}

// focusOptions lists whether the view follows the placed window.
func focusOptions() []placementOption {
	return []placementOption{
		{Value: "", Label: "stay where I am (the default)"},
		{Value: string(placement.FocusFollow), Label: "take me to this window"},
	}
}

// launchOptions lists what happens when a matching window is already open.
//
// The default is served as the EMPTY value rather than as its own name, and
// that is deliberate rather than lazy: writing `launch = "if_missing"` into
// every step the window touched would add a line to routines that never asked
// for one, and config.RoutineFromDefinition already refuses to do that for
// the same reason. The words are the policy's, the spelling is absence.
func launchOptions() []placementOption {
	return []placementOption{
		{Value: "", Label: "re-use a matching window when one is open (the default)"},
		{Value: string(routine.LaunchAlways), Label: "always start a new window"},
	}
}

// unsupportedOptions lists the window states the compositor offers and this
// vocabulary declines, each with the reason it was declined.
//
// Served rather than hidden because the reason is owed to whoever asks for
// one: an option that is simply absent from a dropdown reads as an oversight,
// and "why can't I tab these windows?" should have its answer in the product.
func unsupportedOptions() []map[string]string {
	out := make([]map[string]string, 0, len(placement.UnsupportedModes()))
	for _, u := range placement.UnsupportedModes() {
		out = append(out, map[string]string{"name": u.Name, "reason": u.Reason})
	}
	return out
}
