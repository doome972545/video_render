package variant

import (
	"testing"
	"time"

	"videoremix/internal/analyze"
	"videoremix/internal/recipe"
)

// buildBase produces a frozen base recipe with randomizable hooks.
func buildBase(t *testing.T) recipe.Recipe {
	t.Helper()
	a := analyze.AnalysisResult{
		Fingerprint: "test-fp",
		Metadata:    analyze.Metadata{Duration: 30 * time.Second},
	}
	rules := recipe.RuleSet{
		Version:    "test",
		Constraint: recipe.Constraint{MinEffectSteps: 1, MaxEffectSteps: 8},
		BaseEffects: []recipe.EffectStep{
			{EffectID: "brightness", Params: map[string]float64{"value": 0}, Hooks: map[string]recipe.Range{"value": {Min: -0.2, Max: 0.2}}},
			{EffectID: "contrast", Params: map[string]float64{"value": 1}, Hooks: map[string]recipe.Range{"value": {Min: 0.8, Max: 1.3}}},
			{EffectID: "hflip", Params: map[string]float64{}, Optional: true},
		},
	}
	base, err := recipe.Build(
		recipe.NewRuleBasedGenerator(),
		recipe.NewRuleValidator(),
		recipe.NewStepMergeOptimizer(),
		recipe.NewJSONSerializer(),
		a, rules, recipe.Seed(1),
	)
	if err != nil {
		t.Fatalf("build base: %v", err)
	}
	if base.Status != recipe.StatusFrozen {
		t.Fatalf("expected frozen base, got %s", base.Status)
	}
	return base
}

func newStage(base recipe.Recipe, count int, seed recipe.Seed) *VariantStage {
	return NewVariantStage(
		NewPRNGSeedGen(),
		NewHookPerturber(),
		recipe.NewRuleValidator(),
		recipe.NewStepMergeOptimizer(),
		recipe.NewJSONSerializer(),
		NewStructuralDuplicateDetector(),
		recipe.NewMemoryStore(),
		count, seed,
		DistributionRules{OptionalInclusion: map[string]float64{"hflip": 0.5}},
		DefaultConfig(),
	)
}

func TestGenerateProducesUniqueVariants(t *testing.T) {
	base := buildBase(t)
	stage := newStage(base, 10, recipe.Seed(42))

	batch, err := stage.Generate(base)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(batch) != 10 {
		t.Fatalf("expected 10 variants, got %d", len(batch))
	}
	// All IDs must be unique and frozen.
	ids := map[recipe.RecipeID]bool{}
	for _, r := range batch {
		if r.Status != recipe.StatusFrozen {
			t.Errorf("variant not frozen: %s", r.Status)
		}
		if ids[r.ID] {
			t.Errorf("duplicate variant id: %s", r.ID)
		}
		ids[r.ID] = true
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	base := buildBase(t)

	first, err := newStage(base, 8, recipe.Seed(7)).Generate(base)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	second, err := newStage(base, 8, recipe.Seed(7)).Generate(base)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("length mismatch: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("variant %d differs: %s vs %s", i, first[i].ID, second[i].ID)
		}
	}
}
