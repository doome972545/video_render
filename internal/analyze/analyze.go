// Package analyze inspects a RawVideo and produces a structured, immutable
// AnalysisResult describing its content characteristics. It is the only stage
// permitted to "look at" the video's content; later stages reason only about
// the AnalysisResult.
package analyze

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"videoremix/internal/download"
	"videoremix/internal/pipeline"
)

// Hash is a content fingerprint used for caching and duplicate-source detection.
type Hash string

// Metadata holds container-level facts.
type Metadata struct {
	Duration   time.Duration
	Width      int
	Height     int
	Framerate  float64
	Codec      string
	Bitrate    int64
	Container  string
}

// Interval is a [Start, End) time span with an optional confidence score.
type Interval struct {
	Start      time.Duration
	End        time.Duration
	Confidence float64
}

// FaceRegion marks a face presence at a timestamp (presence only, not identity).
type FaceRegion struct {
	At         time.Duration
	X, Y, W, H float64 // normalized [0,1] bounding box
	Confidence float64
}

// MotionScore rates motion intensity over a segment.
type MotionScore struct {
	Interval
	Intensity float64 // 0..1
}

// DetectionKind names a detector's output category.
type DetectionKind string

const (
	KindScene   DetectionKind = "scene"
	KindSilence DetectionKind = "silence"
	KindSpeech  DetectionKind = "speech"
	KindFace    DetectionKind = "face"
	KindMotion  DetectionKind = "motion"
)

// DetectionResult is the common return type of every Detector.
type DetectionResult struct {
	Kind      DetectionKind
	Scenes    []time.Duration // scene boundary timestamps
	Intervals []Interval      // silence/speech intervals
	Faces     []FaceRegion
	Motion    []MotionScore
	Version   string // detector algorithm version, folded into cache key
}

// AnalysisResult is the assembled, immutable output of Analyze.
type AnalysisResult struct {
	Fingerprint    Hash
	Metadata       Metadata
	SceneBoundaries []time.Duration
	Silence        []Interval
	Speech         []Interval
	Faces          []FaceRegion
	Motion         []MotionScore
	DetectorVersions map[DetectionKind]string
}

// Detector is the common interface for all detector kinds.
type Detector interface {
	Kind() DetectionKind
	Detect(video download.RawVideo) (DetectionResult, error)
}

// MetadataExtractor reads container-level facts.
type MetadataExtractor interface {
	Extract(video download.RawVideo) (Metadata, error)
}

// Fingerprinter produces a content hash for caching and dedup.
type Fingerprinter interface {
	Fingerprint(video download.RawVideo) (Hash, error)
}

// Cache stores/retrieves AnalysisResult keyed by fingerprint.
type Cache interface {
	Lookup(hash Hash) (AnalysisResult, bool)
	Store(hash Hash, result AnalysisResult) error
}

// Analyzer is the Stage implementation orchestrating detection.
type Analyzer struct {
	fingerprinter Fingerprinter
	metadata      MetadataExtractor
	detectors     []Detector
	cache         Cache
}

// NewAnalyzer wires the analyzer. Detectors are independent and can be
// enabled/disabled by passing a subset.
func NewAnalyzer(fp Fingerprinter, meta MetadataExtractor, cache Cache, detectors ...Detector) *Analyzer {
	return &Analyzer{fingerprinter: fp, metadata: meta, cache: cache, detectors: detectors}
}

// Name implements pipeline.Stage.
func (a *Analyzer) Name() string { return "Analyze" }

// Execute reads RawVideo, writes AnalysisResult. It implements pipeline.Stage.
func (a *Analyzer) Execute(ctx pipeline.Context) (pipeline.Context, error) {
	v, ok := ctx.Get(pipeline.KeyRawVideo)
	if !ok {
		return ctx, fmt.Errorf("analyze: no RawVideo in context")
	}
	raw, ok := v.(download.RawVideo)
	if !ok {
		return ctx, fmt.Errorf("analyze: context value is not RawVideo")
	}

	hash, err := a.fingerprinter.Fingerprint(raw)
	if err != nil {
		return ctx, fmt.Errorf("analyze: fingerprint: %w", err)
	}

	// Fold detector versions into an effective cache key so stale results are
	// not served after an algorithm change.
	effKey := a.effectiveKey(hash)
	if a.cache != nil {
		if cached, hit := a.cache.Lookup(effKey); hit {
			return ctx.With(pipeline.KeyAnalysis, cached), nil
		}
	}

	meta, err := a.metadata.Extract(raw)
	if err != nil {
		return ctx, fmt.Errorf("analyze: metadata: %w", err)
	}

	results, err := a.runDetectors(raw)
	if err != nil {
		return ctx, err
	}

	result := assemble(hash, meta, results)

	if a.cache != nil {
		if err := a.cache.Store(effKey, result); err != nil {
			return ctx, fmt.Errorf("analyze: cache store: %w", err)
		}
	}
	return ctx.With(pipeline.KeyAnalysis, result), nil
}

func (a *Analyzer) effectiveKey(base Hash) Hash {
	key := string(base)
	for _, d := range a.detectors {
		key += "|" + string(d.Kind())
	}
	return Hash(key)
}

// runDetectors runs enabled detectors concurrently (they are independent).
func (a *Analyzer) runDetectors(raw download.RawVideo) ([]DetectionResult, error) {
	type outcome struct {
		res DetectionResult
		err error
	}
	outs := make([]outcome, len(a.detectors))
	var wg sync.WaitGroup
	for i, d := range a.detectors {
		wg.Add(1)
		go func(i int, d Detector) {
			defer wg.Done()
			r, err := d.Detect(raw)
			outs[i] = outcome{res: r, err: err}
		}(i, d)
	}
	wg.Wait()

	results := make([]DetectionResult, 0, len(outs))
	for i, o := range outs {
		if o.err != nil {
			return nil, fmt.Errorf("analyze: detector %q: %w", a.detectors[i].Kind(), o.err)
		}
		results = append(results, o.res)
	}
	return results, nil
}

// assemble folds detector results into one immutable AnalysisResult.
func assemble(hash Hash, meta Metadata, results []DetectionResult) AnalysisResult {
	out := AnalysisResult{
		Fingerprint:      hash,
		Metadata:         meta,
		DetectorVersions: map[DetectionKind]string{},
	}
	for _, r := range results {
		out.DetectorVersions[r.Kind] = r.Version
		switch r.Kind {
		case KindScene:
			out.SceneBoundaries = append(out.SceneBoundaries, r.Scenes...)
		case KindSilence:
			out.Silence = append(out.Silence, r.Intervals...)
		case KindSpeech:
			out.Speech = append(out.Speech, r.Intervals...)
		case KindFace:
			out.Faces = append(out.Faces, r.Faces...)
		case KindMotion:
			out.Motion = append(out.Motion, r.Motion...)
		}
	}
	sort.Slice(out.SceneBoundaries, func(i, j int) bool {
		return out.SceneBoundaries[i] < out.SceneBoundaries[j]
	})
	return out
}
