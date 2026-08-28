package daemon

import (
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// The form dialog's daemon surface (issue #99) over a fully wired daemon:
// config.get_entry / validate_entry / upsert_entry / delete_entry against the
// running config file, with every decision pinned on the socket because the
// window's form renders fields and ships drafts, deciding nothing (ADR 0013).
// The mutation checks matter most: every refused call — bad shape, collision,
// stale fingerprint — must leave the file byte-identical, and saving must
// never execute anything.

// entryCall calls one entry-admin method and decodes the result.
func entryCall(t *testing.T, client *ipc.Client, method string, params map[string]any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := client.Call(method, params, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

// entryProblemList decodes the {field, message} problems from a result or an
// error's data.
func entryProblemList(t *testing.T, container map[string]any) []map[string]any {
	t.Helper()
	raw, _ := container["problems"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		m, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("problem %v is not an object", p)
		}
		out = append(out, m)
	}
	return out
}

// problemOn finds the first problem keyed to field.
func problemOn(problems []map[string]any, field string) (string, bool) {
	for _, p := range problems {
		if p["field"] == field {
			msg, _ := p["message"].(string)
			return msg, true
		}
	}
	return "", false
}

// tomlKeys lists a struct's toml tags — the loader's own key set for one
// [[family]] table.
func tomlKeys(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	keys := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0]
		if tag == "" {
			t.Fatalf("%s.%s carries no toml tag", typ.Name(), typ.Field(i).Name)
		}
		if tag == "-" {
			// A field the loader deliberately never reads from the file —
			// Advisor.ReadOnly is computed from the preset table (ADR 0016) —
			// so it is not a key any form can write either.
			continue
		}
		keys = append(keys, tag)
	}
	sort.Strings(keys)
	return keys
}

// TestEntryAdminFamiliesMirrorConfigStructs is the drift guard the registry
// comment promises: each family's key whitelist (and its keyOrder) is
// exactly the config struct's toml tag set. A field added to a struct
// without a registry row entry would silently block form-saving any entry a
// hand edit gave that key — this test makes the drift loud instead.
//
// Three shape-driven adjustments, each of them a rule rather than an excuse
// (#163, #164): a KEYED family carries `name` on the wire although the struct
// has no such field, because the table key is the identity; a declared SECRET
// key is a struct field the form deliberately cannot write, because it is
// written through the credential channel instead; and a SCALAR-MAP family has
// no struct at all — [tts.lexicon] is a map[string]string — so its example is
// nil and its wire keys are `name` plus the one value key it declares.
func TestEntryAdminFamiliesMirrorConfigStructs(t *testing.T) {
	structs := map[string]any{
		"routines":        config.Routine{},
		"scripts":         config.Script{},
		"knowledge.feeds": config.KnowledgeFeed{},
		"ai":              config.Endpoint{},
		"advisors":        config.Advisor{},
		"intents.custom":  config.CustomIntent{},
		"tts.lexicon":     nil,
	}
	if len(structs) != len(entryAdminFamilies) {
		t.Fatalf("registry has %d families, this test knows %d — extend both together",
			len(entryAdminFamilies), len(structs))
	}
	for family, example := range structs {
		spec, ok := entryAdminFamilies[family]
		if !ok {
			t.Errorf("family %q is not in the registry", family)
			continue
		}
		var want []string
		if example == nil {
			if spec.shape != entryShapeScalarMap || spec.valueKey == "" {
				t.Errorf("%s has no config struct, so it must be a scalar-map family "+
					"declaring its value key", family)
				continue
			}
			want = []string{"name", spec.valueKey}
		} else {
			want = make([]string, 0, len(spec.keys))
			for _, key := range tomlKeys(t, example) {
				if spec.secretFor(key) == nil {
					want = append(want, key)
				}
			}
		}
		if spec.shape == entryShapeKeyed {
			want = append(want, "name")
		}
		sort.Strings(want)
		keys := make([]string, 0, len(spec.keys))
		for k := range spec.keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, want) {
			t.Errorf("%s registry keys = %v, want the struct's %v", family, keys, want)
		}
		order := append([]string{}, spec.keyOrder...)
		sort.Strings(order)
		if !reflect.DeepEqual(order, want) {
			t.Errorf("%s keyOrder = %v, want every struct key exactly once", family, spec.keyOrder)
		}
	}
	steps := entryAdminFamilies["routines"].subKeys["steps"]
	want := tomlKeys(t, config.RoutineStep{})
	keys := make([]string, 0, len(steps))
	for k := range steps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("routines steps keys = %v, want the struct's %v", keys, want)
	}
}

