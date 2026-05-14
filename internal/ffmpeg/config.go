// Package ffmpeg builds and supervises FFmpeg invocations for
// easycampus-go. It's the only package in the tree that owns
// exec.Command("ffmpeg", ...) calls — everything else routes through
// Config + Supervisor.
//
// Design notes:
//   - Live encode (capture device -> HLS) is built by BuildLiveArgs.
//     Stall watchdog + restart-with-backoff is handled by Supervisor.
//   - Upload conversion (input file -> HLS) is built by BuildUploadArgs.
//     Runs once to completion; no restart loop.
//   - External / network source (RTSP/SRT/HLS pull -> HLS) is built by
//     BuildExternalArgs. Long-running with reconnect flags but no stall
//     watchdog (upstream pacing isn't ours to control).
//
// The QualityPreset interface (see below) is the seam left open for the
// broadcast engineer who is populating the consumer/prosumer/broadcast/
// cinema quality matrix in parallel. Don't add tier-specific knowledge
// to this file — add it to a QualityPreset implementation and pass it in.
package ffmpeg

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// QualityPreset — broadcast-engineer integration surface.
// -----------------------------------------------------------------------------
//
// The parallel "broadcast engineer" agent owns the tier table (consumer,
// prosumer, broadcast, cinema). They populate concrete QualityPreset
// implementations that the live-encode and upload-conversion code paths
// pick up via dependency injection — Config.Preset is a QualityPreset.
//
// Contract:
//
//   - All methods must be safe to call from any goroutine. Implementations
//     should be value types or guarded by their own locks.
//   - Bitrate strings use FFmpeg's k/M suffix convention ("6M", "8000k").
//   - Resolution() returns "WxH" (e.g. "1920x1080") as accepted by `-s`
//     and the `scale=` filter.
//   - FPSExpr() returns "30", "60", "24000/1001" — passed verbatim to
//     `-r`. Cinema presets MUST return the rational form so 23.976 does
//     not drift to 24.000 over service-length streams.
//   - GOP() returns the frame-count interval between keyframes. For
//     standard live HLS this is FPS() * 2 (two-second segments).
//   - PixelFormat() returns "yuv420p" for SDR HLS (the safe choice that
//     plays everywhere); broadcast / cinema tiers may return "yuv422p10le"
//     or similar for archival uses.
//   - Profile() / Level() are the H.264 profile and level strings
//     ("high" / "4.1"). Hardware encoders translate these per backend.
//   - VideoEncoderPreset() returns the speed preset name. For libx264 this
//     is "ultrafast".."veryslow". For NVENC it's "p1".."p7". For Apple
//     VideoToolbox it's ignored (VT has no preset). The empty string
//     means "use the encoder's default".
//   - Tune() returns "zerolatency", "film", "animation", "", etc. Empty
//     means "do not pass -tune". The live encode path force-overrides
//     this to "zerolatency" when WantsZeroLatency() reports true to
//     guarantee low-latency live behavior regardless of tier choice.
//   - WantsBFrames() lets cinema/archive tiers opt INTO B-frames where
//     consumer/broadcast live tiers turn them off. The encoder-args
//     builder reads this to decide -bf 0 vs leaving the encoder default.
//
// QualityPreset is intentionally narrow. If something doesn't fit
// (HDR mastering display metadata, AV1 toggles, etc.) the parallel
// engineer can extend with a typed extension interface — do NOT widen
// this interface without an audit of every consumer.
type QualityPreset interface {
	ID() string             // stable identifier ("consumer-1080p30", "broadcast-2160p60", ...)
	Label() string          // human-friendly name shown in UI
	Resolution() string     // "1920x1080"
	Width() int             // numeric pieces of Resolution
	Height() int            //
	FPS() int               // integer FPS rounded down (30, 60). Use FPSExpr for the wire value.
	FPSExpr() string        // "30" | "60" | "24000/1001"
	GOP() int               // keyframe interval in frames
	VideoBitrate() string   // "6M" / "8000k"
	AudioBitrate() string   // "128k" / "192k"
	PixelFormat() string    // "yuv420p"
	Profile() string        // "high"
	Level() string          // "4.1"
	VideoEncoderPreset() string
	Tune() string           // "" or "zerolatency" etc.
	WantsZeroLatency() bool // forces -tune zerolatency on the live path
	WantsBFrames() bool     // false => emit -bf 0
}

// DefaultPreset is a minimal, conservative QualityPreset used when the
// broadcast engineer's tier matrix is absent (early commits, tests,
// degraded operation). It targets 1080p30 at 6 Mbps — the existing
// easycampus-go "high" quality setting.
//
// The broadcast engineer's table will replace this with proper
// consumer/prosumer/broadcast/cinema tiers, but having a non-nil
// preset here means the rest of the codebase doesn't need nil checks
// and uploads keep working through the transition.
type DefaultPreset struct{}

