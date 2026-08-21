package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

func fileIn(t *testing.T) *File {
	t.Helper()
	return &File{Path: filepath.Join(t.TempDir(), "state", "history.json")}
}

func TestRoundTrip(t *testing.T) {
	f := fileIn(t)
	in := []ai.Message{
		{Role: ai.RoleUser, Content: "why is my build failing?"},
		{Role: ai.RoleAssistant, Content: "The linker cannot find libfoo."},
	}
	lastTurn := time.Now().Round(0)
	if err := f.Save(in, lastTurn); err != nil {
		t.Fatal(err)
	}
	out, gotTurn, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("loaded %d messages, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Role != in[i].Role || out[i].Content != in[i].Content {
			t.Errorf("message %d = %+v, want %+v", i, out[i], in[i])
		}
	}
	if !gotTurn.Equal(lastTurn) {
		t.Errorf("last turn = %v, want %v", gotTurn, lastTurn)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	f := fileIn(t)
	messages, lastTurn, err := f.Load()
	if err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	if len(messages) != 0 || !lastTurn.IsZero() {
		t.Errorf("missing file loaded %d messages, lastTurn %v", len(messages), lastTurn)
	}
}

func TestLoadCorruptFileIsAnError(t *testing.T) {
	f := fileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A truncated write, as after kill -9 without the atomic rename.
	if err := os.WriteFile(f.Path, []byte(`{"version":1,"messages":[{"ro`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Load(); err == nil {
		t.Error("corrupt file must surface an error for the caller to downgrade")
	}
}

func TestLoadUnknownVersionIsAnError(t *testing.T) {
	f := fileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path, []byte(`{"version":99,"messages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Load(); err == nil {
		t.Error("unknown version must be an error, not guessed at")
	}
}

func TestSaveIsPrivate(t *testing.T) {
	f := fileIn(t)
	if err := f.Save([]ai.Message{{Role: ai.RoleUser, Content: "secretish"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 0700", perm)
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	f := fileIn(t)
	if err := f.Save([]ai.Message{{Role: ai.RoleUser, Content: "hi"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(f.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSaveCapsFileSizeDroppingOldestFirst(t *testing.T) {
	f := fileIn(t)
	// 40 exchanges of ~100 KB each: ~4 MB raw, far over the cap.
	big := strings.Repeat("x", 100*1024)
	var messages []ai.Message
	for i := 0; i < 40; i++ {
		messages = append(messages,
			ai.Message{Role: ai.RoleUser, Content: big},
			ai.Message{Role: ai.RoleAssistant, Content: "answer " + string(rune('a'+i))})
	}
	if err := f.Save(messages, time.Now()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > MaxFileBytes {
		t.Errorf("file is %d bytes, cap is %d", fi.Size(), MaxFileBytes)
	}
	loaded, _, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 {
		t.Fatal("cap must keep the newest exchanges, not drop everything")
	}
	// The newest exchange survives; the survivors are the tail of the input.
	if got, want := loaded[len(loaded)-1].Content, messages[len(messages)-1].Content; got != want {
		t.Errorf("newest message = %q, want %q", got, want)
	}
}

func TestClearRemovesFileAndToleratesAbsence(t *testing.T) {
	f := fileIn(t)
	if err := f.Clear(); err != nil {
		t.Fatalf("clearing an absent history must be a no-op: %v", err)
	}
	if err := f.Save([]ai.Message{{Role: ai.RoleUser, Content: "hi"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
		t.Errorf("history file still exists after Clear: %v", err)
	}
}

func TestSaveOverwritesPreviousHistory(t *testing.T) {
	f := fileIn(t)
	if err := f.Save([]ai.Message{{Role: ai.RoleUser, Content: "old"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.Save([]ai.Message{{Role: ai.RoleUser, Content: "new"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Content != "new" {
		t.Errorf("loaded %+v, want just the new history", loaded)
	}
}

// A file that still parses as JSON but carries a role no provider
// understands is corruption, and corruption has a documented degradation:
// warn and start empty. Loading it unchecked instead pushes the damage into
// the next provider request (raised in review of #16).
func TestLoadRejectsUnknownRole(t *testing.T) {
	f := fileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":1,"messages":[{"role":"user","content":"hi"},{"role":"assistnat","content":"typo"}]}`
	if err := os.WriteFile(f.Path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	messages, _, err := f.Load()
	if err == nil {
		t.Fatalf("unknown role must be a load error, got %+v", messages)
	}
	if !strings.Contains(err.Error(), "assistnat") {
		t.Errorf("error must name the bad role: %v", err)
	}
	if messages != nil {
		t.Errorf("a rejected file must load nothing, got %+v", messages)
	}
}

func TestLoadAcceptsEveryKnownRole(t *testing.T) {
	f := fileIn(t)
	in := []ai.Message{
		{Role: ai.RoleSystem, Content: "system"},
		{Role: ai.RoleUser, Content: "user"},
		{Role: ai.RoleAssistant, Content: "assistant"},
		{Role: ai.RoleTool, Content: "tool"},
	}
	if err := f.Save(in, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, _, err := f.Load()
	if err != nil {
		t.Fatalf("every role the ai package defines must round-trip: %v", err)
	}
	if len(out) != len(in) {
		t.Errorf("loaded %d messages, want %d", len(out), len(in))
	}
}

// MkdirAll leaves a pre-existing directory's modes alone, so the 0700
// privacy requirement has to be asserted, not assumed: the content is user
// speech (raised in review of #16).
func TestSaveTightensPreExistingStateDirectory(t *testing.T) {
	f := fileIn(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o777); err != nil {
		t.Fatal(err)
	}
	// MkdirAll applies the umask; make the directory genuinely world-readable.
	if err := os.Chmod(filepath.Dir(f.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Save([]ai.Message{{Role: ai.RoleUser, Content: "secretish"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Dir(f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("pre-existing state dir mode = %o, want 0700", perm)
	}
	fi, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("history file mode = %o, want 0600", perm)
	}
}

// The durability half of ADR 0011's "fsync, then rename": the rename is only
// atomic, and the new directory entry needs its own fsync to survive a power
// cut. The sync itself is not observable from a test, so this pins the helper
// that performs it — including that a failure is surfaced rather than ignored
// (raised in review of #16).
func TestSyncDirSyncsAndReportsFailure(t *testing.T) {
	dir := t.TempDir()
	if err := syncDir(dir); err != nil {
		t.Errorf("syncing a real directory: %v", err)
	}
	if err := syncDir(filepath.Join(dir, "not-there")); err == nil {
		t.Error("a directory that cannot be opened must surface an error")
	}
}
