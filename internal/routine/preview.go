package routine

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/placement"
)

// This file is the routine editor's preview (issue #181, ADR 0059): what one
// routine WOULD do, per workspace, without doing any of it.
//
// A routine is a description of a desktop, and until now the only way to find
// out whether the description was the one you meant was to say the phrase and
// watch six windows land. That feedback loop is the worst in the product —
// it happens on a live desktop, it moves windows the user was working in, and
// it takes eight seconds per step to tell you that a share was wrong. So the
// editor draws the answer instead, and this is where the answer is computed.
//
// Two things it is careful about:
//
//   - It never runs anything and never asks the compositor to move anything.
//     The only thing it reads is the monitor inventory, which is what
//     Extent.Resolve needs to turn "66%" into pixels.
//   - It refuses rather than guesses. A workspace whose target screen is not
//     plugged in, or whose values the loader already refused, comes back with
//     the reason and nothing to draw. The alternative — a plausible picture
//     drawn from half-understood values — would be worse than no picture,
//     because the user would believe it.

// PreviewBlock is one validation problem the editor already holds, handed
// back in so a workspace built from values the daemon refused is not drawn.
//
// It travels in rather than being recomputed because some refusals never
// reach a Placement at all: `mode = "grouped"` is rejected by ParseMode, and
// the conversion that feeds a runner deliberately reads a refused value as
// "not said" (config.RoutineStep.placement). Previewing the converted value
// would therefore draw a tiled window for a mode the vocabulary declined —
// exactly the layout that will not happen.
type PreviewBlock struct {
	// Step is the step's position in the routine, zero-based.
	Step int
	// Field is the step key the problem belongs to, and Message is the
	// daemon's own sentence for it.
	Field, Message string
}

// PreviewStep is one step's row in the preview: which workspace it is on and
// the sentence that says where its window goes.
//
// The sentence is the accessibility channel and is not optional decoration:
// the arrangement must be conveyed in text as well as in the diagram, and it
// is the ONLY channel that still says something when the target screen is in
// a bag.
type PreviewStep struct {
	Index     int    `json:"index"`
	Workspace int    `json:"workspace"`
	Launches  string `json:"launches"`
	Summary   string `json:"summary"`
}

// PreviewWorkspace is one workspace's drawn arrangement, or the reason there
// is none.
type PreviewWorkspace struct {
	Workspace int `json:"workspace"`
	// Heading is what to call this drawing — the workspace and the screen it
	// is on, in one phrase, composed here so the window cannot word it
	// differently from the picker beside it.
	Heading string `json:"heading"`
	// Monitor is the connector the arrangement is drawn against, "" when none
	// could be resolved.
	Monitor string `json:"monitor,omitempty"`
	// Drawable is whether there is a picture. False is a first-class answer:
	// a screen in a bag, a refused share, a mode the vocabulary declines.
	Drawable bool `json:"drawable"`
	// Unavailable is the one sentence saying why there is nothing to draw,
	// "" when there is.
	Unavailable string `json:"unavailable,omitempty"`
	// Arrangement is the geometry, present only when Drawable.
	Aspect float64           `json:"aspect,omitempty"`
	Usable placement.Rect    `json:"usable"`
	Panels []placement.Panel `json:"panels"`
	// Problems are the field-keyed reasons this arrangement cannot happen,
	// each naming the step it belongs to so the editor pins it to the control
	// the user has to change.
	Problems []placement.StepProblem `json:"problems,omitempty"`
	// Summaries are the sentences of the steps on this workspace, in order —
	// the text the diagram is accompanied by rather than replaced by.
	Summaries []string `json:"summaries"`
}

// Preview is the whole editor-side answer for one routine.
type Preview struct {
	Steps      []PreviewStep      `json:"steps"`
	Workspaces []PreviewWorkspace `json:"workspaces"`
}

// Describe builds the preview for one routine against the screens that are
// plugged in right now.
//
// inventory is what the compositor reports; invErr is why it could not be
// read, which is not fatal — the sentences are still worth having, and saying
// "I cannot see which screens are attached" beside them is a great deal more
// use than an empty panel.
//
// blocks are the problems the editor already has for this draft. Every step
// they name loses its workspace's drawing, with the daemon's own message
// carried through verbatim.
func Describe(def Definition, inventory []placement.Monitor, invErr error,
	resolver placement.Resolver, blocks []PreviewBlock) Preview {
	preview := Preview{Steps: make([]PreviewStep, 0, len(def.Steps))}
	for i, step := range def.Steps {
		preview.Steps = append(preview.Steps, PreviewStep{
			Index: i, Workspace: step.Workspace, Launches: step.Launches(),
			Summary: step.Sentence(stepLabel(i, step)),
		})
	}
	for _, group := range groupByWorkspace(def) {
		preview.Workspaces = append(preview.Workspaces,
			describeWorkspace(def, group, inventory, invErr, resolver, blocks))
	}
	return preview
}

