package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"videoremix/internal/analyze"
	"videoremix/internal/pipeline"
)

// --- Ports ---

// Generator produces a Draft Recipe from an AnalysisResult and a RuleSet.
type Generator interface {
	Generate(analysis analyze.AnalysisResult, rules RuleSet, seed Seed) (Recipe, error)
}

// Validator enforces structural and business rules.
type Validator interface {
	Validate(r Recipe) (ValidationReport, error)
}

// Optimizer merges/reorders redundant steps prior to freezing.
type Optimizer interface {
	Optimize(r Recipe) (Recipe, error)
}

// Serializer converts between the entity and its durable JSON form.
type Serializer interface {
	Serialize(r Recipe) ([]byte, error)
	Deserialize(data []byte) (Recipe, error)
}

// Store persists and retrieves Recipes.
type Store interface {
	Save(r Recipe) (RecipeID, error)
	Get(id RecipeID) (Recipe, error)
	FindByContentHash(hash string) (Recipe, bool, error)
}

// Cache avoids redundant regeneration of equivalent Recipes.
type Cache interface {
	Lookup(key CacheKey) (Recipe, bool)
	Put(key CacheKey, r Recipe)
}

// --- Domain service: Freeze ---

// Freeze computes the content-addressable ID from the serialized form and marks
// the Recipe Frozen. The ID excludes the ID field itself to be deterministic.
func Freeze(ser Serializer, r Recipe) (Recipe, error) {
	r.Status = StatusFrozen
	r.SchemaVersion = SchemaVersion
	r.ID = ""
	data, err := ser.Serialize(r)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe: freeze serialize: %w", err)
	}
	sum := sha256.Sum256(data)
	r.ID = RecipeID(hex.EncodeToString(sum[:]))
	return r, nil
}

// --- RuleBasedGenerator adapter ---

type RuleBasedGenerator struct{}

func NewRuleBasedGenerator() *RuleBasedGenerator { return &RuleBasedGenerator{} }

// Generate builds a baseline Draft: full-source segment (minus leading/trailing
// silence if present) plus the ruleset's base effect steps.
func (RuleBasedGenerator) Generate(a analyze.AnalysisResult, rules RuleSet, seed Seed) (Recipe, error) {
	seg := Segment{Start: 0, End: a.Metadata.Duration}
	// Trim leading/trailing silence conservatively when detected.
	if len(a.Silence) > 0 {
		first := a.Silence[0]
		if first.Start == 0 && first.End < a.Metadata.Duration {
			seg.Start = first.End
		}
		last := a.Silence[len(a.Silence)-1]
		if last.End >= a.Metadata.Duration && last.Start > seg.Start {
			seg.End = last.Start
		}
	}

	steps := make([]EffectStep, len(rules.BaseEffects))
	copy(steps, rules.BaseEffects)
	for i := range steps {
		steps[i].Order = i
		if steps[i].Params == nil {
			steps[i].Params = map[string]float64{}
		}
	}

	r := Recipe{
		SchemaVersion: SchemaVersion,
		SourceRef:     string(a.Fingerprint),
		Segments:      []Segment{seg},
		EffectSteps:   steps,
		Constraint:    rules.Constraint,
		Seed:          seed,
		Status:        StatusDraft,
	}
	return r, nil
}

// --- RuleValidator adapter ---

type RuleValidator struct{}

func NewRuleValidator() *RuleValidator { return &RuleValidator{} }

func (RuleValidator) Validate(r Recipe) (ValidationReport, error) {
	rep := ValidationReport{Valid: true}
	add := func(msg string) { rep.Valid = false; rep.Errors = append(rep.Errors, msg) }

	// Segments: non-overlapping, positive length.
	segs := append([]Segment(nil), r.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
	var prevEnd = int64(-1)
	for _, s := range segs {
		if s.End <= s.Start {
			add(fmt.Sprintf("segment has non-positive length: %v..%v", s.Start, s.End))
		}
		if int64(s.Start) < prevEnd {
			add("overlapping segments detected")
		}
		if int64(s.End) > prevEnd {
			prevEnd = int64(s.End)
		}
	}

	// Effect step count bounds.
	if r.Constraint.MinEffectSteps > 0 && len(r.EffectSteps) < r.Constraint.MinEffectSteps {
		add(fmt.Sprintf("too few effect steps: %d < %d", len(r.EffectSteps), r.Constraint.MinEffectSteps))
	}
	if r.Constraint.MaxEffectSteps > 0 && len(r.EffectSteps) > r.Constraint.MaxEffectSteps {
		add(fmt.Sprintf("too many effect steps: %d > %d", len(r.EffectSteps), r.Constraint.MaxEffectSteps))
	}

	// Duration bound.
	if r.Constraint.MaxDuration > 0 {
		var total int64
		for _, s := range r.Segments {
			total += int64(s.End - s.Start)
		}
		if total > int64(r.Constraint.MaxDuration) {
			add("total segment duration exceeds max")
		}
	}

	// Unknown effect id / duplicate order sanity.
	seenOrder := map[int]bool{}
	for _, s := range r.EffectSteps {
		if s.EffectID == "" {
			add("effect step with empty EffectID")
		}
		if seenOrder[s.Order] {
			add(fmt.Sprintf("duplicate effect step order %d", s.Order))
		}
		seenOrder[s.Order] = true
	}
	return rep, nil
}

// --- StepMergeOptimizer adapter ---

type StepMergeOptimizer struct{}

func NewStepMergeOptimizer() *StepMergeOptimizer { return &StepMergeOptimizer{} }

// Optimize collapses consecutive steps sharing the same EffectID by merging
// their params (later value wins), then reindexes order.
func (StepMergeOptimizer) Optimize(r Recipe) (Recipe, error) {
	out := r.Clone()
	sort.SliceStable(out.EffectSteps, func(i, j int) bool {
		return out.EffectSteps[i].Order < out.EffectSteps[j].Order
	})
	merged := make([]EffectStep, 0, len(out.EffectSteps))
	for _, s := range out.EffectSteps {
		if n := len(merged); n > 0 && merged[n-1].EffectID == s.EffectID {
			for k, v := range s.Params {
				merged[n-1].Params[k] = v
			}
			continue
		}
		merged = append(merged, s)
	}
	for i := range merged {
		merged[i].Order = i
	}
	out.EffectSteps = merged
	out.Status = StatusOptimized
	return out, nil
}

// --- JSONSerializer adapter ---

type JSONSerializer struct{}

func NewJSONSerializer() *JSONSerializer { return &JSONSerializer{} }

func (JSONSerializer) Serialize(r Recipe) ([]byte, error)      { return json.Marshal(r) }
func (JSONSerializer) Deserialize(b []byte) (Recipe, error) {
	var r Recipe
	err := json.Unmarshal(b, &r)
	return r, err
}

// --- MemoryStore adapter ---

type MemoryStore struct {
	mu sync.RWMutex
	m  map[RecipeID]Recipe
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[RecipeID]Recipe{}} }

