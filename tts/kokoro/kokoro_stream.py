#!/usr/bin/env python3
"""Kokoro TTS streaming helper for Jarvix.

Reads one line of UTF-8 text on stdin, synthesizes it with kokoro-onnx, and
writes raw signed-16-bit little-endian PCM to stdout as each chunk is
produced. The Go tts/kokoro adapter owns process lifecycle and cancellation
(it kills this process to interrupt speech), matching how the Piper adapter
works.

Environment:
  JARVIX_KOKORO_MODEL   path to kokoro-v1.0.onnx
  JARVIX_KOKORO_VOICES  path to voices-v1.0.bin
Arguments:
  --voice NAME          voice id (default: af_heart)
  --speed FLOAT         speech rate (default: 1.0)

The sample rate (24000) is printed to stderr as "SAMPLE_RATE=24000" before any
audio, so the adapter can configure playback without hardcoding it.
"""
import argparse
import os
import sys

import numpy as np


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--voice", default="af_heart")
    parser.add_argument("--speed", type=float, default=1.0)
    args = parser.parse_args()

    model = os.environ.get("JARVIX_KOKORO_MODEL")
    voices = os.environ.get("JARVIX_KOKORO_VOICES")
    if not model or not voices:
        print("JARVIX_KOKORO_MODEL and JARVIX_KOKORO_VOICES must be set", file=sys.stderr)
        return 2

    text = sys.stdin.read().strip()
    if not text:
        return 0

    # Imported here so --help and env validation stay fast and dependency-free.
    from kokoro_onnx import Kokoro

    kokoro = Kokoro(model, voices)
    sample_rate = 24000
    print(f"SAMPLE_RATE={sample_rate}", file=sys.stderr, flush=True)

    stdout = sys.stdout.buffer
    # create_stream yields (samples, sample_rate) per sentence-ish chunk, so
    # playback can begin before the whole utterance is synthesized.
    stream = kokoro.create_stream(text, voice=args.voice, speed=args.speed, lang="en-us")

    import asyncio

    async def run() -> None:
        async for samples, sr in stream:
            pcm = np.clip(samples, -1.0, 1.0)
            pcm = (pcm * 32767.0).astype("<i2")
            stdout.write(pcm.tobytes())
            stdout.flush()

    asyncio.run(run())
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except BrokenPipeError:
        # The adapter closed our stdout to cancel; exit quietly.
        sys.exit(0)
    except KeyboardInterrupt:
        sys.exit(0)
