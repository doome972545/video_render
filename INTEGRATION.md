# Integration Guide

This project is split into two layers so it can be reused anywhere:

- **`pkg/app`** — the reusable, UI-agnostic core. Import this from any Go
  program (Wails, HTTP server, another CLI). It has **no dependency on Wails or
  any GUI framework**.
- **`cmd/videoremix`** — the reference CLI front end. It's a thin wrapper around
  `pkg/app` and a good example of how to drive the core.

The core also owns binary resolution (`internal/binaries`), so a portable build
(`-tags embed_binaries`) carries ffmpeg/ffprobe/yt-dlp inside the final
executable — the end user installs nothing.

---

## The core API (`pkg/app`)

```go
// Construct once, reuse for many jobs, Close when done.
svc, err := app.New(app.Config{
    WorkDir:     "work",
    OutputDir:   "output",
    PreferGPU:   false,
    Concurrency: 4,
    OnStatus:    func(u app.StatusUpdate) { /* push to UI */ },
    OnLog:       func(line string)        { /* progress logs */ },
})
defer svc.Close()

id, err := svc.StartJob(app.JobRequest{
    Source:       "https://youtube.com/watch?v=xxxx", // or a local path
    VariantCount: 10,
    Seed:         42,   // 0 = time-based (non-reproducible)
    Priority:     app.PriorityNormal,
})

// Option A: react to OnStatus callbacks (best for GUIs / event systems).
// Option B: poll svc.Status(id).
// Option C: block until finished.
final, err := svc.WaitForJob(context.Background(), id)

svc.Cancel(id) // cancel any time
```

`StatusUpdate` is JSON-friendly (`jobId`, `phase`, `total`, `completed`,
`failed`, `percent`, `error`, ...) so it maps cleanly onto GUI/IPC events.

### Customizing the remix rules

Pass a `RulesProvider` to change which effects are applied and how they're
randomized, without touching the core:

```go
svc, _ := app.New(app.Config{
    RulesProvider: func() (recipe.RuleSet, variant.DistributionRules) {
        rules, dist := app.DefaultRules() // start from defaults
        rules.BaseEffects = append(rules.BaseEffects, recipe.EffectStep{
            EffectID: "brightness",
            Params:   map[string]float64{"value": 0},
            Hooks:    map[string]recipe.Range{"value": {Min: -0.3, Max: 0.3}},
        })
        return rules, dist
    },
})
```

---

## Using it from a Wails desktop app

A **working Wails desktop app already lives in `desktop/`** in this repo. It
imports `videoremix/pkg/app`, binds it to a small web UI, and streams progress
via Wails events. Use it as-is or as a reference. The sections below explain how
it's wired.

Run it:

```powershell
cd desktop
wails dev                          # live-reload dev mode
wails build -tags embed_binaries   # produce a self-contained desktop .exe
```

The built app lands in `desktop/build/bin/desktop.exe`. With the embed tag it is
~490 MB and needs nothing installed on the user's machine.

### 1. How the desktop module imports the core

`desktop/` is its own Go module that points back at the core via a replace
directive in `desktop/go.mod`:

```
require videoremix v0.0.0
replace videoremix => ../
```

That's all it takes to reuse the entire pipeline — no code is duplicated.

### 2. Bind the core in `app.go`

```go
package main

import (
    "context"

    core "videoremix/pkg/app"
    "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails-bound struct.
type App struct {
    ctx context.Context
    svc *core.Service
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
    a.ctx = ctx
    a.svc, _ = core.New(core.Config{
        WorkDir:     "work",
        OutputDir:   "output",
        Concurrency: 4,
        // Forward every status change to the frontend as a Wails event.
        OnStatus: func(u core.StatusUpdate) {
            runtime.EventsEmit(a.ctx, "job:status", u)
        },
        OnLog: func(line string) {
            runtime.EventsEmit(a.ctx, "job:log", line)
        },
    })
}

func (a *App) shutdown(ctx context.Context) {
    if a.svc != nil {
        a.svc.Close()
    }
}

// --- Methods exposed to JavaScript ---

func (a *App) StartJob(source string, variants int, seed int64) (string, error) {
    id, err := a.svc.StartJob(core.JobRequest{
        Source:       source,
        VariantCount: variants,
        Seed:         seed,
    })
    return string(id), err
}

func (a *App) GetStatus(id string) (core.StatusUpdate, error) {
    return a.svc.Status(core.JobID(id))
}

func (a *App) CancelJob(id string) error {
    return a.svc.Cancel(core.JobID(id))
}
```

### 3. Wire startup/shutdown in `main.go`

```go
app := NewApp()
wails.Run(&options.App{
    Title:     "VideoRemix",
    OnStartup:  app.startup,
    OnShutdown: app.shutdown,
    Bind:       []interface{}{app},
})
```

### 4. Frontend (JavaScript)

```js
import { StartJob, CancelJob } from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime";

EventsOn("job:status", (u) => {
    console.log(u.phase, u.percent + "%");
    // update your progress bar
});

async function run() {
    const id = await StartJob("video.mp4", 10, 42);
    console.log("started", id);
}
```

### 5. Build a desktop app that needs nothing installed

Because the core resolves ffmpeg/yt-dlp via `internal/binaries`, build the Wails
app with the embed tag so the tools ship inside the app bundle:

```bash
# populate the embedded binaries once (Windows)
./scripts/fetch-binaries.ps1

# build the desktop app with tools embedded
wails build -tags embed_binaries
```

On first launch the app extracts its embedded tools to a per-user cache and
reuses them afterward. The end user downloads the app and runs it — no ffmpeg,
no yt-dlp, no Go, nothing to install.

> The embedded executables are OS-specific. Ship a Windows build with the
> Windows `.exe` tools, a macOS build with macOS tools, etc. Place the correct
> binaries in `internal/binaries/embedded/` before building for that OS.

---

## Using it from an HTTP server

```go
svc, _ := app.New(app.Config{WorkDir: "work", OutputDir: "output"})
defer svc.Close()

http.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
    var req app.JobRequest
    json.NewDecoder(r.Body).Decode(&req)
    id, err := svc.StartJob(req)
    if err != nil { http.Error(w, err.Error(), 400); return }
    json.NewEncoder(w).Encode(map[string]string{"jobId": string(id)})
})

http.HandleFunc("/jobs/status", func(w http.ResponseWriter, r *http.Request) {
    st, err := svc.Status(app.JobID(r.URL.Query().Get("id")))
    if err != nil { http.Error(w, err.Error(), 404); return }
    json.NewEncoder(w).Encode(st) // StatusUpdate is JSON-ready
})
```

---

## Summary

| You want | Do this |
|---|---|
| CLI | Use `cmd/videoremix` (already built) |
| Desktop app | New Wails project imports `videoremix/pkg/app`; bind `Service` methods; emit `OnStatus` as Wails events; build with `-tags embed_binaries` |
| HTTP/gRPC service | Import `pkg/app`, expose `StartJob`/`Status`/`Cancel` |
| No install for end users | Build with `-tags embed_binaries` after `scripts/fetch-binaries.ps1` |
| Custom effects/rules | Pass `Config.RulesProvider` |
```
