#!/bin/bash
# Install the Kokoro neural TTS engine for Jarvix: a Python venv with
# kokoro-onnx, the ONNX model + voices, and the streaming helper script.
#
# Kokoro's voice is much more natural than Piper's; this is the heavier setup
# path, so it is opt-in. After it finishes, set tts.provider = "kokoro" in
# ~/.config/jarvix/config.toml and restart jarvixd.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/jarvix"
VENV="$DATA_DIR/kokoro-venv"
MODEL_DIR="$DATA_DIR/models/kokoro"
BASE_URL="https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0"

echo "Installing Kokoro TTS into $DATA_DIR"
mkdir -p "$MODEL_DIR"

if [[ ! -x "$VENV/bin/python" ]]; then
  echo "Creating Python venv..."
  python3 -m venv "$VENV"
fi
echo "Installing kokoro-onnx..."
"$VENV/bin/pip" install -q --upgrade pip
"$VENV/bin/pip" install -q kokoro-onnx numpy

fetch() {
  local name="$1"
  if [[ -f "$MODEL_DIR/$name" ]]; then
    echo "  $name already present"
    return
  fi
  echo "  downloading $name..."
  curl -fL --progress-bar -o "$MODEL_DIR/$name" "$BASE_URL/$name"
}
fetch kokoro-v1.0.onnx
fetch voices-v1.0.bin

# The helper script lives beside the models so the daemon finds it without
# needing the repo checkout on the host.
install -m755 "$REPO_DIR/tts/kokoro/kokoro_stream.py" "$DATA_DIR/kokoro_stream.py"

echo ""
echo "Kokoro installed. Enable it:"
echo "  set tts.provider = \"kokoro\" in ~/.config/jarvix/config.toml"
echo "  systemctl --user restart jarvixd"
echo "  jarvix ask \"say hello\"   # verify"
echo ""
echo "The voices file just downloaded holds 54 voices across nine languages,"
echo "including four British female and four British male:"
echo "  jarvix voices                                # list them by language"
echo "  jarvix config set tts.kokoro.voice=bf_emma   # applies without a restart"
