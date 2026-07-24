// Command videoremix is the CLI entry point wiring the entire pipeline together
// and submitting a single remix job via the Engine facade.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"videoremix/internal/analyze"
	"videoremix/internal/download"
	"videoremix/internal/engine"
	"videoremix/internal/pipeline"
	"videoremix/internal/queue"
	"videoremix/internal/recipe"
	"videoremix/internal/render"
	"videoremix/internal/variant"
)

func main() {
	var (
		source   = flag.String("source", "", "source URL or local file path")
		count    = flag.Int("variants", 5, "number of variants to generate")
		seed     = flag.Int64("seed", time.Now().UnixNano(), "master seed for reproducibility")
		workDir  = flag.String("workdir", "work", "working directory for downloads")
		outDir   = flag.String("out", "output", "output directory for rendered videos")
		gpu      = flag.Bool("gpu", false, "prefer GPU (nvenc) encoding when available")
		concurrency = flag.Int("concurrency", 4, "max parallel renders")
	)
	flag.Parse()

	if *source == "" {
		log.Fatal("--source is required (URL or local file path)")
	}

	if err := run(runOpts{
		source:      *source,
		count:       *count,
		seed:        recipe.Seed(*seed),
		workDir:     *workDir,
		outDir:      *outDir,
		gpu:         *gpu,
		concurrency: *concurrency,
	}); err != nil {
		log.Fatal(err)
	}
}

type runOpts struct {
	source      string
	count       int
	seed        recipe.Seed
	workDir     string
	outDir      string
	gpu         bool
	concurrency int
}

