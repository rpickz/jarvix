package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/statehold"
)

// testPaths builds hermetic roots. The socket points into the temp dir too,
// so no test can ever reach a real daemon.
func testPaths(t *testing.T) config.Paths {
	t.Helper()
	dir := t.TempDir()
	return config.Paths{
		Config:  filepath.Join(dir, "config", "jarvix"),
		Data:    filepath.Join(dir, "data", "jarvix"),
		State:   filepath.Join(dir, "state", "jarvix"),
		Runtime: filepath.Join(dir, "run", "jarvix"),
		Socket:  filepath.Join(dir, "run", "jarvix.sock"),
	}
}

// secretKey is what the redaction tests plant and then hunt for in archive
// bytes. A placeholder by construction — never a real credential.
const secretKey = "sk-test-EXAMPLE-0000-not-a-real-key"

const seedConfig = `# test config
[ai]
provider = "openai"

[ai.openai]
base_url = "https://api.openai.com/v1"
api_key = "` + secretKey + `"
`

const seedMemory = `version = 1
next_id = 2

[[fact]]
id = "m1"
content = "the staging server is called atlas"
stored = 2026-08-01T10:00:00Z
updated = 2026-08-01T10:00:00Z
`

// seedRoots writes a state a real machine could hold: config with a secret,
// two known stores, one conversation, and — deliberately — one store this
// package has never heard of, standing in for whatever the next wave adds.
func seedRoots(t *testing.T, paths config.Paths) {
	t.Helper()
	files := map[string]string{
		filepath.Join(paths.Config, "config.toml"):                                seedConfig,
		filepath.Join(paths.State, "memory.toml"):                                 seedMemory,
		filepath.Join(paths.State, "history.json"):                                `{"version":1,"last_turn":"2026-08-01T10:00:00Z","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"Understood."}]}`,
		filepath.Join(paths.State, "futurestore.toml"):                            "version = 7\nanything = \"the store no enumerated list would know\"\n",
		filepath.Join(paths.State, "conversations", "20260801-100000-abcd.json"):  `{"schema":1,"id":"20260801-100000-abcd","started":"2026-08-01T10:00:00Z","last":"2026-08-01T10:00:00Z","turns":2,"preview":"hello"}`,
		filepath.Join(paths.State, "conversations", "20260801-100000-abcd.jsonl"): "{\"schema\":1,\"id\":\"20260801-100000-abcd\"}\n{\"role\":\"user\",\"text\":\"hello\",\"ts\":\"2026-08-01T10:00:00Z\"}\n",
		filepath.Join(paths.State, "conversations", "active"):                     "20260801-100000-abcd\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// treeSnapshot reads every file under root, keyed by relative path.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// archiveEntries reads every entry of a tar.gz, keyed by name.
func archiveEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = data
	}
	return entries
}

