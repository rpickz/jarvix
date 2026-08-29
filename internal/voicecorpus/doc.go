// Package voicecorpus is the harness behind the real-voice test corpus
// (issue #143): the user's own recordings, run through the real speech
// pipeline, asserted on what the pipeline *did* with them.
//
// # Why it exists
//
// Everything in this repository that depends on speech recognition — wake-word
// matching, the name aliases, the bias prompt, taught hard-to-hear vocabulary,
// the intent grammar, number normalisation — is tested with faked transcripts.
// Those tests are worth having: they pin what happens *after* whisper. What no
// test in the tree does is prove that whisper, biased the way this daemon
// biases it, turns real speech into those transcripts in the first place. That
// gap is invisible until the day a bias change makes "Jarvix" arrive as
// "Java X" again and every existing test still passes.
//
// A corpus of recordings closes the loop, but only if it is asserted
// carefully. Two rules shape everything here:
//
//	Assert the OUTCOME, never the transcript. "workspace four" must reach the
//	router as intent workspace.switch with slot 4. Whether whisper wrote
//	"Workspace four." or "workspace 4" is not the test's business, and a test
//	that made it its business would fail on punctuation and be deleted within
//	a month.
//
//	Use the REAL seams. The prompt is the prompt the daemon would send
//	(config.STTBiasPromptFunc, over the live vocabulary store). The engine is
//	the real cold whisper-cli adapter, gates and all. The wake strip and the
//	confirmation parser are the engine's own functions, exported for this
//	(session.StripWakeWord and friends). A harness with private copies would
//	only ever prove that the copies still work.
//
// # Two established facts this design has to respect
//
// **Whisper echoes the bias prompt when handed silence** (issue #191). Given a
// capture with nothing in it, whisper returns its most likely continuation of
// whatever conditioned the decoder, which with a bias prompt is the bias
// prompt. The fix is a voice-activity gate in internal/audio plus
// stt.IsPromptEcho, both of which live inside the adapter this harness calls —
// so the harness inherits them rather than working around them. A corpus file
// with no voiced audio in it is therefore never quietly transcribed: it is
// rejected at load time as a corpus defect (Load, corpus.go), with the level
// it measured, because "this recording is silent" is a thing the person who
// recorded it needs to be told.
//
// **The bias set is not a constant.** It carries the configured assistant name
// (#107) and the taught hard-to-hear phrases (#129), both of which the user
// changes while the daemon runs. "The live bias prompt" therefore means a
// function evaluated at transcription time over the current configuration and
// the current vocabulary store — see Rig and BuildRig — not a string frozen
// into this package.
//
// # Shape
//
// Everything in this package except the run itself is ordinary, hermetic Go
// with no engines behind it, so the harness's own judgement — how a phrase
// declares its expected outcome, how a result is scored, when a baseline
// counts as regressed, what an empty or broken corpus does — is tested by
// `go test ./...` like anything else.
//
// The part that needs whisper and a microphone's worth of personal audio is
// one file behind the `voicecorpus` build tag. It is out of CI by tag rather
// than by skip, for two reasons that are both about honesty: the recordings
// are personal data that should not be uploaded to a runner, and a heavy test
// that "skips" on every machine that lacks an engine is a test nobody notices
// has stopped running.
//
//	go test -tags voicecorpus ./internal/voicecorpus -v      # run it
//	make voice-corpus                                        # the same, named
//
// See docs/voice-corpus.md for how to record and add phrases.
package voicecorpus
