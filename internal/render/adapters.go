package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// EffectKind classifies whether an effect applies to the video or audio chain.
type EffectKind int

const (
	KindVideo EffectKind = iota
	KindAudio
)

// EffectDef defines how one effect ID compiles to an FFmpeg filter fragment,
// and which chain (video/audio) it belongs to.
type EffectDef struct {
	Kind    EffectKind
	Compile func(step recipe.EffectStep) (string, error)
}

// EffectStepToGraph is kept for backward compatibility (video-only mapper).
type EffectStepToGraph func(step recipe.EffectStep) (string, error)

// DefaultEffectRegistry provides mappings for the built-in effect set, covering
// both video (-vf) and audio (-af) effects. Add new effects here without
// touching the compiler loop.
func DefaultEffectRegistry() map[string]EffectDef {
	return map[string]EffectDef{
		// --- Video effects ---
		"brightness": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("eq=brightness=%.3f", s.Params["value"]), nil
		}},
		"contrast": {KindVideo, func(s recipe.EffectStep) (string, error) {
			v := def(s.Params["value"], 1)
			return fmt.Sprintf("eq=contrast=%.3f", v), nil
		}},
		"saturation": {KindVideo, func(s recipe.EffectStep) (string, error) {
			v := def(s.Params["value"], 1)
			return fmt.Sprintf("eq=saturation=%.3f", v), nil
		}},
		"gamma": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("eq=gamma=%.3f", def(s.Params["value"], 1)), nil
		}},
		"hflip": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return "hflip", nil
		}},
		"speed": {KindVideo, func(s recipe.EffectStep) (string, error) {
			factor := def(s.Params["factor"], 1)
			return fmt.Sprintf("setpts=%.4f*PTS", 1.0/factor), nil
		}},
		"scale": {KindVideo, func(s recipe.EffectStep) (string, error) {
			w := int(s.Params["width"])
			h := int(s.Params["height"])
			if w <= 0 || h <= 0 {
				return "scale=iw:ih", nil
			}
			return fmt.Sprintf("scale=%d:%d", w, h), nil
		}},
		"crop": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("crop=iw*%.3f:ih*%.3f",
				clamp01default(s.Params["w"], 1), clamp01default(s.Params["h"], 1)), nil
		}},
		"blur": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("boxblur=%.2f", def(s.Params["radius"], 2)), nil
		}},
		"sharpen": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("unsharp=5:5:%.2f", def(s.Params["amount"], 1)), nil
		}},
		"rotate": {KindVideo, func(s recipe.EffectStep) (string, error) {
			// degrees -> radians
			return fmt.Sprintf("rotate=%.4f*PI/180", s.Params["degrees"]), nil
		}},
		"vignette": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return "vignette", nil
		}},
		"hue": {KindVideo, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("hue=h=%.1f", s.Params["degrees"]), nil
		}},
		"zoom": {KindVideo, func(s recipe.EffectStep) (string, error) {
			z := def(s.Params["factor"], 1.1)
			// Scale up then center-crop back to original size = a static zoom.
			return fmt.Sprintf("scale=iw*%.3f:ih*%.3f,crop=iw/%.3f:ih/%.3f", z, z, z, z), nil
		}},

		// --- Audio effects ---
		"volume": {KindAudio, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("volume=%.3f", def(s.Params["value"], 1)), nil
		}},
		"afade_in": {KindAudio, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("afade=t=in:st=0:d=%.2f", def(s.Params["duration"], 1)), nil
		}},
		"bass": {KindAudio, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("bass=g=%.1f", s.Params["gain"]), nil
		}},
		"treble": {KindAudio, func(s recipe.EffectStep) (string, error) {
			return fmt.Sprintf("treble=g=%.1f", s.Params["gain"]), nil
		}},
		"pitch": {KindAudio, func(s recipe.EffectStep) (string, error) {
			// asetrate trick: multiply sample rate then resample back.
			f := def(s.Params["factor"], 1)
			return fmt.Sprintf("asetrate=44100*%.4f,aresample=44100,atempo=%.4f", f, 1.0/f), nil
		}},
		"echo": {KindAudio, func(s recipe.EffectStep) (string, error) {
			return "aecho=0.8:0.9:1000:0.3", nil
		}},
	}
}

// def returns v when non-zero, otherwise the fallback.
func def(v, fallback float64) float64 {
	if v == 0 {
		return fallback
	}
	return v
}

func clamp01default(v, def float64) float64 {
	if v <= 0 || v > 1 {
		return def
	}
	return v
}

// ChainCompiler converts ordered effect steps into separate video/audio filter
// chains, and appends a subtitle drawtext filter to the video chain.
type ChainCompiler struct {
	registry map[string]EffectDef
}

func NewChainCompiler(registry map[string]EffectDef) *ChainCompiler {
	return &ChainCompiler{registry: registry}
}

func (c *ChainCompiler) Compile(t Timeline, steps []recipe.EffectStep) (FilterGraph, error) {
	ordered := append([]recipe.EffectStep(nil), steps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })

	var video, audio []string
	for _, s := range ordered {
		d, ok := c.registry[s.EffectID]
		if !ok {
			return FilterGraph{}, fmt.Errorf("render: unknown effect id %q", s.EffectID)
		}
		frag, err := d.Compile(s)
		if err != nil {
			return FilterGraph{}, fmt.Errorf("render: compile effect %q: %w", s.EffectID, err)
		}
		if frag == "" {
			continue
		}
		if d.Kind == KindAudio {
			audio = append(audio, frag)
		} else {
			video = append(video, frag)
		}
	}

	// Subtitle burn-in is appended to the end of the video chain.
	if sub := subtitleFilter(t.Recipe.Subtitle); sub != "" {
		video = append(video, sub)
	}

	return FilterGraph{
		Video: strings.Join(video, ","),
		Audio: strings.Join(audio, ","),
	}, nil
}

