// Package backup archives and restores the assistant's memory of its user
// (ADR 0045): everything under the config and state roots — remembered
// facts, taught vocabulary, focus threads, conversations, routines,
// settings — as one tar.gz with a manifest of hashes.
//
// Discovery is wholesale on purpose: the two roots are walked, never an
// enumerated store list. A store this package has never heard of — next
// month's feature — is archived and restored verbatim, because the roots are
// the contract (config.Paths puts every Jarvix file under them) and a list
// here would rot the first time a wave added a store (the wave before this
// one added three).
//
// Consistency comes from the daemon when one is running: Create asks it to
// hold the state write barrier (the state.hold verb, internal/statehold),
// reads every file under the hold, and releases before compressing — the
// hold lasts exactly as long as the reads. A stopped daemon is backed up
// directly; every store writes via atomic rename, so each file is complete
// either way, and the hold's only job is coherence *across* files.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/build"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// Archive geometry. The two top-level directories mirror the two roots, so
// an archive opened by hand five years from now explains itself.
const (
	// ManifestName is the archive's self-description entry.
	ManifestName = "manifest.json"
	// ManifestFormat is bumped when the archive layout changes
	// incompatibly; restore refuses formats newer than it knows.
	ManifestFormat = 1
	// prefixConfig and prefixState are the archive paths of the two roots.
	prefixConfig = "config"
	prefixState  = "state"
)

// Capture values: how consistency was obtained.
const (
	// CaptureHeld means a running daemon held its write barrier while the
	// files were read.
	CaptureHeld = "daemon-held"
	// CaptureDirect means no daemon was running and the files were read
	// directly.
	CaptureDirect = "direct"
)

// RedactedPlaceholder replaces api key values under --no-secrets. Restore
// recognises archives carrying it by the manifest's redacted flag and warns
// which keys need re-entering.
const RedactedPlaceholder = "JARVIX-REDACTED"

// defaultHoldTTL is requested from the daemon's state.hold. Generous — the
// reads are milliseconds and Create releases explicitly; the TTL only
// matters if this process dies mid-copy.
const defaultHoldTTL = 30 * time.Second

// Manifest is the archive's self-description: what Jarvix wrote it, what it
// holds, and the hash of every file — the restore contract.
type Manifest struct {
	Format        int       `json:"format"`
	JarvixVersion string    `json:"jarvix_version"`
	Created       time.Time `json:"created"`
	// Capture records how consistency was obtained (CaptureHeld/CaptureDirect).
	Capture string `json:"capture"`
	// Redacted marks an archive written with --no-secrets; RedactedKeys
	// names the config keys (table-qualified) whose values were replaced.
	Redacted     bool     `json:"redacted,omitempty"`
	RedactedKeys []string `json:"redacted_keys,omitempty"`
	// Files pins the archive's exact contents. Restore refuses an archive
	// whose entries and manifest disagree in either direction — and the
	// list doubles as the "nothing outside jarvix's own dirs" pin: every
	// path is root-relative under config/ or state/.
	Files []File `json:"files"`
	// Schemas records the top-level `version` marker of every store file
	// that carries one, discovered wholesale (any .toml/.json with an
	// integer version field). Informational: the load validation on restore
	// is what actually enforces schema compatibility.
	Schemas map[string]int `json:"schemas,omitempty"`
}

// File is one archived file: its root-relative path (config/... or
// state/...), size, permission bits, and content hash.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

// CreateOptions shape one backup run.
type CreateOptions struct {
	// NoSecrets redacts api key values in the archived config.toml.
	NoSecrets bool
	// Now is the clock, injectable so tests control every timestamp.
	Now func() time.Time
}

// CreateReport says what one backup run did.
type CreateReport struct {
	// Path is the archive written.
	Path string
	// Capture records how consistency was obtained.
	Capture string
	// Files counts archived files.
	Files int
	// RedactedKeys names the config keys redacted under NoSecrets.
	RedactedKeys []string
	// SkippedSymlinks lists symlinks found under the roots and left behind:
	// following one could pull in files outside Jarvix's dirs, which the
	// archive must never contain.
	SkippedSymlinks []string
}

