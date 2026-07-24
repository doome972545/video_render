# videoremix

A content-aware video remix pipeline in Go. One source video fans out into many
unique variant renders through an ordered, hexagonal-architecture pipeline:

```
Download -> Analyze -> Recipe -> Variant -> Queue -> Render
```

Each stage is an independent, testable module conforming to a shared `Stage`
contract, orchestrated by the `Engine` facade (`StartJob` / `CancelJob` /
`GetStatus`).

## Requirements

- Go 1.26+
- `ffmpeg`, `ffprobe`, `yt-dlp` — either on your `PATH`, in a `bin/` folder next
  to the executable, or embedded into a portable build (see below).

## Quick start

```powershell
# Build a small dev binary (relies on ffmpeg/yt-dlp on PATH)
go build -o videoremix.exe ./cmd/videoremix
# ...or use the helper: ./scripts/build.ps1 -Mode dev

# Render 3 variants from a local file
./videoremix.exe --source "work\sample.mp4" --variants 3
```

## Usage

| Flag | Default | Description |
|---|---|---|
| `--source` | *(required)* | Source URL (YouTube/TikTok/Instagram/Facebook) or local file path |
| `--variants` | `5` | Number of variants to generate |
| `--seed` | current time | Master seed; the same seed reproduces the same variant set |
| `--workdir` | `work` | Working directory for downloads |
| `--out` | `output` | Output directory for rendered videos |
| `--gpu` | `false` | Prefer GPU (nvenc) encoding when available |
| `--concurrency` | `4` | Max parallel renders |
| `--music` | | Background music file to mix in (optional) |
| `--music-volume` | `0.3` | Background music volume (0..1) |
| `--subtitle` | | Caption text burned into the video (optional) |
| `--subtitle-pos` | `bottom` | Subtitle position: `bottom`, `top`, `center` |
| `--effects` | `false` | Enable the wider random effect set (blur/zoom/hue/audio) |

```powershell
# Add background music, a burned-in caption, and richer random effects
./videoremix.exe --source "clip.mp4" --variants 10 --effects `
  --music "song.mp3" --music-volume 0.25 --subtitle "My Channel" --subtitle-pos bottom
```

```powershell
# Reproducible run
./videoremix.exe --source "video.mp4" --variants 10 --seed 42

# Download from YouTube, 50 variants, 8 parallel renders on GPU
./videoremix.exe --source "https://youtube.com/watch?v=xxxx" --variants 50 --gpu --concurrency 8
```

## Development

```powershell
go run ./cmd/videoremix --source "work\sample.mp4" --variants 3   # run without building
go test ./...                                                     # unit tests (no ffmpeg needed)
go vet ./...                                                      # static checks
```

### Project layout

```
cmd/videoremix/       CLI front end (thin wrapper over pkg/app)
pkg/app/              Reusable, UI-agnostic core (import this from Wails/HTTP/etc.)
internal/
  pipeline/   Stage, Context (immutable/append-only), Runner
  download/   RawVideo, InputClassifier, platform fetchers, validator
  analyze/    AnalysisResult, detectors, fingerprinter, cache
  recipe/     Recipe entity, generator/validator/optimizer/serializer/store
  variant/    seed gen, hook perturber, structural duplicate detector
  render/     timeline, filter-graph compiler, ffmpeg executor, output validator
  queue/      job state machine, worker pool, retry policy, dead-letter
  engine/     facade orchestrating all stages
  binaries/   embed/resolve ffmpeg/ffprobe/yt-dlp
scripts/      helper scripts (build.ps1, fetch-binaries.ps1)
```

> A ready-to-run **Wails desktop app** lives in [`desktop/`](desktop/). It imports
> `videoremix/pkg/app` and ships ffmpeg embedded so end users install nothing.
> See **[INTEGRATION.md](INTEGRATION.md)** for how it's wired and how to embed the
> core into your own Wails/HTTP app.

### Desktop app (Wails)

```powershell
cd desktop
wails dev                          # live-reload during development
wails build -tags embed_binaries   # self-contained desktop .exe (~490 MB)
# output: desktop/build/bin/desktop.exe
```

Requires the [Wails CLI](https://wails.io) and Node.js. Run
`scripts/fetch-binaries.ps1` once before an embedded build so the tools are
present.

### Common tasks

- **Add an effect**: add a mapping in `render.DefaultEffectRegistry()` and a
  base step (with `Hooks` for randomization) in `defaultRuleSet()` in
  `cmd/videoremix/main.go`.
- **Add a download platform**: implement `download.PlatformFetcher` and register
  it in `main.go`.
- **Change remix rules**: edit `defaultRuleSet()` in `main.go`.

## Portable build (self-contained, ~484 MB)

The tool executables are not committed to git. Fetch them into
`internal/binaries/embedded/` first, then build with the embed tag:

```powershell
# 1. Populate internal/binaries/embedded/ (installs via winget, then copies)
./scripts/fetch-binaries.ps1

# 2. Build the portable, single-file binary
go build -tags embed_binaries -o videoremix-portable.exe ./cmd/videoremix
```

On first run the portable binary extracts its embedded tools to
`%LOCALAPPDATA%\videoremix\bin-<hash>\` and reuses them on subsequent runs. Copy
`videoremix-portable.exe` to any Windows machine and run it — no installation of
ffmpeg/yt-dlp required.

> The embedded executables are Windows-only. To target Linux/macOS, place the
> corresponding OS binaries in `internal/binaries/embedded/` before building.

## Binary resolution order

At runtime the tool locates ffmpeg/ffprobe/yt-dlp in this order:

1. **Embedded** binary (portable build) — extracted once to a per-user cache.
2. **`bin/`** directory next to the executable (bundle mode).
3. **System `PATH`** (development / pre-installed environments).

## Notes on git

Large third-party binaries are intentionally excluded from the repository:

- `*.exe`, `work/`, `output/` are gitignored.
- `internal/binaries/embedded/*` is ignored except the committed `README.txt`
  placeholder (which keeps the directory present so `//go:embed embedded/*`
  compiles on a fresh clone).

A fresh clone builds and runs the small dev binary immediately; run
`scripts/fetch-binaries.ps1` only when you want a portable build.
