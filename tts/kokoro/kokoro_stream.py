#!/usr/bin/env python3
"""Kokoro TTS streaming helper for Jarvix.

Two modes, one script:

*One-shot* (no arguments beyond the voice/speed): reads one line of UTF-8 text
on stdin, synthesizes it with kokoro-onnx, and writes raw signed-16-bit
little-endian PCM to stdout as each chunk is produced. The Go tts/kokoro
adapter owns process lifecycle and cancellation (it kills this process to
interrupt speech). This is the original protocol and the cold path.

*Serve* (--serve): the same synthesis, but the interpreter and the ONNX model
stay loaded across utterances, which is what removes ~0.5s from the start of
every spoken answer (ADR 0018). Booting Python and loading the model is the
single most expensive step in the response path, and it does not depend on
what is being said, so it should happen once per daemon rather than once per
sentence.

Serve protocol, version 1. stdout is binary and framed; stderr stays free for
diagnostics.

    stdout:  READY 1 <sample_rate>\\n        once, after the model is loaded
             CHUNK <id> <nbytes>\\n<nbytes>  one frame of s16le PCM
             END <id>\\n                     utterance complete
             ABORTED <id>\\n                 utterance stopped by ABORT
             ERROR <id> <message>\\n         utterance failed; helper stays up

    stdin:   SPEAK <id> <text>\\n            synthesize text as utterance id
             ABORT <id>\\n                   stop utterance id right now
             QUIT\\n                         exit cleanly

ABORT is why this is a protocol rather than a pipe. Interrupting Jarvix
mid-sentence must be instant, and with a persistent worker "kill the process"
is no longer an acceptable way to achieve that — it would throw away the model
load the whole design exists to keep. So ABORT is read on a separate thread
and checked between chunks: speech stops within one chunk (tens of
milliseconds) and the worker stays warm for the next question.

Environment:
  JARVIX_KOKORO_MODEL   path to kokoro-v1.0.onnx
  JARVIX_KOKORO_VOICES  path to voices-v1.0.bin
Arguments:
  --voice NAME          voice id (default: af_heart)
  --speed FLOAT         speech rate (default: 1.0)
  --lang CODE           phonemiser language (default: en-us)
  --serve               run the persistent protocol described above

--lang is separate from --voice because Kokoro treats them separately: the
voice supplies the timbre, the language supplies the letter-to-sound rules.
They used to be allowed to disagree — this script hardcoded lang="en-us" — so
a British voice spoke British-sounding American English, with rhotic R's and
T's flapped to D's. The caller derives the code from the voice id's family
letter (a=en-us, b=en-gb, e=es, f=fr-fr, h=hi, i=it, j=ja, p=pt-br, z=zh) and
passes it here, so the two halves of a voice can no longer come apart. The
default keeps a hand-run invocation working as it always did.

In one-shot mode the sample rate (24000) is printed to stderr as
"SAMPLE_RATE=24000" before any audio, so the adapter can configure playback
without hardcoding it. In serve mode it is part of the READY line.
"""
import argparse
import asyncio
import os
import queue
import sys
import threading

import numpy as np

SAMPLE_RATE = 24000
PROTOCOL_VERSION = 1


def to_pcm(samples) -> bytes:
    """Convert float samples in [-1, 1] to s16le bytes."""
    pcm = np.clip(samples, -1.0, 1.0)
    return (pcm * 32767.0).astype("<i2").tobytes()


def load_kokoro():
    """Import and construct Kokoro. Imported late so --help and env
    validation stay fast and dependency-free."""
    model = os.environ.get("JARVIX_KOKORO_MODEL")
    voices = os.environ.get("JARVIX_KOKORO_VOICES")
    if not model or not voices:
        raise RuntimeError("JARVIX_KOKORO_MODEL and JARVIX_KOKORO_VOICES must be set")
    from kokoro_onnx import Kokoro

    return Kokoro(model, voices)


