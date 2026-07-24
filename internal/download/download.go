// Package download is the sole entry point for acquiring source video bytes
// into the system. It normalizes YouTube, TikTok, Instagram, Facebook and
// local file inputs into one uniform RawVideo output.
//
// Download performs no analysis and no transformation — only acquisition,
// verification and normalization.
package download

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"videoremix/internal/pipeline"
)

// Platform identifies the origin of an input.
type Platform string

const (
	PlatformYouTube   Platform = "youtube"
	PlatformTikTok    Platform = "tiktok"
	PlatformInstagram Platform = "instagram"
	PlatformFacebook  Platform = "facebook"
	PlatformLocalFile Platform = "local"
	PlatformUnknown   Platform = "unknown"
)

// RawVideo is the uniform output value object produced regardless of source.
type RawVideo struct {
	FilePath  string        // local path to the validated asset
	Duration  time.Duration // container duration
	Container string        // e.g. "mp4", "mkv"
	Checksum  string        // integrity checksum
	Source    SourceMeta    // where it came from
}

// SourceMeta describes provenance of the raw video.
type SourceMeta struct {
	Platform Platform
	Input    string // original input string (URL or path)
	Title    string
}

// ProgressEvent reports download progress for long-running fetches.
type ProgressEvent struct {
	Input      string
	Percent    float64
	BytesDone  int64
	BytesTotal int64
	Message    string
}

// Sentinel errors distinguishing transient (retryable) vs permanent failures.
var (
	// ErrTransient indicates a retryable failure (e.g. network timeout).
	ErrTransient = errors.New("download: transient error")
	// ErrPermanent indicates a non-retryable failure (e.g. 404, private video).
	ErrPermanent = errors.New("download: permanent error")
	// ErrUnsupported indicates the input platform is not supported.
	ErrUnsupported = errors.New("download: unsupported input")
)

// PlatformFetcher encapsulates platform-specific acquisition logic behind a
// common interface.
type PlatformFetcher interface {
	Platform() Platform
	// Fetch acquires the source to a local file, emitting progress events.
	Fetch(input string, progress chan<- ProgressEvent) (filePath string, err error)
}

// FileValidator confirms an acquired file is playable, non-corrupt and within
// configured limits, and produces the normalized RawVideo.
type FileValidator interface {
	Validate(filePath string, src SourceMeta) (RawVideo, error)
}

// Config controls download limits and policy.
type Config struct {
	MaxDuration time.Duration
	MaxSizeByte int64
	WorkDir     string
	MaxRetries  int
}

// DefaultConfig returns sane defaults.
func DefaultConfig(workDir string) Config {
	return Config{
		MaxDuration: 6 * time.Hour,
		MaxSizeByte: 8 << 30, // 8 GiB
		WorkDir:     workDir,
		MaxRetries:  3,
	}
}

// InputClassifier determines the platform (or local file) from a raw string.
type InputClassifier struct{}

// Classify inspects the input and returns its platform.
func (InputClassifier) Classify(input string) Platform {
	trimmed := strings.TrimSpace(input)
	u, err := url.Parse(trimmed)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		host := strings.ToLower(u.Host)
		switch {
		case strings.Contains(host, "youtube.") || strings.Contains(host, "youtu.be"):
			return PlatformYouTube
		case strings.Contains(host, "tiktok."):
			return PlatformTikTok
		case strings.Contains(host, "instagram."):
			return PlatformInstagram
		case strings.Contains(host, "facebook.") || strings.Contains(host, "fb.watch"):
			return PlatformFacebook
		default:
			return PlatformUnknown
		}
	}
	// Not a URL: treat as a local path if it exists.
	if _, statErr := os.Stat(trimmed); statErr == nil {
		return PlatformLocalFile
	}
	return PlatformUnknown
}

// Downloader is the Stage implementation for acquisition.
type Downloader struct {
	classifier InputClassifier
	fetchers   map[Platform]PlatformFetcher
	validator  FileValidator
	cfg        Config
	progress   chan<- ProgressEvent
}

// NewDownloader wires a Downloader with its fetchers and validator.
func NewDownloader(cfg Config, validator FileValidator, progress chan<- ProgressEvent, fetchers ...PlatformFetcher) *Downloader {
	m := make(map[Platform]PlatformFetcher, len(fetchers))
	for _, f := range fetchers {
		m[f.Platform()] = f
	}
	return &Downloader{
		classifier: InputClassifier{},
		fetchers:   m,
		validator:  validator,
		cfg:        cfg,
		progress:   progress,
	}
}

// Name implements pipeline.Stage.
func (d *Downloader) Name() string { return "Download" }

// Execute reads the input from Context, acquires + validates it, and writes a
// RawVideo back. It implements pipeline.Stage.
func (d *Downloader) Execute(ctx pipeline.Context) (pipeline.Context, error) {
	v, ok := ctx.Get(pipeline.KeyInput)
	if !ok {
		return ctx, fmt.Errorf("%w: no input in context", ErrPermanent)
	}
	input, ok := v.(string)
	if !ok {
		return ctx, fmt.Errorf("%w: input is not a string", ErrPermanent)
	}

	platform := d.classifier.Classify(input)
	fetcher, ok := d.fetchers[platform]
	if !ok {
		return ctx, fmt.Errorf("%w: no fetcher for platform %q (input %q)", ErrUnsupported, platform, input)
	}

	filePath, err := d.fetchWithRetry(fetcher, input)
	if err != nil {
		return ctx, err
	}

	raw, err := d.validator.Validate(filePath, SourceMeta{Platform: platform, Input: input})
	if err != nil {
		return ctx, fmt.Errorf("validation failed for %q: %w", filePath, err)
	}

	return ctx.With(pipeline.KeyRawVideo, raw), nil
}

// fetchWithRetry retries only on classified-transient errors.
func (d *Downloader) fetchWithRetry(fetcher PlatformFetcher, input string) (string, error) {
	var lastErr error
	attempts := d.cfg.MaxRetries + 1
	for i := 0; i < attempts; i++ {
		path, err := fetcher.Fetch(input, d.progress)
		if err == nil {
			return path, nil
		}
		lastErr = err
		if !errors.Is(err, ErrTransient) {
			return "", err // permanent: do not retry
		}
	}
	return "", fmt.Errorf("exhausted %d attempts: %w", attempts, lastErr)
}
