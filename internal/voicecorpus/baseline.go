package voicecorpus

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
)

// baselineTOML is the committed baseline, embedded for the same reason the
// manifest is: so `jarvix doctor` can report the corpus's state from a binary
// that has no source tree behind it.
//
//go:embed baseline.toml
var baselineTOML string

// BaselineFile is the baseline's name inside this package's directory. The
// harness writes it in place when explicitly asked to.
const BaselineFile = "baseline.toml"

// ScoreTolerance is how far a phrase's score may fall before the drop counts
// as a regression.
//
// Not zero, and not because scores are noisy — whisper.cpp decodes the same
// file the same way twice, so a rerun on unchanged inputs reproduces its
// numbers exactly. The tolerance is for the changes that legitimately move
// them a little: a whisper.cpp release, a model file swapped for a bigger one,
// a taught word added to the bias. Five points of recall is roughly one word
// in a twenty-word phrase — small enough that a bias regression worth catching
// still trips it, wide enough that upgrading whisper does not turn the whole
// corpus red for a reason nobody wants to read about.
const ScoreTolerance = 0.05

// BaselineEntry is one recording's last agreed outcome.
type BaselineEntry struct {
	// ID is the recording id (the file stem), so the two takes of a starred
	// phrase are tracked apart.
	ID string `toml:"id"`
	// Pass is whether that recording met its expectations when the baseline
	// was taken.
	//
	// A false here is not a bug in the baseline: some phrases in this corpus
	// are meant to be hard — a deliberate mispronunciation, the fastest and
	// most slurred register anyone actually speaks in — and the honest record
	// of "this one does not work today" is worth more than an aspiration. It
	// keeps the run green for a known weakness while still failing the moment
	// something that DID work stops working.
	Pass bool `toml:"pass"`
	// Score is Score() at that moment; see score.go.
	Score float64 `toml:"score"`
}

// Baseline is the committed record of what the pipeline currently manages,
// together with enough about the conditions to explain a change.
type Baseline struct {
	// Updated is when it was last written, as a date. A date rather than a
	// timestamp: the file is committed, and the useful question is which
	// change it belongs to, which git already answers precisely.
	Updated string `toml:"updated"`
	// Model is the whisper model file the recordings were transcribed with,
	// by name. A model swap explains a whole-corpus shift at a glance.
	Model string `toml:"model"`
	// PromptHash identifies the bias prompt in force when the baseline was
	// taken — a hash rather than the prompt itself, deliberately. The prompt
	// carries the user's taught vocabulary, which is personal in exactly the
	// way the recordings are, and a committed file has no business holding a
	// list of the words somebody has trouble being understood saying. The
	// hash still answers the only question the baseline needs to ask: is the
	// bias the same as it was?
	PromptHash string `toml:"prompt_hash"`
	// Entries are the per-recording outcomes, sorted by id.
	Entries []BaselineEntry `toml:"entry"`
}

// PromptFingerprint reduces a bias prompt to the short hash a baseline stores.
func PromptFingerprint(prompt string) string {
	if prompt == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:6])
}

// CommittedBaseline returns the baseline compiled into this binary.
func CommittedBaseline() (Baseline, error) {
	return ParseBaseline(baselineTOML)
}

// ParseBaseline reads a baseline document.
func ParseBaseline(document string) (Baseline, error) {
	var b Baseline
	if _, err := toml.Decode(document, &b); err != nil {
		return Baseline{}, fmt.Errorf("voice corpus baseline: %w", err)
	}
	seen := make(map[string]bool, len(b.Entries))
	for _, e := range b.Entries {
		if e.ID == "" {
			return Baseline{}, fmt.Errorf("voice corpus baseline: an entry has no id")
		}
		if seen[e.ID] {
			return Baseline{}, fmt.Errorf("voice corpus baseline: two entries for %q", e.ID)
		}
		seen[e.ID] = true
	}
	return b, nil
}

// Entry finds one recording's baseline.
func (b Baseline) Entry(id string) (BaselineEntry, bool) {
	for _, e := range b.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return BaselineEntry{}, false
}

// Passing counts the recordings the baseline records as working.
func (b Baseline) Passing() (passing, total int) {
	for _, e := range b.Entries {
		if e.Pass {
			passing++
		}
	}
	return passing, len(b.Entries)
}

// Finding is one difference between a run and the committed baseline.
type Finding struct {
	// ID is the recording the finding is about, empty for a finding about the
	// run as a whole.
	ID string
	// Message says what changed, in the terms the reader has to act in.
	Message string
	// Regression marks the findings that must fail the run. Everything else
	// is context: an improvement, a changed bias prompt, a recording that has
	// been removed. Those are worth printing and worth NOT failing on —
	// a harness that goes red when something gets better is a harness people
	// stop running.
	Regression bool
}