// workspaceGroup is one workspace and the steps that open on it, in the order
// the routine opens them. Order is kept because insertion order is what
// decides the tiling structure — reorder two steps and the layout is a
// different layout, which is precisely why the editor draws this.
type workspaceGroup struct {
	workspace int
	steps     []int
}

// groupByWorkspace collects the steps per workspace, workspaces in the order
// the routine first mentions them.
func groupByWorkspace(def Definition) []workspaceGroup {
	var groups []workspaceGroup
	at := make(map[int]int, len(def.Steps))
	for i, step := range def.Steps {
		pos, seen := at[step.Workspace]
		if !seen {
			at[step.Workspace] = len(groups)
			groups = append(groups, workspaceGroup{workspace: step.Workspace, steps: []int{i}})
			continue
		}
		groups[pos].steps = append(groups[pos].steps, i)
	}
	return groups
}

// describeWorkspace draws one workspace, or says why it cannot be drawn.
func describeWorkspace(def Definition, group workspaceGroup, inventory []placement.Monitor,
	invErr error, resolver placement.Resolver, blocks []PreviewBlock) PreviewWorkspace {
	out := PreviewWorkspace{
		Workspace: group.workspace,
		Panels:    []placement.Panel{},
		Summaries: make([]string, 0, len(group.steps)),
	}
	for _, i := range group.steps {
		out.Summaries = append(out.Summaries, def.Steps[i].Sentence(stepLabel(i, def.Steps[i])))
	}
	// A refused value first: there is no point resolving a screen for an
	// arrangement whose numbers the loader has already turned down, and
	// showing the daemon's sentence is more use than showing a picture drawn
	// around it.
	if refused := blocksFor(group, blocks); len(refused) > 0 {
		out.Problems = refused
		out.Heading = headingFor(group.workspace, "")
		out.Unavailable = "there is nothing to draw until this is fixed"
		return out
	}
	monitor, err := targetMonitor(def, group, inventory, invErr, resolver)
	if err != nil {
		out.Heading = headingFor(group.workspace, "")
		out.Unavailable = err.Error()
		return out
	}
	out.Monitor = monitor.Name
	out.Heading = headingFor(group.workspace, monitor.Describe())
	windows := make([]placement.Arranged, 0, len(group.steps))
	for _, i := range group.steps {
		windows = append(windows, placement.Arranged{
			Step: i, Label: stepLabel(i, def.Steps[i]), Placement: def.Steps[i].Placement,
		})
	}
	arrangement := placement.Arrange(monitor, windows)
	out.Problems = arrangement.Problems
	if !arrangement.Drawable() {
		out.Unavailable = "this arrangement cannot happen, so it is not drawn"
		return out
	}
	out.Drawable = true
	out.Aspect = arrangement.Aspect
	out.Usable = arrangement.Usable
	out.Panels = arrangement.Panels
	return out
}

// targetMonitor is the screen a workspace's percentages resolve against: the
// one the first step naming a screen asks for, exactly as the runner decides
// it (a workspace is moved once, however many steps name it), and otherwise
// whichever screen the workspace is on right now.
func targetMonitor(def Definition, group workspaceGroup, inventory []placement.Monitor,
	invErr error, resolver placement.Resolver) (placement.Monitor, error) {
	if invErr != nil {
		return placement.Monitor{}, fmt.Errorf(
			"I cannot see which screens are attached, so there is nothing to draw against: %w", invErr)
	}
	if len(inventory) == 0 {
		return placement.Monitor{}, fmt.Errorf("the window manager reports no monitors")
	}
	for _, i := range group.steps {
		ref := def.Steps[i].Monitor
		if ref == "" {
			continue
		}
		return resolver.Resolve(ref, inventory)
	}
	return placement.ForWorkspace(group.workspace, inventory), nil
}

// blocksFor collects the refusals belonging to one workspace's steps.
func blocksFor(group workspaceGroup, blocks []PreviewBlock) []placement.StepProblem {
	var out []placement.StepProblem
	for _, i := range group.steps {
		for _, b := range blocks {
			if b.Step == i {
				out = append(out, placement.StepProblem{
					Step: b.Step, Field: b.Field, Message: b.Message})
			}
		}
	}
	return out
}

// headingFor names one drawing: the workspace and the screen it is on.
func headingFor(workspace int, screen string) string {
	where := fmt.Sprintf("Workspace %d", workspace)
	if workspace <= 0 {
		// A step must name a workspace and the loader says so; the drawing
		// still has to be labelled something while the user is fixing it.
		where = "No workspace named"
	}
	if strings.TrimSpace(screen) == "" {
		return where
	}
	return where + " on " + screen
}

// stepLabel is what to call one step in a sentence: what it launches, or its
// position while it is still empty — a half-written step is the normal state
// of a form and its sentence must still read.
func stepLabel(index int, step Step) string {
	if launches := strings.TrimSpace(step.Launches()); launches != "" {
		return launches
	}
	return fmt.Sprintf("Step %d", index+1)
}