// subtitleFilter builds a drawtext filter for a burned-in caption, or "".
func subtitleFilter(s recipe.Subtitle) string {
	if strings.TrimSpace(s.Text) == "" {
		return ""
	}
	size := s.FontSize
	if size <= 0 {
		size = 24
	}
	color := s.Color
	if color == "" {
		color = "white"
	}
	// Vertical placement.
	y := "h-(text_h*2)" // bottom (default)
	switch s.Position {
	case "top":
		y = "text_h"
	case "center":
		y = "(h-text_h)/2"
	}
	txt := escapeDrawtext(s.Text)

	// A fontfile is required on systems without fontconfig (e.g. Windows).
	fontArg := ""
	if f := systemFontFile(); f != "" {
		fontArg = "fontfile='" + escapeFontPath(f) + "':"
	}

	return fmt.Sprintf(
		"drawtext=%stext='%s':fontcolor=%s:fontsize=%d:x=(w-text_w)/2:y=%s:box=1:boxcolor=black@0.5:boxborderw=8",
		fontArg, txt, color, size, y,
	)
}

// systemFontFile returns a path to a TrueType font available on this OS, or ""
// if none is found (ffmpeg then relies on fontconfig, where available).
func systemFontFile() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		win := os.Getenv("WINDIR")
		if win == "" {
			win = `C:\Windows`
		}
		candidates = []string{
			filepath.Join(win, "Fonts", "arial.ttf"),
			filepath.Join(win, "Fonts", "segoeui.ttf"),
			filepath.Join(win, "Fonts", "tahoma.ttf"),
		}
	case "darwin":
		candidates = []string{
			"/System/Library/Fonts/Supplemental/Arial.ttf",
			"/Library/Fonts/Arial.ttf",
			"/System/Library/Fonts/Helvetica.ttc",
		}
	default: // linux
		candidates = []string{
			"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
			"/usr/share/fonts/TTF/DejaVuSans.ttf",
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// escapeFontPath escapes a Windows path for use inside a drawtext filter
// (backslashes and the drive colon must be escaped).
func escapeFontPath(p string) string {
	p = strings.ReplaceAll(p, `\`, `/`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	return p
}

// escapeDrawtext escapes characters that break ffmpeg's drawtext parser.
func escapeDrawtext(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`'`, "\u2019", // replace straight quote with a typographic one
		`:`, `\:`,
		`%`, `\%`,
	)
	return r.Replace(s)
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

	music := t.Recipe.Audio
	hasMusic := strings.TrimSpace(music.FilePath) != ""

	args := []string{"-y"}

	// Trim window applies to the source input (before -i for fast seek).
	var trimArgs []string
	if len(t.Segments) > 0 {
		seg := t.Segments[0]
		trimArgs = append(trimArgs, "-ss", strconv.FormatFloat(seg.Start.Seconds(), 'f', 3, 64))
		if seg.End > seg.Start {
			trimArgs = append(trimArgs, "-t", strconv.FormatFloat((seg.End-seg.Start).Seconds(), 'f', 3, 64))
		}
	}

	// Source input (input 0) with trim.
	args = append(args, trimArgs...)
	args = append(args, "-i", t.Source.FilePath)

	// Optional music input (input 1).
	if hasMusic {
		if music.Loop {
			args = append(args, "-stream_loop", "-1")
		}
		args = append(args, "-i", music.FilePath)
	}

	if hasMusic {
		// Use filter_complex to apply video filters + mix two audio streams.
		fc := buildFilterComplex(graph, music)
		args = append(args, "-filter_complex", fc,
			"-map", "[vout]", "-map", "[aout]",
			// End when the (trimmed) source ends, not when looped music ends.
			"-shortest")
	} else {
		// Simple path: independent -vf / -af.
		if graph.Video != "" {
			args = append(args, "-vf", graph.Video)
		}
		if graph.Audio != "" {
			args = append(args, "-af", graph.Audio)
		}
	}

	args = append(args, "-c:v", path.Codec, "-preset", path.Preset, "-c:a", "aac", outputPath)

	if out, err := e.run(e.binary, args...); err != nil {
		return "", fmt.Errorf("ffmpeg exec: %w (output: %s)", err, tail(string(out), 400))
	}
	return outputPath, nil
}

// buildFilterComplex assembles a -filter_complex graph that applies the video
// chain to the source video and mixes source audio with background music.
func buildFilterComplex(graph FilterGraph, music recipe.AudioTrack) string {
	var parts []string

	// Video: [0:v] -> filters -> [vout]
	if graph.Video != "" {
		parts = append(parts, fmt.Sprintf("[0:v]%s[vout]", graph.Video))
	} else {
		parts = append(parts, "[0:v]copy[vout]")
	}

	// Source audio chain -> [a0]
	srcVol := music.SourceVolume
	if srcVol == 0 {
		srcVol = 1
	}
	a0 := fmt.Sprintf("[0:a]volume=%.3f", srcVol)
	if graph.Audio != "" {
		a0 += "," + graph.Audio
	}
	a0 += "[a0]"
	parts = append(parts, a0)

	// Music chain -> [a1]
	mvol := music.Volume
	if mvol == 0 {
		mvol = 0.3
	}
	parts = append(parts, fmt.Sprintf("[1:a]volume=%.3f[a1]", mvol))

	// Mix the two audio streams -> [aout]
	parts = append(parts, "[a0][a1]amix=inputs=2:duration=first:dropout_transition=0[aout]")

	return strings.Join(parts, ";")
}

// tail returns the last n chars of s (for error snippets).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
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
