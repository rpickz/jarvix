package backup

// Restore: validate everything, then swap directories — in that order,
// always. The archive is extracted into staging siblings of the real roots,
// every hash is checked against the manifest, and every store the staged
// tree holds is proven loadable, all before a single existing file moves.
// The swap itself is two renames per root: the existing root steps aside
// into a timestamped safety copy (never deleted — restore-over must be
// reversible by hand with one mv), and the staged root takes its place.
// There is no rm-then-copy anywhere on this path.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/managed"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/monitors"
	"github.com/rpickz/jarvix/internal/undo"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// RefusalError is a restore that declined to proceed: the archive failed
// validation, or the machine is not in a restorable state. Nothing under
// the real roots was touched.
type RefusalError struct {
	Reason string
}

// Error implements the error interface.
func (e *RefusalError) Error() string { return "restore refused: " + e.Reason }

// refuse builds a RefusalError.
func refuse(format string, args ...any) error {
	return &RefusalError{Reason: fmt.Sprintf(format, args...)}
}

// RestoreOptions shape one restore run.
type RestoreOptions struct {
	// Now is the clock, injectable so tests control the safety-copy names.
	Now func() time.Time
}

// RestoreReport says what one restore run did.
type RestoreReport struct {
	// Files counts restored files.
	Files int
	// SafetyCopies names where the pre-restore roots were moved — one per
	// root that existed. Empty on a fresh machine.
	SafetyCopies []string
	// RedactedKeys names config keys the archive carries placeholders for
	// (--no-secrets at backup time); the user must re-enter these.
	RedactedKeys []string
	// Warnings are non-fatal observations (an unreadable conversation the
	// archive faithfully preserved, a symlink that was skipped at backup).
	Warnings []string
}

// Restore replaces the config and state roots with an archive's contents.
// It refuses — touching nothing — when the daemon is running, the archive
// is corrupt or truncated, any hash disagrees with the manifest, the
// archive's format or any store's schema is newer than this build
// understands, or any staged store fails to load.
func Restore(paths config.Paths, archivePath string, opts RestoreOptions) (*RestoreReport, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	// A running daemon holds the stores in memory and would rewrite the
	// restored files with its own view on the next save — silently undoing
	// the restore. Refuse while the socket answers.
	if client, err := ipc.Dial(paths.Socket); err == nil {
		_ = client.Close()
		return nil, refuse("jarvixd is running — stop it first (systemctl --user stop jarvixd)")
	}

	manifest, files, err := readArchive(archivePath)
	if err != nil {
		return nil, err
	}
	report := &RestoreReport{RedactedKeys: manifest.RedactedKeys}

	// Stage into siblings of the real roots: same filesystem, so the final
	// swap is two atomic renames — and a crash mid-restore leaves staging
	// debris beside intact roots, never a half-replaced root.
	ts := now().UTC().Format("20060102-150405")
	stageConfig := paths.Config + ".restore-stage-" + ts
	stageState := paths.State + ".restore-stage-" + ts
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(stageConfig)
			_ = os.RemoveAll(stageState)
		}
	}()
	for _, dir := range []string{stageConfig, stageState} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create staging dir: %w", err)
		}
	}
	stageFor := func(arcPath string) string {
		rel := strings.SplitN(arcPath, "/", 2)[1]
		if strings.HasPrefix(arcPath, prefixConfig+"/") {
			return filepath.Join(stageConfig, filepath.FromSlash(rel))
		}
		return filepath.Join(stageState, filepath.FromSlash(rel))
	}
	paths2 := make([]string, 0, len(files))
	for p := range files {
		paths2 = append(paths2, p)
	}
	sort.Strings(paths2)
	for _, arcPath := range paths2 {
		f := files[arcPath]
		target := stageFor(arcPath)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("stage %s: %w", arcPath, err)
		}
		mode := os.FileMode(f.mode).Perm()
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(target, f.data, mode); err != nil {
			return nil, fmt.Errorf("stage %s: %w", arcPath, err)
		}
		report.Files++
	}

	// The load validation: prove the staged tree is one Jarvix can boot
	// from, before anything real moves. This is also where a newer-schema
	// archive is caught — every store loader refuses versions it does not
	// know, with the version in the message.
	if problems := validateStagedRoots(stageConfig, stageState, report); len(problems) > 0 {
		return nil, refuse("the archive's contents would not load:\n  %s",
			strings.Join(problems, "\n  "))
	}

	// The swap. Existing roots step aside — never deleted — and the staged
	// roots take their places.
	swaps := []rootSwap{{real: paths.Config, stage: stageConfig}, {real: paths.State, stage: stageState}}
	var done []rootSwap // for rollback
	var safety []string
	for _, sw := range swaps {
		if err := os.MkdirAll(filepath.Dir(sw.real), 0o755); err != nil {
			return nil, fmt.Errorf("create parent of %s: %w", sw.real, err)
		}
		aside := ""
		if _, err := os.Stat(sw.real); err == nil {
			aside = sw.real + ".pre-restore-" + ts
			if err := os.Rename(sw.real, aside); err != nil {
				rollback(done)
				return nil, fmt.Errorf("move existing %s aside: %w", sw.real, err)
			}
		}
		if err := os.Rename(sw.stage, sw.real); err != nil {
			if aside != "" {
				_ = os.Rename(aside, sw.real)
			}
			rollback(done)
			return nil, fmt.Errorf("swap %s into place: %w", sw.real, err)
		}
		done = append(done, rootSwap{real: sw.real, aside: aside})
		if aside != "" {
			safety = append(safety, aside)
		}
	}
	staged = true
	report.SafetyCopies = safety

	// Prove the result where it now lives — the same checks, against the
	// real roots. They passed seconds ago on the same bytes; failing here
	// means the disk itself misbehaved, and the safety copies still exist.
	if problems := validateStagedRoots(paths.Config, paths.State, &RestoreReport{}); len(problems) > 0 {
		return report, fmt.Errorf("restore swapped but the result does not load (safety copies intact: %s):\n  %s",
			strings.Join(safety, ", "), strings.Join(problems, "\n  "))
	}
	return report, nil
}