func (DefaultPreset) ID() string                { return "default-1080p30" }
func (DefaultPreset) Label() string             { return "1080p30 @ 6 Mbps" }
func (DefaultPreset) Resolution() string        { return "1920x1080" }
func (DefaultPreset) Width() int                { return 1920 }
func (DefaultPreset) Height() int               { return 1080 }
func (DefaultPreset) FPS() int                  { return 30 }
func (DefaultPreset) FPSExpr() string           { return "30" }
func (DefaultPreset) GOP() int                  { return 60 }
func (DefaultPreset) VideoBitrate() string      { return "6000k" }
func (DefaultPreset) AudioBitrate() string      { return "128k" }
func (DefaultPreset) PixelFormat() string       { return "yuv420p" }
func (DefaultPreset) Profile() string           { return "high" }
func (DefaultPreset) Level() string             { return "4.1" }
func (DefaultPreset) VideoEncoderPreset() string { return "veryfast" }
func (DefaultPreset) Tune() string              { return "" }
func (DefaultPreset) WantsZeroLatency() bool    { return true }
func (DefaultPreset) WantsBFrames() bool        { return false }

// SimplePreset is a flexible QualityPreset implementation for the existing
// "low" / "standard" / "high" / "ultra" buckets used by upload conversion.
// Lets handlers_upload migrate before the broadcast engineer's tier table
// lands without diverging the upload code path.
type SimplePreset struct {
	IDValue            string
	LabelValue         string
	W, H               int
	FPSInt             int
	FPSString          string
	GOPFrames          int
	VBitrate, ABitrate string
	PixFmt             string
	ProfileValue       string
	LevelValue         string
	EncoderPreset      string
	TuneValue          string
	ZeroLatency        bool
	BFrames            bool
}

func (p SimplePreset) ID() string                { return p.IDValue }
func (p SimplePreset) Label() string             { return p.LabelValue }
func (p SimplePreset) Resolution() string        { return fmt.Sprintf("%dx%d", p.W, p.H) }
func (p SimplePreset) Width() int                { return p.W }
func (p SimplePreset) Height() int               { return p.H }
func (p SimplePreset) FPS() int                  { return p.FPSInt }
func (p SimplePreset) FPSExpr() string {
	if p.FPSString != "" {
		return p.FPSString
	}
	return fmt.Sprintf("%d", p.FPSInt)
}
func (p SimplePreset) GOP() int                  { return p.GOPFrames }
func (p SimplePreset) VideoBitrate() string      { return p.VBitrate }
func (p SimplePreset) AudioBitrate() string      { return p.ABitrate }
func (p SimplePreset) PixelFormat() string {
	if p.PixFmt == "" {
		return "yuv420p"
	}
	return p.PixFmt
}
func (p SimplePreset) Profile() string           { return p.ProfileValue }
func (p SimplePreset) Level() string             { return p.LevelValue }
func (p SimplePreset) VideoEncoderPreset() string { return p.EncoderPreset }
func (p SimplePreset) Tune() string              { return p.TuneValue }
func (p SimplePreset) WantsZeroLatency() bool    { return p.ZeroLatency }
func (p SimplePreset) WantsBFrames() bool        { return p.BFrames }

// -----------------------------------------------------------------------------
// Encoder + hwaccel detection
// -----------------------------------------------------------------------------

// Encoder selects which H.264 video encoder FFmpeg uses.
type Encoder string

const (
	EncoderAuto         Encoder = ""                  // pick the best available
	EncoderX264         Encoder = "libx264"           // Software (CPU)
	EncoderVideoToolbox Encoder = "h264_videotoolbox" // macOS hardware
	EncoderNVENC        Encoder = "h264_nvenc"        // NVIDIA GPU
	EncoderVAAPI        Encoder = "h264_vaapi"        // Linux Intel/AMD GPU
	EncoderQSV          Encoder = "h264_qsv"          // Intel QuickSync
)

// HWAccel is the hardware-accelerated decode path FFmpeg uses on inputs.
// Distinct from the encoder choice — a v4l2 input can decode via vaapi
// while encoding with libx264, and an HLS pull can decode via
// videotoolbox while encoding with anything.
type HWAccel string

const (
	HWAccelNone         HWAccel = ""
	HWAccelAuto         HWAccel = "auto"
	HWAccelVideoToolbox HWAccel = "videotoolbox"
	HWAccelCUDA         HWAccel = "cuda"
	HWAccelVAAPI        HWAccel = "vaapi"
	HWAccelQSV          HWAccel = "qsv"
	HWAccelD3D11        HWAccel = "d3d11va"
	HWAccelDXVA2        HWAccel = "dxva2"
)

