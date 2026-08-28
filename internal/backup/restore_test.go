package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// The refusal matrix. Every case asserts three things: the error is a
// RefusalError carrying the specific reason, the reason names the actual
// problem, and nothing under the target roots was touched — no partial
// restore, no leftover staging debris.

// makeArchive backs up freshly seeded roots and returns the archive path.
func makeArchive(t *testing.T) string {
	t.Helper()
	src := testPaths(t)
	seedRoots(t, src)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(src, archive, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return archive
}

// tarEntry is one entry for hand-built (broken) archives.
type tarEntry struct {
	name     string
	data     []byte
	typeflag byte
	linkname string
}

// writeTarGz builds an archive from entries verbatim — including the broken
// shapes Create would never produce.
func writeTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.data)),
			Typeflag: typeflag, Linkname: e.linkname}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mutateArchive unpacks a valid archive, applies fn to its entries, and
// repacks — for tampering with single aspects of an otherwise-valid archive.
func mutateArchive(t *testing.T, src string, fn func(entries map[string][]byte)) string {
	t.Helper()
	entries := archiveEntries(t, src)
	fn(entries)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	// Manifest first, the rest in walk order — same shape Create writes.
	list := make([]tarEntry, 0, len(entries))
	if data, ok := entries[ManifestName]; ok {
		list = append(list, tarEntry{name: ManifestName, data: data})
	}
	for _, name := range names {
		if name != ManifestName {
			list = append(list, tarEntry{name: name, data: entries[name]})
		}
	}
	out := filepath.Join(t.TempDir(), "tampered.tar.gz")
	writeTarGz(t, out, list)
	return out
}

// assertUntouched fails if the target roots exist (these tests restore onto
// a fresh machine: any appearance means the refusal touched something) or if
// staging/safety debris was left beside them.
func assertUntouched(t *testing.T, paths config.Paths) {
	t.Helper()
	for _, root := range []string{paths.Config, paths.State} {
		if _, err := os.Stat(root); err == nil {
			t.Errorf("refused restore created %s", root)
		}
		matches, _ := filepath.Glob(root + ".*")
		if len(matches) > 0 {
			t.Errorf("refused restore left debris: %v", matches)
		}
	}
}

func requireRefusal(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatal("restore proceeded, want refusal")
	}
	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a RefusalError", err)
	}
	if !strings.Contains(refusal.Reason, wantSubstring) {
		t.Errorf("refusal reason %q does not name the problem (want %q)", refusal.Reason, wantSubstring)
	}
}

func TestRestoreRefusesNonGzip(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "not-an-archive.tar.gz")
	if err := os.WriteFile(archive, []byte("plain text, not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "not a gzip archive")
	assertUntouched(t, dst)
}

func TestRestoreRefusesTruncatedArchive(t *testing.T) {
	whole, err := os.ReadFile(makeArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "truncated.tar.gz")
	if err := os.WriteFile(archive, whole[:len(whole)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	dst := testPaths(t)
	_, err = Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "truncated or corrupt")
	assertUntouched(t, dst)
}

func TestRestoreRefusesMissingManifest(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "no-manifest.tar.gz")
	writeTarGz(t, archive, []tarEntry{{name: "state/memory.toml", data: []byte(seedMemory)}})
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "no manifest.json")
	assertUntouched(t, dst)
}

func TestRestoreRefusesUnreadableManifest(t *testing.T) {
	archive := mutateArchive(t, makeArchive(t), func(entries map[string][]byte) {
		entries[ManifestName] = []byte("{ not json")
	})
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "manifest is unreadable")
	assertUntouched(t, dst)
}

func TestRestoreRefusesNewerArchiveFormat(t *testing.T) {
	archive := mutateArchive(t, makeArchive(t), func(entries map[string][]byte) {
		entries[ManifestName] = bytes.Replace(entries[ManifestName],
			[]byte(`"format": 1`), []byte(`"format": 99`), 1)
	})
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "format 99 is newer")
	assertUntouched(t, dst)
}

func TestRestoreRefusesHashMismatch(t *testing.T) {
	archive := mutateArchive(t, makeArchive(t), func(entries map[string][]byte) {
		entries["state/memory.toml"] = append(entries["state/memory.toml"], []byte("\n# tampered\n")...)
	})
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "does not match its manifest hash")
	assertUntouched(t, dst)
}

func TestRestoreRefusesFileMissingFromManifest(t *testing.T) {
	archive := mutateArchive(t, makeArchive(t), func(entries map[string][]byte) {
		entries["state/smuggled.toml"] = []byte("version = 1\n")
	})
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "the manifest does not list it")
	assertUntouched(t, dst)
}

