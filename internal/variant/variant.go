// Package variant takes a single Frozen base Recipe and generates a
// configurable number of distinct, non-duplicate derivative Recipes by
// perturbing the base Recipe's exposed Randomizer Hooks with a deterministic
// seed-based strategy. Uniqueness is enforced structurally.
package variant

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"sync"

	"videoremix/internal/pipeline"
	"videoremix/internal/recipe"
)

// Key is a structural uniqueness key for a candidate Recipe.
type Key string

// DistributionRules express weighted preferences over optional effects across
// the batch. Weight is the probability [0,1] an optional effect is included.
type DistributionRules struct {
	OptionalInclusion map[string]float64 // effectID -> probability
}

// SeedGenerator produces a deterministic sequence of seeds.
type SeedGenerator interface {
	NextSeeds(master recipe.Seed, n int) ([]recipe.Seed, error)
}

// Perturber applies seed-driven changes to a base Recipe's Randomizer Hooks.
type Perturber interface {
	Perturb(base recipe.Recipe, seed recipe.Seed, rules DistributionRules) (recipe.Recipe, error)
}

// DuplicateDetector computes and tracks structural uniqueness keys.
type DuplicateDetector interface {
	UniquenessKey(r recipe.Recipe) (Key, error)
	IsDuplicate(key Key) bool
	Record(key Key)
}

// Config bounds the generation loop.
type Config struct {
	MaxAttemptsPerSlot int
}

// DefaultConfig returns sane defaults.
func DefaultConfig() Config { return Config{MaxAttemptsPerSlot: 20} }

// --- PRNGSeedGen adapter ---

type PRNGSeedGen struct{}

func NewPRNGSeedGen() *PRNGSeedGen { return &PRNGSeedGen{} }

func (PRNGSeedGen) NextSeeds(master recipe.Seed, n int) ([]recipe.Seed, error) {
	if n <= 0 {
		return nil, fmt.Errorf("variant: seed count must be positive")
	}
	rng := rand.New(rand.NewSource(int64(master)))
	seeds := make([]recipe.Seed, n)
	for i := range seeds {
		seeds[i] = recipe.Seed(rng.Int63())
	}
	return seeds, nil
}

// --- HookPerturber adapter ---

type HookPerturber struct{}

func NewHookPerturber() *HookPerturber { return &HookPerturber{} }

// Perturb clones the base, samples each hook range, and decides optional-step
// inclusion, all driven by the seed for reproducibility.
func (HookPerturber) Perturb(base recipe.Recipe, seed recipe.Seed, rules DistributionRules) (recipe.Recipe, error) {
	rng := rand.New(rand.NewSource(int64(seed)))
	out := base.Clone()
	out.Seed = seed
	out.ID = ""
	out.Status = recipe.StatusDraft

	kept := out.EffectSteps[:0]
	for _, step := range out.EffectSteps {
		// Optional-step inclusion controlled by distribution weight.
		if step.Optional {
			p := 0.5
			if rules.OptionalInclusion != nil {
				if w, ok := rules.OptionalInclusion[step.EffectID]; ok {
					p = w
				}
			}
			if rng.Float64() > p {
				continue // exclude this optional step
			}
		}
		// Perturb params within their declared hook ranges.
		for name, rg := range step.Hooks {
			if rg.Max < rg.Min {
				continue
			}
			step.Params[name] = rg.Min + rng.Float64()*(rg.Max-rg.Min)
		}
		kept = append(kept, step)
	}
	out.EffectSteps = kept
	// Reindex order after possible removals.
	for i := range out.EffectSteps {
		out.EffectSteps[i].Order = i
	}
	return out, nil
}

// --- StructuralDuplicateDetector adapter ---

type StructuralDuplicateDetector struct {
	mu   sync.Mutex
	seen map[Key]struct{}
}

func NewStructuralDuplicateDetector() *StructuralDuplicateDetector {
	return &StructuralDuplicateDetector{seen: map[Key]struct{}{}}
}

