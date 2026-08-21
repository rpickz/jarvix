#!/bin/bash
# Build and package the Jarvix release tarballs: static linux/amd64 and
# linux/arm64 binaries plus everything a machine needs beyond them (Omarchy
# plugin, Kokoro helper, systemd unit, install scripts, install notes), with
# a SHA256SUMS manifest.
#
# Used by .github/workflows/release.yml on tags and by `make release-snapshot`
# locally — same code path, so a PR dry run proves what a tag will ship.
#
#   VERSION=v0.2.0 scripts/package-release.sh   # explicit version (CI tags)
#   scripts/package-release.sh                  # git describe (snapshots)
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
ARCHES="${ARCHES:-amd64 arm64}"
DIST="$REPO_DIR/dist"
LDFLAGS="-X github.com/rpickz/jarvix/internal/build.Version=$VERSION"

rm -rf "$DIST"
mkdir -p "$DIST"

for arch in $ARCHES; do
  name="jarvix_${VERSION}_linux_${arch}"
  stage="$DIST/$name"
  echo "── $name"

  echo "  building jarvix + jarvixd..."
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$stage/bin/jarvix" ./cmd/jarvix
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$stage/bin/jarvixd" ./cmd/jarvixd

  echo "  staging plugin, helpers, unit..."
  mkdir -p "$stage/plugin" "$stage/tts/kokoro" "$stage/systemd" "$stage/scripts"
  cp -r plugin/omarchy "$stage/plugin/omarchy"
  cp -r plugin/hyprland "$stage/plugin/hyprland"
  cp tts/kokoro/kokoro_stream.py "$stage/tts/kokoro/"
  cp systemd/jarvixd.service "$stage/systemd/"
  cp scripts/setup-kokoro.sh scripts/install-plugin.sh scripts/install-hyprland-bindings.sh "$stage/scripts/"
  cp LICENSE "$stage/"

  cat >"$stage/INSTALL.md" <<EOF
# Jarvix $VERSION — install notes

Manual install from this tarball (Arch users: prefer the AUR package):

    install -Dm755 bin/jarvix  ~/.local/bin/jarvix
    install -Dm755 bin/jarvixd ~/.local/bin/jarvixd
    install -Dm644 systemd/jarvixd.service ~/.config/systemd/user/jarvixd.service
    systemctl --user daemon-reload
    systemctl --user enable --now jarvixd

Install the helper scripts too. \`jarvix setup\` delegates the Kokoro voice
and Hyprland binding steps to them, and this is one of the directories it
looks in — leave them in the unpacked tarball and those steps silently
disappear from the wizard:

    mkdir -p ~/.local/share/jarvix/scripts
    install -m755 scripts/*.sh ~/.local/share/jarvix/scripts/

Copy the Omarchy overlay plugin (don't symlink from a temporary directory):

    mkdir -p ~/.config/omarchy/plugins
    cp -r plugin/omarchy ~/.config/omarchy/plugins/jarvix

Then let the first-run wizard walk you through the rest — voice engine,
push-to-talk access, AI provider, advisor CLIs — verifying each step:

    jarvix setup

Health check any time: \`jarvix doctor\`. Version: \`jarvix --version\`.
Upgrading: overwrite the two binaries and restart the daemon
(\`systemctl --user restart jarvixd\`); your config and state are untouched.
EOF

  echo "  creating $name.tar.gz..."
  tar -C "$DIST" -czf "$DIST/$name.tar.gz" "$name"
  rm -rf "$stage"
done

(cd "$DIST" && sha256sum ./*.tar.gz > SHA256SUMS)

echo ""
echo "Release artifacts in $DIST:"
(cd "$DIST" && ls -l ./*.tar.gz SHA256SUMS)