// TestRoundTripRestoresEverythingVerbatim is the backbone: a stopped-daemon
// backup restored onto a fresh machine reproduces every byte — including the
// store this package has never heard of, which is the wholesale-discovery
// pin: a wave adding a store must never need to touch backup code.
func TestRoundTripRestoresEverythingVerbatim(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")

	report, err := Create(src, archive, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Capture != CaptureDirect {
		t.Errorf("capture = %q, want %q with no daemon", report.Capture, CaptureDirect)
	}
	if report.Files != 7 {
		t.Errorf("archived %d files, want 7", report.Files)
	}

	dst := testPaths(t) // fresh machine: the roots do not even exist
	restored, err := Restore(dst, archive, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.SafetyCopies) != 0 {
		t.Errorf("fresh restore made safety copies: %v", restored.SafetyCopies)
	}
	for root, srcRoot := range map[string]string{dst.Config: src.Config, dst.State: src.State} {
		want := treeSnapshot(t, srcRoot)
		got := treeSnapshot(t, root)
		if len(got) != len(want) {
			t.Errorf("%s: restored %d files, want %d", root, len(got), len(want))
		}
		for rel, content := range want {
			if got[rel] != content {
				t.Errorf("%s/%s differs after restore", root, rel)
			}
		}
	}
	// The unknown store came through verbatim — the #141-and-beyond pin.
	data, err := os.ReadFile(filepath.Join(dst.State, "futurestore.toml"))
	if err != nil || !strings.Contains(string(data), "no enumerated list") {
		t.Errorf("unknown store not restored verbatim: %v", err)
	}
}

// TestArchiveContainsNothingOutsideTheRoots is the manifest pin of the "no
// shell configs, no ssh keys" criterion: every entry is the manifest or
// lives under config/ or state/, and a symlink pointing out of the roots is
// skipped, not followed.
func TestArchiveContainsNothingOutsideTheRoots(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	outside := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(outside, []byte("PRIVATE KEY MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src.State, "sneaky-link")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	report, err := Create(src, archive, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SkippedSymlinks) != 1 {
		t.Errorf("skipped symlinks = %v, want the one planted", report.SkippedSymlinks)
	}

	entries := archiveEntries(t, archive)
	var manifest Manifest
	if err := json.Unmarshal(entries[ManifestName], &manifest); err != nil {
		t.Fatal(err)
	}
	check := func(name string) {
		if name == ManifestName {
			return
		}
		if !strings.HasPrefix(name, "config/") && !strings.HasPrefix(name, "state/") {
			t.Errorf("entry %q is outside the roots", name)
		}
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			t.Errorf("entry %q has an unsafe path", name)
		}
	}
	for name, data := range entries {
		check(name)
		if bytes.Contains(data, []byte("PRIVATE KEY MATERIAL")) {
			t.Errorf("entry %q carries bytes from outside the roots", name)
		}
	}
	for _, f := range manifest.Files {
		check(f.Path)
	}
	if len(manifest.Files) != len(entries)-1 {
		t.Errorf("manifest lists %d files, archive has %d", len(manifest.Files), len(entries)-1)
	}
}

// Scratch files from the stores' atomic writes are exactly what a backup
// must not capture: they are the mid-write state.
func TestAtomicWriteScratchFilesAreNotArchived(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	scratch := filepath.Join(src.State, ".memory-1234.tmp")
	if err := os.WriteFile(scratch, []byte("half a store"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(src, archive, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for name := range archiveEntries(t, archive) {
		if strings.Contains(name, ".tmp") {
			t.Errorf("scratch file %q was archived", name)
		}
	}
}

// Secrets are included by default — api keys live in config.toml and a
// backup that silently dropped them would not restore a working machine.
func TestSecretsAreIncludedByDefault(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(src, archive, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	entries := archiveEntries(t, archive)
	if !bytes.Contains(entries["config/config.toml"], []byte(secretKey)) {
		t.Error("api key missing from default backup — secrets are included by default")
	}
	var manifest Manifest
	if err := json.Unmarshal(entries[ManifestName], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Redacted {
		t.Error("manifest claims redaction on a default backup")
	}
}

// --no-secrets: the key value appears nowhere in the archive, the manifest
// names what was redacted, and restoring warns which keys need re-entry.
func TestNoSecretsRedactsAPIKeys(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	report, err := Create(src, archive, CreateOptions{NoSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RedactedKeys) != 1 || report.RedactedKeys[0] != "ai.openai.api_key" {
		t.Errorf("redacted keys = %v, want [ai.openai.api_key]", report.RedactedKeys)
	}

	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secretKey)) {
		t.Fatal("compressed archive bytes contain the api key")
	}
	entries := archiveEntries(t, archive)
	cfg := entries["config/config.toml"]
	if bytes.Contains(cfg, []byte(secretKey)) {
		t.Fatal("archived config contains the api key")
	}
	if !bytes.Contains(cfg, []byte(RedactedPlaceholder)) {
		t.Error("archived config carries no placeholder")
	}
	// Everything except the secret survives byte-identically.
	if !bytes.Contains(cfg, []byte(`base_url = "https://api.openai.com/v1"`)) {
		t.Error("redaction disturbed unrelated lines")
	}

	dst := testPaths(t)
	restored, err := Restore(dst, archive, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.RedactedKeys) != 1 || restored.RedactedKeys[0] != "ai.openai.api_key" {
		t.Errorf("restore reported redacted keys %v, want [ai.openai.api_key]", restored.RedactedKeys)
	}
}

func TestRedactAPIKeysHandlesVariants(t *testing.T) {
	in := strings.Join([]string{
		`api_key = "top-level"`,
		`[ai.one]`,
		`api_key = 'single-quoted'`,
		`api_key_env = "OPENAI_API_KEY"`, // an env NAME is not a secret
		`[ai.two]`,
		`api_key = ""`,                            // empty: nothing to redact
		`api_key = "` + RedactedPlaceholder + `"`, // already redacted
	}, "\n")
	out, keys := redactAPIKeys([]byte(in))
	if strings.Contains(string(out), "top-level") || strings.Contains(string(out), "single-quoted") {
		t.Errorf("secrets survived redaction:\n%s", out)
	}
	if !strings.Contains(string(out), `api_key_env = "OPENAI_API_KEY"`) {
		t.Error("api_key_env (an env var name, not a secret) was disturbed")
	}
	want := []string{"api_key", "ai.one.api_key"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Errorf("redacted keys = %v, want %v", keys, want)
	}
}

// Restore over existing state: the old roots move aside whole, named in the
// report, recoverable by hand with one mv — never destroyed.
func TestRestoreOverMovesExistingStateToSafetyCopies(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(src, archive, CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	dst := testPaths(t)
	seedRoots(t, dst)
	canary := filepath.Join(dst.State, "canary.toml")
	if err := os.WriteFile(canary, []byte("about-to-be-replaced = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := Restore(dst, archive, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SafetyCopies) != 2 {
		t.Fatalf("safety copies = %v, want one per existing root", report.SafetyCopies)
	}
	var foundCanary bool
	for _, aside := range report.SafetyCopies {
		if _, err := os.Stat(aside); err != nil {
			t.Errorf("safety copy %s named but absent: %v", aside, err)
		}
		if _, err := os.Stat(filepath.Join(aside, "canary.toml")); err == nil {
			foundCanary = true
		}
	}
	if !foundCanary {
		t.Error("the pre-restore state's canary is in no safety copy")
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("canary survived in the restored root — the roots were merged, not swapped")
	}
}

// TestBackupUnderARunningDaemonHoldsTheGate proves Create takes the
// consistency path: state.hold before reading, state.release after, capture
// recorded as daemon-held.
func TestBackupUnderARunningDaemonHoldsTheGate(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)

	gate := &statehold.Gate{}
	var order []string
	srv := ipc.NewServer(src.Socket, nil, nil)
	var release func()
	srv.Handle("state.hold", func(json.RawMessage) (any, error) {
		r, err := gate.Hold(context.Background(), time.Minute)
		if err != nil {
			return nil, err
		}
		release = r
		order = append(order, "hold")
		return map[string]any{"held": true}, nil
	})
	srv.Handle("state.release", func(json.RawMessage) (any, error) {
		order = append(order, "release")
		release()
		return map[string]any{"held": false}, nil
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-served })

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	report, err := Create(src, archive, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Capture != CaptureHeld {
		t.Errorf("capture = %q, want %q", report.Capture, CaptureHeld)
	}
	if len(order) != 2 || order[0] != "hold" || order[1] != "release" {
		t.Errorf("verb order = %v, want [hold release]", order)
	}
	if gate.Held() {
		t.Error("gate left held after backup")
	}
}

// A daemon that answers the socket but cannot hold must fail the backup:
// proceeding would silently drop the consistency the feature promises.
func TestBackupFailsWhenTheDaemonCannotHold(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	srv := ipc.NewServer(src.Socket, nil, nil)
	srv.Handle("state.hold", func(json.RawMessage) (any, error) {
		return nil, ipc.Errorf(ipc.CodeInvalidRequest, "state writes are already held — another backup is running")
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-served })

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(src, archive, CreateOptions{}); err == nil {
		t.Fatal("backup succeeded although the daemon refused to hold")
	}
	if _, err := os.Stat(archive); err == nil {
		t.Error("a failed backup left an archive behind")
	}
}

// TestTornWriteSimulation rewrites a store atomically in a tight loop while
// the backup reads it — the exact scenario the atomic-rename discipline plus
// whole-file reads must survive. The archive must validate and restore.
func TestTornWriteSimulation(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		target := filepath.Join(src.State, "memory.toml")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tmp, err := os.CreateTemp(src.State, ".memory-*.tmp")
			if err != nil {
				return
			}
			doc := seedMemory + "\n# rewrite " + strings.Repeat("x", i%512) + "\n"
			_, _ = tmp.WriteString(doc)
			_ = tmp.Close()
			_ = os.Rename(tmp.Name(), target)
		}
	}()
	for i := range 5 {
		archive := filepath.Join(t.TempDir(), "backup.tar.gz")
		if _, err := Create(src, archive, CreateOptions{}); err != nil {
			t.Fatalf("backup %d under concurrent writes: %v", i, err)
		}
		dst := testPaths(t)
		if _, err := Restore(dst, archive, RestoreOptions{}); err != nil {
			t.Fatalf("restore %d of an under-write backup: %v", i, err)
		}
	}
	close(stop)
	<-done
}

func TestResolveDest(t *testing.T) {
	now := time.Date(2026, 8, 28, 3, 0, 0, 0, time.UTC)
	dated := "jarvix-backup-20260828-030000.tar.gz"
	dir := t.TempDir()
	cases := map[string]string{
		"":                        dated,
		dir:                       filepath.Join(dir, dated),
		"/tmp/nope/custom.tar.gz": "/tmp/nope/custom.tar.gz",
	}
	for arg, want := range cases {
		if got := ResolveDest(arg, now); got != want {
			t.Errorf("ResolveDest(%q) = %q, want %q", arg, got, want)
		}
	}
}