// TestConfigGetEntryOverSocket: the form's read — the whole entry as the
// parser sees it, keys the listing never carries included (report), paired
// with the file's fingerprint. Unknown families and names refuse by name.
func TestConfigGetEntryOverSocket(t *testing.T) {
	client, _, _ := startAutomationsDaemon(t, false)

	fp, _ := automationsList(t, client)
	out := entryCall(t, client, "config.get_entry",
		map[string]any{"family": "scripts", "name": "Backup Notes"})
	if out["fingerprint"] != fp {
		t.Errorf("fingerprint = %v, want the listing's %q", out["fingerprint"], fp)
	}
	entry, _ := out["entry"].(map[string]any)
	if entry["name"] != "backup notes" || entry["report"] != "stdout" ||
		entry["schedule"] != "02:00" {
		t.Errorf("entry = %v, want the whole [[scripts]] table, report included", entry)
	}

	// [tools.policy] is the family that must never become editable through this
	// surface: the permission gate is administered on its own screen, with its
	// own refusal matrix (#164, ADR 0053), not as an entry family.
	err := client.Call("config.get_entry",
		map[string]any{"family": "tools.policy", "name": "x"}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"tools.policy"`) {
		t.Errorf("unknown family err = %v, want CodeInvalidParams naming it", err)
	}
	err = client.Call("config.get_entry",
		map[string]any{"family": "scripts", "name": "no such"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"no such"`) {
		t.Errorf("unknown name err = %v, want CodeInvalidParams naming it", err)
	}
}

// TestConfigValidateEntrySchedule: the schedule field validates through the
// real parser — its own worked-example error on the field — and a valid one
// comes back with the scheduler's next-fire arithmetic as a preview. Neither
// direction writes a byte.
func TestConfigValidateEntrySchedule(t *testing.T) {
	client, configFile, _ := startAutomationsDaemon(t, false)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	_, rows := automationsList(t, client)
	scriptPath := rows[1].Path

	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "scripts",
		"entry": map[string]any{"name": "weather report", "phrases": []string{"weather report"},
			"path": scriptPath, "schedule": "9am"},
	})
	if out["valid"] != false {
		t.Fatalf("validate = %v, want invalid", out)
	}
	msg, ok := problemOn(entryProblemList(t, out), "schedule")
	if !ok || !strings.Contains(msg, `"9am"`) || !strings.Contains(msg, "08:30") {
		t.Errorf("schedule problem = %q (%v), want the parser's worked-example error", msg, ok)
	}
	if _, ok := out["next_fire"]; ok {
		t.Error("an unparsable schedule fabricated a next fire")
	}

	out = entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "scripts",
		"entry": map[string]any{"name": "weather report", "phrases": []string{"weather report"},
			"path": scriptPath, "schedule": "07:15"},
	})
	if out["valid"] != true || len(entryProblemList(t, out)) != 0 {
		t.Fatalf("validate = %v, want clean", out)
	}
	next, _ := out["next_fire"].(string)
	at, err := time.Parse(time.RFC3339, next)
	if err != nil || at.Hour() != 7 || at.Minute() != 15 {
		t.Errorf("next_fire = %q (%v), want the scheduler's 07:15", next, err)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Error("a dry-run validate changed the file")
	}
}

