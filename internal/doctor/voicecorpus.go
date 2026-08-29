package doctor

import (
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/voicecorpus"
)

// checkVoiceCorpus reports whether anything has ever proved that real speech
// reaches this pipeline as the right thing (issue #143).
//
// It sits beside the name-recognition and bias-budget lines because it is the
// same subject seen from the other end. Those two report that the bias prompt
// is active and that the taught-word budget has room — both true, both useful,
// and both statements about configuration. This one reports whether the
// configuration has ever been *heard*: a recording of the user saying "Jarvix"
// that came out of whisper as something the strip accepts, an utterance that
// reached the intent router as the intent it meant. Until the corpus is
// recorded, every claim in this repository about speech recognition rests on
// transcripts a developer typed, and doctor is the right place to say so
// plainly rather than leave it to be discovered.
//
// It reads the manifest and baseline compiled into the binary and touches
// nothing else: no whisper run, no audio, no directory that only exists in a
// source checkout. A doctor check that took a minute and needed the repository
// would be a check nobody ran. The corpus itself is run deliberately, with
// `make voice-corpus`.
//
// Status is OK in every state, including "nothing recorded". An unrecorded
// corpus is not a broken installation and warning about it on every run would
// train the reader to skim the whole report — the sentence does the work.
func checkVoiceCorpus(config.Config, config.Paths) Result {
	return Result{Status: OK, Name: "voice corpus", Detail: voicecorpus.Summary()}
}
