package analyze

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"videoremix/internal/binaries"
	"videoremix/internal/download"
)

type commandRunner func(name string, args ...string) ([]byte, error)

func execRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// resolve returns the resolved path for a tool, or its bare name as fallback.
func resolve(t binaries.Tool) string {
	if p, err := binaries.Path(t); err == nil {
		return p
	}
	return string(t)
}

// ChecksumFingerprinter uses the RawVideo checksum (already computed in
// Download) as a stable content fingerprint. A perceptual hash could replace
// this behind the same port.
type ChecksumFingerprinter struct{}

func NewChecksumFingerprinter() *ChecksumFingerprinter { return &ChecksumFingerprinter{} }

func (ChecksumFingerprinter) Fingerprint(video download.RawVideo) (Hash, error) {
	if video.Checksum != "" {
		return Hash(video.Checksum), nil
	}
	return Hash(video.FilePath), nil
}

// FFprobeMetadata extracts container metadata via ffprobe.
type FFprobeMetadata struct {
	binary string
	run    commandRunner
}

func NewFFprobeMetadata() *FFprobeMetadata {
	return &FFprobeMetadata{binary: resolve(binaries.FFprobe), run: execRunner}
}

func (m *FFprobeMetadata) Extract(video download.RawVideo) (Metadata, error) {
	meta := Metadata{
		Duration:  video.Duration,
		Container: video.Container,
	}
	// width,height,codec,framerate from the first video stream.
	out, err := m.run(m.binary,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,codec_name,avg_frame_rate",
		"-of", "default=noprint_wrappers=1",
		video.FilePath,
	)
	if err != nil {
		// Degrade gracefully: return what we have from RawVideo.
		return meta, nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "width":
			meta.Width, _ = strconv.Atoi(v)
		case "height":
			meta.Height, _ = strconv.Atoi(v)
		case "codec_name":
			meta.Codec = v
		case "avg_frame_rate":
			meta.Framerate = parseFrameRate(v)
		}
	}
	return meta, nil
}

func parseFrameRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	n, _ := strconv.ParseFloat(num, 64)
	d, _ := strconv.ParseFloat(den, 64)
	if d == 0 {
		return 0
	}
	return n / d
}

// --- Detectors ---

// SceneDetector detects shot/scene boundaries via ffmpeg's scene filter.
type SceneDetector struct {
	Threshold float64
	binary    string
	run       commandRunner
}

func NewSceneDetector(threshold float64) *SceneDetector {
	if threshold <= 0 {
		threshold = 0.4
	}
	return &SceneDetector{Threshold: threshold, binary: resolve(binaries.FFmpeg), run: execRunner}
}

func (d *SceneDetector) Kind() DetectionKind { return KindScene }

func (d *SceneDetector) Detect(video download.RawVideo) (DetectionResult, error) {
	res := DetectionResult{Kind: KindScene, Version: "ffmpeg-scene-v1"}
	// ffmpeg prints scene scores to stderr via showinfo; parsing is
	// best-effort. On any failure we return an empty (but valid) result so the
	// overall analysis still succeeds.
	out, err := d.run(d.binary,
		"-i", video.FilePath,
		"-filter:v", "select='gt(scene,"+strconv.FormatFloat(d.Threshold, 'f', 2, 64)+")',showinfo",
		"-f", "null", "-",
	)
	if err != nil {
		return res, nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "pts_time:") {
			continue
		}
		idx := strings.Index(line, "pts_time:")
		field := strings.Fields(line[idx+len("pts_time:"):])
		if len(field) == 0 {
			continue
		}
		if secs, perr := strconv.ParseFloat(field[0], 64); perr == nil {
			res.Scenes = append(res.Scenes, time.Duration(secs*float64(time.Second)))
		}
	}
	return res, nil
}

// SilenceDetector detects silence intervals via ffmpeg silencedetect.
type SilenceDetector struct {
	NoiseDB     float64 // e.g. -30
	MinDuration time.Duration
	binary      string
	run         commandRunner
}

func NewSilenceDetector(noiseDB float64, minDur time.Duration) *SilenceDetector {
	if noiseDB == 0 {
		noiseDB = -30
	}
	if minDur == 0 {
		minDur = 500 * time.Millisecond
	}
	return &SilenceDetector{NoiseDB: noiseDB, MinDuration: minDur, binary: resolve(binaries.FFmpeg), run: execRunner}
}

