package main

// `jarvix voices` — the answer to "what can this thing sound like?".
//
// Every voice listed here was already on disk: the Kokoro archive
// setup-kokoro.sh downloads holds 54 of them across nine language families.
// They were unreachable in practice because reaching one meant hand-editing
// config.toml with an id you would have to already know exists, and the ids
// (bm_george, ff_siwis, zf_xiaoni) are not guessable. Listing them grouped by
// language, with the gender and the exact command to switch, is most of what
// turns a hidden capability into a feature.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
	"github.com/rpickz/jarvix/internal/voice"
)

// cmdVoices lists the voices installed for the configured TTS engine.
func cmdVoices(cfg config.Config, paths config.Paths, asJSON bool) error {
	installed, err := cfg.InstalledVoices(paths).Voices()
	if err != nil {
		return fmt.Errorf("no %s voices available: %w", cfg.TTS.Provider, err)
	}
	active := activeVoice(cfg)
	if asJSON {
		return printVoicesJSON(installed, active)
	}
	printVoices(cfg.TTS.Provider, installed, active)
	return nil
}

func activeVoice(cfg config.Config) string {
	if cfg.TTS.Provider == "kokoro" {
		return cfg.TTS.Kokoro.Voice
	}
	return cfg.TTS.Piper.Voice
}

func voiceKey(provider string) string {
	if provider == "kokoro" {
		return "tts.kokoro.voice"
	}
	return "tts.piper.voice"
}

func printVoices(provider string, installed []voice.Voice, active string) {
	fmt.Printf("%d %s voice(s) installed\n", len(installed), provider)
	for _, g := range voice.Grouped(installed) {
		fmt.Printf("\n%s  (%s)\n", g.Language.Name, g.Language.Code)
		for _, v := range g.Voices {
			marker := "  "
			if v.ID == active {
				marker = "* "
			}
			gender := ""
			if v.Gender != voice.GenderUnknown {
				gender = v.Gender.String()
			}
			fmt.Printf("%s%-18s %-10s %s\n", marker, v.ID, gender, v.Name)
		}
	}
	key := voiceKey(provider)
	fmt.Printf("\n* is the voice in use. Change it without restarting:\n  jarvix config set %s=<id>\n", key)
	fmt.Println("\nA non-English voice also needs speech recognition to match, or Jarvix")
	fmt.Println("would speak one language and listen in another:")
	fmt.Printf("  jarvix setup whisper %s\n  jarvix config set %s=<id> stt.whisper.model=%s stt.whisper.language=<code>\n",
		config.MultilingualWhisperModel, key, config.MultilingualWhisperModel)
	fmt.Println("`jarvix setup` walks through both together.")
}

// voiceJSON is the machine-readable shape, for the settings screen and for
// anything scripting a voice change.
type voiceJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Language string `json:"language"`
	Code     string `json:"code"`
	Whisper  string `json:"whisper_language"`
	Gender   string `json:"gender"`
	Active   bool   `json:"active"`
}

func printVoicesJSON(installed []voice.Voice, active string) error {
	out := make([]voiceJSON, 0, len(installed))
	for _, v := range installed {
		out = append(out, voiceJSON{
			ID: v.ID, Name: v.Name, Language: v.Language.Name, Code: v.Language.Code,
			Whisper: v.Language.Whisper, Gender: v.Gender.String(), Active: v.ID == active,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// previewVoice speaks a sample sentence in one voice, straight through the
// cold adapter and PipeWire.
//
// It deliberately bypasses the daemon. The wizard runs before jarvixd is
// necessarily up, and previewing must not depend on the very configuration it
// is helping the user write — so this builds a synthesizer for the candidate
// voice, plays it, and throws it away. The sample sentence is in the target
// language because the point is to hear the phonemiser, not the timbre.
func previewVoice(cfg config.Config, voiceID string) error {
	lang, ok := voiceLanguage(cfg.TTS.Provider, voiceID)
	if !ok {
		lang = voice.DefaultLanguage()
	}
	var synth tts.Synthesizer
	if cfg.TTS.Provider == "kokoro" {
		synth = &kokoro.Synthesizer{Voice: voiceID, Speed: cfg.TTS.Kokoro.Speed}
	} else {
		synth = &piper.Synthesizer{Binary: cfg.TTS.Piper.Binary, Voice: voiceID}
	}

	// A preview is a courtesy, not a session: bound it so a wedged engine
	// cannot hold the wizard open indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	format, chunks, err := synth.Speak(ctx, tts.Request{Text: lang.Sample})
	if err != nil {
		return err
	}
	pcm := make(chan []byte)
	errc := make(chan error, 1)
	go func() {
		defer close(pcm)
		for c := range chunks {
			if c.Err != nil {
				errc <- c.Err
				return
			}
			select {
			case pcm <- c.PCM:
			case <-ctx.Done():
				return
			}
		}
		errc <- nil
	}()
	player := &audio.PipeWirePlayer{Device: cfg.Audio.OutputDevice}
	if err := player.Play(ctx, format.SampleRate, format.Channels, pcm); err != nil {
		return err
	}
	return <-errc
}

// voiceLanguage derives a voice's language for whichever engine names it.
func voiceLanguage(provider, id string) (voice.Language, bool) {
	if provider == "kokoro" {
		return voice.LanguageForKokoroVoice(id)
	}
	return voice.LanguageForPiperVoice(id)
}