// EncoderInfo describes a detected encoder for UI consumption.
type EncoderInfo struct {
	ID          Encoder `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Available   bool    `json:"available"`
}

// knownEncoders lists all encoders we know how to configure, in display order.
var knownEncoders = []EncoderInfo{
	{EncoderX264, "Software (x264)", "CPU-based, widest compatibility. Always available.", true},
	{EncoderVideoToolbox, "Apple VideoToolbox", "macOS hardware encoder.", false},
	{EncoderNVENC, "NVIDIA NVENC", "NVIDIA GPU hardware encoder.", false},
	{EncoderVAAPI, "VA-API", "Linux Intel/AMD GPU hardware encoder.", false},
	{EncoderQSV, "Intel QuickSync", "Intel GPU hardware encoder.", false},
}

// DetectEncoders probes ffmpeg for available H.264 encoders.
func DetectEncoders(binary string) []EncoderInfo {
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		result := make([]EncoderInfo, len(knownEncoders))
		copy(result, knownEncoders)
		return result
	}
	output := string(out)
	result := make([]EncoderInfo, len(knownEncoders))
	copy(result, knownEncoders)
	for i := range result {
		if result[i].ID == EncoderX264 {
			result[i].Available = true
			continue
		}
		if strings.Contains(output, string(result[i].ID)) {
			result[i].Available = true
		}
	}
	return result
}

// DetectHWAccels probes `ffmpeg -hwaccels` for usable input acceleration.
func DetectHWAccels(binary string) []HWAccel {
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "-hide_banner", "-hwaccels").CombinedOutput()
	if err != nil {
		return nil
	}
	var avail []HWAccel
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "videotoolbox":
			avail = append(avail, HWAccelVideoToolbox)
		case "cuda":
			avail = append(avail, HWAccelCUDA)
		case "vaapi":
			avail = append(avail, HWAccelVAAPI)
		case "qsv":
			avail = append(avail, HWAccelQSV)
		case "d3d11va":
			avail = append(avail, HWAccelD3D11)
		case "dxva2":
			avail = append(avail, HWAccelDXVA2)
		}
	}
	return avail
}

// PickEncoder returns the best available encoder for the current platform.
// Order of preference:
//
//	macOS: VideoToolbox -> libx264
//	Linux: NVENC -> QSV -> VAAPI -> libx264
//	Windows: NVENC -> QSV -> libx264
//
// We never return Auto — callers get a concrete encoder they can pass to
// `-c:v` without further branching.
func PickEncoder(available []EncoderInfo) Encoder {
	has := map[Encoder]bool{}
	for _, e := range available {
		if e.Available {
			has[e.ID] = true
		}
	}
	switch runtime.GOOS {
	case "darwin":
		if has[EncoderVideoToolbox] {
			return EncoderVideoToolbox
		}
	case "linux":
		if has[EncoderNVENC] {
			return EncoderNVENC
		}
		if has[EncoderQSV] {
			return EncoderQSV
		}
		if has[EncoderVAAPI] {
			return EncoderVAAPI
		}
	case "windows":
		if has[EncoderNVENC] {
			return EncoderNVENC
		}
		if has[EncoderQSV] {
			return EncoderQSV
		}
	}
	return EncoderX264
}

// PickHWAccel chooses the best input-decode hardware accelerator that
// matches the chosen encoder (using the same HW pipeline for decode +
// encode is materially faster than crossing hwaccels). Returns
// HWAccelNone when nothing is available; callers omit -hwaccel entirely
// in that case.
func PickHWAccel(available []HWAccel, encoder Encoder) HWAccel {
	has := map[HWAccel]bool{}
	for _, a := range available {
		has[a] = true
	}
	switch encoder {
	case EncoderVideoToolbox:
		if has[HWAccelVideoToolbox] {
			return HWAccelVideoToolbox
		}
	case EncoderNVENC:
		if has[HWAccelCUDA] {
			return HWAccelCUDA
		}
	case EncoderVAAPI:
		if has[HWAccelVAAPI] {
			return HWAccelVAAPI
		}
	case EncoderQSV:
		if has[HWAccelQSV] {
			return HWAccelQSV
		}
	}
	// Mismatched encoder/hwaccel: fall back to auto if anything is
	// available so the input decode still gets accelerated.
	if len(available) > 0 {
		return HWAccelAuto
	}
	return HWAccelNone
}

// -----------------------------------------------------------------------------
// Inputs
// -----------------------------------------------------------------------------

// InputKind names the source type for a live encode.
type InputKind string

const (
	InputAVFoundation InputKind = "avfoundation"
	InputV4L2         InputKind = "v4l2"
	InputDShow        InputKind = "dshow"
	InputDeckLink     InputKind = "decklink"
	InputNDI          InputKind = "libndi_newtek"
	InputFile         InputKind = "file"    // upload conversion
	InputNetwork      InputKind = "network" // external RTSP/SRT/HLS pull
	InputTest         InputKind = "test"    // lavfi testsrc
)

// Input describes the source side of an FFmpeg invocation.
type Input struct {
	Kind        InputKind
	VideoDevice string // device path / index / URL / file path
	AudioDevice string // optional secondary audio source (avfoundation only)
	URL         string // when Kind == InputNetwork
	Framerate   int    // device framerate hint for avfoundation/v4l2
	VideoSize   string // device resolution hint ("1280x720"); empty = device default
}

// -----------------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------------

// Mode picks which output mode the args builder emits.
type Mode string

const (
	ModeLiveHLS Mode = "live-hls" // capture device -> HLS playlist (encoder)
	ModeUpload  Mode = "upload"   // file or URL -> HLS playlist (one-shot)
)

// Config carries every parameter the args builder needs.
//
// Live encode: Mode = ModeLiveHLS, Input.Kind in {avfoundation/v4l2/dshow/
// decklink/ndi}. Output is an HLS playlist; the args builder enables
// `delete_segments` if HLSDeleteOld is set.
//
// Upload conversion: Mode = ModeUpload, Input.Kind = InputFile or
// InputNetwork. Output is an HLS playlist with `append_list` so resuming
// is safe; `delete_segments` is explicitly NOT used (archive).
type Config struct {
	Binary string

	Mode    Mode
	Input   Input
	Preset  QualityPreset
	Encoder Encoder // EncoderAuto = pick the best available
	HWAccel HWAccel // HWAccelNone = let auto-pick decide; HWAccelAuto = use "-hwaccel auto"

	// HLS output settings.
	OutputDir          string // directory to write playlist + segments
	HLSSegmentSeconds  int    // -hls_time; default 2
	HLSListSize        int    // -hls_list_size; 0 = unlimited (archive default)
	HLSDeleteOld       bool   // live: drop old segments to bound disk usage
	HLSPlaylistType    string // "event" / "vod" / ""
	HLSSegmentType     string // "fmp4" / "mpegts"; default "mpegts" for upload, "fmp4" for live
	HLSSegmentPattern  string // overrides the default segment_%06d pattern when set
	HLSInitFile        string // fmp4 init filename ("init.mp4")
	PlaylistFilename   string // defaults to "master.m3u8"

	// LogLevel passed to `-loglevel`. We emit `-nostats` always so the
	// stderr stream is parse-friendly; per-line audio meter / progress
	// goes through `-progress pipe:1`. Default: "error".
	LogLevel string

	// DisableAudio drops the audio mapping/encoder entirely.
	DisableAudio bool

	// StreamKey is logged-but-redacted-if-included. Currently unused by
	// args (we don't push to RTMP), but kept here so the redaction path
	// is exercised consistently with easystream's pattern.
	StreamKey string

	// SourceHasMissingPTS hints that the input lacks valid PTS so the
	// builder adds `-fflags +genpts`. Don't blanket-apply this — it's
	// only needed for damaged or container-quirk inputs.
	SourceHasMissingPTS bool

	// Realtime: when true, use `-re` on file/network sources so the
	// pipeline paces at wallclock (live-emulation). Off for upload
	// conversions where we want to transcode as fast as possible.
	Realtime bool
}

// Validate returns nil iff the config can produce args.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Binary) == "" {
		return fmt.Errorf("ffmpeg binary path is required")
	}
	if c.Preset == nil {
		return fmt.Errorf("preset is required")
	}
	if c.Preset.ID() == "" {
		return fmt.Errorf("preset.ID() must be non-empty")
	}
	switch c.Mode {
	case ModeLiveHLS:
		switch c.Input.Kind {
		case InputAVFoundation, InputV4L2, InputDShow, InputDeckLink, InputNDI, InputTest:
			// ok
		case InputFile, InputNetwork:
			// also allowed (mirror-stream from a file / URL)
		default:
			return fmt.Errorf("unsupported live input kind: %q", c.Input.Kind)
		}
		if strings.TrimSpace(c.OutputDir) == "" {
			return fmt.Errorf("OutputDir is required for live HLS")
		}
	case ModeUpload:
		if c.Input.Kind != InputFile && c.Input.Kind != InputNetwork {
			return fmt.Errorf("upload mode requires InputFile or InputNetwork, got %q", c.Input.Kind)
		}
		if c.Input.Kind == InputFile && strings.TrimSpace(c.Input.VideoDevice) == "" {
			return fmt.Errorf("upload InputFile requires Input.VideoDevice = file path")
		}
		if c.Input.Kind == InputNetwork && strings.TrimSpace(c.Input.URL) == "" {
			return fmt.Errorf("upload InputNetwork requires Input.URL")
		}
		if strings.TrimSpace(c.OutputDir) == "" {
			return fmt.Errorf("OutputDir is required for upload conversion")
		}
	default:
		return fmt.Errorf("unsupported mode: %q", c.Mode)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Args builder
// -----------------------------------------------------------------------------

// Args builds the argv FFmpeg will see. Returns an error if the config is
// invalid; never panics. Safe to call from any goroutine.
func (c Config) Args() ([]string, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-nostats",
		"-loglevel", defaultString(c.LogLevel, "error"),
		"-progress", "pipe:1",
	}
	// `-y` overwrites output without prompting. The live + upload paths
	// always overwrite (each event has its own directory), so include it
	// unconditionally. RedactArgsForLog scrubs it from log output to
	// avoid suggesting "ffmpeg silently overwrote your file."
	args = append(args, "-y")

	encoder := c.EffectiveEncoder()
	hwaccel := c.EffectiveHWAccel(encoder)

	// Input-side hwaccel. Must precede `-i`. Mismatched encoder/hwaccel
	// falls back to `-hwaccel auto` in EffectiveHWAccel which lets
	// FFmpeg pick a transfer path that still avoids CPU decode where
	// possible.
	if hwaccel != HWAccelNone {
		args = append(args, "-hwaccel", string(hwaccel))
	}

	// SourceHasMissingPTS: opt-in genpts only. Blanket genpts on
	// well-formed inputs can shift A/V sync.
	if c.SourceHasMissingPTS {
		args = append(args, "-fflags", "+genpts")
	}

	// Live capture devices benefit from -thread_queue_size to avoid
	// "real-time buffer too full, frame dropped" on slow filesystems
	// when the disk write side stalls briefly.
	switch c.Input.Kind {
	case InputAVFoundation, InputV4L2, InputDShow, InputDeckLink, InputNDI:
		args = append(args, "-thread_queue_size", "1024")
	}

	inputArgs, err := c.inputArgs()
	if err != nil {
		return nil, err
	}
	args = append(args, inputArgs...)

	// Video mapping + encoder
	args = append(args, "-map", "0:v:0")
	// Broadcast-safe limiter — broadcast-tier presets clamp luma to
	// the legal 16-235 / chroma 16-240 range so out-of-range whites
	// from a confidence monitor or PC-range graphics don't trip a
	// downstream QC system. Opt-in via BroadcastSafeAware.
	if bs, ok := c.Preset.(BroadcastSafeAware); ok && bs.BroadcastSafe() {
		args = append(args, "-vf", "limiter=min=16:max=235")
	}
	args = append(args, c.encoderArgs(encoder)...)
	args = append(args, "-pix_fmt", c.Preset.PixelFormat())

	// Output framerate. We pin CFR explicitly: -fps_mode cfr replaces the
	// deprecated -vsync; this prevents duplicate-PTS frames slipping into
	// HLS segments on streamcopy-adjacent code paths.
	args = append(args, "-fps_mode", "cfr", "-r", c.Preset.FPSExpr())

	// Force keyframes on a strict 2-second boundary so HLS segment cuts
	// land on IDR frames. Mirrors easystream's expression; the boundary
	// value is the same as the GOP-in-seconds derived from FPS().
	segSec := c.hlsSegmentSeconds()
	args = append(args,
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segSec),
		"-g", fmt.Sprintf("%d", c.Preset.GOP()),
		"-keyint_min", fmt.Sprintf("%d", c.Preset.GOP()),
		"-sc_threshold", "0",
	)

	// Audio mapping + encoder
	if !c.DisableAudio {
		args = append(args, "-map", "0:a:0?")
		args = append(args, c.audioEncoderArgs()...)
	} else {
		args = append(args, "-an")
	}

	// HLS output
	args = append(args, c.hlsOutputArgs()...)
	return args, nil
}

// EffectiveEncoder returns the encoder to use, resolving EncoderAuto by
// querying the binary. The result is cached on the FFmpeg-binary level
// inside this function only when Config.Encoder == EncoderAuto.
func (c Config) EffectiveEncoder() Encoder {
	if c.Encoder != EncoderAuto {
		return c.Encoder
	}
	return PickEncoder(DetectEncoders(c.Binary))
}

// EffectiveHWAccel returns the input hwaccel decision. Pass the encoder
// you'll use so we can match them (videotoolbox decode + videotoolbox
// encode skips a CPU roundtrip).
func (c Config) EffectiveHWAccel(encoder Encoder) HWAccel {
	if c.HWAccel == HWAccelNone {
		return HWAccelNone
	}
	if c.HWAccel != HWAccelAuto {
		return c.HWAccel
	}
	return PickHWAccel(DetectHWAccels(c.Binary), encoder)
}

func (c Config) inputArgs() ([]string, error) {
	in := c.Input
	switch in.Kind {
	case InputAVFoundation:
		fr := in.Framerate
		if fr <= 0 {
			fr = c.Preset.FPS()
		}
		size := in.VideoSize
		if size == "" {
			size = "1280x720"
		}
		videoIdx := in.VideoDevice
		if videoIdx == "" {
			videoIdx = "0"
		}
		audioIdx := in.AudioDevice
		args := []string{
			"-f", "avfoundation",
			"-framerate", fmt.Sprintf("%d", fr),
			"-video_size", size,
		}
		if c.DisableAudio || audioIdx == "" {
			args = append(args, "-i", videoIdx+":none")
		} else {
			args = append(args, "-i", videoIdx+":"+audioIdx)
		}
		return args, nil
	case InputV4L2:
		size := in.VideoSize
		if size == "" {
			size = "1280x720"
		}
		dev := in.VideoDevice
		if dev == "" {
			dev = "/dev/video0"
		}
		return []string{"-f", "v4l2", "-video_size", size, "-i", dev}, nil
	case InputDShow:
		dev := in.VideoDevice
		if dev == "" {
			dev = "video=Integrated Camera"
		}
		return []string{"-f", "dshow", "-i", dev}, nil
	case InputDeckLink:
		return []string{"-f", "decklink", "-i", in.VideoDevice}, nil
	case InputNDI:
		return []string{"-f", "libndi_newtek", "-i", in.VideoDevice}, nil
	case InputTest:
		return []string{
			"-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=%s:rate=%d", c.Preset.Resolution(), c.Preset.FPS()),
		}, nil
	case InputFile:
		args := []string{}
		if c.Realtime {
			args = append(args, "-re")
		}
		args = append(args, "-i", in.VideoDevice)
		return args, nil
	case InputNetwork:
		args := networkInputFlags(in.URL)
		if c.Realtime {
			args = append([]string{"-re"}, args...)
		}
		args = append(args, "-i", in.URL)
		return args, nil
	}
	return nil, fmt.Errorf("unsupported input kind: %q", in.Kind)
}

// networkInputFlags returns the scheme-appropriate input flags for a
// network URL. RTSP wants tcp transport on lossy LANs, HLS wants
// reconnect on intermittent CDNs, MPEG-TS over UDP/RTP wants a big
// FIFO. Mirrors easystream's NetworkInputArgs.
func networkInputFlags(rawURL string) []string {
	scheme := ""
	if i := strings.Index(rawURL, "://"); i > 0 {
		scheme = strings.ToLower(rawURL[:i])
	}
	args := []string{
		"-fflags", "discardcorrupt+genpts",
		"-analyzeduration", "1000000",
		"-probesize", "1000000",
	}
	switch scheme {
	case "rtsp", "rtsps":
		args = append(args, "-rtsp_transport", "tcp")
	case "udp", "rtp":
		args = append(args, "-fifo_size", "1000000", "-overrun_nonfatal", "1")
	case "http", "https":
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
			"-multiple_requests", "1",
		)
	}
	return args
}

func (c Config) hlsSegmentSeconds() int {
	if c.HLSSegmentSeconds > 0 {
		return c.HLSSegmentSeconds
	}
	return 2
}

func (c Config) hlsOutputArgs() []string {
	segSec := c.hlsSegmentSeconds()
	playlist := c.PlaylistFilename
	if playlist == "" {
		playlist = "master.m3u8"
	}
	segType := c.HLSSegmentType
	if segType == "" {
		if c.Mode == ModeLiveHLS {
			segType = "fmp4"
		} else {
			segType = "mpegts"
		}
	}
	segPattern := c.HLSSegmentPattern
	if segPattern == "" {
		if segType == "fmp4" {
			segPattern = "chunk-%06d.m4s"
		} else {
			segPattern = "segment_%06d.ts"
		}
	}

	flags := "independent_segments+temp_file"
	switch c.Mode {
	case ModeLiveHLS:
		if c.HLSDeleteOld {
			flags += "+delete_segments"
		}
	case ModeUpload:
		// Resumable archive: never delete, append on restart.
		flags += "+append_list"
	}

	listSize := c.HLSListSize
	if c.Mode == ModeUpload {
		listSize = 0 // unbounded archive
	}

	playlistType := c.HLSPlaylistType
	if playlistType == "" && c.Mode == ModeLiveHLS && !c.HLSDeleteOld {
		playlistType = "event"
	}

	args := []string{
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", segSec),
		"-hls_list_size", fmt.Sprintf("%d", listSize),
		"-hls_segment_type", segType,
		"-hls_flags", flags,
		"-hls_segment_filename", joinPath(c.OutputDir, segPattern),
	}
	if segType == "fmp4" {
		initName := c.HLSInitFile
		if initName == "" {
			initName = "init.mp4"
		}
		args = append(args, "-hls_fmp4_init_filename", initName)
	}
	if playlistType != "" {
		args = append(args, "-hls_playlist_type", playlistType)
	}
	args = append(args, joinPath(c.OutputDir, playlist))
	return args
}

// ColorPresetAware is an optional capability interface that
// QualityPreset implementations can satisfy to surface broadcast-grade
// VUI metadata. When present, encoderArgs emits -color_primaries /
// -color_trc / -colorspace / -color_range BEFORE -c:v so hardware
// encoders bake the hints into the SPS at init.
//
// Implemented by internal/quality.FFmpegPreset; not implemented by
// the local DefaultPreset / SimplePreset fallbacks (those stay
// implicit / unspecified — historic behaviour).
type ColorPresetAware interface {
	ColorPrimaries() string
	ColorTransfer() string
	ColorSpace() string
	ColorRange() string
}

// RateControlAware lets a preset declare its rate-control envelope
// (CBR vs VBR, plus the VBV ceiling and buffer size). encoderArgs
// honours this on the libx264 path with `nal-hrd=cbr filler=1` plus
// 1-second VBV when RateControlMode() == "cbr".
type RateControlAware interface {
	RateControlMode() string
	VideoMaxBitrate() string
	VideoBufSize() string
}

// BroadcastSafeAware lets a preset opt INTO a 16-235 luma limiter
// applied as a -vf filter. Set on broadcast-tier presets so out-of-
// range whites don't trip downstream QC systems.
type BroadcastSafeAware interface {
	BroadcastSafe() bool
}

// encoderArgs returns the encoder-specific flags. Each hardware encoder
// has its own knobs because they don't share x264's CLI surface.
//
// When the bound preset satisfies the optional ColorPresetAware /
// RateControlAware interfaces, the relevant broadcast-grade flags
// are emitted automatically. Plain QualityPreset implementations
// (DefaultPreset / SimplePreset) keep working with the legacy
// behaviour — bitrate / profile / level only.
func (c Config) encoderArgs(encoder Encoder) []string {
	preset := c.Preset
	level := preset.Level()
	levelNoDot := strings.ReplaceAll(level, ".", "")

	wantsTune := preset.Tune()
	if c.Mode == ModeLiveHLS && preset.WantsZeroLatency() {
		wantsTune = "zerolatency"
	}
	// Upload conversions explicitly do NOT use zerolatency — it disables
	// B-frames and hurts quality at the same bitrate.
	if c.Mode == ModeUpload {
		wantsTune = preset.Tune()
		if wantsTune == "zerolatency" {
			wantsTune = ""
		}
	}

	bfArgs := []string{}
	if !preset.WantsBFrames() {
		bfArgs = []string{"-bf", "0"}
	}

	// Color VUI signalling lives ahead of -c:v so the encoder bakes
	// the values into the SPS at init. The narrow QualityPreset
	// interface doesn't carry color metadata — opt-in via the
	// ColorPresetAware extension. Presets without it keep the
	// historic "unspecified" behaviour.
	var colorArgs []string
	if cp, ok := preset.(ColorPresetAware); ok {
		colorArgs = []string{
			"-color_primaries", cp.ColorPrimaries(),
			"-color_trc", cp.ColorTransfer(),
			"-colorspace", cp.ColorSpace(),
			"-color_range", cp.ColorRange(),
		}
	}

	// Rate control — broadcast tier presets ship CBR with 1-second
	// VBV. Consumer presets ship VBR with 2x headroom. Implementations
	// that don't satisfy RateControlAware get the legacy "soft CBR"
	// path (maxrate == bufsize == target on x264, -rc cbr on NVENC).
	//
	// rateControlAware tracks whether we should emit RateControlAware-
	// specific extras (nal-hrd=cbr on x264). The legacy SimplePreset
	// path keeps the historic "no x264-params" behavior to avoid
	// churning every caller that relied on it.
	rcMode := "cbr"
	maxrate := preset.VideoBitrate()
	bufsize := preset.VideoBitrate()
	rc, rateControlAware := preset.(RateControlAware)
	if rateControlAware {
		if m := rc.RateControlMode(); m != "" {
			rcMode = strings.ToLower(m)
		}
		if v := rc.VideoMaxBitrate(); v != "" {
			maxrate = v
		}
		if v := rc.VideoBufSize(); v != "" {
			bufsize = v
		}
	}

	switch encoder {
	case EncoderVideoToolbox:
		args := append([]string{}, colorArgs...)
		args = append(args,
			"-c:v", "h264_videotoolbox",
			"-profile:v", preset.Profile(),
			"-level:v", level,
			"-allow_sw", "1",
			"-b:v", preset.VideoBitrate(),
		)
		if c.Mode == ModeLiveHLS {
			args = append(args, "-realtime", "1")
			if rcMode == "cbr" {
				args = append(args, "-constant_bit_rate", "1")
			}
		}
		args = append(args, bfArgs...)
		return args
	case EncoderNVENC:
		args := append([]string{}, colorArgs...)
		nvencRC := "cbr"
		if rcMode == "vbr" || rcMode == "crf" || rcMode == "cqp" {
			nvencRC = "vbr"
		}
		args = append(args,
			"-c:v", "h264_nvenc",
			"-preset", defaultString(preset.VideoEncoderPreset(), "p4"),
			"-profile:v", preset.Profile(),
			"-level:v", level,
			"-rc", nvencRC,
			"-no-scenecut", "1",
			"-forced-idr", "1",
			"-strict_gop", "1",
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
		args = append(args, bfArgs...)
		return args
	case EncoderVAAPI:
		args := append([]string{}, colorArgs...)
		args = append(args,
			"-vaapi_device", "/dev/dri/renderD128",
			"-c:v", "h264_vaapi",
			"-profile:v", preset.Profile(),
			"-level", levelNoDot,
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
		args = append(args, bfArgs...)
		return args
	case EncoderQSV:
		args := append([]string{}, colorArgs...)
		args = append(args,
			"-c:v", "h264_qsv",
			"-preset", defaultString(preset.VideoEncoderPreset(), "veryfast"),
			"-profile:v", preset.Profile(),
			"-level", levelNoDot,
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
		args = append(args, bfArgs...)
		return args
	default: // libx264
		args := append([]string{}, colorArgs...)
		args = append(args,
			"-c:v", "libx264",
			"-preset", defaultString(preset.VideoEncoderPreset(), "veryfast"),
			"-profile:v", preset.Profile(),
			"-level:v", level,
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
		// Broadcast CBR: declare HRD parameters in the SPS VUI and
		// emit filler NALs so strict downstream re-muxers see CBR,
		// not VBR-with-filler. Gated on RateControlAware so the
		// legacy SimplePreset path keeps its historic libx264
		// argv (no -x264-params).
		if rateControlAware && rcMode == "cbr" {
			gopFrames := preset.GOP()
			x264Params := fmt.Sprintf(
				"nal-hrd=cbr:filler=1:keyint=%d:min-keyint=%d:scenecut=0:open-gop=0",
				gopFrames, gopFrames,
			)
			args = append(args, "-x264-params", x264Params)
		}
		if wantsTune != "" {
			args = append(args, "-tune", wantsTune)
		}
		args = append(args, bfArgs...)
		return args
	}
}

func (c Config) audioEncoderArgs() []string {
	// AAC-LC is the universal HLS audio codec. macOS prefers aac_at for
	// quality; everywhere else uses the native encoder with the twoloop
	// coder for better rate-distortion than the default fast coder.
	if runtime.GOOS == "darwin" {
		return []string{
			"-c:a", "aac_at",
			"-b:a", c.Preset.AudioBitrate(),
			"-ar", "48000",
			"-ac", "2",
		}
	}
	return []string{
		"-c:a", "aac",
		"-aac_coder", "twoloop",
		"-b:a", c.Preset.AudioBitrate(),
		"-ar", "48000",
		"-ac", "2",
	}
}

// -----------------------------------------------------------------------------
// Redaction
// -----------------------------------------------------------------------------

// RedactedCredentialSentinel replaces credentials in URLs that get logged.
const RedactedCredentialSentinel = "REDACTED"

// secretQueryParams names URL query parameters whose values are always
// sensitive. Their values are scrubbed alongside any userinfo (user:pass@)
// component when URLs get serialised for logs.
var secretQueryParams = map[string]bool{
	"passphrase": true,
	"password":   true,
	"token":      true,
	"key":        true,
	"secret":     true,
}

// RedactURLCredentials returns u with embedded credentials replaced by
// the redaction sentinel.
//
//   - userinfo (rtsp://user:pass@host/p -> rtsp://REDACTED:REDACTED@host/p)
//   - secret query params (srt://...?passphrase=X -> ?passphrase=REDACTED)
func RedactURLCredentials(u string) string {
	if u == "" {
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return redactRawURL(u)
	}
	if parsed.User != nil {
		parsed.User = url.UserPassword(RedactedCredentialSentinel, RedactedCredentialSentinel)
	}
	if parsed.RawQuery != "" {
		q := parsed.Query()
		changed := false
		for k := range q {
			if secretQueryParams[strings.ToLower(k)] {
				q.Set(k, RedactedCredentialSentinel)
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = q.Encode()
		}
	}
	return parsed.String()
}

func redactRawURL(u string) string {
	schemeIdx := strings.Index(u, "://")
	if schemeIdx < 0 {
		return u
	}
	rest := u[schemeIdx+3:]
	atIdx := strings.Index(rest, "@")
	slashIdx := strings.Index(rest, "/")
	if atIdx < 0 || (slashIdx >= 0 && atIdx > slashIdx) {
		return u
	}
	return u[:schemeIdx+3] + RedactedCredentialSentinel + ":" + RedactedCredentialSentinel + rest[atIdx:]
}

// urlInLogRegex matches URL-shaped substrings inside a log line.
var urlInLogRegex = regexp.MustCompile(`(?i)(rtmp|rtmps|rtsp|rtsps|srt|http|https|udp|rtp)://[^\s'"]+`)

// RedactURLsInLog scans a free-form log line for URLs and runs each one
// through RedactURLCredentials, preserving trailing punctuation.
func RedactURLsInLog(line string) string {
	return urlInLogRegex.ReplaceAllStringFunc(line, func(match string) string {
		trailing := ""
		for len(match) > 0 {
			last := match[len(match)-1]
			if last == ',' || last == '.' || last == ')' || last == ']' || last == ';' || last == ':' {
				trailing = string(last) + trailing
				match = match[:len(match)-1]
				continue
			}
			break
		}
		return RedactURLCredentials(match) + trailing
	})
}

// redactStreamKey replaces a stream key in a log line with <redacted>.
// Skipped when the key is empty or shorter than 8 characters (those are
// almost certainly not a real key — refusing to redact "abc" prevents
// matching common words and leaking surrounding context).
func redactStreamKey(line, key string) string {
	if key == "" || len(key) < 8 {
		return line
	}
	if !strings.Contains(line, key) {
		return line
	}
	return strings.ReplaceAll(line, key, "<redacted>")
}

// RedactArgsForLog returns a copy of argv with secrets scrubbed. Used to
// emit FFmpeg invocations into logs without leaking RTSP/SRT credentials
// or the stream key. The streamKey arg redacts a fixed sentinel value
// (live encode key); URL-shaped args go through RedactURLCredentials.
//
// `-y` is preserved verbatim — it's not a secret. The argument is documented
// here because the original spec asked us to mention it: callers who don't
// want `-y` shown in logs can pass strings.NewReplacer / their own filter.
func RedactArgsForLog(argv []string, streamKey string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		redacted := RedactURLsInLog(a)
		redacted = redactStreamKey(redacted, streamKey)
		out[i] = redacted
	}
	return out
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// joinPath joins dir + name using OS-native separators. We don't import
// path/filepath here to keep this file unit-testable on every platform
// without changing slashes; the supervisor and handlers use filepath.Join
// to construct OutputDir and we just append the segment pattern.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	if strings.HasSuffix(dir, "/") || strings.HasSuffix(dir, "\\") {
		return dir + name
	}
	sep := "/"
	if runtime.GOOS == "windows" && strings.Contains(dir, "\\") {
		sep = "\\"
	}
	return dir + sep + name
}