// TestConfigValidateEntryCollisionIsFieldKeyed is the acceptance criterion on
// phrase collisions: the draft's phrase already belongs to another entry, and
// the router's own error — naming the other owner — lands on exactly the
// phrase field that carries it, whichever side of the compile order the
// complaint comes from.
func TestConfigValidateEntryCollisionIsFieldKeyed(t *testing.T) {
	client, _, _ := startAutomationsDaemon(t, false)
	_, rows := automationsList(t, client)
	scriptPath := rows[1].Path

	// A new script stealing the routine's phrase: scripts compile after
	// routines, so the complaint is labelled with the draft itself.
	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "scripts",
		"entry": map[string]any{"name": "evening chime", "phrases": []string{"ding", "evening mode"},
			"path": scriptPath},
	})
	msg, ok := problemOn(entryProblemList(t, out), "phrases[1]")
	if !ok || !strings.Contains(msg, `already the trigger for routine "evening"`) {
		t.Errorf("collision problem = %q (%v), want it on phrases[1] naming the routine", msg, ok)
	}

	// The routine stealing the script's phrase: the script compiles later and
	// complains under its own label, and the classifier still pins the quoted
	// phrase back to the draft's field.
	out = entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "routines", "name": "evening",
		"entry": map[string]any{"name": "evening", "phrases": []string{"evening mode", "backup my notes"},
			"steps": []map[string]any{{"app": "mpv", "workspace": 5}}},
	})
	msg, ok = problemOn(entryProblemList(t, out), "phrases[1]")
	if !ok || !strings.Contains(msg, `"backup my notes"`) || !strings.Contains(msg, "already") {
		t.Errorf("reverse collision problem = %q (%v), want it on phrases[1]", msg, ok)
	}
}

// TestConfigUpsertEntryCreateScriptOverSocket is the acceptance path for New:
// the entry is appended with every byte above it untouched, the standard
// reload picks it up — the row appears, the phrase routes, running works —
// and the activity feed names the creation.
func TestConfigUpsertEntryCreateScriptOverSocket(t *testing.T) {
	client, configFile, marker := startAutomationsDaemon(t, true)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp, rows := automationsList(t, client)
	scriptPath := rows[1].Path

	out := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "scripts", "fingerprint": fp,
		"entry": map[string]any{"name": "weather report", "phrases": []string{"weather report"},
			"path": scriptPath, "timeout_sec": 30},
	})
	if out["created"] != true || out["applied"] != true {
		t.Fatalf("upsert = %v, want created and applied", out)
	}
	waitForActivityRow(t, client, "Script created: weather report")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimRight(string(original), "\n") + "\n\n" +
		"[[scripts]]\n" +
		"name = \"weather report\"\n" +
		"phrases = [\"weather report\"]\n" +
		"path = \"" + scriptPath + "\"\n" +
		"timeout_sec = 30\n"
	if string(raw) != want {
		t.Errorf("config after create:\n%s\n--- want ---\n%s", raw, want)
	}

	_, rows = automationsList(t, client)
	if len(rows) != 4 || rows[3].Name != "weather report" || rows[3].Kind != "script" {
		t.Fatalf("rows after create = %+v", rows)
	}
	// The reload compiled the new definition: the run surface knows the name
	// and executes the file the entry names.
	if err := client.Call("scripts.run", map[string]string{"name": "weather report"}, nil); err != nil {
		t.Fatalf("running the created script: %v", err)
	}
	waitForActivityRow(t, client, "Script finished: weather report")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the created script never ran: %v", err)
	}
}

