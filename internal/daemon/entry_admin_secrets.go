package daemon

// This file is the credential half of the entry surface (issue #163). It is
// small on purpose, because the rule it implements is absolute and an
// absolute rule cannot be spread out:
//
//	A stored credential is never returned over the wire, never rendered,
//	never included in a validation problem or an error string, never in an
//	activity row, and never length-leaked by a mask.
//
// Three mechanisms enforce it, and each is the ONLY way its direction of
// travel is possible:
//
//	Reading  — stripEntrySecrets removes the declared secret keys from every
//	           entry that leaves the daemon, and entrySecretStates replaces
//	           them with facts ABOUT the credential: is one available, does
//	           it come from the environment or the file, which variable is
//	           expected, does that variable currently resolve. Presence and a
//	           variable NAME are configuration; the value is not, and nothing
//	           here reads it. No mask is offered, because a mask whose length
//	           is the value's length is the value's length.
//	Writing  — entrySecretWrite is a separate parameter carrying an
//	           instruction, not a field of the entry: keep what is stored, set
//	           a new value, or clear it. A draft that carries the secret key
//	           itself is refused by the family's key whitelist
//	           (sanitizeEntry), so the value has exactly one route in.
//	Escaping — every message that can carry a value out (a validator's
//	           problem, a rewriter's error, an engine's refusal reason) passes
//	           through the scrubber built here, which knows the values that
//	           were in play for this one call and replaces them. It is
//	           defence in depth: no validator quotes a credential today, and
//	           the leak-salted tests exist so nobody has to trust that.
//
// The mechanism is generic — a family with no `secrets` row pays nothing —
// and it is the endpoint family's api_key that needs it (config.Endpoint).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// entrySecretWrite is the write-only channel for one credential: an
// instruction about the stored value, never a copy of it. The zero value
// ("") means keep, so a form that does not touch the credential — and a
// caller that has never heard of credentials — cannot delete one by omission.
type entrySecretWrite struct {
	// Action is "keep" (the default), "set", or "clear".
	Action string `json:"action"`
	// Value is the new credential, read only for "set". It travels INBOUND
	// only: nothing echoes it, and the write path's one use of it is the
	// file.
	Value string `json:"value"`
}

// Credential write actions.
const (
	entrySecretKeep  = "keep"
	entrySecretSet   = "set"
	entrySecretClear = "clear"
)

// entrySecretRedacted is what a scrubbed value becomes. A fixed string, so
// the replacement never carries the length of what it replaced.
const entrySecretRedacted = "[redacted]"

// entrySecretStates describes each of the family's credentials from outside:
// what a form may know about a key it must never see. The shape reuses the
// settings screen's existing vocabulary (config.get's `secrets` array — env,
// env_set, inline_key) so the window has one dialect for credentials, plus
// the summary a form needs to word "a key is set" without asking where from.
func entrySecretStates(spec entryFamilySpec, entry map[string]any) map[string]any {
	if len(spec.secrets) == 0 {
		return nil
	}
	out := make(map[string]any, len(spec.secrets))
	for _, s := range spec.secrets {
		stored, _ := entry[s.key].(string)
		envName, _ := entry[s.envKey].(string)
		envName = strings.TrimSpace(envName)
		// os.Getenv, never its result: whether the variable resolves is the
		// answer, and the answer is a boolean.
		envSet := envName != "" && os.Getenv(envName) != ""
		source := "none"
		switch {
		case envSet:
			// The environment wins when it resolves, exactly as
			// config.Endpoint.Key decides it, so the form never claims a
			// source the runtime would not use.
			source = "env"
		case strings.TrimSpace(stored) != "":
			source = "config"
		}
		out[s.key] = map[string]any{
			"label":      s.label,
			"env_key":    s.envKey,
			"env":        envName,
			"env_set":    envSet,
			"inline_key": strings.TrimSpace(stored) != "",
			"set":        source != "none",
			"source":     source,
		}
	}
	return out
}

// stripEntrySecrets returns entry without its credential keys — the map that
// may go on the wire. It copies rather than deletes in place because the
// caller's map is also what the write path reads the stored value from.
func stripEntrySecrets(spec entryFamilySpec, entry map[string]any) map[string]any {
	if len(spec.secrets) == 0 {
		return entry
	}
	out := make(map[string]any, len(entry))
	for k, v := range entry {
		if spec.secretFor(k) == nil {
			out[k] = v
		}
	}
	return out
}

