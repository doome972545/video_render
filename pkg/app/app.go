// Package app is the reusable, UI-agnostic core of the video remix pipeline.
//
// It wires together all internal stages (Download → Analyze → Recipe → Variant
// → Queue → Render) behind a small, stable public API that any front end — the
// CLI, a Wails desktop app, or an HTTP server — can import and drive without
// knowing anything about the internal stage types.
//
//	svc, _ := app.New(app.Config{WorkDir: "work", OutputDir: "output"})
//	defer svc.Close()
//	id, _ := svc.StartJob(app.JobRequest{Source: "video.mp4", VariantCount: 5})
//	status, _ := svc.WaitForJob(context.Background(), id)
//
// The core has no dependency on Wails or any GUI framework; front ends observe
// progress via OnStatus callbacks (ideal for emitting Wails runtime events).
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"videoremix/internal/analyze"
	"videoremix/internal/download"
	"videoremix/internal/engine"
	"videoremix/internal/pipeline"
	"videoremix/internal/queue"
	"videoremix/internal/recipe"
	"videoremix/internal/render"
	"videoremix/internal/variant"
)

// Config configures a Service. Zero values fall back to sane defaults.
type Config struct {
	// WorkDir is where downloaded/source files are placed.
	WorkDir string
	// OutputDir is where rendered MP4s are written.
	OutputDir string
	// PreferGPU opportunistically uses GPU (nvenc) encoding when available.
	PreferGPU bool
	// Concurrency is the max number of parallel renders.
	Concurrency int
	// RulesProvider optionally supplies a custom RuleSet + distribution. When
	// nil, DefaultRules is used.
	RulesProvider func() (recipe.RuleSet, variant.DistributionRules)
	// OnStatus, when set, is invoked on every job status change. Front ends
	// (e.g. Wails) can emit runtime events from here. It must be non-blocking.
	OnStatus func(StatusUpdate)
	// OnLog, when set, receives human-readable progress lines (e.g. download
	// progress). Optional.
	OnLog func(string)
}

