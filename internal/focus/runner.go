package focus

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rpickz/jarvix/internal/intent"
)

// IntentRunner is the focus service dressed as the session engine's intent
// runner: it satisfies intent.Runner by delegating the argv and shell halves
// to the real runner untouched, and adds RunFocus — the seam the engine's
// focus dispatch asserts for (session.FocusRunner). Injection rides the
// existing Options.IntentRunner field, so wiring focus threads costs the
// engine no new option and the daemon one assignment.
type IntentRunner struct {
	// Service owns every focus action.
	Service *Service
	// Fallback executes ordinary intents; nil uses the real ExecRunner,
	// exactly as the engine would have with no runner injected at all.
	Fallback intent.Runner
	// Log is handed to a constructed fallback.
	Log *slog.Logger
}

// Run executes a built-in intent's fixed argv — not a focus concern.
func (r *IntentRunner) Run(ctx context.Context, argv []string) error {
	return r.fallback().Run(ctx, argv)
}

// RunShell executes a user-defined intent's command — not a focus concern.
func (r *IntentRunner) RunShell(ctx context.Context, command string) error {
	return r.fallback().RunShell(ctx, command)
}

func (r *IntentRunner) fallback() intent.Runner {
	if r.Fallback != nil {
		return r.Fallback
	}
	return &intent.ExecRunner{Log: r.Log}
}

// RunFocus carries out one matched focus phrase and returns the sentence the
// engine speaks as its acknowledgement. The error path is the ordinary
// intent-failure ack ("Sorry, …"), so a refusal — no such thread, nothing
// active, an ambiguous name — is one honest spoken sentence, never a stuck
// session.
func (r *IntentRunner) RunFocus(ctx context.Context, m intent.Match) (string, error) {
	svc := r.Service
	if svc == nil {
		return "", fmt.Errorf("focus threads are not available on this daemon")
	}
	switch m.Focus {
	case intent.FocusNew:
		_, ack, err := svc.Create(ctx, m.FocusText, m.FocusWindows)
		return ack, err
	case intent.FocusAnchor:
		return svc.Anchor(ctx, m.FocusWindows)
	case intent.FocusSwitch:
		_, ack, err := svc.Switch(ctx, m.FocusText)
		return ack, err
	case intent.FocusPark:
		return svc.Park(m.FocusText)
	case intent.FocusParked:
		return svc.ParkedSpoken()
	case intent.FocusStatus:
		return svc.Status(), nil
	case intent.FocusCheck:
		return svc.Check(ctx, m.FocusText)
	case intent.FocusEnd:
		return svc.End(m.FocusText)
	case intent.FocusTimebox:
		return svc.StartSession(ctx, m.FocusText, m.Slot)
	case intent.FocusTimeboxEnd:
		return svc.EndSession()
	case intent.FocusTick:
		return svc.Tick()
	case intent.FocusBreak:
		return svc.Break()
	case intent.FocusContinue:
		return svc.Continue()
	case intent.FocusRemind:
		return svc.Remind(m.Slot)
	case intent.FocusRemindStop:
		return svc.RemindStop()
	default:
		// Unreachable for a compiled table; a new action added without a
		// case here must be a spoken failure, never a silent success.
		return "", fmt.Errorf("I do not know that focus action")
	}
}
