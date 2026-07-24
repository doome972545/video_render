// Package pipeline defines the structural contract every stage conforms to
// and the shared Context mechanism through which stages communicate.
//
// This is the architectural constitution binding all stages together. It
// implements no stage itself; it only defines Stage, Context and the
// PipelineRunner that threads an immutable Context through an ordered list of
// stages.
package pipeline

import (
	"context"
	"fmt"
)

// ContextKey is a namespaced key used to read/write values on a Context.
// Keys should be namespaced per stage (e.g. "download.rawVideo") to avoid
// silent field shadowing between stages.
type ContextKey string

// Well-known Context keys, namespaced per producing stage.
const (
	KeyInput         ContextKey = "engine.input"          // JobInput
	KeyRawVideo      ContextKey = "download.rawVideo"      // download.RawVideo
	KeyAnalysis      ContextKey = "analyze.result"         // analyze.AnalysisResult
	KeyBaseRecipe    ContextKey = "recipe.base"            // recipe.Recipe (frozen baseline)
	KeyVariants      ContextKey = "variant.recipes"        // []recipe.Recipe
	KeyBatchID       ContextKey = "queue.batchID"          // queue.BatchID
)

// Context is an immutable, append-only carrier of data passed between stages.
// A stage never mutates fields written by a previous stage; With returns a new
// Context leaving the original untouched.
type Context struct {
	// ctx carries cancellation and deadlines cooperatively across stages.
	ctx    context.Context
	values map[ContextKey]any
}

// NewContext creates an initial Context wrapping a cancellation context.
func NewContext(ctx context.Context) Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return Context{ctx: ctx, values: map[ContextKey]any{}}
}

// Ctx returns the underlying cancellation context for cooperative cancellation.
func (c Context) Ctx() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// With returns a new Context with an added field; the original is untouched.
func (c Context) With(key ContextKey, value any) Context {
	next := make(map[ContextKey]any, len(c.values)+1)
	for k, v := range c.values {
		next[k] = v
	}
	next[key] = value
	return Context{ctx: c.ctx, values: next}
}

// Get returns the value stored at key, and whether it was present.
func (c Context) Get(key ContextKey) (any, bool) {
	v, ok := c.values[key]
	return v, ok
}

// MustGet returns the value at key or panics; use only when a prior stage
// contract guarantees the value exists.
func (c Context) MustGet(key ContextKey) any {
	v, ok := c.Get(key)
	if !ok {
		panic(fmt.Sprintf("pipeline: missing required context key %q", key))
	}
	return v
}

// Stage is the uniform contract every pipeline step implements.
type Stage interface {
	// Name identifies the stage for error attribution and logging.
	Name() string
	// Execute transforms the incoming Context and returns an updated one.
	// A non-nil error halts the pipeline uniformly.
	Execute(ctx Context) (Context, error)
}

// StageError wraps a stage failure with which stage produced it (the
// "Error Envelope" in the architecture doc), for attribution.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string {
	return fmt.Sprintf("pipeline: stage %q failed: %v", e.Stage, e.Err)
}

func (e *StageError) Unwrap() error { return e.Err }

// Runner sequences a fixed ordered list of Stages, threading Context through
// each in order. On error it halts and returns a wrapped StageError.
type Runner struct {
	stages []Stage
}

// NewRunner constructs a Runner over an ordered list of stages.
func NewRunner(stages ...Stage) *Runner {
	return &Runner{stages: stages}
}

// Run threads the initial Context through every stage in order. On success the
// returned Context replaces the current one; on error the run halts and a
// StageError identifying the failing stage is returned.
func (r *Runner) Run(initial Context) (Context, error) {
	cur := initial
	for _, s := range r.stages {
		// Cooperative cancellation check at stage boundaries.
		if err := cur.Ctx().Err(); err != nil {
			return cur, &StageError{Stage: s.Name(), Err: err}
		}
		next, err := s.Execute(cur)
		if err != nil {
			return cur, &StageError{Stage: s.Name(), Err: err}
		}
		cur = next
	}
	return cur, nil
}
