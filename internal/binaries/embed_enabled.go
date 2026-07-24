//go:build embed_binaries

package binaries

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// embedded holds the tool executables. It is only compiled when building with
// `-tags embed_binaries`, producing a large but fully self-contained binary.
//
//go:embed embedded/*
var embedded embed.FS

// hasEmbedded reports whether real tool executables were embedded at build
// time. It returns false when only the committed placeholder is present (fresh
// clone without fetched binaries), so resolution falls back to bundle/PATH.
func hasEmbedded() bool {
	for _, t := range []Tool{FFmpeg, FFprobe, YTDLP} {
		if _, err := embedded.ReadFile("embedded/" + exeName(t)); err == nil {
			return true
		}
	}
	return false
}

// extractEmbedded materializes an embedded tool to a per-user cache directory,
// reusing an already-extracted copy when its size matches (cheap integrity
// check that avoids re-writing ~230MB on every run).
func (r *Resolver) extractEmbedded(t Tool) (string, error) {
	name := exeName(t)
	data, err := embedded.ReadFile("embedded/" + name)
	if err != nil {
		return "", fmt.Errorf("%w: read %s: %v", errNoEmbed, name, err)
	}

	dir, err := r.cacheDir()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, name)

	// Reuse if an extracted copy already exists with the same size.
	if fi, statErr := os.Stat(dst); statErr == nil && fi.Size() == int64(len(data)) {
		return dst, nil
	}

	if err := writeExecutable(dst, data); err != nil {
		return "", err
	}
	return dst, nil
}

// cacheDir returns (and lazily creates) a stable per-user cache directory keyed
// by a short hash of the embedded contents, so different builds don't collide.
func (r *Resolver) cacheDir() (string, error) {
	if r.extractDir != "" {
		return r.extractDir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "videoremix", "bin-"+embeddedFingerprint())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("binaries: mkdir cache: %w", err)
	}
	r.extractDir = dir
	return dir, nil
}

// embeddedFingerprint hashes the names+sizes of embedded files to produce a
// short, stable cache key without hashing hundreds of MB of content.
func embeddedFingerprint() string {
	h := sha256.New()
	_ = fs.WalkDir(embedded, "embedded", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		fmt.Fprintf(h, "%s:%d\n", p, info.Size())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// writeExecutable writes data to path atomically and marks it executable.
func writeExecutable(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("binaries: create %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, bytesReader(data)); err != nil {
		f.Close()
		return fmt.Errorf("binaries: write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("binaries: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("binaries: rename %s: %w", path, err)
	}
	return nil
}
