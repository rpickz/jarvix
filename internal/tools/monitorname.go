package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/monitors"
	"github.com/rpickz/jarvix/internal/placement"
)

// This file is the monitor-nickname surface (#180) on the window tools'
// shared state: assignment, removal, the spoken listing, and the picker's
// data. It is the mirror image of nickname.go, deliberately — one idiom for
// "call this X <name>", one collision vocabulary, one shape of refusal — and
// the differences from it are only where windows and monitors genuinely
// differ.
//
// The differences worth naming. A window is resolved by matching; a monitor
// is resolved by the placement vocabulary (ADR 0056), so the seam here is
// placement.Resolver rather than resolveWindow. And a window nickname dies
// with the daemon while a monitor nickname is persisted (ADR 0057), so this
// file has a Forget, which windows do not need — a window releases its name
// by closing, and a monitor never does.
//
// Assignment and removal are ungated, on nickname.go's exact reading: they
// change nothing on screen, the opposite act undoes them, and the user just
// said the name out loud.

// MonitorResolver is the one seam every monitor reference in this package
// goes through. Built per call rather than cached so the nickname table is
// the live one — the resolver is a value, and the value is cheap.
func (d *Desktop) MonitorResolver() placement.Resolver {
	return d.screens.Resolver()
}

// AssignMonitorNickname names a screen. connector empty means the monitor
// holding focus — what "call this monitor top" means, and the only thing the
// voice surface ever sends.
//
// spoken is the confirmation to say; err is a spoken-ready refusal that
// starts lowercase, so "Sorry, %s." frames it without rewording. Every
// surface names a screen through here: the deterministic intent, the daemon's
// monitors.name verb, and the window's form.
func (d *Desktop) AssignMonitorNickname(ctx context.Context, name, connector string) (spoken string, err error) {
	present, err := d.presentMonitors(ctx)
	if err != nil {
		return "", err
	}
	target, err := d.targetMonitor(connector, present)
	if err != nil {
		return "", err
	}
	assigned, err := d.screens.Assign(name, target.Name, present)
	if err != nil {
		d.publishRefusal("name_monitor", target.Describe(), err.Error())
		return "", err
	}
	d.log.Info("monitor named", "component", "tools",
		"connector", assigned.Connector, "nickname", assigned.Name)
	d.publish("name_monitor", target.Describe()+" → "+assigned.Name)
	return fmt.Sprintf("Okay — %s is now called %s.", target.Describe(), assigned.Name), nil
}

// RepointMonitorNickname moves an existing name to another screen — the
// cable-moved case. It is a separate verb from assignment for the reason
// monitors.Store.Repoint is: re-pointing a name changes what every routine
// mentioning it does, so it is something the user asks for on a name they can
// see, never something a misheard sentence can do.
func (d *Desktop) RepointMonitorNickname(ctx context.Context, name, connector string) (spoken string, err error) {
	present, err := d.presentMonitors(ctx)
	if err != nil {
		return "", err
	}
	target, err := d.targetMonitor(connector, present)
	if err != nil {
		return "", err
	}
	updated, previous, err := d.screens.Repoint(name, target.Name, present)
	if err != nil {
		if errors.Is(err, monitors.ErrUnknownNickname) {
			return "", fmt.Errorf("no monitor is called %q right now, so nothing was moved", name)
		}
		d.publishRefusal("name_monitor", target.Describe(), err.Error())
		return "", err
	}
	d.log.Info("monitor nickname moved", "component", "tools",
		"connector", updated.Connector, "nickname", updated.Name, "previous", previous)
	d.publish("name_monitor", target.Describe()+" → "+updated.Name)
	spoken = fmt.Sprintf("Okay — %s is now called %s.", target.Describe(), updated.Name)
	if previous != "" {
		spoken += fmt.Sprintf(" %s no longer is.", previous)
	}
	return spoken, nil
}

// ForgetMonitorNickname drops a screen name. A name nothing holds is said so
// rather than answered with "done" — the honest miss the whole feature is
// built on.
func (d *Desktop) ForgetMonitorNickname(_ context.Context, name string) (spoken string, err error) {
	gone, err := d.screens.Forget(name)
	if err != nil {
		if errors.Is(err, monitors.ErrUnknownNickname) {
			return "", fmt.Errorf("no monitor is called %q right now, so nothing was forgotten",
				strings.TrimSpace(name))
		}
		return "", err
	}
	d.log.Info("monitor nickname forgotten", "component", "tools",
		"connector", gone.Connector, "nickname", gone.Name)
	d.publish("name_monitor", gone.Connector+" → (no name)")
	return fmt.Sprintf("Okay — %s is no longer called %s.", gone.Connector, gone.Name), nil
}

