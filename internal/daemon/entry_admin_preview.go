package daemon

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/routine"
)

// This file is the routine editor's preview, on the daemon side (issue #181,
// ADR 0059): the entry-admin registry's `preview` hook, and the one family
// that declares it.
//
// It is a registry field rather than a branch in the flow, for the reason
// `notes`, `pending`, `probe` and `guardDelete` are (ADR 0033): the validate
// pipeline is one piece of code that must not learn which family it is
// serving. A family that wants a picture beside its form declares how to
// compute one; every other family declares nothing and the reply is exactly
// what it was.
//
// The draft is previewed as the DOCUMENT THE SAVE WOULD WRITE, not as the map
// the wire carried. That is the whole reason there is no second conversion
// here: config.ParseBytes and Config.RoutineDefinitions are the same two steps
// the daemon takes when it loads the file for real, so the preview is drawn
// from the values a run would use rather than from a parallel reading of the
// same keys — which is how the two would eventually disagree about what "66%"
// means.

// routinePreview builds the editor's diagram for one routine draft.
//
// doc is the rewritten document (nil when the draft never got that far, in
// which case there is nothing to preview and the form shows its problems
// instead), name is the draft's own name, and problems are what validation
// has already found — handed in so a workspace whose values were refused is
// not drawn around them.
func routinePreview(d *Daemon, doc []byte, name string, problems []entryProblem) any {
	if len(doc) == 0 {
		return nil
	}
	cfg, err := config.ParseBytes(doc)
	if err != nil {
		// Unparsable is not a preview state: the pipeline's own problems say
		// so in words, and a half-read document is exactly the input a
		// drawing must not be made from.
		return nil
	}
	def, ok := routineNamed(cfg, name)
	if !ok {
		return nil
	}
	inventory, invErr := d.monitorInventory()
	preview := routine.Describe(def, inventory, invErr, d.monitorResolver(),
		previewBlocks(problems))
	return preview
}

// routineNamed finds the draft among the document's routines, matched the way
// every array family addresses an entry: by name, case-insensitively.
func routineNamed(cfg config.Config, name string) (routine.Definition, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, def := range cfg.RoutineDefinitions() {
		if strings.ToLower(strings.TrimSpace(def.Name)) == want {
			return def, true
		}
	}
	return routine.Definition{}, false
}

// monitorInventory reads the screens the preview resolves percentages
// against, turning a daemon with the window tools switched off into the same
// "I cannot see which screens are attached" the compositor's own absence
// produces — because from the editor's point of view they are the same fact.
func (d *Daemon) monitorInventory() ([]placement.Monitor, error) {
	if d.windows == nil {
		return nil, errNoWindowTools
	}
	return d.windows.MonitorInventory(context.Background())
}

// monitorResolver is the nickname-aware resolver the RUN would use, so a
// routine naming "top" previews against the screen it will actually open on
// (#180).
//
// A daemon without the window tools has no store to consult, so the table is
// nil — said explicitly rather than left off, because a bare Resolver is the
// mistake that makes nicknames silently stop working and the vocabulary's own
// contract test refuses one. Nothing reaches this resolver on such a daemon
// anyway: without an inventory the preview never gets as far as resolving a
// screen.
func (d *Daemon) monitorResolver() placement.Resolver {
	if d.windows == nil {
		return placement.Resolver{Nicknames: nil}
	}
	return d.windows.MonitorResolver()
}

// errNoWindowTools is why there are no screens to draw against on a daemon
// with tools.desktop switched off. A sentence rather than an empty inventory,
// because "no monitors" and "I am not allowed to look" are different facts
// and the editor should say which one it is.
var errNoWindowTools = &previewUnavailable{
	"the window tools are switched off on this daemon (tools.desktop)"}

// previewUnavailable is a reason with no cause underneath it.
type previewUnavailable struct{ reason string }

func (e *previewUnavailable) Error() string { return e.reason }

// previewStepField matches a field key the form keyed to one step —
// "steps[2].width", or the bare "steps[2]" a whole-step problem carries.
var previewStepField = regexp.MustCompile(`^steps\[(\d+)\](?:\.([A-Za-z_]+))?`)

// previewBlocks turns the validation problems into the preview's refusals:
// the ones that make an ARRANGEMENT impossible.
//
// The filter is the placement half of a step's keys, plus the whole-step
// problems (a dangling `place_next`, two steps that cannot be told apart) —
// because those are the messages that say the layout is wrong. A step whose
// `app` is empty still has a knowable placement, and blanking its workspace's
// diagram over a half-typed program name would take the picture away exactly
// while the user is building the routine.
func previewBlocks(problems []entryProblem) []routine.PreviewBlock {
	placementKeys := make(map[string]bool, len(placement.Fields()))
	for _, field := range placement.Fields() {
		placementKeys[field] = true
	}
	var blocks []routine.PreviewBlock
	for _, p := range problems {
		m := previewStepField.FindStringSubmatch(p.Field)
		if m == nil {
			continue
		}
		if m[2] != "" && !placementKeys[m[2]] {
			continue
		}
		step, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		blocks = append(blocks, routine.PreviewBlock{
			Step: step, Field: p.Field, Message: p.Message})
	}
	return blocks
}
