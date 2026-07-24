// Package render consumes a single Frozen Recipe plus its referenced RawVideo
// and produces a final MP4 output. It is the only stage that touches FFmpeg and
// makes no creative decisions — every parameter comes from the Recipe.
package render

import (
	"fmt"

	"videoremix/internal/download"
	"videoremix/internal/recipe"
)

// EncodingPath describes the selected hardware encoding route.
type EncodingPath struct {
	UseGPU bool
	Codec  string // e.g. "libx264", "h264_nvenc"
	Preset string
}

// Timeline is the assembled segment/layer/track structure from a Recipe.
type Timeline struct {
	Source   download.RawVideo
	Segments []recipe.Segment
	// Recipe is the full recipe so the executor can access audio/subtitle.
	Recipe recipe.Recipe
}

// FilterGraph is the compiled FFmpeg filter description, split by stream kind.
type FilterGraph struct {
	// Video is the -vf chain (video filters), comma-joined.
	Video string
	// Audio is the -af chain (audio filters), comma-joined.
	Audio string
	// MapLabels are the output stream labels to -map.
	MapLabels []string
}

// RenderResult descriptor for a completed render.
type RenderResult struct {
	OutputPath string
	Checksum   string
	DurationOK bool
	RecipeID   recipe.RecipeID
}

// FilterGraphCompiler converts a timeline + ordered effect steps into a graph.
type FilterGraphCompiler interface {
	Compile(t Timeline, steps []recipe.EffectStep) (FilterGraph, error)
}

// HardwareSelector chooses the encoding path.
type HardwareSelector interface {
	Select(cfg RenderConfig) (EncodingPath, error)
}

// FFmpegExecutor invokes FFmpeg and returns the produced output path.
type FFmpegExecutor interface {
	Execute(t Timeline, graph FilterGraph, path EncodingPath, outputPath string) (string, error)
}

// OutputValidator confirms the produced MP4 is valid.
type OutputValidator interface {
	Validate(outputPath string, expected Timeline) (RenderResult, error)
}

// RenderConfig holds hardware/encoding preferences.
type RenderConfig struct {
	PreferGPU  bool
	Preset     string
	OutputDir  string
}

// Renderer is invoked per Job by the Queue's Worker Dispatcher.
type Renderer struct {
	compiler  FilterGraphCompiler
	hardware  HardwareSelector
	executor  FFmpegExecutor
	validator OutputValidator
	cfg       RenderConfig
	// resolveSource maps a Recipe.SourceRef back to its RawVideo.
	resolveSource func(sourceRef string) (download.RawVideo, error)
}

// NewRenderer wires the render pipeline.
func NewRenderer(
	compiler FilterGraphCompiler,
	hardware HardwareSelector,
	executor FFmpegExecutor,
	validator OutputValidator,
	cfg RenderConfig,
	resolveSource func(string) (download.RawVideo, error),
) *Renderer {
	return &Renderer{
		compiler: compiler, hardware: hardware, executor: executor,
		validator: validator, cfg: cfg, resolveSource: resolveSource,
	}
}

// Render is the pure function (RawVideo, Recipe) -> MP4.
func (r *Renderer) Render(rec recipe.Recipe, raw download.RawVideo) (RenderResult, error) {
	if raw.FilePath == "" && r.resolveSource != nil {
		resolved, err := r.resolveSource(rec.SourceRef)
		if err != nil {
			return RenderResult{}, fmt.Errorf("render: resolve source: %w", err)
		}
		raw = resolved
	}

	timeline := Timeline{Source: raw, Segments: rec.Segments, Recipe: rec}

	graph, err := r.compiler.Compile(timeline, rec.EffectSteps)
	if err != nil {
		return RenderResult{}, fmt.Errorf("render: compile: %w", err)
	}
	path, err := r.hardware.Select(r.cfg)
	if err != nil {
		return RenderResult{}, fmt.Errorf("render: hardware select: %w", err)
	}

	outputPath := outputPathFor(r.cfg.OutputDir, rec.ID)
	produced, err := r.executor.Execute(timeline, graph, path, outputPath)
	if err != nil {
		return RenderResult{}, fmt.Errorf("render: ffmpeg: %w", err)
	}

	result, err := r.validator.Validate(produced, timeline)
	if err != nil {
		return RenderResult{}, fmt.Errorf("render: output validation: %w", err)
	}
	result.RecipeID = rec.ID
	return result, nil
}