// CompareToBaseline reports every difference between a run's results and the
// committed baseline.
//
// The contract, stated as a sentence: nothing that used to work may stop
// working, and nothing may run unbaselined. The first half is what catches a
// bias or alias regression; the second is what stops a freshly dropped-in
// recording from being counted as proof of anything before a human has looked
// at it once. Both are only ever resolved by a person passing the update flag,
// never by the harness deciding for itself.
func CompareToBaseline(b Baseline, results []Result, promptHash string) []Finding {
	var findings []Finding
	if b.PromptHash != "" && promptHash != "" && b.PromptHash != promptHash {
		findings = append(findings, Finding{Message: fmt.Sprintf(
			"the bias prompt has changed since the baseline was taken (%s → %s); "+
				"if recognition moved, this is the first thing to look at",
			b.PromptHash, promptHash)})
	}
	ran := make(map[string]bool, len(results))
	for _, r := range results {
		id := r.Recording.ID
		ran[id] = true
		prior, ok := b.Entry(id)
		if !ok {
			findings = append(findings, Finding{ID: id, Regression: true, Message: fmt.Sprintf(
				"%s is not in the baseline (it scored %.2f and %s); "+
					"review it and record it with -voicecorpus.update-baseline",
				id, r.Score, passed(r.Pass()))})
			continue
		}
		if prior.Pass && !r.Pass() {
			findings = append(findings, Finding{ID: id, Regression: true, Message: fmt.Sprintf(
				"%s used to pass and now fails", id)})
		}
		if !prior.Pass && r.Pass() {
			findings = append(findings, Finding{ID: id, Message: fmt.Sprintf(
				"%s now passes, where the baseline records it failing — worth recording", id)})
		}
		// The epsilon is float arithmetic, not judgement: 1.0 - 0.95 is
		// 0.050000000000000044 in binary floating point, so a drop of exactly
		// the tolerance would otherwise be reported as exceeding it, and the
		// documented boundary would be off by one representation error.
		if drop := prior.Score - r.Score; drop > ScoreTolerance+1e-9 {
			findings = append(findings, Finding{ID: id, Regression: true, Message: fmt.Sprintf(
				"%s recognised %.2f of its words, down from %.2f in the baseline (tolerance %.2f)",
				id, r.Score, prior.Score, ScoreTolerance)})
		}
	}
	for _, e := range b.Entries {
		if !ran[e.ID] {
			findings = append(findings, Finding{ID: e.ID, Message: fmt.Sprintf(
				"%s is in the baseline but was not recorded in this run", e.ID)})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	return findings
}

// Regressions filters findings down to the ones that must fail a run.
func Regressions(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Regression {
			out = append(out, f)
		}
	}
	return out
}

// passed renders a boolean outcome for a message.
func passed(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}

// NewBaseline builds the baseline a run would record.
func NewBaseline(results []Result, model, promptHash string, now time.Time) Baseline {
	b := Baseline{
		Updated:    now.Format(time.DateOnly),
		Model:      filepath.Base(model),
		PromptHash: promptHash,
	}
	for _, r := range results {
		b.Entries = append(b.Entries, BaselineEntry{
			ID: r.Recording.ID, Pass: r.Pass(),
			// Two decimals: the file is read by people in diffs, and a score
			// printed to fifteen digits makes every rerun look like a change.
			Score: math.Round(r.Score*100) / 100,
		})
	}
	sort.Slice(b.Entries, func(i, j int) bool { return b.Entries[i].ID < b.Entries[j].ID })
	return b
}

// baselineHeader is the comment written above every generated baseline. It is
// there because the file is committed and will be read, in a diff, by someone
// deciding whether a number moving is fine.
const baselineHeader = `# The voice corpus baseline: what the speech pipeline manages today.
#
# GENERATED. Written only by an explicit
#
#     go test -tags voicecorpus ./internal/voicecorpus -voicecorpus.update-baseline
#
# and never by an ordinary run — a baseline that updated itself would agree
# with every regression it was supposed to catch.
#
# pass = false is not a defect in the file. Some phrases in this corpus are
# meant to be hard, and recording that they do not work today is what keeps the
# run honest about the ones that do.
#
# See docs/voice-corpus.md.
`

// WriteBaseline writes the baseline to path, header and all.
func WriteBaseline(path string, b Baseline) error {
	var buf bytes.Buffer
	buf.WriteString(baselineHeader)
	if err := toml.NewEncoder(&buf).Encode(b); err != nil {
		return fmt.Errorf("encode the voice corpus baseline: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