// rootSwap is one root mid-replacement: the live path, the staged tree
// destined for it, and (once moved) where the pre-restore content went.
type rootSwap struct {
	real, stage, aside string
}

// rollback undoes completed swaps, best-effort, newest first: the staged
// content steps back out and the pre-restore root returns.
func rollback(done []rootSwap) {
	for i := len(done) - 1; i >= 0; i-- {
		sw := done[i]
		_ = os.Rename(sw.real, sw.real+".restore-failed")
		if sw.aside != "" {
			_ = os.Rename(sw.aside, sw.real)
		}
	}
}

// archiveFile is one extracted entry, held in memory until validation ends.
type archiveFile struct {
	data []byte
	mode int64
}

// readArchive reads and fully validates an archive: gzip and tar intact, a
// parseable manifest of a format this build knows, every path safe and
// root-relative, and the manifest and contents agreeing exactly — every
// file listed, every hash matching, nothing extra in either direction.
func readArchive(archivePath string) (*Manifest, map[string]archiveFile, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, refuse("%s is not a gzip archive: %v", archivePath, err)
	}
	defer func() { _ = gz.Close() }()

	files := map[string]archiveFile{}
	var manifest *Manifest
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, refuse("archive is truncated or corrupt: %v", err)
		}
		name := filepath.ToSlash(hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir:
			continue
		default:
			// Symlinks and devices have no business in a Jarvix backup;
			// extracting one could write outside the staging dirs.
			return nil, nil, refuse("archive entry %q has unsupported type", name)
		}
		if filepath.IsAbs(name) || !filepath.IsLocal(filepath.FromSlash(name)) {
			return nil, nil, refuse("archive entry %q has an unsafe path", name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, refuse("archive is truncated or corrupt: %v", err)
		}
		if name == ManifestName {
			var m Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, nil, refuse("manifest is unreadable: %v", err)
			}
			manifest = &m
			continue
		}
		if !strings.HasPrefix(name, prefixConfig+"/") && !strings.HasPrefix(name, prefixState+"/") {
			return nil, nil, refuse("archive entry %q is outside the config and state roots", name)
		}
		files[name] = archiveFile{data: data, mode: hdr.Mode}
	}
	if manifest == nil {
		return nil, nil, refuse("archive carries no %s — not a Jarvix backup, or an incomplete one", ManifestName)
	}
	if manifest.Format > ManifestFormat {
		return nil, nil, refuse("archive format %d is newer than this jarvix understands (%d) — upgrade jarvix, then restore",
			manifest.Format, ManifestFormat)
	}

	// Manifest and contents must agree exactly, both directions.
	listed := map[string]File{}
	for _, mf := range manifest.Files {
		listed[mf.Path] = mf
	}
	for path, af := range files {
		mf, ok := listed[path]
		if !ok {
			return nil, nil, refuse("archive carries %q but the manifest does not list it", path)
		}
		sum := sha256.Sum256(af.data)
		if got := hex.EncodeToString(sum[:]); got != mf.SHA256 {
			return nil, nil, refuse("%s does not match its manifest hash — the archive is corrupt", path)
		}
		if int64(len(af.data)) != mf.Size {
			return nil, nil, refuse("%s is %d bytes but the manifest says %d — the archive is corrupt",
				path, len(af.data), mf.Size)
		}
	}
	for path := range listed {
		if _, ok := files[path]; !ok {
			return nil, nil, refuse("the manifest lists %q but the archive does not carry it", path)
		}
	}
	return manifest, files, nil
}