func (c *Config) applyDefaults() {
	if c.WorkDir == "" {
		c.WorkDir = "work"
	}
	if c.OutputDir == "" {
		c.OutputDir = "output"
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.RulesProvider == nil {
		c.RulesProvider = DefaultRules
	}
}

// JobRequest is a single remix submission.
type JobRequest struct {
	// Source is a URL (YouTube/TikTok/Instagram/Facebook) or a local file path.
	Source string
	// VariantCount is how many unique variants to generate. Defaults to 1.
	VariantCount int
	// Seed makes generation reproducible. Zero uses the current time.
	Seed int64
	// Priority orders the render batch relative to others.
	Priority Priority
}

// Priority mirrors the queue priority levels for callers.
type Priority int

const (
	PriorityLow    Priority = Priority(queue.PriorityLow)
	PriorityNormal Priority = Priority(queue.PriorityNormal)
	PriorityHigh   Priority = Priority(queue.PriorityHigh)
)

// Phase is the high-level lifecycle phase of a job.
type Phase string

const (
	PhaseDownloading Phase = Phase(engine.PhaseDownloading)
	PhaseAnalyzing   Phase = Phase(engine.PhaseAnalyzing)
	PhaseRecipe      Phase = Phase(engine.PhaseRecipe)
	PhaseVariant     Phase = Phase(engine.PhaseVariant)
	PhaseRendering   Phase = Phase(engine.PhaseRendering)
	PhaseCompleted   Phase = Phase(engine.PhaseCompleted)
	PhaseFailed      Phase = Phase(engine.PhaseFailed)
	PhaseCancelled   Phase = Phase(engine.PhaseCancelled)
)

// JobID is the public handle for a submitted job.
type JobID string

// StatusUpdate is a snapshot of a job's progress, JSON-friendly for GUI/IPC.
type StatusUpdate struct {
	JobID     JobID   `json:"jobId"`
	Phase     Phase   `json:"phase"`
	Total     int     `json:"total"`
	Completed int     `json:"completed"`
	Failed    int     `json:"failed"`
	Pending   int     `json:"pending"`
	Running   int     `json:"running"`
	Cancelled int     `json:"cancelled"`
	Percent   float64 `json:"percent"`
	Error     string  `json:"error,omitempty"`
}

// Terminal reports whether the job has reached a final phase.
func (s StatusUpdate) Terminal() bool {
	return s.Phase == PhaseCompleted || s.Phase == PhaseFailed || s.Phase == PhaseCancelled
}

// Service is the reusable core. Construct with New; always Close when done.
type Service struct {
	cfg         Config
	engine      *engine.Engine
	queueSvc    *queue.Service
	recipeStore recipe.Store

	mu            sync.Mutex
	pollDone      chan struct{}
	pollerStarted bool
	closed        bool
}

// New constructs a Service with all stages wired. It does not start any job.
func New(cfg Config) (*Service, error) {
	cfg.applyDefaults()

	recipeStore := recipe.NewMemoryStore()
	jobStore := queue.NewMemoryJobStore()
	reporter := queue.NewChannelReporter()

	s := &Service{
		cfg:         cfg,
		recipeStore: recipeStore,
		pollDone:    make(chan struct{}),
	}

	// One RawVideo captured per process run so the render dispatcher can
	// resolve the source without re-downloading.
	var capturedRaw download.RawVideo

	buildStages := func(in engine.JobInput, ctx context.Context) ([]pipeline.Stage, error) {
		return s.buildStages(in, &capturedRaw)
	}

	renderer := render.NewRenderer(
		render.NewChainCompiler(render.DefaultEffectRegistry()),
		render.NewGPUProbe(),
		render.NewCLIFFmpegExecutor(),
		render.NewFileOutputValidator(),
		render.RenderConfig{PreferGPU: cfg.PreferGPU, Preset: "medium", OutputDir: cfg.OutputDir},
		func(sourceRef string) (download.RawVideo, error) {
			if capturedRaw.FilePath == "" {
				return download.RawVideo{}, fmt.Errorf("source not resolved for ref %s", sourceRef)
			}
			return capturedRaw, nil
		},
	)

	dispatcher := &lazyDispatcher{renderer: renderer, recipes: recipeStore, raw: &capturedRaw}
	s.queueSvc = queue.NewService(jobStore, dispatcher, reporter, queue.DefaultRetryPolicy(), cfg.Concurrency)
	s.engine = engine.NewEngine(buildStages, s.queueSvc)

	return s, nil
}

// StartJob submits a remix job and returns immediately with a JobID. Rendering
// proceeds asynchronously; observe progress via Config.OnStatus, Status, or
// WaitForJob.
func (s *Service) StartJob(req JobRequest) (JobID, error) {
	if req.VariantCount <= 0 {
		req.VariantCount = 1
	}
	if req.Seed == 0 {
		req.Seed = time.Now().UnixNano()
	}

	id, err := s.engine.StartJob(engine.JobInput{
		Source:       req.Source,
		VariantCount: req.VariantCount,
		Priority:     queue.Priority(req.Priority),
		MasterSeed:   recipe.Seed(req.Seed),
	})
	if err != nil {
		// Emit a failed status so callers relying purely on OnStatus still see it.
		s.emit(s.snapshot(id))
		return JobID(id), err
	}

	// Start (or ensure) the background poller that pushes OnStatus updates.
	s.ensurePoller()
	s.emit(s.snapshot(id))
	return JobID(id), nil
}

// Status returns the current status snapshot for a job.
func (s *Service) Status(id JobID) (StatusUpdate, error) {
	return s.snapshotErr(engine.JobID(id))
}

// Cancel requests cancellation of a running job.
func (s *Service) Cancel(id JobID) error {
	return s.engine.CancelJob(engine.JobID(id))
}

// WaitForJob blocks until the job reaches a terminal phase or ctx is done.
func (s *Service) WaitForJob(ctx context.Context, id JobID) (StatusUpdate, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		st, err := s.snapshotErr(engine.JobID(id))
		if err != nil {
			return st, err
		}
		if st.Terminal() {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close shuts down the queue workers. Safe to call once.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.pollDone)
	s.mu.Unlock()
	s.queueSvc.Shutdown()
}

// --- internals ---

func (s *Service) buildStages(in engine.JobInput, capturedRaw *download.RawVideo) ([]pipeline.Stage, error) {
	dlCfg := download.DefaultConfig(s.cfg.WorkDir)
	validator := download.NewFFprobeValidator(dlCfg)

	progress := make(chan download.ProgressEvent, 64)
	go func() {
		for ev := range progress {
			if ev.Message != "" && s.cfg.OnLog != nil {
				s.cfg.OnLog(fmt.Sprintf("download: %.0f%% %s", ev.Percent, ev.Message))
			}
		}
	}()

	downloader := download.NewDownloader(
		dlCfg, validator, progress,
		download.NewYouTubeFetcher(s.cfg.WorkDir),
		download.NewTikTokFetcher(s.cfg.WorkDir),
		download.NewInstagramFetcher(s.cfg.WorkDir),
		download.NewFacebookFetcher(s.cfg.WorkDir),
		download.NewLocalFileLoader(),
	)

	captureStage := stageFunc{
		name: "Download",
		fn: func(pc pipeline.Context) (pipeline.Context, error) {
			out, err := downloader.Execute(pc)
			if err != nil {
				return out, err
			}
			if v, ok := out.Get(pipeline.KeyRawVideo); ok {
				if raw, ok := v.(download.RawVideo); ok {
					*capturedRaw = raw
				}
			}
			return out, nil
		},
	}

	silence := analyze.NewSilenceDetector(-30, 500*time.Millisecond)
	analyzer := analyze.NewAnalyzer(
		analyze.NewChecksumFingerprinter(),
		analyze.NewFFprobeMetadata(),
		analyze.NewMemoryCache(),
		analyze.NewSceneDetector(0.4),
		silence,
		analyze.NewSpeechDetector(silence),
		analyze.NewNoopFaceDetector(),
		analyze.NewUniformMotionDetector(),
	)

	rules, dist := s.cfg.RulesProvider()

	recipeStage := recipe.NewRecipeStage(
		recipe.NewRuleBasedGenerator(),
		recipe.NewRuleValidator(),
		recipe.NewStepMergeOptimizer(),
		recipe.NewJSONSerializer(),
		s.recipeStore,
		recipe.NewMemoryCache(),
		rules,
		in.MasterSeed,
	)

	variantStage := variant.NewVariantStage(
		variant.NewPRNGSeedGen(),
		variant.NewHookPerturber(),
		recipe.NewRuleValidator(),
		recipe.NewStepMergeOptimizer(),
		recipe.NewJSONSerializer(),
		variant.NewStructuralDuplicateDetector(),
		s.recipeStore,
		in.VariantCount,
		in.MasterSeed,
		dist,
		variant.DefaultConfig(),
	)

	return []pipeline.Stage{captureStage, analyzer, recipeStage, variantStage}, nil
}

// snapshot maps engine JobStatus to a public StatusUpdate (ignoring not-found).
func (s *Service) snapshot(id engine.JobID) StatusUpdate {
	st, _ := s.snapshotErr(id)
	return st
}

func (s *Service) snapshotErr(id engine.JobID) (StatusUpdate, error) {
	js, err := s.engine.GetStatus(id)
	if err != nil {
		return StatusUpdate{JobID: JobID(id)}, err
	}
	p := js.Progress
	percent := 0.0
	if p.Total > 0 {
		percent = float64(p.Completed+p.Failed+p.Cancelled) / float64(p.Total) * 100
	}
	return StatusUpdate{
		JobID:     JobID(id),
		Phase:     Phase(js.Phase),
		Total:     p.Total,
		Completed: p.Completed,
		Failed:    p.Failed,
		Pending:   p.Pending,
		Running:   p.Running,
		Cancelled: p.Cancelled,
		Percent:   percent,
		Error:     js.Error,
	}, nil
}

func (s *Service) emit(st StatusUpdate) {
	if s.cfg.OnStatus != nil {
		s.cfg.OnStatus(st)
	}
}

// ensurePoller starts a single background loop that pushes status updates for
// all active jobs to OnStatus, so front ends don't have to poll themselves.
func (s *Service) ensurePoller() {
	if s.cfg.OnStatus == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pollerStarted {
		return
	}
	s.pollerStarted = true
	go s.pollLoop()
}

func (s *Service) pollLoop() {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	lastPhase := map[engine.JobID]Phase{}
	lastDone := map[engine.JobID]int{}
	for {
		select {
		case <-s.pollDone:
			return
		case <-ticker.C:
			for _, id := range s.engine.ActiveJobs() {
				st, err := s.snapshotErr(id)
				if err != nil {
					continue
				}
				done := st.Completed + st.Failed + st.Cancelled
				// Only emit on meaningful change to avoid event spam.
				if lastPhase[id] != st.Phase || lastDone[id] != done {
					lastPhase[id] = st.Phase
					lastDone[id] = done
					s.emit(st)
				}
			}
		}
	}
}