// DefaultArchiveName returns the dated archive filename for t.
func DefaultArchiveName(t time.Time) string {
	return "jarvix-backup-" + t.Format("20060102-150405") + ".tar.gz"
}

// ResolveDest turns the CLI's optional path argument into the archive file
// to write: empty means a dated name in the current directory, an existing
// directory means a dated name inside it, anything else is the file itself.
func ResolveDest(arg string, now time.Time) string {
	if arg == "" {
		return DefaultArchiveName(now)
	}
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return filepath.Join(arg, DefaultArchiveName(now))
	}
	return arg
}

// Create writes one archive of the config and state roots to dest and
// returns what it did. With a running daemon the state write barrier is held
// for the duration of the file reads; with none the files are read directly.
func Create(paths config.Paths, dest string, opts CreateOptions) (*CreateReport, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	report := &CreateReport{Path: dest}

	// Hold first, walk second: a file appearing mid-walk must not slip
	// between the coherence promise and the copy.
	release, capture, err := holdDaemon(paths.Socket)
	if err != nil {
		return nil, err
	}
	defer release()
	report.Capture = capture

	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dest, err)
	}
	roots := []struct{ prefix, dir string }{
		{prefixConfig, paths.Config},
		{prefixState, paths.State},
	}
	manifest := Manifest{
		Format:        ManifestFormat,
		JarvixVersion: build.Version,
		Created:       now().UTC(),
		Capture:       capture,
		Schemas:       map[string]int{},
	}
	type entry struct {
		file File
		data []byte
	}
	var entries []entry
	for _, root := range roots {
		err := filepath.WalkDir(root.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root.dir && os.IsNotExist(err) {
					return filepath.SkipAll // a fresh machine has no state yet
				}
				return err
			}
			name := d.Name()
			if d.IsDir() {
				return nil
			}
			// Dot-prefixed names are the atomic-write scratch files every
			// store creates beside its target (.memory-*.tmp and kin) — a
			// mid-rename leftover, never user data. Jarvix writes no real
			// dotfiles under its roots.
			if strings.HasPrefix(name, ".") {
				return nil
			}
			if !d.Type().IsRegular() {
				// Symlinks (and stranger things) are recorded and left
				// behind: following one could reach outside the roots, and
				// this archive must contain nothing but Jarvix's own files.
				report.SkippedSymlinks = append(report.SkippedSymlinks, path)
				return nil
			}
			if abs, err := filepath.Abs(path); err == nil && abs == destAbs {
				return nil // never archive the archive being written
			}
			rel, err := filepath.Rel(root.dir, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			arcPath := filepath.ToSlash(filepath.Join(root.prefix, rel))
			if opts.NoSecrets && arcPath == prefixConfig+"/config.toml" {
				var keys []string
				data, keys = redactAPIKeys(data)
				report.RedactedKeys = keys
				manifest.Redacted = true
				manifest.RedactedKeys = keys
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			f := File{
				Path:   arcPath,
				Size:   int64(len(data)),
				Mode:   uint32(info.Mode().Perm()),
				SHA256: hex.EncodeToString(sum[:]),
			}
			if v, ok := schemaVersion(arcPath, data); ok {
				manifest.Schemas[arcPath] = v
			}
			entries = append(entries, entry{file: f, data: data})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// The reads are done; the daemon may write again while we compress.
	release()

	sort.Slice(entries, func(i, j int) bool { return entries[i].file.Path < entries[j].file.Path })
	for _, e := range entries {
		manifest.Files = append(manifest.Files, e.file)
	}
	report.Files = len(entries)

	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}

	// Written beside the destination and renamed into place: a backup killed
	// mid-write must never leave a plausible-looking corrupt archive where
	// cron will happily rotate real ones away around it.
	if dir := filepath.Dir(destAbs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create archive dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(destAbs), ".jarvix-backup-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	writeOne := func(name string, mode int64, data []byte) error {
		hdr := &tar.Header{
			Name:    name,
			Mode:    mode,
			Size:    int64(len(data)),
			ModTime: manifest.Created,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := writeOne(ManifestName, 0o600, manifestJSON); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	for _, e := range entries {
		if err := writeOne(e.file.Path, int64(e.file.Mode), e.data); err != nil {
			return nil, fmt.Errorf("write archive: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	// The state dirs are 0700 but the archive lands wherever the user (or
	// their cron line) pointed; it carries their conversations, so it gets
	// the stores' own privacy.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return nil, fmt.Errorf("secure archive: %w", err)
	}
	if err := os.Rename(tmp.Name(), destAbs); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	return report, nil
}

// holdDaemon asks a running daemon to hold the state write barrier. No
// daemon means direct capture (every file is still individually complete —
// atomic renames — there is just nobody writing). A daemon that answers the
// socket but cannot hold is an error, not a shrug: backing up under live
// writes would silently downgrade the consistency this feature promises.
func holdDaemon(socket string) (release func(), capture string, err error) {
	client, dialErr := ipc.Dial(socket)
	if dialErr != nil {
		return func() {}, CaptureDirect, nil
	}
	var held struct {
		Held  bool  `json:"held"`
		TTLMs int64 `json:"ttl_ms"`
	}
	params := map[string]any{"ttl_ms": defaultHoldTTL.Milliseconds()}
	if err := client.Call("state.hold", params, &held); err != nil {
		_ = client.Close()
		return nil, "", fmt.Errorf("the daemon is running but could not hold state writes: %w", err)
	}
	var released bool
	return func() {
		if released {
			return
		}
		released = true
		_ = client.Call("state.release", nil, nil)
		_ = client.Close()
	}, CaptureHeld, nil
}

// apiKeyLine matches an api_key assignment; everything after the `=` is the
// secret and is replaced whole, whatever quoting it used.
var apiKeyLine = regexp.MustCompile(`^(\s*)api_key(\s*)=\s*(\S.*?)\s*$`)

// tomlTable matches a [table] or [[table]] header, for qualifying which
// api_key was redacted.
var tomlTable = regexp.MustCompile(`^\s*\[+\s*([^\]\s]+)\s*\]+`)

// redactAPIKeys replaces every api_key value in a config document with the
// placeholder and returns which (table-qualified) keys were touched. Line
// based on purpose: the file is rewritten byte-identically except for the
// secrets, so a restored config diffs cleanly against the original.
func redactAPIKeys(data []byte) ([]byte, []string) {
	lines := strings.Split(string(data), "\n")
	table := ""
	var keys []string
	for i, line := range lines {
		if m := tomlTable.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		m := apiKeyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := strings.Trim(m[3], `"'`)
		if value == "" || value == RedactedPlaceholder {
			continue
		}
		lines[i] = m[1] + "api_key" + m[2] + `= "` + RedactedPlaceholder + `"`
		key := "api_key"
		if table != "" {
			key = table + ".api_key"
		}
		keys = append(keys, key)
	}
	return []byte(strings.Join(lines, "\n")), keys
}

// schemaVersion extracts a top-level integer `version` marker from a store
// file, wholesale: any .toml or .json that carries one is recorded, whether
// or not this build of Jarvix knows the store.
func schemaVersion(arcPath string, data []byte) (int, bool) {
	switch {
	case strings.HasSuffix(arcPath, ".toml"):
		// The one line every store writes first; a full TOML parse here
		// would make the manifest opinionated about files it only carries.
		for _, line := range strings.Split(string(data), "\n") {
			var v int
			if n, err := fmt.Sscanf(strings.TrimSpace(line), "version = %d", &v); n == 1 && err == nil {
				return v, true
			}
		}
	case strings.HasSuffix(arcPath, ".json"):
		var doc struct {
			Version *int `json:"version"`
		}
		if err := json.Unmarshal(data, &doc); err == nil && doc.Version != nil {
			return *doc.Version, true
		}
	}
	return 0, false
}