// applyEntrySecrets folds the credential instructions into the draft and
// returns the scrubber for this call.
//
// "keep" is the default and the interesting case: the form never received the
// stored value, so a save would drop it — the classic credential bug — unless
// the daemon puts it back. It reads it from the file and writes it to the
// file, and it exists nowhere else in between.
func (d *Daemon) applyEntrySecrets(spec entryFamilySpec, raw []byte, name string,
	draft map[string]any, writes map[string]entrySecretWrite) (func(string) string, []entryProblem) {
	if len(spec.secrets) == 0 {
		return func(s string) string { return s }, nil
	}
	var inPlay []string
	var problems []entryProblem
	existing := map[string]any{}
	if strings.TrimSpace(name) != "" {
		if entry, ok, err := entryReadValue(spec, raw, name); err == nil && ok {
			existing = entry
		}
	}
	for _, s := range spec.secrets {
		stored, _ := existing[s.key].(string)
		if stored != "" {
			// In play whether or not it is kept: a cleared or replaced key is
			// still the value a failure path could quote from the document it
			// was read out of.
			inPlay = append(inPlay, stored)
		}
		write := writes[s.key]
		switch strings.TrimSpace(write.Action) {
		case entrySecretSet:
			value := strings.TrimSpace(write.Value)
			if value != "" {
				inPlay = append(inPlay, value)
			}
			if value == "" {
				problems = append(problems, entryProblem{Field: s.key, Message: fmt.Sprintf(
					"the new %s is empty; clear the stored one instead if that is what you meant",
					s.label)})
				continue
			}
			draft[s.key] = value
		case entrySecretClear:
			// Absent from the draft is absent from the rendered table: the
			// key is removed, not blanked, so the file does not keep an empty
			// credential that reads as "configured".
			delete(draft, s.key)
		case "", entrySecretKeep:
			if stored != "" {
				draft[s.key] = stored
			}
		default:
			problems = append(problems, entryProblem{Field: s.key, Message: fmt.Sprintf(
				"%q is not something to do with a credential; use %q, %q or %q",
				write.Action, entrySecretKeep, entrySecretSet, entrySecretClear)})
		}
	}
	return entrySecretScrubber(inPlay), problems
}

// entrySecretScrubber returns a function replacing every value in play with a
// fixed marker. Longest first, so a value that contains another is not left
// half-revealed by the shorter one's replacement.
func entrySecretScrubber(values []string) func(string) string {
	filtered := make([]string, 0, len(values))
	for _, v := range values {
		// A very short value would match everywhere and scrub the message
		// into noise; a credential is never that short, and a message that
		// happened to contain "a" is not a leak.
		if len(v) >= 4 {
			filtered = append(filtered, v)
		}
	}
	if len(filtered) == 0 {
		return func(s string) string { return s }
	}
	for i := range filtered {
		for j := i + 1; j < len(filtered); j++ {
			if len(filtered[j]) > len(filtered[i]) {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
		}
	}
	return func(s string) string {
		for _, v := range filtered {
			s = strings.ReplaceAll(s, v, entrySecretRedacted)
		}
		return s
	}
}

// scrubProblems runs every problem message through the scrubber. Fields are
// key names and never carry values, so only the message is rewritten.
func scrubProblems(scrub func(string) string, problems []entryProblem) []entryProblem {
	if scrub == nil {
		return problems
	}
	for i := range problems {
		problems[i].Message = scrub(problems[i].Message)
	}
	return problems
}

// entryAdminTest serves config.test_entry: the family's live probe. It is a
// read of the FILE, never of a draft — a Test proves what is saved, so the
// answer cannot be "it works" about a configuration that is not there.
//
// The family decides what a probe is (spec.probe); a family without one is
// refused rather than answered, because "no test available" is honest and a
// fabricated success is the one outcome the acceptance criteria forbid.
func (d *Daemon) entryAdminTest(params json.RawMessage) (any, error) {
	var p struct {
		Family string `json:"family"`
		Name   string `json:"name"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "config.test_entry params: %v", err)
		}
	}
	spec, ipcErr := entryFamily(p.Family)
	if ipcErr != nil {
		return nil, ipcErr
	}
	if spec.probe == nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "there is nothing to test on a %s; %s entries have no live endpoint",
			spec.kind, spec.family)
	}
	raw, err := os.ReadFile(d.paths.ConfigFile())
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	entry, ok, err := entryReadValue(spec, raw, p.Name)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
	}
	if !ok {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "no %s entry is named %q", spec.kind, p.Name)
	}
	cfg, err := config.ParseBytes(raw)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
	}
	result := spec.probe(context.Background(), cfg, p.Name)
	result["family"] = spec.family
	result["name"] = p.Name
	// The same presence facts the form already shows, refreshed alongside the
	// probe: an unauthorised answer is read completely differently depending
	// on whether a key was sent at all, and the user should not have to
	// remember which.
	if states := entrySecretStates(spec, entry); len(states) > 0 {
		result["secrets"] = states
	}
	return result, nil
}
