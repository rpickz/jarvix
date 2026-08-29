package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// This file answers one question about a finished capture: did the microphone
// deliver anything at all?
//
// It exists because whisper always answers. Handed two seconds of digital
// silence it does not return the empty string — it returns its most likely
// continuation of whatever it was conditioned on, which with Jarvix's bias
// prompt is the bias prompt itself ("The assistant is called Jarvix.") and
// without one is the familiar " you" / "Thank you." / "Thanks for watching!"
// family (issue #191). A hallucinated transcript is not cosmetic: it reaches
// the intent router and the model, counts as the user being present, and
// lands in the archive as something they said. The cheapest way to be certain
// none of that happens is not to ask the question — and the honest reason not
// to ask is that there was no audio to ask about.
//
// The measure is root-mean-square energy per short frame, the same shape the
// wake endpointer uses (internal/wake/endpoint.go) and for the same reasons:
// a neural voice-activity model would be a second model to install and a
// second thing to go missing, for a decision that only has to separate "the
// microphone produced a signal" from "the microphone produced nothing".
//
// The thresholds and the measurements behind them are recorded in ADR 0060.

// silenceFrameMillis is the analysis window. 20 ms is the standard
// voice-activity frame: long enough
// that a frame's RMS means something, short enough that a single word in an
// otherwise quiet ten-second capture still shows up as a loud frame instead
// of being averaged into the silence around it.
const silenceFrameMillis = 20

// SilenceFloorRMS is the loudest-frame level, in raw signed-16-bit sample
// units, at or below which a capture is treated as having no voiced audio.
//
// 8.0 is -72 dBFS (20*log10(8/32768)). The number is chosen from the shape of
// the problem, not tuned against a corpus, and it is deliberately set far
// below anything a person could produce:
//
//   - Digital silence — a muted source, the wrong device, a stream that never
//     opened — is exactly 0. Sub-LSB dither of ±1 is RMS ≈ 0.8, or -92 dBFS.
//     Both sit an order of magnitude below the floor.
//   - A real microphone in a genuinely quiet room, through PipeWire at default
//     gain, sits around -60 to -50 dBFS: RMS 33 to 104. This package's sibling
//     fixtures agree — internal/wake's roomTone is ±60, RMS ≈ 35 — and the
//     wake endpointer's own minNoiseFloor is 40. The floor here is five times
//     lower than that on purpose.
//
// So the floor sits roughly 20 dB above a dead line and 12 dB below the
// quietest room anyone actually records in, which is where a gate belongs
// when the two failure modes are not symmetric.
//
// **A quiet speaker is safe, and this is why.** Whisper mean-normalises its
// mel spectrogram, so absolute level tells it nothing: measured on this
// machine, ggml-base.en still transcribed "What is the weather like today in
// London?" perfectly from a clip attenuated to a peak frame RMS of 1.7
// (-86 dBFS). Taken alone that says no absolute gate can be proven safe. But
// that clip was synthesised and then scaled — noise-free, which no capture
// from a microphone ever is. A real quiet talker arrives with their room
// attached: the preamp hiss, the fan, the keyboard, all of which are already
// above this floor before they open their mouth. What falls below 8.0 is not
// a quiet person; it is an input that produced no signal.
//
// The gate therefore errs, deliberately and in every ambiguous case, towards
// asking whisper: a clip that cannot be read, cannot be parsed, or is too
// short to measure is transcribed. Missing a real question is a worse failure
// than transcribing silence, because transcribing silence has a second line
// of defence behind it (the bias-prompt echo rule in internal/stt) and a
// missed question has none.
const SilenceFloorRMS = 8.0

// CaptureLevel is what a capture measured.
type CaptureLevel struct {
	// PeakFrameRMS is the loudest analysis frame's root-mean-square level, in
	// raw signed-16-bit sample units (0 to 32767). The peak rather than the
	// mean: a capture is voiced if *any* of it was, and a half-second of
	// speech inside a ten-second press has a mean indistinguishable from
	// silence.
	PeakFrameRMS float64
	// Frames is how many whole analysis frames the clip yielded. Zero means
	// the recording was shorter than one frame, which is not evidence of
	// silence — see Voiced.
	Frames int
}

// Voiced reports whether the capture carries anything worth transcribing.
//
// A clip too short to fill a single analysis frame counts as voiced: 20 ms is
// below the shortest press the engine accepts (audio.min_recording_ms) and a
// capture that arrives that short is a bug in the recorder, not proof that
// the room was silent. Erring towards transcribing is the rule everywhere in
// this file.
func (l CaptureLevel) Voiced() bool {
	if l.Frames == 0 {
		return true
	}
	return l.PeakFrameRMS > SilenceFloorRMS
}

