package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"videoremix/internal/binaries"
	"videoremix/internal/download"
	"videoremix/internal/recipe"
)

type commandRunner func(name string, args ...string) ([]byte, error)

func execRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// resolve returns the resolved path for a tool, or its bare name as fallback.
func resolve(t binaries.Tool) string {
	if p, err := binaries.Path(t); err == nil {
		return p
	}
	return string(t)
}

// EffectStepToGraph maps a single EffectStep to an FFmpeg filter fragment. New
// effects register their mapping here without modifying the compiler loop.
type EffectStepToGraph func(step recipe.EffectStep) (string, error)

// DefaultEffectRegistry provides mappings for a small built-in effect set.
func DefaultEffectRegistry() map[string]EffectStepToGraph {
	return map[string]EffectStepToGraph{
		"brightness": func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("eq=brightness=%.3f", s.Params["value"]), nil
		},
		"contrast": func(s recipe.EffectStep) (string, error) {
			v := s.Params["value"]
			if v == 0 {
				v = 1
			}
			return fmt.Sprintf("eq=contrast=%.3f", v), nil
		},
		"saturation": func(s recipe.EffectStep) (string, error) {
			v := s.Params["value"]
			if v == 0 {
				v = 1
			}
			return fmt.Sprintf("eq=saturation=%.3f", v), nil
		},
		"hflip": func(s recipe.EffectStep) (string, error) {
			return "hflip", nil
		},
		"speed": func(s recipe.EffectStep) (string, error) {
			factor := s.Params["factor"]
			if factor <= 0 {
				factor = 1
			}
			// setpts is 1/factor for video speed.
			return fmt.Sprintf("setpts=%.4f*PTS", 1.0/factor), nil
		},
		"scale": func(s recipe.EffectStep) (string, error) {
			w := int(s.Params["width"])
			h := int(s.Params["height"])
			if w <= 0 || h <= 0 {
				return "scale=iw:ih", nil
			}
			return fmt.Sprintf("scale=%d:%d", w, h), nil
		},
		"crop": func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("crop=iw*%.3f:ih*%.3f",
				clamp01default(s.Params["w"], 1), clamp01default(s.Params["h"], 1)), nil
		},
	}
}

func clamp01default(v, def float64) float64 {
	if v <= 0 || v > 1 {
		return def
	}
	return v
}

// ChainCompiler converts ordered effect steps into a single video filter chain.
type ChainCompiler struct {
	registry map[string]EffectStepToGraph
}

func NewChainCompiler(registry map[string]EffectStepToGraph) *ChainCompiler {
	return &ChainCompiler{registry: registry}
}

func (c *ChainCompiler) Compile(t Timeline, steps []recipe.EffectStep) (FilterGraph, error) {
	ordered := append([]recipe.EffectStep(nil), steps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })

	fragments := make([]string, 0, len(ordered))
	for _, s := range ordered {
		mapper, ok := c.registry[s.EffectID]
		if !ok {
			return FilterGraph{}, fmt.Errorf("render: unknown effect id %q", s.EffectID)
		}
		frag, err := mapper(s)
		if err != nil {
			return FilterGraph{}, fmt.Errorf("render: compile effect %q: %w", s.EffectID, err)
		}
		if frag != "" {
			fragments = append(fragments, frag)
		}
	}
	chain := strings.Join(fragments, ",")
	return FilterGraph{FilterComplex: chain}, nil
}

// GPUProbe selects a hardware path, falling back to CPU when GPU is
// unavailable/unconfigured.
type GPUProbe struct {
	run    commandRunner
	binary string
}

func NewGPUProbe() *GPUProbe { return &GPUProbe{run: execRunner, binary: resolve(binaries.FFmpeg)} }

func (p *GPUProbe) Select(cfg RenderConfig) (EncodingPath, error) {
	preset := cfg.Preset
	if preset == "" {
		preset = "medium"
	}
	if cfg.PreferGPU && p.hasNVENC() {
		return EncodingPath{UseGPU: true, Codec: "h264_nvenc", Preset: preset}, nil
	}
	return EncodingPath{UseGPU: false, Codec: "libx264", Preset: preset}, nil
}

func (p *GPUProbe) hasNVENC() bool {
	out, err := p.run(p.binary, "-hide_banner", "-encoders")
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "h264_nvenc")
}

// CLIFFmpegExecutor invokes the local ffmpeg CLI.
type CLIFFmpegExecutor struct {
	binary string
	run    commandRunner
}

func NewCLIFFmpegExecutor() *CLIFFmpegExecutor {
	return &CLIFFmpegExecutor{binary: resolve(binaries.FFmpeg), run: execRunner}
}

func (e *CLIFFmpegExecutor) Execute(t Timeline, graph FilterGraph, path EncodingPath, outputPath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir output: %w", err)
	}
	args := []string{"-y", "-i", t.Source.FilePath}

	// Apply the first segment as a trim window when present.
	if len(t.Segments) > 0 {
		seg := t.Segments[0]
		args = append(args, "-ss", strconv.FormatFloat(seg.Start.Seconds(), 'f', 3, 64))
		if seg.End > seg.Start {
			args = append(args, "-t", strconv.FormatFloat((seg.End-seg.Start).Seconds(), 'f', 3, 64))
		}
	}
	if graph.FilterComplex != "" {
		args = append(args, "-vf", graph.FilterComplex)
	}
	args = append(args, "-c:v", path.Codec, "-preset", path.Preset, "-c:a", "aac", outputPath)

	if _, err := e.run(e.binary, args...); err != nil {
		return "", fmt.Errorf("ffmpeg exec: %w", err)
	}
	return outputPath, nil
}

// FileOutputValidator confirms the output exists and is non-empty.
type FileOutputValidator struct{}

func NewFileOutputValidator() *FileOutputValidator { return &FileOutputValidator{} }

func (FileOutputValidator) Validate(outputPath string, _ Timeline) (RenderResult, error) {
	info, err := os.Stat(outputPath)
	if err != nil {
		return RenderResult{}, fmt.Errorf("stat output: %w", err)
	}
	if info.Size() == 0 {
		return RenderResult{}, fmt.Errorf("output is zero-byte (truncated render)")
	}
	sum, err := fileChecksum(outputPath)
	if err != nil {
		return RenderResult{}, err
	}
	return RenderResult{OutputPath: outputPath, Checksum: sum, DurationOK: true}, nil
}

// --- helpers ---

func outputPathFor(dir string, id recipe.RecipeID) string {
	if dir == "" {
		dir = "output"
	}
	name := string(id)
	if len(name) > 16 {
		name = name[:16]
	}
	return filepath.Join(dir, "render_"+name+".mp4")
}

func fileChecksum(path string) (string, error) {
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

var _ = download.RawVideo{}
