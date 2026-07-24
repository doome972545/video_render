# Releasing & cross-platform builds

This project ships two front ends — a **CLI** and a **Wails desktop app** — for
**Windows, macOS, and Linux**. Because native GUI toolkits and OS-specific
ffmpeg binaries can't be cross-compiled from a single machine, builds run on
**GitHub Actions**, which provides real Windows/macOS/Linux runners.

## What gets built

For every tag push (or a manual run), the workflow produces, per OS:

| File | What it is |
|---|---|
| `VideoRemix-windows-amd64.exe` | Desktop app (Windows) |
| `VideoRemix-macos.app.zip` | Desktop app (macOS `.app`, zipped) |
| `VideoRemix-linux-amd64` | Desktop app (Linux) |
| `videoremix-windows-amd64.exe` | CLI (Windows) |
| `videoremix-macos-arm64` | CLI (macOS Apple Silicon) |
| `videoremix-linux-amd64` | CLI (Linux) |

Each build embeds the correct OS-specific ffmpeg/ffprobe/yt-dlp, so **your
customers install nothing** — they download one file and run it.

## How to cut a release

```bash
git tag v1.0.0
git push origin v1.0.0
```

That's it. GitHub Actions will:
1. Spin up Windows, macOS, and Linux runners in parallel.
2. Download the OS-specific ffmpeg/ffprobe/yt-dlp into `internal/binaries/embedded/`.
3. Build the CLI and the Wails desktop app with `-tags embed_binaries`.
4. Create a **GitHub Release** for the tag and attach all six artifacts.

Find the files under the repo's **Releases** page and send the relevant one to
each customer.

## Manual run (no tag / no release)

From the GitHub **Actions** tab, choose **Build & Release → Run workflow**. It
builds everything and uploads the artifacts to the run (downloadable from the
run page) without creating a public Release.

## Local builds (single OS)

You can still build locally for your own OS:

```powershell
# Windows
./scripts/fetch-binaries.ps1
go build -tags embed_binaries -o videoremix-portable.exe ./cmd/videoremix
cd desktop; wails build -tags embed_binaries
```

```bash
# macOS / Linux
bash scripts/fetch-binaries.sh
go build -tags embed_binaries -o videoremix ./cmd/videoremix
cd desktop && wails build -tags embed_binaries
```

## Notes / limitations

- **Desktop apps cannot be cross-compiled.** A macOS `.app` must be built on
  macOS, Linux on Linux, etc. This is why we use CI runners.
- **The CLI can be cross-compiled** for the dev (non-embedded) case, but the
  embedded portable CLI still needs the target OS's ffmpeg, so CI is used for
  consistency.
- **Code signing / notarization** (so macOS/Windows don't warn users) is not set
  up here. For public distribution you'd add an Apple Developer ID / Windows
  code-signing certificate to the workflow. Ask if you want this added.
- The macOS CLI is built for **arm64 (Apple Silicon)**. Add an `amd64` matrix
  entry if you also need Intel Macs.