def one_shot(args) -> int:
    """The original per-utterance protocol: text in, PCM out, exit."""
    text = sys.stdin.read().strip()
    if not text:
        return 0

    kokoro = load_kokoro()
    print(f"SAMPLE_RATE={SAMPLE_RATE}", file=sys.stderr, flush=True)

    stdout = sys.stdout.buffer
    # create_stream yields (samples, sample_rate) per sentence-ish chunk, so
    # playback can begin before the whole utterance is synthesized.
    stream = kokoro.create_stream(text, voice=args.voice, speed=args.speed, lang=args.lang)

    async def run() -> None:
        async for samples, _ in stream:
            stdout.write(to_pcm(samples))
            stdout.flush()

    asyncio.run(run())
    return 0


class Commands:
    """Commands read off stdin by a background thread.

    The thread exists for ABORT alone: the main thread is inside ONNX while an
    utterance renders, so a command that must be noticed *during* synthesis
    cannot be waiting behind it on the same reader.
    """

    def __init__(self) -> None:
        self.queue: "queue.Queue[tuple[str, str]]" = queue.Queue()
        self._lock = threading.Lock()
        self._aborted: set[str] = set()

    def start(self) -> None:
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self) -> None:
        for line in sys.stdin:
            line = line.rstrip("\n")
            if not line:
                continue
            verb, _, rest = line.partition(" ")
            if verb == "ABORT":
                # Recorded immediately, not queued: the point is to be seen by
                # a synthesis loop that is already running.
                with self._lock:
                    self._aborted.add(rest.strip())
                continue
            self.queue.put((verb, rest))
        self.queue.put(("QUIT", ""))

    def aborted(self, utterance_id: str) -> bool:
        with self._lock:
            return utterance_id in self._aborted

    def clear(self, utterance_id: str) -> None:
        with self._lock:
            self._aborted.discard(utterance_id)


def serve(args) -> int:
    """The persistent protocol: one model load, many utterances."""
    kokoro = load_kokoro()
    stdout = sys.stdout.buffer

    def emit(line: str, payload: bytes = b"") -> None:
        stdout.write(line.encode("utf-8"))
        if payload:
            stdout.write(payload)
        stdout.flush()

    emit(f"READY {PROTOCOL_VERSION} {SAMPLE_RATE}\n")

    commands = Commands()
    commands.start()

    async def speak(utterance_id: str, text: str) -> None:
        stream = kokoro.create_stream(text, voice=args.voice, speed=args.speed, lang=args.lang)
        async for samples, _ in stream:
            if commands.aborted(utterance_id):
                emit(f"ABORTED {utterance_id}\n")
                return
            payload = to_pcm(samples)
            emit(f"CHUNK {utterance_id} {len(payload)}\n", payload)
        # An abort that lands after the last chunk still ends the utterance as
        # aborted, so the adapter's bookkeeping never depends on a race.
        if commands.aborted(utterance_id):
            emit(f"ABORTED {utterance_id}\n")
            return
        emit(f"END {utterance_id}\n")

    while True:
        verb, rest = commands.queue.get()
        if verb == "QUIT":
            return 0
        if verb != "SPEAK":
            continue
        utterance_id, _, text = rest.partition(" ")
        utterance_id = utterance_id.strip()
        text = text.strip()
        if not utterance_id:
            continue
        if not text:
            emit(f"END {utterance_id}\n")
            continue
        if commands.aborted(utterance_id):
            # Cancelled before synthesis even started.
            commands.clear(utterance_id)
            emit(f"ABORTED {utterance_id}\n")
            continue
        try:
            asyncio.run(speak(utterance_id, text))
        except Exception as exc:  # a bad utterance must not kill the worker
            emit(f"ERROR {utterance_id} {type(exc).__name__}: {exc}\n")
        finally:
            commands.clear(utterance_id)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--voice", default="af_heart")
    parser.add_argument("--speed", type=float, default=1.0)
    parser.add_argument("--lang", default="en-us",
                        help="phonemiser language code, derived from the voice")
    parser.add_argument("--serve", action="store_true",
                        help="run the persistent line-wise protocol")
    args = parser.parse_args()

    try:
        if args.serve:
            return serve(args)
        return one_shot(args)
    except RuntimeError as exc:
        print(exc, file=sys.stderr)
        return 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except BrokenPipeError:
        # The adapter closed our stdout to cancel; exit quietly.
        sys.exit(0)
    except KeyboardInterrupt:
        sys.exit(0)