func run(o runOpts) error {
	if err := os.MkdirAll(o.workDir, 0o755); err != nil {
		return err
	}

	// Shared stores/caches used across stages and the render dispatcher.
	recipeStore := recipe.NewMemoryStore()
	jobStore := queue.NewMemoryJobStore()
	reporter := queue.NewChannelReporter()

	// A RawVideo captured from Download so the Render dispatcher can resolve
	// the source without re-downloading. We capture it via a closure hook.
	var capturedRaw download.RawVideo

	// buildStages assembles the synchronous stage list per job. Engine calls
	// this so it never imports concrete stage types directly.
	buildStages := func(in engine.JobInput, ctx context.Context) ([]pipeline.Stage, error) {
		dlCfg := download.DefaultConfig(o.workDir)
		validator := download.NewFFprobeValidator(dlCfg)
		progress := make(chan download.ProgressEvent, 64)
		go drainProgress(progress)

		downloader := download.NewDownloader(
			dlCfg, validator, progress,
			download.NewYouTubeFetcher(o.workDir),
			download.NewTikTokFetcher(o.workDir),
			download.NewInstagramFetcher(o.workDir),
			download.NewFacebookFetcher(o.workDir),
			download.NewLocalFileLoader(),
		)

		// Wrap the downloader to capture RawVideo for the render dispatcher.
		captureStage := stageFunc{
			name: "Download",
			fn: func(pc pipeline.Context) (pipeline.Context, error) {
				out, err := downloader.Execute(pc)
				if err != nil {
					return out, err
				}
				if v, ok := out.Get(pipeline.KeyRawVideo); ok {
					if raw, ok := v.(download.RawVideo); ok {
						capturedRaw = raw
					}
				}
				return out, nil
			},
		}

		// Analyze with a shared set of independent detectors.
		silence := analyze.NewSilenceDetector(-30, 500*time.Millisecond)
		analyzer := analyze.NewAnalyzer(
			analyze.NewChecksumFingerprinter(),
			analyze.NewFFprobeMetadata(),
			analyze.NewMemoryCache(),
			analyze.NewSceneDetector(0.4),
			silence,
			analyze.NewSpeechDetector(silence),
			analyze.NewNoopFaceDetector(),
			analyze.NewUniformMotionDetector(),
		)

		// Recipe baseline generation with an externalized RuleSet.
		rules := defaultRuleSet()
		recipeStage := recipe.NewRecipeStage(
			recipe.NewRuleBasedGenerator(),
			recipe.NewRuleValidator(),
			recipe.NewStepMergeOptimizer(),
			recipe.NewJSONSerializer(),
			recipeStore,
			recipe.NewMemoryCache(),
			rules,
			in.MasterSeed,
		)

		// Variant fan-out.
		variantStage := variant.NewVariantStage(
			variant.NewPRNGSeedGen(),
			variant.NewHookPerturber(),
			recipe.NewRuleValidator(),
			recipe.NewStepMergeOptimizer(),
			recipe.NewJSONSerializer(),
			variant.NewStructuralDuplicateDetector(),
			recipeStore,
			in.VariantCount,
			in.MasterSeed,
			defaultDistribution(),
			variant.DefaultConfig(),
		)

		return []pipeline.Stage{captureStage, analyzer, recipeStage, variantStage}, nil
	}

	// Render wiring, resolving the source from the captured RawVideo.
	renderer := render.NewRenderer(
		render.NewChainCompiler(render.DefaultEffectRegistry()),
		render.NewGPUProbe(),
		render.NewCLIFFmpegExecutor(),
		render.NewFileOutputValidator(),
		render.RenderConfig{PreferGPU: o.gpu, Preset: "medium", OutputDir: o.outDir},
		func(sourceRef string) (download.RawVideo, error) {
			if capturedRaw.FilePath == "" {
				return download.RawVideo{}, fmt.Errorf("source not resolved for ref %s", sourceRef)
			}
			return capturedRaw, nil
		},
	)

	// The dispatcher reads the captured raw lazily at dispatch time.
	dispatcher := &lazyDispatcher{renderer: renderer, recipes: recipeStore, raw: &capturedRaw}
	qsvc := queue.NewService(jobStore, dispatcher, reporter, queue.DefaultRetryPolicy(), o.concurrency)
	defer qsvc.Shutdown()

	eng := engine.NewEngine(buildStages, qsvc)

	fmt.Printf("Starting job for %q (variants=%d, seed=%d)\n", o.source, o.count, o.seed)
	id, err := eng.StartJob(engine.JobInput{
		Source:       o.source,
		VariantCount: o.count,
		Priority:     queue.PriorityNormal,
		MasterSeed:   o.seed,
	})
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}
	fmt.Printf("Job started: %s\n", id)

	// Poll status until the render batch is done.
	for {
		st, err := eng.GetStatus(id)
		if err != nil {
			return err
		}
		fmt.Printf("[%s] phase=%s batch=%s progress=%+v\n",
			time.Now().Format("15:04:05"), st.Phase, st.Batch, st.Progress)
		if st.Phase == engine.PhaseCompleted || st.Phase == engine.PhaseFailed || st.Phase == engine.PhaseCancelled {
			if st.Error != "" {
				return fmt.Errorf("job ended in %s: %s", st.Phase, st.Error)
			}
			fmt.Printf("Job %s finished: %s. Outputs in %s\n", id, st.Phase, filepath.Clean(o.outDir))
			return nil
		}
		time.Sleep(1 * time.Second)
	}
}

// stageFunc adapts a function to the pipeline.Stage interface.
type stageFunc struct {
	name string
	fn   func(pipeline.Context) (pipeline.Context, error)
}

func (s stageFunc) Name() string                                       { return s.name }
func (s stageFunc) Execute(c pipeline.Context) (pipeline.Context, error) { return s.fn(c) }

// lazyDispatcher resolves the captured RawVideo at dispatch time.
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

func drainProgress(ch <-chan download.ProgressEvent) {
	for ev := range ch {
		if ev.Message != "" {
			fmt.Printf("  download: %.0f%% %s\n", ev.Percent, ev.Message)
		}
	}
}

// defaultRuleSet is an externalized, versioned set of business constraints with
// a small template of randomizable effect steps.
func defaultRuleSet() recipe.RuleSet {
	return recipe.RuleSet{
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
}

func defaultDistribution() variant.DistributionRules {
	return variant.DistributionRules{
		OptionalInclusion: map[string]float64{
			"hflip": 0.5,
		},
	}
}
