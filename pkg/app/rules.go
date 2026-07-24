package app

import (
	"fmt"

	"videoremix/internal/download"
	"videoremix/internal/pipeline"
	"videoremix/internal/queue"
	"videoremix/internal/recipe"
	"videoremix/internal/render"
	"videoremix/internal/variant"
)

// stageFunc adapts a function to the pipeline.Stage interface.
type stageFunc struct {
	name string
	fn   func(pipeline.Context) (pipeline.Context, error)
}

func (s stageFunc) Name() string                                        { return s.name }
func (s stageFunc) Execute(c pipeline.Context) (pipeline.Context, error) { return s.fn(c) }

// lazyDispatcher resolves the captured RawVideo at dispatch time and delegates
// to the renderer. It adapts render.Renderer to queue.WorkerDispatcher.
type lazyDispatcher struct {
	renderer *render.Renderer
	recipes  recipe.Store
	raw      *download.RawVideo
}

func (d *lazyDispatcher) Dispatch(job queue.Job) (queue.RenderResult, error) {
	rec, err := d.recipes.Get(job.RecipeID)
	if err != nil {
		return queue.RenderResult{}, fmt.Errorf("%w: recipe lookup: %v", download.ErrPermanent, err)
	}
	return d.renderer.Render(rec, *d.raw)
}

// DefaultRules returns the built-in RuleSet and distribution used when a Config
// does not supply its own RulesProvider. It defines a small set of randomizable
// color/geometry effects. Callers can copy and customize this.
func DefaultRules() (recipe.RuleSet, variant.DistributionRules) {
	rules := recipe.RuleSet{
		Version:        "v1",
		AllowedEffects: []string{"brightness", "contrast", "saturation", "hflip", "speed"},
		Constraint: recipe.Constraint{
			MinEffectSteps: 1,
			MaxEffectSteps: 16,
		},
		BaseEffects: []recipe.EffectStep{
			{
				EffectID: "brightness",
				Params:   map[string]float64{"value": 0.0},
				Hooks:    map[string]recipe.Range{"value": {Min: -0.15, Max: 0.15}},
			},
			{
				EffectID: "contrast",
				Params:   map[string]float64{"value": 1.0},
				Hooks:    map[string]recipe.Range{"value": {Min: 0.85, Max: 1.2}},
			},
			{
				EffectID: "saturation",
				Params:   map[string]float64{"value": 1.0},
				Hooks:    map[string]recipe.Range{"value": {Min: 0.8, Max: 1.3}},
			},
			{
				EffectID: "speed",
				Params:   map[string]float64{"factor": 1.0},
				Hooks:    map[string]recipe.Range{"factor": {Min: 0.95, Max: 1.1}},
			},
			{
				EffectID: "hflip",
				Params:   map[string]float64{},
				Optional: true,
			},
		},
	}
	dist := variant.DistributionRules{
		OptionalInclusion: map[string]float64{"hflip": 0.5},
	}
	return rules, dist
}
