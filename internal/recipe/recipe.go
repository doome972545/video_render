// Package recipe defines the single, immutable, serializable contract that
// describes how one specific output video should be produced from a source.
//
// A Recipe is data, not behavior: it only describes intent. It is fully
// decoupled from Render and Effect subsystems.
package recipe

import "time"

// Status is a point in the Recipe lifecycle.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusValidated Status = "validated"
	StatusOptimized Status = "optimized"
	StatusFrozen    Status = "frozen"
	StatusExecuted  Status = "executed"
	StatusArchived  Status = "archived"
	StatusRejected  Status = "rejected"
)

// SchemaVersion tracks Recipe schema evolution for migration.
const SchemaVersion = 1

// RecipeID is a content-addressable identifier (hash of serialized form once
// frozen).
type RecipeID string

// Seed is the deterministic seed making generation reproducible.
type Seed int64

// Segment is a used/trimmed slice of the source timeline.
type Segment struct {
	Start time.Duration `json:"start"`
	End   time.Duration `json:"end"`
}

// EffectStep is one ordered, independent effect application. Params are a plain
// map so the Recipe never references effect implementations.
type EffectStep struct {
	EffectID string             `json:"effect_id"`
	Order    int                `json:"order"`
	Params   map[string]float64 `json:"params"`
	// Hooks names the randomizable mutation points and their allowed range.
	// Variant perturbs Params within these ranges without Recipe knowing about
	// Variant.
	Hooks map[string]Range `json:"hooks,omitempty"`
	// Optional marks a step that Variant may include or exclude.
	Optional bool `json:"optional"`
}

// Range is an inclusive numeric range for a randomizer hook.
type Range struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Constraint captures a structural/business limit used during generation.
type Constraint struct {
	MaxDuration    time.Duration `json:"max_duration"`
	MinEffectSteps int           `json:"min_effect_steps"`
	MaxEffectSteps int           `json:"max_effect_steps"`
}

// AudioTrack describes optional background music mixed under the source audio.
type AudioTrack struct {
	// FilePath is a local path to a music/audio file. Empty means "no music".
	FilePath string `json:"file_path,omitempty"`
	// Volume scales the music (1.0 = original). Typical background: 0.1–0.4.
	Volume float64 `json:"volume,omitempty"`
	// SourceVolume scales the original audio (1.0 = keep). 0 = mute source.
	SourceVolume float64 `json:"source_volume,omitempty"`
	// Loop repeats the music to cover the whole video when true.
	Loop bool `json:"loop,omitempty"`
}

// Subtitle describes text burned into the video.
type Subtitle struct {
	// Text is the literal caption to burn in. Empty means "no subtitle".
	Text string `json:"text,omitempty"`
	// FontSize in pixels. 0 = default (24).
	FontSize int `json:"font_size,omitempty"`
	// Color is an ffmpeg color name/hex (e.g. "white", "yellow"). Empty=white.
	Color string `json:"color,omitempty"`
	// Position: "bottom" (default), "top", or "center".
	Position string `json:"position,omitempty"`
}

// Recipe is the canonical entity.
type Recipe struct {
	ID            RecipeID     `json:"id"`
	SchemaVersion int          `json:"schema_version"`
	SourceRef     string       `json:"source_ref"` // fingerprint of the source
	Segments      []Segment    `json:"segments"`
	EffectSteps   []EffectStep `json:"effect_steps"`
	Audio         AudioTrack   `json:"audio,omitempty"`
	Subtitle      Subtitle     `json:"subtitle,omitempty"`
	Constraint    Constraint   `json:"constraint"`
	Seed          Seed         `json:"seed"`
	Status        Status       `json:"status"`
}

// Clone returns a deep copy so callers never mutate a shared Recipe.
func (r Recipe) Clone() Recipe {
	c := r
	c.Segments = append([]Segment(nil), r.Segments...)
	c.EffectSteps = make([]EffectStep, len(r.EffectSteps))
	for i, s := range r.EffectSteps {
		ns := s
		ns.Params = map[string]float64{}
		for k, v := range s.Params {
			ns.Params[k] = v
		}
		if s.Hooks != nil {
			ns.Hooks = map[string]Range{}
			for k, v := range s.Hooks {
				ns.Hooks[k] = v
			}
		}
		c.EffectSteps[i] = ns
	}
	return c
}

// RuleSet holds externalized, versioned business constraints for generation.
type RuleSet struct {
	Version        string       `json:"version"`
	AllowedEffects []string     `json:"allowed_effects"`
	Constraint     Constraint   `json:"constraint"`
	BaseEffects    []EffectStep `json:"base_effects"` // template steps to seed a Draft
	// BaseAudio is background music applied to every generated recipe.
	BaseAudio AudioTrack `json:"base_audio,omitempty"`
	// BaseSubtitle is a caption applied to every generated recipe.
	BaseSubtitle Subtitle `json:"base_subtitle,omitempty"`
}

// ValidationReport is the outcome of validating a Recipe.
type ValidationReport struct {
	Valid  bool
	Errors []string
}

// CacheKey identifies an equivalent (source, ruleset, seed) generation input.
type CacheKey string