// TestConfigUpsertEntryEditRoutineOverSocket is the acceptance path for Edit:
// phrases changed, a step added and the order rearranged, and only that
// entry's table moves on disk — the comments and both script entries are
// byte-identical — while the reload recompiles the grammar and re-arms the
// schedule.
func TestConfigUpsertEntryEditRoutineOverSocket(t *testing.T) {
	client, configFile, _ := startAutomationsDaemon(t, false)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := automationsList(t, client)

	out := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "routines", "name": "evening", "fingerprint": fp,
		"entry": map[string]any{
			"name": "evening", "phrases": []string{"evening mode", "wind down"},
			"schedule": "19:00",
			"steps": []map[string]any{
				{"app": "spotify", "workspace": 6},
				{"app": "mpv", "workspace": 5},
			},
		},
	})
	if out["created"] != false || out["applied"] != true {
		t.Fatalf("upsert = %v, want an applied edit", out)
	}
	waitForActivityRow(t, client, "Routine edited: evening")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	oldBlock := "[[routines]]\nname = \"evening\"\nphrases = [\"evening mode\"]\nschedule = \"18:00\"\n\n" +
		"  [[routines.steps]]\n  app = \"mpv\"\n  workspace = 5\n"
	newBlock := "[[routines]]\nname = \"evening\"\nphrases = [\"evening mode\", \"wind down\"]\nschedule = \"19:00\"\n\n" +
		"  [[routines.steps]]\n  app = \"spotify\"\n  workspace = 6\n\n" +
		"  [[routines.steps]]\n  app = \"mpv\"\n  workspace = 5\n"
	want := strings.Replace(string(original), oldBlock, newBlock, 1)
	if want == string(original) {
		t.Fatal("the test's oldBlock no longer matches the fixture")
	}
	if string(raw) != want {
		t.Errorf("config after edit:\n%s\n--- want ---\n%s", raw, want)
	}

	_, rows := automationsList(t, client)
	if rows[0].Steps == nil || *rows[0].Steps != 2 || rows[0].Schedule != "19:00" ||
		rows[0].NextFire == "" || len(rows[0].Phrases) != 2 {
		t.Errorf("row after edit = %+v, want two steps and the re-armed 19:00 schedule", rows[0])
	}
}