func (d *SilenceDetector) Kind() DetectionKind { return KindSilence }

func (d *SilenceDetector) Detect(video download.RawVideo) (DetectionResult, error) {
	res := DetectionResult{Kind: KindSilence, Version: "ffmpeg-silencedetect-v1"}
	filter := "silencedetect=noise=" + strconv.FormatFloat(d.NoiseDB, 'f', 0, 64) +
		"dB:d=" + strconv.FormatFloat(d.MinDuration.Seconds(), 'f', 2, 64)
	out, err := d.run(d.binary, "-i", video.FilePath, "-af", filter, "-f", "null", "-")
	if err != nil {
		return res, nil
	}
	var start time.Duration
	var haveStart bool
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "silence_start:"); i >= 0 {
			if s := parseTrailingFloat(line[i+len("silence_start:"):]); s >= 0 {
				start = time.Duration(s * float64(time.Second))
				haveStart = true
			}
		}
		if i := strings.Index(line, "silence_end:"); i >= 0 && haveStart {
			if s := parseTrailingFloat(line[i+len("silence_end:"):]); s >= 0 {
				end := time.Duration(s * float64(time.Second))
				res.Intervals = append(res.Intervals, Interval{Start: start, End: end, Confidence: 1})
				haveStart = false
			}
		}
	}
	return res, nil
}

// SpeechDetector infers speech intervals as the complement of silence over the
// video duration. This reuses silence output rather than duplicating detection.
type SpeechDetector struct {
	silence *SilenceDetector
}

func NewSpeechDetector(sil *SilenceDetector) *SpeechDetector { return &SpeechDetector{silence: sil} }

func (d *SpeechDetector) Kind() DetectionKind { return KindSpeech }

func (d *SpeechDetector) Detect(video download.RawVideo) (DetectionResult, error) {
	res := DetectionResult{Kind: KindSpeech, Version: "complement-silence-v1"}
	sil, _ := d.silence.Detect(video)
	cursor := time.Duration(0)
	for _, s := range sil.Intervals {
		if s.Start > cursor {
			res.Intervals = append(res.Intervals, Interval{Start: cursor, End: s.Start, Confidence: 0.7})
		}
		if s.End > cursor {
			cursor = s.End
		}
	}
	if video.Duration > cursor {
		res.Intervals = append(res.Intervals, Interval{Start: cursor, End: video.Duration, Confidence: 0.7})
	}
	return res, nil
}

// NoopFaceDetector is a safe default face detector (presence only) that
// reports no faces. Replace with an OpenCV/DNN adapter behind the same port.
type NoopFaceDetector struct{}

func NewNoopFaceDetector() *NoopFaceDetector { return &NoopFaceDetector{} }
func (NoopFaceDetector) Kind() DetectionKind { return KindFace }
func (NoopFaceDetector) Detect(video download.RawVideo) (DetectionResult, error) {
	return DetectionResult{Kind: KindFace, Version: "noop-v1"}, nil
}

// UniformMotionDetector emits a single low-intensity motion score spanning the
// whole video as a safe default. Replace with optical-flow analysis behind the
// same port.
type UniformMotionDetector struct{}

func NewUniformMotionDetector() *UniformMotionDetector { return &UniformMotionDetector{} }
func (UniformMotionDetector) Kind() DetectionKind      { return KindMotion }
func (UniformMotionDetector) Detect(video download.RawVideo) (DetectionResult, error) {
	res := DetectionResult{Kind: KindMotion, Version: "uniform-v1"}
	if video.Duration > 0 {
		res.Motion = []MotionScore{{
			Interval:  Interval{Start: 0, End: video.Duration, Confidence: 0.5},
			Intensity: 0.3,
		}}
	}
	return res, nil
}

// --- caches ---

// MemoryCache is an in-memory AnalysisCache suitable for a single process run.
type MemoryCache struct {
	mu sync.RWMutex
	m  map[Hash]AnalysisResult
}

func NewMemoryCache() *MemoryCache { return &MemoryCache{m: map[Hash]AnalysisResult{}} }

func (c *MemoryCache) Lookup(hash Hash) (AnalysisResult, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.m[hash]
	return r, ok
}

func (c *MemoryCache) Store(hash Hash, result AnalysisResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[hash] = result
	return nil
}

// --- helpers ---

func parseTrailingFloat(s string) float64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return -1
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1
	}
	return f
}
