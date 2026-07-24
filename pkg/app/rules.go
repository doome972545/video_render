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
// does not supply its own RulesProvider. It defines a rich set of randomizable
// video + audio effects. Callers can copy and customize this.
func DefaultRules() (recipe.RuleSet, variant.DistributionRules) {
	return BuildRules(RemixOptions{})
}

// RemixOptions customizes the remix behavior from a front end (CLI/desktop).
type RemixOptions struct {
	// MusicPath, when set, mixes a background music track under the audio.
	MusicPath string
	// MusicVolume scales the music (default 0.3). SourceVolume keeps original.
	MusicVolume  float64
	SourceVolume float64
	MusicLoop    bool

	// SubtitleText, when set, burns a caption into every output.
	SubtitleText     string
	SubtitlePosition string // "bottom" (default), "top", "center"

	// ExtraEffects enables the wider set of random effects (blur, zoom, hue,
	// rotate, audio) on top of the base color/geometry set.
	ExtraEffects bool

	// Flip controls horizontal-flip behavior: "off", "always", or "random"
	// (default). "off" never flips, "always" flips every variant, "random"
	// flips roughly half of them.
	Flip string
}

// Flip modes.
const (
	FlipOff    = "off"
	FlipAlways = "always"
	FlipRandom = "random"
)

// BuildRules constructs a RuleSet + distribution from RemixOptions.
func BuildRules(o RemixOptions) (recipe.RuleSet, variant.DistributionRules) {
	base := []recipe.EffectStep{
		{EffectID: "brightness", Params: map[string]float64{"value": 0}, Hooks: map[string]recipe.Range{"value": {Min: -0.15, Max: 0.15}}},
		{EffectID: "contrast", Params: map[string]float64{"value": 1}, Hooks: map[string]recipe.Range{"value": {Min: 0.85, Max: 1.2}}},
		{EffectID: "saturation", Params: map[string]float64{"value": 1}, Hooks: map[string]recipe.Range{"value": {Min: 0.8, Max: 1.3}}},
		{EffectID: "speed", Params: map[string]float64{"factor": 1}, Hooks: map[string]recipe.Range{"factor": {Min: 0.95, Max: 1.1}}},
	}
	dist := variant.DistributionRules{
		OptionalInclusion: map[string]float64{},
	}

	// Horizontal flip mode.
	switch o.Flip {
	case FlipOff:
		// Do not add hflip at all.
	case FlipAlways:
		// Mandatory flip on every variant.
		base = append(base, recipe.EffectStep{EffectID: "hflip", Params: map[string]float64{}})
	default: // FlipRandom / empty
		base = append(base, recipe.EffectStep{EffectID: "hflip", Params: map[string]float64{}, Optional: true})
		dist.OptionalInclusion["hflip"] = 0.5
	}

	if o.ExtraEffects {
		base = append(base,
			recipe.EffectStep{EffectID: "gamma", Params: map[string]float64{"value": 1}, Hooks: map[string]recipe.Range{"value": {Min: 0.85, Max: 1.15}}},
			recipe.EffectStep{EffectID: "hue", Params: map[string]float64{"degrees": 0}, Hooks: map[string]recipe.Range{"degrees": {Min: -20, Max: 20}}},
			recipe.EffectStep{EffectID: "zoom", Params: map[string]float64{"factor": 1.05}, Hooks: map[string]recipe.Range{"factor": {Min: 1.0, Max: 1.12}}, Optional: true},
			recipe.EffectStep{EffectID: "blur", Params: map[string]float64{"radius": 1}, Hooks: map[string]recipe.Range{"radius": {Min: 0.5, Max: 2.5}}, Optional: true},
			recipe.EffectStep{EffectID: "vignette", Params: map[string]float64{}, Optional: true},
			// Audio randomization
			recipe.EffectStep{EffectID: "volume", Params: map[string]float64{"value": 1}, Hooks: map[string]recipe.Range{"value": {Min: 0.9, Max: 1.15}}},
			recipe.EffectStep{EffectID: "bass", Params: map[string]float64{"gain": 0}, Hooks: map[string]recipe.Range{"gain": {Min: -3, Max: 4}}, Optional: true},
		)
		dist.OptionalInclusion["zoom"] = 0.4
		dist.OptionalInclusion["blur"] = 0.25
		dist.OptionalInclusion["vignette"] = 0.4
		dist.OptionalInclusion["bass"] = 0.5
	}

	rules := recipe.RuleSet{
		Version:        "v2",
		AllowedEffects: allowedIDs(base),
		Constraint:     recipe.Constraint{MinEffectSteps: 1, MaxEffectSteps: 32},
		BaseEffects:    base,
	}

	if o.MusicPath != "" {
		mv := o.MusicVolume
		if mv == 0 {
			mv = 0.3
		}
		sv := o.SourceVolume
		if sv == 0 {
			sv = 1
		}
		rules.BaseAudio = recipe.AudioTrack{
			FilePath:     o.MusicPath,
			Volume:       mv,
			SourceVolume: sv,
			Loop:         o.MusicLoop,
		}
	}
	if o.SubtitleText != "" {
		pos := o.SubtitlePosition
		if pos == "" {
			pos = "bottom"
		}
		rules.BaseSubtitle = recipe.Subtitle{Text: o.SubtitleText, Position: pos}
	}

	return rules, dist
}

func allowedIDs(steps []recipe.EffectStep) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range steps {
		if !seen[s.EffectID] {
			seen[s.EffectID] = true
			out = append(out, s.EffectID)
		}
	}
	return out
}