// TestConfigUpsertEntryInvalidWritesNothing is the half-write criterion: a
// draft the whole-document validation refuses — here a collision — returns
// the field-keyed problems and leaves the file byte-identical.
func TestConfigUpsertEntryInvalidWritesNothing(t *testing.T) {
	client, configFile, _ := startAutomationsDaemon(t, false)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp, rows := automationsList(t, client)

	err = client.Call("config.upsert_entry", map[string]any{
		"family": "scripts", "fingerprint": fp,
		"entry": map[string]any{"name": "evening chime", "phrases": []string{"evening mode"},
			"path": rows[1].Path},
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	msg, ok := problemOn(entryProblemList(t, data), "phrases[0]")
	if !ok || !strings.Contains(msg, `already the trigger for routine "evening"`) {
		t.Errorf("problems = %v, want the collision on phrases[0] naming both owners", data)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Errorf("a refused save still changed the file:\n%s", raw)
	}
}

// TestConfigUpsertEntryRefusesUnknownKeys pins the zero-argument shape (ADR
// 0030) at the wire: a script draft smuggling an `args` key is refused by the
// whitelist — field-keyed, nothing written, and (the point of the shape)
// nothing executed: saving is never an execution path.
func TestConfigUpsertEntryRefusesUnknownKeys(t *testing.T) {
	client, configFile, marker := startAutomationsDaemon(t, true)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	_, rows := automationsList(t, client)

	err = client.Call("config.upsert_entry", map[string]any{
		"family": "scripts",
		"entry": map[string]any{"name": "sneaky", "phrases": []string{"sneak"},
			"path": rows[1].Path, "args": []string{"--from-speech"}},
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if msg, ok := problemOn(entryProblemList(t, data), "args"); !ok ||
		!strings.Contains(msg, `"args"`) {
		t.Errorf("problems = %v, want the refused key named on its own field", data)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Error("a refused draft still changed the file")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("saving a draft executed the script")
	}
}

// TestConfigUpsertEntryRefusesStaleFingerprint: a hand edit made after the
// form opened is never clobbered — the save is a conflict carrying the fresh
// fingerprint, and the hand edit survives byte-for-byte.
func TestConfigUpsertEntryRefusesStaleFingerprint(t *testing.T) {
	client, configFile, _ := startAutomationsDaemon(t, false)
	fp, rows := automationsList(t, client)

	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	edited := string(original) + "\n# a hand note made while the form sat open\n"
	if err := os.WriteFile(configFile, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	err = client.Call("config.upsert_entry", map[string]any{
		"family": "scripts", "name": "backup notes", "fingerprint": fp,
		"entry": map[string]any{"name": "backup notes", "phrases": []string{"backup my notes"},
			"path": rows[1].Path},
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigConflict ||
		!strings.Contains(rpcErr.Message, "outside the window") {
		t.Fatalf("err = %v, want CodeConfigConflict wording the outside edit", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if fresh, _ := data["fingerprint"].(string); fresh == "" || fresh == fp {
		t.Errorf("conflict data = %v, want the fresh fingerprint", rpcErr.Data)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != edited {
		t.Errorf("the hand edit was clobbered:\n%s", raw)
	}
}

// TestConfigDeleteEntryOverSocket is the acceptance path for Delete: the
// entry's block and its glued comment go, every other byte stays, the reload
// takes the phrases out of the grammar and the schedule off the clock, and
// the activity feed names the deletion.
func TestConfigDeleteEntryOverSocket(t *testing.T) {
	client, configFile, marker := startAutomationsDaemon(t, false)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp, rows := automationsList(t, client)
	scriptPath := rows[1].Path

	out := entryCall(t, client, "config.delete_entry",
		map[string]any{"family": "scripts", "name": "backup notes", "fingerprint": fp})
	if out["applied"] != true {
		t.Fatalf("delete = %v, want it applied", out)
	}
	waitForActivityRow(t, client, "Script deleted: backup notes")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	removed := "# the nightly backup\n[[scripts]]\nname = \"backup notes\"\nphrases = [\"backup my notes\"]\n" +
		"path = \"" + scriptPath + "\"\nreport = \"stdout\"\nschedule = \"02:00\"\n\n"
	want := strings.Replace(string(original), removed, "", 1)
	if want == string(original) {
		t.Fatal("the test's removed block no longer matches the fixture")
	}
	if string(raw) != want {
		t.Errorf("config after delete:\n%s\n--- want ---\n%s", raw, want)
	}

	_, rows = automationsList(t, client)
	if len(rows) != 2 {
		t.Fatalf("rows after delete = %+v", rows)
	}
	err = client.Call("scripts.run", map[string]string{"name": "backup notes"}, nil)
	if err == nil || !strings.Contains(err.Error(), "backup notes") {
		t.Errorf("run after delete = %v, want the unknown-script refusal", err)
	}
	var schedules struct {
		Schedules []struct {
			Name string `json:"name"`
		} `json:"schedules"`
	}
	if err := client.Call("automations.schedules", nil, &schedules); err != nil {
		t.Fatal(err)
	}
	for _, s := range schedules.Schedules {
		if s.Name == "backup notes" {
			t.Error("a deleted entry is still on the clock")
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("deleting executed the script")
	}
}

// TestConfigDeleteEntryRefusals: an unknown name and a stale fingerprint each
// refuse without touching the file.
func TestConfigDeleteEntryRefusals(t *testing.T) {
	client, configFile, _ := startAutomationsDaemon(t, false)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := automationsList(t, client)

	err = client.Call("config.delete_entry",
		map[string]any{"family": "scripts", "name": "no such", "fingerprint": fp}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"no such"`) {
		t.Errorf("unknown name err = %v, want CodeInvalidParams naming it", err)
	}

	err = client.Call("config.delete_entry",
		map[string]any{"family": "scripts", "name": "backup notes", "fingerprint": "sha256:stale"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigConflict {
		t.Errorf("stale fingerprint err = %v, want CodeConfigConflict", err)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Error("a refused delete still changed the file")
	}
}