func TestRestoreRefusesManifestListingMissingFile(t *testing.T) {
	archive := mutateArchive(t, makeArchive(t), func(entries map[string][]byte) {
		delete(entries, "state/memory.toml")
	})
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "does not carry it")
	assertUntouched(t, dst)
}

func TestRestoreRefusesUnsafePaths(t *testing.T) {
	for name, entry := range map[string]tarEntry{
		"dotdot":   {name: "state/../../evil.toml", data: []byte("boo = 1\n")},
		"absolute": {name: "/etc/passwd", data: []byte("root::0:0\n")},
	} {
		t.Run(name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "unsafe.tar.gz")
			writeTarGz(t, archive, []tarEntry{
				{name: ManifestName, data: []byte(`{"format":1,"files":[]}`)},
				entry,
			})
			dst := testPaths(t)
			_, err := Restore(dst, archive, RestoreOptions{})
			requireRefusal(t, err, "unsafe path")
			assertUntouched(t, dst)
		})
	}
}

func TestRestoreRefusesSymlinkEntries(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "symlink.tar.gz")
	writeTarGz(t, archive, []tarEntry{
		{name: ManifestName, data: []byte(`{"format":1,"files":[]}`)},
		{name: "state/link.toml", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "unsupported type")
	assertUntouched(t, dst)
}

// A store schema newer than this build understands is caught by the load
// validation — the store's own loader speaks the refusal — before anything
// real moves.
func TestRestoreRefusesNewerStoreSchema(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	newer := strings.Replace(seedMemory, "version = 1", "version = 99", 1)
	if err := os.WriteFile(filepath.Join(src.State, "memory.toml"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(src, archive, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "version 99 is not supported")
	assertUntouched(t, dst)
}

// A corrupt store inside an otherwise-valid archive (hand-tampered, hashes
// updated) is still refused by the load validation: hashes prove transport,
// loading proves the content.
func TestRestoreRefusesUnloadableStore(t *testing.T) {
	src := testPaths(t)
	seedRoots(t, src)
	if err := os.WriteFile(filepath.Join(src.State, "memory.toml"),
		[]byte("version = 1\n[[fact\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(src, archive, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dst := testPaths(t)
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "would not load")
	assertUntouched(t, dst)
}

// Restoring under a running daemon is refused: the daemon's in-memory view
// would rewrite the restored files on its next save.
func TestRestoreRefusesWhileDaemonIsRunning(t *testing.T) {
	archive := makeArchive(t)
	dst := testPaths(t)
	srv := ipc.NewServer(dst.Socket, nil, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-served })

	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "jarvixd is running")
	assertUntouched(t, dst)
}

// A refusal must also leave EXISTING state alone — the restore-over variant
// of nothing-is-touched.
func TestRefusedRestoreLeavesExistingStateAlone(t *testing.T) {
	archive := mutateArchive(t, makeArchive(t), func(entries map[string][]byte) {
		entries["state/memory.toml"] = append(entries["state/memory.toml"], []byte("# tampered\n")...)
	})
	dst := testPaths(t)
	seedRoots(t, dst)
	before := map[string]map[string]string{
		dst.Config: treeSnapshot(t, dst.Config),
		dst.State:  treeSnapshot(t, dst.State),
	}
	_, err := Restore(dst, archive, RestoreOptions{})
	requireRefusal(t, err, "does not match its manifest hash")
	for root, want := range before {
		got := treeSnapshot(t, root)
		if len(got) != len(want) {
			t.Fatalf("%s changed shape under a refused restore", root)
		}
		for rel, content := range want {
			if got[rel] != content {
				t.Errorf("%s/%s changed under a refused restore", root, rel)
			}
		}
	}
	for _, root := range []string{dst.Config, dst.State} {
		matches, _ := filepath.Glob(root + ".*")
		if len(matches) > 0 {
			t.Errorf("refused restore left debris: %v", matches)
		}
	}
}

// The safety-copy names are timestamped so repeated restores never clobber
// an earlier safety copy's content.
func TestRepeatedRestoresKeepDistinctSafetyCopies(t *testing.T) {
	archive := makeArchive(t)
	dst := testPaths(t)
	seedRoots(t, dst)
	times := []time.Time{
		time.Date(2026, 8, 28, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 28, 4, 0, 0, 0, time.UTC),
	}
	var copies []string
	for _, at := range times {
		report, err := Restore(dst, archive, RestoreOptions{Now: func() time.Time { return at }})
		if err != nil {
			t.Fatal(err)
		}
		copies = append(copies, report.SafetyCopies...)
	}
	seen := map[string]bool{}
	for _, c := range copies {
		if seen[c] {
			t.Errorf("safety copy name %s reused", c)
		}
		seen[c] = true
		if _, err := os.Stat(c); err != nil {
			t.Errorf("safety copy %s missing: %v", c, err)
		}
	}
}