// validateStagedRoots proves a config+state tree is one Jarvix can boot
// from. Known stores get their own loaders — the exact code the daemon will
// run — which is what catches a newer store schema with its documented
// message. Everything else is held to well-formedness wholesale (.toml
// parses, .json parses), so a store this build has never heard of restores
// verbatim without being guessed at.
func validateStagedRoots(configRoot, stateRoot string, report *RestoreReport) []string {
	var problems []string
	check := func(label string, err error) {
		if err != nil {
			problems = append(problems, label+": "+err.Error())
		}
	}
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	if p := filepath.Join(configRoot, "config.toml"); exists(p) {
		_, err := config.Load(p)
		check("config.toml", err)
	}
	deep := map[string]func(string) error{
		filepath.Join(stateRoot, "memory.toml"):     memory.ValidateFile,
		filepath.Join(stateRoot, "vocabulary.toml"): vocabulary.ValidateFile,
		filepath.Join(stateRoot, "focus.toml"):      focus.ValidateFile,
		filepath.Join(stateRoot, "monitors.toml"):   monitors.ValidateFile,
		filepath.Join(stateRoot, "managed.toml"):    managed.ValidateFile,
		filepath.Join(stateRoot, "undo.toml"):       undo.ValidateFile,
		filepath.Join(stateRoot, "history.json"): func(p string) error {
			_, _, err := (&history.File{Path: p}).Load()
			return err
		},
	}
	deepPaths := make([]string, 0, len(deep))
	for p := range deep {
		deepPaths = append(deepPaths, p)
	}
	sort.Strings(deepPaths)
	for _, p := range deepPaths {
		if exists(p) {
			check(filepath.Base(p), deep[p](p))
		}
	}
	if dir := filepath.Join(stateRoot, "conversations"); exists(dir) {
		_, unreadable, err := (&conversations.FileStore{Dir: dir}).List()
		check("conversations", err)
		for _, u := range unreadable {
			// Faithfully archived damage is restored faithfully: the backup
			// promise is "what you had", not "better than you had".
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("conversation %s was already unreadable when this archive was made", u.ID))
		}
	}

	// Wholesale well-formedness for everything not deep-checked above.
	for _, root := range []string{configRoot, stateRoot} {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if _, deepChecked := deep[path]; deepChecked ||
				path == filepath.Join(configRoot, "config.toml") ||
				strings.HasPrefix(path, filepath.Join(stateRoot, "conversations")+string(os.PathSeparator)) {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				check(d.Name(), readErr)
				return nil
			}
			switch {
			case strings.HasSuffix(path, ".toml"):
				var doc map[string]any
				_, tomlErr := toml.Decode(string(data), &doc)
				check(d.Name(), tomlErr)
			case strings.HasSuffix(path, ".json"):
				var doc any
				check(d.Name(), json.Unmarshal(data, &doc))
			}
			return nil
		})
	}
	return problems
}
