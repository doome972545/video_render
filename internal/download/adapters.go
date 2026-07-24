package download

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"videoremix/internal/binaries"
)

// commandRunner allows tests to stub external command execution.
type commandRunner func(name string, args ...string) ([]byte, error)

func execRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// resolve returns the resolved path for a tool, falling back to its bare name
// (so tests that stub commandRunner still work without a real binary present).
func resolve(t binaries.Tool) string {
	if p, err := binaries.Path(t); err == nil {
		return p
	}
	return string(t)
}

// ytDLPFetcher is a PlatformFetcher backed by the yt-dlp CLI. It covers
// YouTube, TikTok, Instagram and Facebook — each platform gets a distinct
// instance so breakage stays contained to one adapter (per the doc's risk
// mitigation).
type ytDLPFetcher struct {
	platform Platform
	workDir  string
	binary   string
	run      commandRunner
}

// NewYouTubeFetcher, etc. construct platform-scoped yt-dlp fetchers.
func NewYouTubeFetcher(workDir string) PlatformFetcher   { return newYTDLP(PlatformYouTube, workDir) }
func NewTikTokFetcher(workDir string) PlatformFetcher    { return newYTDLP(PlatformTikTok, workDir) }
func NewInstagramFetcher(workDir string) PlatformFetcher { return newYTDLP(PlatformInstagram, workDir) }
func NewFacebookFetcher(workDir string) PlatformFetcher  { return newYTDLP(PlatformFacebook, workDir) }

func newYTDLP(p Platform, workDir string) *ytDLPFetcher {
	return &ytDLPFetcher{platform: p, workDir: workDir, binary: resolve(binaries.YTDLP), run: execRunner}
}

func (f *ytDLPFetcher) Platform() Platform { return f.platform }

func (f *ytDLPFetcher) Fetch(input string, progress chan<- ProgressEvent) (string, error) {
	if err := os.MkdirAll(f.workDir, 0o755); err != nil {
		return "", fmt.Errorf("%w: mkdir workdir: %v", ErrPermanent, err)
	}
	emit(progress, ProgressEvent{Input: input, Percent: 0, Message: "starting " + string(f.platform) + " fetch"})

	out := filepath.Join(f.workDir, "src_%(id)s.%(ext)s")
	// Point yt-dlp at our resolved ffmpeg so format merging works even when
	// ffmpeg isn't on PATH (embedded/bundled mode).
	args := []string{"-o", out, "--no-playlist"}
	if ff := resolve(binaries.FFmpeg); ff != "" {
		args = append(args, "--ffmpeg-location", ff)
	}
	args = append(args, input)
	_, err := f.run(f.binary, args...)
	if err != nil {
		// yt-dlp exit codes don't cleanly separate transient/permanent; classify
		// heuristically from stderr where available.
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.ToLower(string(ee.Stderr))
			if strings.Contains(msg, "timed out") || strings.Contains(msg, "temporary") {
				return "", fmt.Errorf("%w: %v", ErrTransient, err)
			}
		}
		return "", fmt.Errorf("%w: %s fetch: %v", ErrPermanent, f.platform, err)
	}

	// Locate the most recently written file in workDir as the result.
	path, err := newestFile(f.workDir, "src_")
	if err != nil {
		return "", fmt.Errorf("%w: locate downloaded file: %v", ErrPermanent, err)
	}
	emit(progress, ProgressEvent{Input: input, Percent: 100, Message: "fetch complete"})
	return path, nil
}

// LocalFileLoader validates and registers an existing local file as a source,
// without network access.
type LocalFileLoader struct{}

func NewLocalFileLoader() PlatformFetcher { return &LocalFileLoader{} }

func (LocalFileLoader) Platform() Platform { return PlatformLocalFile }

func (LocalFileLoader) Fetch(input string, progress chan<- ProgressEvent) (string, error) {
	if _, err := os.Stat(input); err != nil {
		return "", fmt.Errorf("%w: local file not found: %v", ErrPermanent, err)
	}
	emit(progress, ProgressEvent{Input: input, Percent: 100, Message: "local file loaded"})
	return input, nil
}

// FFprobeValidator confirms the file is playable and within limits using
// ffprobe, and constructs the RawVideo.
type FFprobeValidator struct {
	cfg    Config
	binary string
	run    commandRunner
}

func NewFFprobeValidator(cfg Config) *FFprobeValidator {
	return &FFprobeValidator{cfg: cfg, binary: resolve(binaries.FFprobe), run: execRunner}
}

func (v *FFprobeValidator) Validate(filePath string, src SourceMeta) (RawVideo, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return RawVideo{}, fmt.Errorf("%w: stat: %v", ErrPermanent, err)
	}
	if info.Size() == 0 {
		return RawVideo{}, fmt.Errorf("%w: zero-byte file (partial download)", ErrPermanent)
	}
	if v.cfg.MaxSizeByte > 0 && info.Size() > v.cfg.MaxSizeByte {
		return RawVideo{}, fmt.Errorf("%w: file %d bytes exceeds max %d", ErrPermanent, info.Size(), v.cfg.MaxSizeByte)
	}

	dur, container := v.probe(filePath)
	if v.cfg.MaxDuration > 0 && dur > v.cfg.MaxDuration {
		return RawVideo{}, fmt.Errorf("%w: duration %s exceeds max %s", ErrPermanent, dur, v.cfg.MaxDuration)
	}

	sum, err := checksum(filePath)
	if err != nil {
		return RawVideo{}, fmt.Errorf("%w: checksum: %v", ErrPermanent, err)
	}

	return RawVideo{
		FilePath:  filePath,
		Duration:  dur,
		Container: container,
		Checksum:  sum,
		Source:    src,
	}, nil
}

// probe returns duration and container format; on ffprobe failure it falls back
// to conservative defaults rather than failing validation outright (the file
// still passed size/existence checks).
func (v *FFprobeValidator) probe(filePath string) (time.Duration, string) {
	container := strings.TrimPrefix(strings.ToLower(filepath.Ext(filePath)), ".")
	out, err := v.run(v.binary,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	if err != nil {
		return 0, container
	}
	secs, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if perr != nil {
		return 0, container
	}
	return time.Duration(secs * float64(time.Second)), container
}

// --- helpers ---

func emit(ch chan<- ProgressEvent, ev ProgressEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default: // never block the fetch on a slow consumer
	}
}

func checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func newestFile(dir, prefix string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best, bestMod = filepath.Join(dir, e.Name()), fi.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no file with prefix %q in %q", prefix, dir)
	}
	return best, nil
}
