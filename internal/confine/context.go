package confine

import "context"

// The seam between the code that knows the boundary and the code that runs the
// command.
//
// It rides a context for the same reason internal/undo's recorder and job id
// do: the shell tool is one of thirty in a registry that dispatches them all
// the same way, and threading a boundary through Registry.Execute would make
// every tool's signature carry a parameter one tool uses. A job runner installs
// the spec once, on the context it dispatches through.
//
// What makes that safe rather than a fail-open hole is the pairing on the other
// end. internal/tools' shell tool does not ask "is there a boundary?" and run
// unconfined when the answer is no — it asks whether the call belongs to a JOB
// (undo.JobFrom, on the same context) and refuses outright when a job's command
// arrives without one. So the failure mode of forgetting to install a spec is a
// command that does not run, not a command that runs unconfined. See
// TestAJobsCommandWithNoBoundaryIsRefused, which is that rule's test.

type specKey struct{}

// With installs the boundary a command must be run inside.
//
// A spec with no roots installs nothing. That is not leniency: Spec.Check
// refuses an empty spec anyway, and a context carrying one would make the
// shell tool's "is this call confined?" question answer yes for a boundary that
// could never be built.
func With(ctx context.Context, s Spec) context.Context {
	if len(s.Roots) == 0 {
		return ctx
	}
	return context.WithValue(ctx, specKey{}, s)
}

// From reads the boundary installed on ctx, and reports whether there was one.
func From(ctx context.Context) (Spec, bool) {
	if ctx == nil {
		return Spec{}, false
	}
	s, ok := ctx.Value(specKey{}).(Spec)
	return s, ok
}
