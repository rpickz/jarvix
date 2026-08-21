#!/usr/bin/env python3
"""Wake-word detection helper for Jarvix.

Reads raw 16 kHz mono s16le PCM on stdin, one fixed-size frame at a time, and
writes one score line per frame to stdout. It holds a local openWakeWord model
and nothing else: no network, no disk writes, no buffering beyond the frame it
is scoring.

Protocol, version 1 (the Go side is internal/wake/detector.go):

    stdout:  READY 1 <frame_samples> <model>\\n   once, after the model loads
             SCORE <score>\\n                     one per frame, 0..1
             ERROR <message>\\n                   fatal; Jarvix replaces us

    stdin:   <frame_samples> samples of 16 kHz mono s16le, repeatedly

Thresholds, the consecutive-frame rule and the refractory period are
deliberately *not* here. They live in Go (internal/wake/policy.go) where they
are tested and where changing them does not mean reinstalling this helper.
This script only ever answers "how much did that sound like the wake word?".

Privacy: nothing read on stdin is stored, echoed, or written anywhere. Each
frame is scored and dropped. stderr carries diagnostics only.
"""
import argparse
import os
import sys


# openWakeWord ships a small set of pretrained models. None of them is
# "jarvix" — training a custom wake word is explicitly out of scope for the
# feature this helper serves — so the wake word is mapped onto the closest
# bundled model and the *actual* model name is reported in the READY line, so
# that `jarvix status` and `jarvix doctor` show what is really listening
# rather than what was asked for.
BUNDLED = {
    "jarvix": "hey_jarvis",
    "jarvis": "hey_jarvis",
    "hey jarvis": "hey_jarvis",
    "alexa": "alexa",
    "hey mycroft": "hey_mycroft",
    "hey rhasspy": "hey_rhasspy",
    "ok nabu": "ok_nabu",
}


def resolve_model(word: str) -> str:
    """Map a configured wake word onto a model name or a model file path."""
    # An explicit path wins: this is how somebody uses a model they trained
    # themselves without this script needing to know about it.
    if os.path.sep in word or word.endswith((".onnx", ".tflite")):
        return word
    return BUNDLED.get(word.strip().lower(), "hey_jarvis")


def fail(message: str) -> int:
    sys.stdout.write("ERROR %s\n" % message.replace("\n", " "))
    sys.stdout.flush()
    return 1


def main() -> int:
    parser = argparse.ArgumentParser(description="Jarvix wake-word detector")
    parser.add_argument("--word", default="jarvix", help="wake word, or a path to a model")
    parser.add_argument("--frame", type=int, default=1280, help="samples per frame")
    parser.add_argument("--rate", type=int, default=16000, help="sample rate in Hz")
    args = parser.parse_args()

    try:
        import numpy as np
        from openwakeword.model import Model
    except Exception as exc:  # pragma: no cover - environment dependent
        return fail("openwakeword is not installed (%s); run scripts/setup-wake.sh" % exc)

    if args.rate != 16000:
        return fail("openWakeWord models are trained at 16 kHz; got %d" % args.rate)

    model_name = resolve_model(args.word)
    try:
        model = Model(wakeword_models=[model_name], inference_framework="onnx")
    except Exception as exc:  # pragma: no cover - environment dependent
        return fail("could not load model %r: %s" % (model_name, exc))

    sys.stdout.write("READY 1 %d %s\n" % (args.frame, model_name))
    sys.stdout.flush()

    frame_bytes = args.frame * 2
    stdin = sys.stdin.buffer
    while True:
        chunk = stdin.read(frame_bytes)
        if not chunk or len(chunk) < frame_bytes:
            return 0  # Jarvix closed the pipe; nothing to clean up
        samples = np.frombuffer(chunk, dtype=np.int16)
        try:
            scores = model.predict(samples)
        except Exception as exc:  # pragma: no cover - environment dependent
            return fail("prediction failed: %s" % exc)
        best = max(scores.values()) if scores else 0.0
        sys.stdout.write("SCORE %.4f\n" % float(best))
        sys.stdout.flush()


if __name__ == "__main__":
    sys.exit(main())
