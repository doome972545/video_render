// Package engine is the single entry point owning the lifecycle of a video
// remix job from submission to completion. It orchestrates Download → Analyze →
// Recipe → Variant synchronously, then fans out to Queue asynchronously,
// exposing exactly three methods: StartJob, CancelJob, GetStatus.
package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"videoremix/internal/download"
	"videoremix/internal/pipeline"
	"videoremix/internal/queue"
	"videoremix/internal/recipe"
)

// JobInput is the caller-supplied submission.
type JobInput struct {
	Source       string        // URL or local path
	VariantCount int           // number of variants to generate
	Priority     queue.Priority
	MasterSeed   recipe.Seed
}

// JobID is the public handle for a submitted job.
type JobID string

// JobPhase describes where a job is in its lifecycle.
type JobPhase string

const (
	PhaseDownloading JobPhase = "downloading"
	PhaseAnalyzing   JobPhase = "analyzing"
	PhaseRecipe      JobPhase = "recipe"
	PhaseVariant     JobPhase = "variant"
	PhaseRendering   JobPhase = "rendering"
	PhaseCompleted   JobPhase = "completed"
	PhaseFailed      JobPhase = "failed"
	PhaseCancelled   JobPhase = "cancelled"
)

// JobStatus is the aggregate status returned to callers.
type JobStatus struct {
	JobID    JobID
	Phase    JobPhase
	Batch    queue.BatchID
	Progress queue.ProgressSnapshot
	Error    string
}

// QueueService is the subset of queue behavior Engine depends on.
type QueueService interface {
	EnqueueBatch(recipes []recipe.Recipe, priority queue.Priority) (queue.BatchID, error)
	CancelBatch(batch queue.BatchID) error
	BatchProgress(batch queue.BatchID) (queue.ProgressSnapshot, error)
}

// jobRecord is Engine's internal per-job bookkeeping.
type jobRecord struct {
	status JobStatus
	cancel context.CancelFunc
}

// Engine is the Facade over the entire pipeline.
type Engine struct {
	// buildStages constructs the synchronous stage list (Download..Variant)
	// for a specific input + cancellation context.
	buildStages func(input JobInput, ctx context.Context) ([]pipeline.Stage, error)
	queue       QueueService

	mu   sync.RWMutex
	jobs map[JobID]*jobRecord
	seq  int
}

// NewEngine constructs the facade. buildStages injects stage wiring so Engine
// never imports concrete stage implementations directly.
func NewEngine(
	buildStages func(input JobInput, ctx context.Context) ([]pipeline.Stage, error),
	q QueueService,
) *Engine {
	return &Engine{
		buildStages: buildStages,
		queue:       q,
		jobs:        map[JobID]*jobRecord{},
	}
}

// StartJob accepts a submission, drives stages 1–4 synchronously, enqueues the
// variant batch, and returns a JobID immediately (rendering is async).
func (e *Engine) StartJob(input JobInput) (JobID, error) {
	if input.VariantCount <= 0 {
		input.VariantCount = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	id := e.newJob(cancel)

	stages, err := e.buildStages(input, ctx)
	if err != nil {
		cancel()
		e.setPhase(id, PhaseFailed, "", fmt.Sprintf("wiring: %v", err))
		return id, err
	}

	e.setPhase(id, PhaseDownloading, "", "")

	initial := pipeline.NewContext(ctx).With(pipeline.KeyInput, input.Source)
	runner := pipeline.NewRunner(stages...)
	final, err := runner.Run(initial)
	if err != nil {
		e.setPhase(id, e.phaseFromError(err), "", err.Error())
		return id, err
	}

	// Extract the variant batch and enqueue for rendering.
	vv, ok := final.Get(pipeline.KeyVariants)
	if !ok {
		e.setPhase(id, PhaseFailed, "", "no variants produced")
		return id, fmt.Errorf("engine: no variants produced")
	}
	recipes, ok := vv.([]recipe.Recipe)
	if !ok {
		e.setPhase(id, PhaseFailed, "", "variants have unexpected type")
		return id, fmt.Errorf("engine: variants have unexpected type")
	}

	batch, err := e.queue.EnqueueBatch(recipes, input.Priority)
	if err != nil {
		e.setPhase(id, PhaseFailed, "", fmt.Sprintf("enqueue: %v", err))
		return id, err
	}
	e.setPhase(id, PhaseRendering, batch, "")
	return id, nil
}

// CancelJob propagates cancellation to the active stage and the render batch.
func (e *Engine) CancelJob(id JobID) error {
	e.mu.Lock()
	rec, ok := e.jobs[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("engine: unknown job %s", id)
	}
	if rec.cancel != nil {
		rec.cancel()
	}
	if rec.status.Batch != "" {
		_ = e.queue.CancelBatch(rec.status.Batch)
	}
	e.setPhase(id, PhaseCancelled, rec.status.Batch, "")
	return nil
}

// GetStatus returns aggregate status, refreshing render progress from Queue.
func (e *Engine) GetStatus(id JobID) (JobStatus, error) {
	e.mu.RLock()
	rec, ok := e.jobs[id]
	e.mu.RUnlock()
	if !ok {
		return JobStatus{}, fmt.Errorf("engine: unknown job %s", id)
	}

	status := rec.status
	if status.Phase == PhaseRendering && status.Batch != "" {
		snap, err := e.queue.BatchProgress(status.Batch)
		if err == nil {
			status.Progress = snap
			if snap.Done() && snap.Total > 0 {
				status.Phase = PhaseCompleted
				e.setPhase(id, PhaseCompleted, status.Batch, "")
			}
		}
	}
	return status, nil
}

// --- internal helpers ---

func (e *Engine) newJob(cancel context.CancelFunc) JobID {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	id := JobID(fmt.Sprintf("engine-job-%d-%d", time.Now().UnixNano(), e.seq))
	e.jobs[id] = &jobRecord{
		status: JobStatus{JobID: id, Phase: PhaseDownloading},
		cancel: cancel,
	}
	return id
}

func (e *Engine) setPhase(id JobID, phase JobPhase, batch queue.BatchID, errMsg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.jobs[id]
	if !ok {
		return
	}
	rec.status.Phase = phase
	if batch != "" {
		rec.status.Batch = batch
	}
	rec.status.Error = errMsg
}

// phaseFromError maps a pipeline StageError to the corresponding failed phase.
func (e *Engine) phaseFromError(err error) JobPhase {
	var se *pipeline.StageError
	if asStageError(err, &se) {
		switch se.Stage {
		case "Download":
			return PhaseDownloading
		case "Analyze":
			return PhaseAnalyzing
		case "Recipe":
			return PhaseRecipe
		case "Variant":
			return PhaseVariant
		}
	}
	return PhaseFailed
}

func asStageError(err error, target **pipeline.StageError) bool {
	for e := err; e != nil; {
		if se, ok := e.(*pipeline.StageError); ok {
			*target = se
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

var _ = download.RawVideo{}
