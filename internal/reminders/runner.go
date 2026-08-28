package reminders

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rpickz/jarvix/internal/intent"
)

// IntentRunner is the reminder service dressed as the session engine's
// intent runner (the focus.IntentRunner pattern): it satisfies intent.Runner
// by delegating the argv and shell halves to the chain untouched, forwards
// RunFocus to the focus runner it wraps, and adds RunReminder — the seam the
// engine's reminder dispatch asserts for (session.ReminderRunner). Injection
// rides the existing Options.IntentRunner field, so wiring reminders costs
// the engine no new option and the daemon one assignment.
type IntentRunner struct {
	// Service owns every reminder action.
	Service *Service
	// Fallback executes everything else — in the daemon, the focus runner,
	// which in turn falls back to the real ExecRunner. Nil uses an
	// ExecRunner directly, exactly as the engine would with no runner
	// injected at all.
	Fallback intent.Runner
	// Log is handed to a constructed fallback.
	Log *slog.Logger
}

// Run executes a built-in intent's fixed argv — not a reminder concern.
func (r *IntentRunner) Run(ctx context.Context, argv []string) error {
	return r.fallback().Run(ctx, argv)
}

// RunShell executes a user-defined intent's command — not a reminder concern.
func (r *IntentRunner) RunShell(ctx context.Context, command string) error {
	return r.fallback().RunShell(ctx, command)
}

// RunFocus forwards a focus action to the wrapped focus runner, so this
// wrapper is transparent to the engine's focus dispatch (which asserts for
// the method on the one injected runner).
func (r *IntentRunner) RunFocus(ctx context.Context, m intent.Match) (string, error) {
	if fr, ok := r.Fallback.(interface {
		RunFocus(ctx context.Context, m intent.Match) (string, error)
	}); ok {
		return fr.RunFocus(ctx, m)
	}
	return "", fmt.Errorf("focus threads are not available on this daemon")
}

func (r *IntentRunner) fallback() intent.Runner {
	if r.Fallback != nil {
		return r.Fallback
	}
	return &intent.ExecRunner{Log: r.Log}
}

// RunReminder carries out one matched reminder phrase and returns the
// sentence the engine speaks as its acknowledgement. The error path is the
// ordinary intent-failure ack ("Sorry, …"), so a refusal — an unparseable
// time, an unknown or ambiguous reminder — is one honest spoken sentence,
// never a stuck session.
func (r *IntentRunner) RunReminder(_ context.Context, m intent.Match) (string, error) {
	svc := r.Service
	if svc == nil {
		return "", fmt.Errorf("reminders are not available on this daemon")
	}
	switch m.Reminder {
	case intent.ReminderSet:
		return svc.Create(m.ReminderWhen, m.ReminderText)
	case intent.ReminderList:
		return svc.ListSpoken(), nil
	case intent.ReminderHistory:
		return svc.HistorySpoken(), nil
	case intent.ReminderDue:
		spoken, _ := svc.ClaimDue()
		return spoken, nil
	case intent.ReminderCancel:
		return svc.Cancel(m.ReminderText)
	default:
		// Unreachable for a compiled table; a new action added without a
		// case here must be a spoken failure, never a silent success.
		return "", fmt.Errorf("I do not know that reminder action")
	}
}
