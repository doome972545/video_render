package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	core "videoremix/pkg/app"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails-bound struct. Its exported methods are callable from the
// frontend (JavaScript). All video work is delegated to the reusable core
// (videoremix/pkg/app) — this file only translates between the GUI and core.
type App struct {
	ctx context.Context

	mu        sync.Mutex
	svc       *core.Service
	outputDir string // the output dir the current svc was built for
}

// NewApp creates a new App application struct.
func NewApp() *App { return &App{} }

// startup is called when the app starts. It stores the Wails context; the core
// Service is created lazily so the output directory can be chosen per job.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// shutdown is called on app close; it stops the queue workers cleanly.
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.svc != nil {
		a.svc.Close()
	}
}

// service returns a core.Service configured for the given output directory,
// (re)creating it when the output directory changes. The core Service is cheap
// to construct, so swapping it on output change avoids any core API changes.
func (a *App) service(outputDir string) (*core.Service, error) {
	if outputDir == "" {
		outputDir = filepath.Join(".", "output")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.svc != nil && a.outputDir == outputDir {
		return a.svc, nil
	}
	// Output changed (or first use): rebuild the Service.
	if a.svc != nil {
		a.svc.Close()
		a.svc = nil
	}
	svc, err := core.New(core.Config{
		WorkDir:     filepath.Join(".", "work"),
		OutputDir:   outputDir,
		Concurrency: 4,
		OnStatus: func(u core.StatusUpdate) {
			runtime.EventsEmit(a.ctx, "job:status", u)
		},
		OnLog: func(line string) {
			runtime.EventsEmit(a.ctx, "job:log", line)
		},
	})
	if err != nil {
		return nil, err
	}
	a.svc = svc
	a.outputDir = outputDir
	return svc, nil
}

// --- Methods exposed to the frontend ---

// StartJobOptions carries all remix parameters from the frontend in one object
// (Wails binds this as a JS object), keeping the method signature stable.
type StartJobOptions struct {
	Source       string `json:"source"`
	Variants     int    `json:"variants"`
	Seed         int64  `json:"seed"`
	OutputDir    string `json:"outputDir"`
	MusicPath    string `json:"musicPath"`
	MusicVolume  float64 `json:"musicVolume"`
	Subtitle     string `json:"subtitle"`
	SubtitlePos  string `json:"subtitlePos"`
	ExtraEffects bool   `json:"extraEffects"`
}

// StartJob submits a remix job with full options and returns its JobID.
func (a *App) StartJob(o StartJobOptions) (string, error) {
	svc, err := a.service(o.OutputDir)
	if err != nil {
		return "", fmt.Errorf("init core: %w", err)
	}
	id, err := svc.StartJob(core.JobRequest{
		Source:       o.Source,
		VariantCount: o.Variants,
		Seed:         o.Seed,
		Priority:     core.PriorityNormal,
		Options: core.RemixOptions{
			MusicPath:        o.MusicPath,
			MusicVolume:      o.MusicVolume,
			SubtitleText:     o.Subtitle,
			SubtitlePosition: o.SubtitlePos,
			ExtraEffects:     o.ExtraEffects,
		},
	})
	return string(id), err
}

// GetStatus returns the current status snapshot for a job.
func (a *App) GetStatus(id string) (core.StatusUpdate, error) {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return core.StatusUpdate{}, fmt.Errorf("no active service")
	}
	return svc.Status(core.JobID(id))
}

// CancelJob requests cancellation of a running job.
func (a *App) CancelJob(id string) error {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return fmt.Errorf("no active service")
	}
	return svc.Cancel(core.JobID(id))
}

// PickFile opens a native file dialog and returns the chosen video path.
func (a *App) PickFile() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select a video file",
		Filters: []runtime.FileFilter{
			{DisplayName: "Videos", Pattern: "*.mp4;*.mkv;*.mov;*.webm;*.avi"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
}

// PickMusic opens a native file dialog for choosing a background music file.
func (a *App) PickMusic() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select a music file",
		Filters: []runtime.FileFilter{
			{DisplayName: "Audio", Pattern: "*.mp3;*.m4a;*.aac;*.wav;*.ogg;*.flac"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
}

// PickOutputDir opens a native folder dialog and returns the chosen directory.
func (a *App) PickOutputDir() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select an output folder",
	})
}

// OpenOutputDir opens the current output folder in the system file explorer.
func (a *App) OpenOutputDir() {
	a.mu.Lock()
	dir := a.outputDir
	a.mu.Unlock()
	if dir == "" {
		dir = "output"
	}
	abs, _ := filepath.Abs(dir)
	runtime.BrowserOpenURL(a.ctx, "file:///"+filepath.ToSlash(abs))
}