func (s *MemoryStore) Save(r Recipe) (RecipeID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[r.ID] = r
	return r.ID, nil
}

func (s *MemoryStore) Get(id RecipeID) (Recipe, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.m[id]
	if !ok {
		return Recipe{}, fmt.Errorf("recipe: not found: %s", id)
	}
	return r, nil
}

func (s *MemoryStore) FindByContentHash(hash string) (Recipe, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.m[RecipeID(hash)]
	return r, ok, nil
}

// --- MemoryCache adapter ---

type MemoryCache struct {
	mu sync.RWMutex
	m  map[CacheKey]Recipe
}

func NewMemoryCache() *MemoryCache { return &MemoryCache{m: map[CacheKey]Recipe{}} }

func (c *MemoryCache) Lookup(key CacheKey) (Recipe, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.m[key]
	return r, ok
}

func (c *MemoryCache) Put(key CacheKey, r Recipe) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = r
}

// --- RecipeStage: pipeline.Stage implementation for baseline generation ---

// RecipeStage runs Generate → Validate → Optimize → Freeze → Store and writes
// the baseline Frozen Recipe into the Context.
type RecipeStage struct {
	gen   Generator
	val   Validator
	opt   Optimizer
	ser   Serializer
	store Store
	cache Cache
	rules RuleSet
	seed  Seed
}

func NewRecipeStage(gen Generator, val Validator, opt Optimizer, ser Serializer, store Store, cache Cache, rules RuleSet, seed Seed) *RecipeStage {
	return &RecipeStage{gen: gen, val: val, opt: opt, ser: ser, store: store, cache: cache, rules: rules, seed: seed}
}

func (s *RecipeStage) Name() string { return "Recipe" }

func (s *RecipeStage) Execute(ctx pipeline.Context) (pipeline.Context, error) {
	v, ok := ctx.Get(pipeline.KeyAnalysis)
	if !ok {
		return ctx, fmt.Errorf("recipe: no AnalysisResult in context")
	}
	a, ok := v.(analyze.AnalysisResult)
	if !ok {
		return ctx, fmt.Errorf("recipe: context value is not AnalysisResult")
	}

	key := s.cacheKey(a)
	if s.cache != nil {
		if cached, hit := s.cache.Lookup(key); hit {
			return ctx.With(pipeline.KeyBaseRecipe, cached), nil
		}
	}

	frozen, err := Build(s.gen, s.val, s.opt, s.ser, a, s.rules, s.seed)
	if err != nil {
		return ctx, err
	}
	if _, err := s.store.Save(frozen); err != nil {
		return ctx, fmt.Errorf("recipe: store: %w", err)
	}
	if s.cache != nil {
		s.cache.Put(key, frozen)
	}
	return ctx.With(pipeline.KeyBaseRecipe, frozen), nil
}

func (s *RecipeStage) cacheKey(a analyze.AnalysisResult) CacheKey {
	rulesJSON, _ := json.Marshal(s.rules)
	sum := sha256.Sum256(rulesJSON)
	return CacheKey(fmt.Sprintf("%s|%x|%d", a.Fingerprint, sum[:8], s.seed))
}

// Build is the shared generate→validate→optimize→freeze routine, reused by
// both the RecipeStage and the Variant stage.
func Build(gen Generator, val Validator, opt Optimizer, ser Serializer, a analyze.AnalysisResult, rules RuleSet, seed Seed) (Recipe, error) {
	draft, err := gen.Generate(a, rules, seed)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe: generate: %w", err)
	}
	rep, err := val.Validate(draft)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe: validate: %w", err)
	}
	if !rep.Valid {
		return Recipe{}, fmt.Errorf("recipe: invalid: %v", rep.Errors)
	}
	draft.Status = StatusValidated
	optimized, err := opt.Optimize(draft)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe: optimize: %w", err)
	}
	return Freeze(ser, optimized)
}
