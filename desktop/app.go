package main

import (
	"context"
	"path/filepath"

	core "videoremix/pkg/app"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails-bound struct. Its exported methods are callable from the
// frontend (JavaScript). All video work is delegated to the reusable core
// (videoremix/pkg/app) — this file only translates between the GUI and core.
type App struct {
	ctx context.Context
	svc *core.Service
}

// NewApp creates a new App application struct.
func NewApp() *App { return &App{} }

// startup is called when the app starts. It constructs the core Service and
// forwards every status/log update to the frontend as Wails runtime events.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	workDir := filepath.Join(".", "work")
	outDir := filepath.Join(".", "output")

	svc, err := core.New(core.Config{
		WorkDir:     workDir,
		OutputDir:   outDir,
		Concurrency: 4,
		OnStatus: func(u core.StatusUpdate) {
			runtime.EventsEmit(a.ctx, "job:status", u)
		},
		OnLog: func(line string) {
			runtime.EventsEmit(a.ctx, "job:log", line)
		},
	})
	if err != nil {
		runtime.LogErrorf(a.ctx, "init core: %v", err)
		return
	}
	a.svc = svc
}

// shutdown is called on app close; it stops the queue workers cleanly.
func (a *App) shutdown(ctx context.Context) {
	if a.svc != nil {
		a.svc.Close()
	}
}

// --- Methods exposed to the frontend ---

// StartJob submits a remix job and returns its JobID immediately.
func (a *App) StartJob(source string, variants int, seed int64) (string, error) {
	id, err := a.svc.StartJob(core.JobRequest{
		Source:       source,
		VariantCount: variants,
		Seed:         seed,
		Priority:     core.PriorityNormal,
	})
	return string(id), err
}

// GetStatus returns the current status snapshot for a job.
func (a *App) GetStatus(id string) (core.StatusUpdate, error) {
	return a.svc.Status(core.JobID(id))
}

// CancelJob requests cancellation of a running job.
func (a *App) CancelJob(id string) error {
	return a.svc.Cancel(core.JobID(id))
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

// OpenOutputDir opens the output folder in the system file explorer.
func (a *App) OpenOutputDir() {
	abs, _ := filepath.Abs("output")
	runtime.BrowserOpenURL(a.ctx, "file:///"+filepath.ToSlash(abs))
}
