//go:build voicecorpus

// This is the half of the corpus that needs a whisper install and the user's
// own voice, so it is behind a build tag and out of the CI gate by
// construction rather than by a skip somebody has to remember to trust. See
// doc.go for why the tag rather than an environment check, and
// docs/voice-corpus.md for how to record.
//
//	go test -tags voicecorpus ./internal/voicecorpus -v
//	go test -tags voicecorpus ./internal/voicecorpus -v -voicecorpus.update-baseline
//
// Everything the run decides — how a phrase is scored, what counts as a
// regression, what an empty or broken corpus means — lives in the untagged
// files beside this one and is tested there without an engine. What is here is
// only the wiring: read the live configuration, build the real transcriber,
// run each file through it, and hand the results to code that has been tested
// on its own.

package voicecorpus

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
)

// updateBaseline is the explicit, human-typed flag that lets a run rewrite the
// committed baseline. It is a flag and not an environment variable, and not a
// "write it if it is missing" convenience, because the baseline's only value
// is that a person agreed to it: anything that updated itself would agree with
// every regression it exists to catch.
var updateBaseline = flag.Bool("voicecorpus.update-baseline", false,
	"rewrite "+BaselineFile+" from this run's results (review the diff before committing it)")

// perFileTimeout bounds one transcription. Generous — a cold whisper-cli on a
// large model against a ten-second clip is seconds, not tens of them — but
// present, because a whisper that has wedged should fail the run with a named
// file rather than sit until the test binary's own timeout kills everything
// with no idea which recording it was on.
const perFileTimeout = 2 * time.Minute

func TestVoiceCorpus(t *testing.T) {
	manifest, err := Phrases()
	if err != nil {
		t.Fatalf("phrase manifest: %v", err)
	}
	dir, explicit, err := ResolveDir()
	if err != nil {
		t.Fatalf("%v", err)
	}
	corpus, err := Load(dir, manifest)
	if err != nil {
		// Loud, always. A corpus directory that exists and holds something
		// unreadable is not a reason to skip: it is somebody's recording
		// session that did not produce what they think it did.
		t.Fatalf("%v", err)
	}
	if corpus.Empty() {
		where := dir + " (set " + DirEnv + " to read them from elsewhere)"
		if explicit {
			where = dir + " (from " + DirEnv + ")"
		}
		t.Skipf("no recordings in %s — %d phrases are waiting in phrases.toml; "+
			"see docs/voice-corpus.md. Until they exist, nothing in this repository proves that "+
			"real speech reaches the intent router as anything in particular.",
			where, len(manifest.Phrases))
	}

	cfg, paths := loadLiveConfig(t)
	rig, err := BuildRig(cfg, paths)
	if err != nil {
		t.Fatalf("build the live rig: %v", err)
	}
	transcriber := coldWhisper(t, cfg, paths, rig)
	prompt := rig.BiasPrompt()
	t.Logf("bias prompt in force: %q", prompt)
	t.Logf("assistant %q, aliases %v", rig.WakeWord, rig.WakeAliases)

	results := make([]Result, 0, len(corpus.Recordings))
	for _, rec := range corpus.Recordings {
		results = append(results, transcribeOne(t, transcriber, rec, rig))
	}

	baseline, err := CommittedBaseline()
	if err != nil {
		t.Fatalf("%v", err)
	}
	findings := CompareToBaseline(baseline, results, PromptFingerprint(prompt))
	t.Log("\n" + Render(corpus, results, findings))

	if *updateBaseline {
		path := filepath.Join(".", BaselineFile)
		// The resolved model FILE, not the short name from configuration: a
		// baseline read six months later has to say which weights produced
		// it, and "base.en" is a setting whose meaning can move.
		if err := WriteBaseline(path, NewBaseline(results, transcriber.ModelPath, PromptFingerprint(prompt), time.Now())); err != nil {
			t.Fatalf("%v", err)
		}
		t.Logf("wrote %s from this run — read the diff before committing it", path)
		return
	}

	for _, f := range Regressions(findings) {
		t.Errorf("%s", f.Message)
	}
	for _, r := range results {
		for _, f := range r.Failures {
			t.Errorf("%s: %s", r.Recording.ID, f)
		}
	}
}

// loadLiveConfig reads the configuration this machine's daemon runs on.
//
// The live file, not a default: the corpus's whole claim is that it tests the
// pipeline as deployed, and the deployed pipeline has the user's terminal,
// their routines and scripts (whose trigger phrases join the intent grammar),
// their assistant name and their taught vocabulary in it.
func loadLiveConfig(t *testing.T) (config.Config, config.Paths) {
	t.Helper()
	paths := config.DefaultPaths()
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		t.Fatalf("load %s: %v", paths.ConfigFile(), err)
	}
	return cfg, paths
}

// coldWhisper builds the real cold whisper-cli adapter — the same struct
// daemon.fillDeps builds, with the same prompt function behind it.
//
// Cold rather than the warm server path on purpose. The cold path is the one
// every install has, it is the fallback the warm path drops to, and it is the
// one whose behaviour on silence and on a prompt echo is the fix for #191. A
// corpus graded through a persistent server would be grading an optimisation.
func coldWhisper(t *testing.T, cfg config.Config, paths config.Paths, rig Rig) *whispercpp.Transcriber {
	t.Helper()
	model := whispercpp.ResolveModelPath(cfg.STT.Whisper.Model, paths.WhisperModelDir())
	if _, err := os.Stat(model); err != nil {
		t.Fatalf("whisper model not found at %s (run: jarvix setup whisper)", model)
	}
	return &whispercpp.Transcriber{
		Binary:     cfg.STT.Whisper.Binary,
		ModelPath:  model,
		Language:   cfg.STT.Whisper.Language,
		PromptFunc: rig.BiasPrompt,
	}
}

// transcribeOne runs one recording through the engine and evaluates it.
//
// The stream is consumed the way the session engine consumes it: take the
// final event, keep its text and its reason. A partial would be a future
// streaming engine's business and is ignored here rather than concatenated,
// because concatenating hypotheses would invent a transcript nothing produced.
func transcribeOne(t *testing.T, engine *whispercpp.Transcriber, rec Recording, rig Rig) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), perFileTimeout)
	defer cancel()

	start := time.Now()
	events, err := engine.Transcribe(ctx, stt.AudioInput{WAVPath: rec.Path})
	if err != nil {
		t.Fatalf("%s: %v", rec.ID, err)
	}
	var text, reason string
	for e := range events {
		switch e.Type {
		case stt.EventError:
			t.Fatalf("%s: whisper failed: %v\n"+
				"(whisper-cli reads 16 kHz mono WAV only; convert with "+
				"ffmpeg -i in.wav -ar 16000 -ac 1 -c:a pcm_s16le out.wav)", rec.ID, e.Err)
		case stt.EventFinal:
			text, reason = e.Text, e.Reason
		case stt.EventPartial:
			// Ignored; see the doc comment.
		}
	}
	return Evaluate(rec, text, reason, time.Since(start), rig)
}