// MonitorNicknameListing answers "what are my screens called" in one spoken
// sentence, judged against the outputs that are plugged in right now so a
// name whose screen is in a bag is said to be exactly that.
func (d *Desktop) MonitorNicknameListing(ctx context.Context) (string, error) {
	named := d.screens.List()
	if len(named) == 0 {
		return "No screens have names right now. Say call this monitor and then a name to give one.", nil
	}
	present, err := d.presentMonitors(ctx)
	if err != nil {
		// The names are still worth saying: they live in a file, not in the
		// compositor, and a window manager that will not answer is a reason to
		// omit the sizes rather than the answer.
		present = nil
	}
	parts := make([]string, 0, len(named))
	for _, n := range named {
		if mon, ok := findMonitor(n.Connector, present); ok {
			parts = append(parts, fmt.Sprintf("%s is %s", n.Name, mon.Describe()))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s is %s, which is not plugged in right now",
			n.Name, n.Connector))
	}
	return fmt.Sprintf("%s: %s.", plural(len(named), "screen has a name", "screens have names"),
		strings.Join(parts, "; ")), nil
}

// MonitorListing is one screen as the daemon's monitors.list verb serves it:
// what a picker has to show — the connector, the size a person recognises the
// screen by, whether it holds focus, and the name the user gave it.
type MonitorListing struct {
	Connector string `json:"connector"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Focused   bool   `json:"focused"`
	// Nickname is what the user calls this screen, "" when they have not
	// named it.
	Nickname string `json:"nickname,omitempty"`
	// Describe is the one-line rendering every surface shows, composed here
	// so the window and a spoken sentence cannot word it differently.
	Describe string `json:"describe"`
}

// NicknameListingEntry is one stored screen name as the wire carries it. It
// is listed separately from the monitors because a name whose screen is
// unplugged still exists and must still be visible — a nickname the user
// cannot see is a nickname they cannot correct.
type NicknameListingEntry struct {
	Name      string `json:"name"`
	Connector string `json:"connector"`
	// Present is whether that connector is plugged in right now. False is not
	// an error; it is the state a dock in a bag produces.
	Present bool   `json:"present"`
	Named   string `json:"named,omitempty"`
	Updated string `json:"updated,omitempty"`
}

// MonitorListings renders the present outputs and the stored names for the
// daemon's monitors.list verb — the picker's whole data source.
func (d *Desktop) MonitorListings(ctx context.Context) ([]MonitorListing, []NicknameListingEntry, error) {
	present, err := d.presentMonitors(ctx)
	if err != nil {
		return nil, nil, err
	}
	byConnector := make(map[string]string)
	stored := d.screens.List()
	for _, n := range stored {
		byConnector[strings.ToLower(n.Connector)] = n.Name
	}
	out := make([]MonitorListing, 0, len(present))
	for _, m := range present {
		out = append(out, MonitorListing{
			Connector: m.Name, Width: m.Width, Height: m.Height, Focused: m.Focused,
			Nickname: byConnector[strings.ToLower(m.Name)], Describe: m.Describe(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Connector < out[j].Connector })
	names := make([]NicknameListingEntry, 0, len(stored))
	for _, n := range stored {
		_, ok := findMonitor(n.Connector, present)
		entry := NicknameListingEntry{Name: n.Name, Connector: n.Connector, Present: ok}
		if !n.Named.IsZero() {
			entry.Named = n.Named.UTC().Format("2006-01-02T15:04:05Z")
		}
		if !n.Updated.IsZero() {
			entry.Updated = n.Updated.UTC().Format("2006-01-02T15:04:05Z")
		}
		names = append(names, entry)
	}
	return out, names, nil
}

// MonitorNicknamePath is where the names live, so every surface can tell the
// user which file to open. "" when no store is configured.
func (d *Desktop) MonitorNicknamePath() string {
	if d.screens == nil {
		return ""
	}
	return d.screens.Path()
}

// presentMonitors reads the output inventory, turning a compositor that will
// not answer into the sentence the placement seam already uses for it.
func (d *Desktop) presentMonitors(ctx context.Context) ([]placement.Monitor, error) {
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	present, err := d.comp.Monitors(callCtx)
	if err != nil {
		d.log.Warn("compositor unavailable", "component", "tools", "error", err.Error())
		return nil, fmt.Errorf("I cannot see which screens are attached")
	}
	if len(present) == 0 {
		return nil, fmt.Errorf("the window manager reports no monitors")
	}
	return present, nil
}

// targetMonitor is which screen a name is being given to: the one named, or —
// when nothing was named — the one holding focus, which is what "this
// monitor" means. It resolves through the vocabulary's own resolver, so
// "current" and a nickname work here exactly as they do in a routine step.
func (d *Desktop) targetMonitor(connector string, present []placement.Monitor) (placement.Monitor, error) {
	ref := placement.MonitorRef(strings.TrimSpace(connector))
	if ref == "" {
		ref = placement.MonitorCurrent
	}
	return d.MonitorResolver().Resolve(ref, present)
}

// findMonitor looks one connector up in an inventory, folding case because
// the user may have typed the name into the file themselves.
func findMonitor(connector string, present []placement.Monitor) (placement.Monitor, bool) {
	for _, m := range present {
		if strings.EqualFold(m.Name, connector) {
			return m, true
		}
	}
	return placement.Monitor{}, false
}
