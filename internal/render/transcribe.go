package render

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"videoremix/internal/binaries"
)

// Transcriber turns speech in a media file into a subtitle (.srt) file using
// ffmpeg's built-in whisper.cpp filter.
type Transcriber struct {
	binary    string
	modelPath string
	run       commandRunner
}

// NewTranscriber constructs a Transcriber. modelPath may be empty, in which
// case binaries.WhisperModel() is used to locate a model.
func NewTranscriber(modelPath string) *Transcriber {
	if modelPath == "" {
		modelPath = binaries.WhisperModel()
	}
	return &Transcriber{
		binary:    resolve(binaries.FFmpeg),
		modelPath: modelPath,
		run:       execRunner,
	}
}

// Available reports whether a whisper model was found (auto-subtitle possible).
func (t *Transcriber) Available() bool { return t.modelPath != "" }

// Transcribe extracts speech from srcPath and writes an .srt file to srtPath.
// language "" or "auto" enables auto-detection. Returns the srt path on success.
func (t *Transcriber) Transcribe(srcPath, srtPath, language string) (string, error) {
	if t.modelPath == "" {
		return "", fmt.Errorf("transcribe: no whisper model found (set VIDEOREMIX_WHISPER_MODEL)")
	}
	if language == "" {
		language = "auto"
	}
	if err := os.MkdirAll(filepath.Dir(srtPath), 0o755); err != nil {
		return "", fmt.Errorf("transcribe: mkdir: %w", err)
	}
	// Remove a stale file so a failed run doesn't look successful.
	_ = os.Remove(srtPath)

	af := fmt.Sprintf(
		"whisper=model='%s':language=%s:format=srt:destination=%s",
		escapeFilterPath(t.modelPath), language, escapeFilterPath(srtPath),
	)

	out, err := t.run(t.binary, "-i", srcPath, "-af", af, "-f", "null", "-")
	if err != nil {
		return "", fmt.Errorf("transcribe: ffmpeg whisper: %w (%s)", err, tail(string(out), 300))
	}
	if !fileExistsNonEmpty(srtPath) {
		return "", fmt.Errorf("transcribe: no subtitles produced (no speech detected?)")
	}
	return srtPath, nil
}

// escapeFilterPath escapes a path for use inside an ffmpeg filter argument
// (backslashes to forward slashes; escape the drive colon on Windows).
func escapeFilterPath(p string) string {
	p = strings.ReplaceAll(p, `\`, `/`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	return p
}

func fileExistsNonEmpty(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}
