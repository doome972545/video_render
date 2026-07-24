#!/usr/bin/env bash
# Fetch ffmpeg, ffprobe and yt-dlp for the current OS/arch into
# internal/binaries/embedded/ so a portable build (-tags embed_binaries) can
# embed them. Intended for CI (macOS/Linux) and local Unix builds.
#
# Windows users should run scripts/fetch-binaries.ps1 instead.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EMBED="$ROOT/internal/binaries/embedded"
mkdir -p "$EMBED"

OS="$(uname -s)"
ARCH="$(uname -m)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Fetching binaries for $OS/$ARCH into $EMBED"

fetch_ytdlp() {
  local url
  case "$OS" in
    Darwin) url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos" ;;
    Linux)  url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux" ;;
    *) echo "unsupported OS for yt-dlp: $OS"; return 1 ;;
  esac
  echo "  yt-dlp <- $url"
  curl -fL --retry 5 --retry-all-errors -s "$url" -o "$EMBED/yt-dlp"
  chmod +x "$EMBED/yt-dlp"
}

fetch_ffmpeg_linux() {
  # BtbN/FFmpeg-Builds on GitHub — reliable release assets (amd64/arm64).
  local a
  case "$ARCH" in
    x86_64|amd64) a="linux64" ;;
    aarch64|arm64) a="linuxarm64" ;;
    *) echo "unsupported Linux arch: $ARCH"; return 1 ;;
  esac
  local url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-${a}-gpl.tar.xz"
  echo "  ffmpeg/ffprobe <- $url"
  curl -fL --retry 5 --retry-all-errors -s "$url" -o "$TMP/ff.tar.xz"
  tar -xJf "$TMP/ff.tar.xz" -C "$TMP"
  local dir
  dir="$(find "$TMP" -maxdepth 1 -type d -name 'ffmpeg-*' | head -n1)"
  cp "$dir/bin/ffmpeg" "$EMBED/ffmpeg"
  cp "$dir/bin/ffprobe" "$EMBED/ffprobe"
  chmod +x "$EMBED/ffmpeg" "$EMBED/ffprobe"
}

fetch_ffmpeg_macos() {
  # evermeet.cx provides notarized static ffmpeg/ffprobe (universal/x86_64).
  echo "  ffmpeg  <- evermeet.cx"
  curl -fL --retry 5 --retry-all-errors -s "https://evermeet.cx/ffmpeg/getrelease/ffmpeg/zip" -o "$TMP/ffmpeg.zip"
  unzip -o -q "$TMP/ffmpeg.zip" -d "$EMBED"
  echo "  ffprobe <- evermeet.cx"
  curl -fL --retry 5 --retry-all-errors -s "https://evermeet.cx/ffmpeg/getrelease/ffprobe/zip" -o "$TMP/ffprobe.zip"
  unzip -o -q "$TMP/ffprobe.zip" -d "$EMBED"
  chmod +x "$EMBED/ffmpeg" "$EMBED/ffprobe"
}

case "$OS" in
  Linux)  fetch_ffmpeg_linux ;;
  Darwin) fetch_ffmpeg_macos ;;
  *) echo "unsupported OS: $OS"; exit 1 ;;
esac
fetch_ytdlp

echo "Done. Embedded binaries:"
ls -lh "$EMBED" | grep -Ev 'README|gitkeep' || true
