// Package binaries resolves the filesystem path to the external tools the
// pipeline shells out to (ffmpeg, ffprobe, yt-dlp).
//
// Resolution order:
//  1. Embedded binary (when built with the `embed_binaries` tag) — extracted
//     once to a per-user cache directory and reused across runs.
//  2. A sibling `bin/` directory next to the running executable (bundle mode).
//  3. The system PATH (development / pre-installed environments).
//
// This lets a single `.exe` be fully portable when built with embedding, while
// keeping normal `go build`/`go test` fast and small by default.
package binaries

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// errNoEmbed is a shared sentinel used by both build variants.
var errNoEmbed = errors.New("binaries: no embedded tools in this build")

// bytesReader wraps a byte slice as an io.Reader (used by the embed variant).
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Tool identifies one external executable.
type Tool string

const (
	FFmpeg  Tool = "ffmpeg"
	FFprobe Tool = "ffprobe"
	YTDLP   Tool = "yt-dlp"
)

// exeName returns the platform-specific executable file name for a tool.
func exeName(t Tool) string {
	if runtime.GOOS == "windows" {
		return string(t) + ".exe"
	}
	return string(t)
}

// WhisperModel locates a whisper.cpp model file for speech-to-text. Resolution
// order:
//  1. VIDEOREMIX_WHISPER_MODEL env var (explicit path).
//  2. A "whisper" folder next to the executable (bundle mode).
//  3. Known local install paths (e.g. the LM Studio models dir).
//
// It returns the first existing model file, or "" if none is found. The model
// is intentionally not embedded (it is ~1.5 GB); it is resolved at runtime.
func WhisperModel() string {
	if p := os.Getenv("VIDEOREMIX_WHISPER_MODEL"); p != "" {
		if fileExists(p) {
			return p
		}
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "whisper", "ggml-large-v3-turbo.bin"),
			filepath.Join(dir, "whisper", "ggml-base.bin"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".lmstudio", "models", "whisper-cpp", "ggml-large-v3-turbo.bin"),
			filepath.Join(home, ".cache", "whisper", "ggml-large-v3-turbo.bin"),
		)
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// Resolver locates tool executables and caches the resolved paths.
type Resolver struct {
	mu       sync.Mutex
	resolved map[Tool]string
	// extractDir is where embedded binaries are materialized. Empty when not
	// using embedded binaries.
	extractDir string
}

// defaultResolver is the process-wide resolver used by the package-level Path.
var defaultResolver = &Resolver{resolved: map[Tool]string{}}

// NewResolver constructs an isolated resolver (useful in tests).
func NewResolver() *Resolver { return &Resolver{resolved: map[Tool]string{}} }

// Path returns an absolute path to the given tool using the default resolver.
func Path(t Tool) (string, error) { return defaultResolver.Path(t) }

// Path returns an absolute path to the given tool, resolving and caching it.
func (r *Resolver) Path(t Tool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, ok := r.resolved[t]; ok {
		return p, nil
	}

	// 1. Embedded (only present when built with the embed tag).
	if hasEmbedded() {
		p, err := r.extractEmbedded(t)
		if err == nil {
			r.resolved[t] = p
			return p, nil
		}
		// fall through to bundle/PATH on extraction failure
	}

	// 2. Sibling bin/ directory next to the executable.
	if p, ok := siblingBin(t); ok {
		r.resolved[t] = p
		return p, nil
	}

	// 3. System PATH.
	if p, err := exec.LookPath(string(t)); err == nil {
		r.resolved[t] = p
		return p, nil
	}
	// LookPath on Windows also matches with .exe; try explicit name too.
	if p, err := exec.LookPath(exeName(t)); err == nil {
		r.resolved[t] = p
		return p, nil
	}

	return "", fmt.Errorf("binaries: %q not found (not embedded, no bin/ sibling, not on PATH)", t)
}

// siblingBin looks for the tool in a `bin/` folder beside the executable.
func siblingBin(t Tool) (string, bool) {
	exePath, err := os.Executable()
	if err != nil {
		return "", false
	}
	candidate := filepath.Join(filepath.Dir(exePath), "bin", exeName(t))
	if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
		return candidate, true
	}
	return "", false
}