// DBFS renders the peak frame level in decibels relative to full scale, for
// logs and for the sentence a user debugging a microphone reads.
func (l CaptureLevel) DBFS() float64 { return DBFS(l.PeakFrameRMS) }

// DBFS converts a raw signed-16-bit RMS level to decibels relative to full
// scale. Digital silence has no logarithm, so it is reported as the negative
// infinity it is; callers putting this in JSON should send the raw level
// instead, since -Inf is not a JSON number.
func DBFS(rms float64) float64 {
	if rms <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms/32768)
}

// MeasureWAV reads a signed-16-bit PCM RIFF/WAVE file and reports its peak
// frame level.
//
// It runs on every push-to-talk release, so the cost matters: one sequential
// read of a file that is already in the page cache (recordings live on the
// tmpfs runtime directory and were written seconds ago), then one multiply
// and add per sample. A ten-second 16 kHz mono capture is 160k samples —
// a fraction of a millisecond, against the hundreds of milliseconds whisper
// takes to decode the same clip.
//
// An error means "we could not tell", never "there was no voice". Callers are
// expected to transcribe on error.
func MeasureWAV(path string) (CaptureLevel, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return CaptureLevel{}, fmt.Errorf("read recording: %w", err)
	}
	sampleRate, channels, data, err := parsePCM16WAV(raw)
	if err != nil {
		return CaptureLevel{}, err
	}
	return measurePCM16(data, sampleRate, channels), nil
}

// measurePCM16 walks little-endian signed-16-bit samples and returns the
// loudest frame. Channels are measured together rather than per channel:
// capture is mono in practice, and for anything else "was there a signal on
// any channel" is the question being asked.
func measurePCM16(data []byte, sampleRate, channels int) CaptureLevel {
	if sampleRate <= 0 || channels <= 0 {
		return CaptureLevel{}
	}
	frameSamples := sampleRate * silenceFrameMillis / 1000 * channels
	if frameSamples <= 0 {
		return CaptureLevel{}
	}
	frameBytes := frameSamples * 2

	var level CaptureLevel
	for off := 0; off+frameBytes <= len(data); off += frameBytes {
		var sum float64
		for i := off; i < off+frameBytes; i += 2 {
			// Computed in float64 rather than integer arithmetic for the same
			// reason internal/wake does it: the sum of squares of a full-scale
			// frame overflows int32, and integer rounding would bite hardest
			// at the quiet end — which is the only end this decision is about.
			v := float64(int16(binary.LittleEndian.Uint16(data[i : i+2])))
			sum += v * v
		}
		if r := math.Sqrt(sum / float64(frameSamples)); r > level.PeakFrameRMS {
			level.PeakFrameRMS = r
		}
		level.Frames++
	}
	return level
}

// parsePCM16WAV walks the RIFF chunk list far enough to find the format and
// the samples. It is deliberately minimal — this package writes the only WAV
// files Jarvix reads (WriteWAV above, and pw-record's own canonical output) —
// and anything it does not understand is an error, which the caller turns
// into "transcribe it anyway" rather than "assume silence".
func parsePCM16WAV(raw []byte) (sampleRate, channels int, data []byte, err error) {
	const headerBytes = 12 // "RIFF" + size + "WAVE"
	if len(raw) < headerBytes || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return 0, 0, nil, fmt.Errorf("not a RIFF/WAVE file")
	}
	for off := headerBytes; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		// A truncated final chunk is normal: pw-record is killed mid-write on
		// cancellation, and the header then over-declares the data it has.
		// Measure what is there rather than refusing the whole clip.
		if size < 0 || body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return 0, 0, nil, fmt.Errorf("truncated WAV fmt chunk")
			}
			format := binary.LittleEndian.Uint16(raw[body : body+2])
			bits := binary.LittleEndian.Uint16(raw[body+14 : body+16])
			// 1 is WAVE_FORMAT_PCM. 0xFFFE (extensible) carries its real
			// format in a sub-chunk this parser does not read; treating it as
			// unknown costs one whisper call on a file Jarvix never writes.
			if format != 1 || bits != 16 {
				return 0, 0, nil, fmt.Errorf("unsupported WAV format %d at %d bits", format, bits)
			}
			channels = int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			sampleRate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
		case "data":
			data = raw[body : body+size]
		}
		if data != nil && sampleRate > 0 {
			break
		}
		// Chunks are word-aligned: an odd size is followed by a pad byte.
		off = body + size + size%2
	}
	if sampleRate <= 0 || channels <= 0 {
		return 0, 0, nil, fmt.Errorf("WAV file has no fmt chunk")
	}
	if data == nil {
		return 0, 0, nil, fmt.Errorf("WAV file has no data chunk")
	}
	return sampleRate, channels, data, nil
}
