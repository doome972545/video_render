// Package queue is the scheduling boundary between "a Frozen Recipe is ready to
// render" and "a worker is actively rendering it". It owns job lifecycle,
// concurrency control, retries, prioritization and cancellation — but none of
// the rendering logic itself.
package queue

import (
	"time"

	"videoremix/internal/recipe"
	"videoremix/internal/render"
)

// JobID uniquely identifies a render job.
type JobID string

// BatchID groups jobs produced from one Variant batch.
type BatchID string

// Priority orders scheduling; higher runs sooner (with fairness).
type Priority int

const (
	PriorityLow    Priority = 0
	PriorityNormal Priority = 5
	PriorityHigh   Priority = 10
)

// State is a point in a Job's state machine.
type State string

const (
	StatePending    State = "pending"
	StateRunning    State = "running"
	StateCompleted  State = "completed"
	StateFailed     State = "failed"
	StateDeadLetter State = "dead_letter"
	StateCancelled  State = "cancelled"
)

// Job is an immutable unit of work: one Recipe to render.
type Job struct {
	ID         JobID
	BatchID    BatchID
	RecipeID   recipe.RecipeID
	Priority   Priority
	RetryCount int
	State      State
	CreatedAt  time.Time
	UpdatedAt  time.Time
	OutputRef  string
	LastError  string
}

// RenderResult is the successful outcome recorded on completion.
type RenderResult = render.RenderResult

// ProgressEvent is emitted on every Job state transition.
type ProgressEvent struct {
	JobID   JobID
	BatchID BatchID
	State   State
	Error   string
}

// ProgressSnapshot aggregates batch progress for Engine.GetStatus.
type ProgressSnapshot struct {
	BatchID   BatchID
	Total     int
	Pending   int
	Running   int
	Completed int
	Failed    int
	Cancelled int
}

// Done reports whether all jobs in the batch have reached a terminal state.
func (s ProgressSnapshot) Done() bool {
	return s.Pending == 0 && s.Running == 0
}

// RetryPolicy defines retry limits, backoff and error classification.
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
}

// DefaultRetryPolicy returns sane defaults.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 3, BaseDelay: 500 * time.Millisecond}
}

// Backoff returns the delay for a given attempt (exponential).
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	return d
}
