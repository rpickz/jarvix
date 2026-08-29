# Voice corpus recordings

Drop the recordings here, one WAV per phrase, named after its id in
`internal/voicecorpus/phrases.toml` — `10-workspace-four.wav`, and
`10-workspace-four-noisy.wav` for a second take in a noisy room.

16 kHz mono, signed 16-bit; convert anything else first:

    ffmpeg -i take.m4a -ar 16000 -ac 1 -c:a pcm_s16le 10-workspace-four.wav

Then:

    make voice-corpus

Full instructions, and what each phrase asserts, are in
[docs/voice-corpus.md](../../docs/voice-corpus.md).

These files are recordings of one person's voice. They stay on this machine and
in this private repository; nothing in the harness uploads, copies or transmits
them. If the repository ever opens, move them somewhere outside the working
tree and point `JARVIX_VOICE_CORPUS` at it.

This directory is empty of audio today. That is not an oversight — it is the
honest state of the thing, and `jarvix doctor` says so on every run.