// UniquenessKey hashes the semantic content (segments + ordered effects +
// params), not raw serialized bytes, so insignificant ordering differences do
// not produce false uniqueness.
func (*StructuralDuplicateDetector) UniquenessKey(r recipe.Recipe) (Key, error) {
	type stepView struct {
		EffectID string
		Params   [][2]string
	}
	steps := make([]stepView, 0, len(r.EffectSteps))
	ordered := append([]recipe.EffectStep(nil), r.EffectSteps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
	for _, s := range ordered {
		keys := make([]string, 0, len(s.Params))
		for k := range s.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pv := make([][2]string, 0, len(keys))
		for _, k := range keys {
			// Round params to 3 decimals to avoid float noise inflating uniqueness.
			pv = append(pv, [2]string{k, fmt.Sprintf("%.3f", s.Params[k])})
		}
		steps = append(steps, stepView{EffectID: s.EffectID, Params: pv})
	}
	view := struct {
		Segments []recipe.Segment
		Steps    []stepView
	}{Segments: r.Segments, Steps: steps}

	b, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return Key(hex.EncodeToString(sum[:])), nil
}

func (d *StructuralDuplicateDetector) IsDuplicate(key Key) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.seen[key]
	return ok
}

func (d *StructuralDuplicateDetector) Record(key Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[key] = struct{}{}
}

// --- VariantStage: pipeline.Stage implementation ---

// VariantStage generates N unique Frozen variant recipes from the base recipe
// found in the Context.
type VariantStage struct {
	seedGen   SeedGenerator
	perturber Perturber
	validator recipe.Validator
	optimizer recipe.Optimizer
	serializer recipe.Serializer
	detector  DuplicateDetector
	store     recipe.Store
	count     int
	master    recipe.Seed
	rules     DistributionRules
	cfg       Config
}

func NewVariantStage(
	seedGen SeedGenerator,
	perturber Perturber,
	validator recipe.Validator,
	optimizer recipe.Optimizer,
	serializer recipe.Serializer,
	detector DuplicateDetector,
	store recipe.Store,
	count int,
	master recipe.Seed,
	rules DistributionRules,
	cfg Config,
) *VariantStage {
	return &VariantStage{
		seedGen: seedGen, perturber: perturber, validator: validator,
		optimizer: optimizer, serializer: serializer, detector: detector,
		store: store, count: count, master: master, rules: rules, cfg: cfg,
	}
}

func (s *VariantStage) Name() string { return "Variant" }

func (s *VariantStage) Execute(ctx pipeline.Context) (pipeline.Context, error) {
	v, ok := ctx.Get(pipeline.KeyBaseRecipe)
	if !ok {
		return ctx, fmt.Errorf("variant: no base Recipe in context")
	}
	base, ok := v.(recipe.Recipe)
	if !ok {
		return ctx, fmt.Errorf("variant: context value is not Recipe")
	}

	batch, err := s.Generate(base)
	if err != nil {
		return ctx, err
	}
	return ctx.With(pipeline.KeyVariants, batch), nil
}

// Generate produces the batch of unique, validated, frozen variant recipes.
func (s *VariantStage) Generate(base recipe.Recipe) ([]recipe.Recipe, error) {
	if s.count <= 0 {
		return nil, fmt.Errorf("variant: count must be positive")
	}
	// Over-provision seeds to allow retries for invalid/duplicate candidates.
	seedBudget := s.count * s.cfg.MaxAttemptsPerSlot
	seeds, err := s.seedGen.NextSeeds(s.master, seedBudget)
	if err != nil {
		return nil, fmt.Errorf("variant: seeds: %w", err)
	}

	accepted := make([]recipe.Recipe, 0, s.count)
	attempts := 0
	for _, seed := range seeds {
		if len(accepted) >= s.count {
			break
		}
		attempts++

		cand, err := s.perturber.Perturb(base, seed, s.rules)
		if err != nil {
			continue
		}
		rep, err := s.validator.Validate(cand)
		if err != nil || !rep.Valid {
			continue
		}
		key, err := s.detector.UniquenessKey(cand)
		if err != nil {
			continue
		}
		if s.detector.IsDuplicate(key) {
			continue
		}

		cand.Status = recipe.StatusValidated
		optimized, err := s.optimizer.Optimize(cand)
		if err != nil {
			continue
		}
		frozen, err := recipe.Freeze(s.serializer, optimized)
		if err != nil {
			continue
		}
		if s.store != nil {
			if _, err := s.store.Save(frozen); err != nil {
				return nil, fmt.Errorf("variant: store: %w", err)
			}
		}
		s.detector.Record(key)
		accepted = append(accepted, frozen)
	}

	if len(accepted) < s.count {
		return accepted, fmt.Errorf(
			"variant: insufficient entropy: produced %d/%d unique recipes after %d attempts",
			len(accepted), s.count, attempts,
		)
	}
	return accepted, nil
}
