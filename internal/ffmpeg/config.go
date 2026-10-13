// Package ffmpeg builds and supervises FFmpeg invocations for
// easycampus-go. It's the only package in the tree that owns
// exec.Command("ffmpeg", ...) calls — everything else routes through
// Config + Supervisor.
//
// Design notes:
//   - Live encode (capture device -> HLS) is built by Config.Args with
//     Mode = ModeLiveHLS. Stall watchdog + restart-with-backoff is
//     handled by Supervisor; restarts append to the existing playlist.
//   - Upload conversion (input file / URL -> HLS) is Mode = ModeUpload.
//     Runs once to completion (Job); no restart loop.
//
// The QualityPreset interface (see below) is the seam for the
// consumer/prosumer/broadcast/cinema quality matrix in internal/quality.
// Don't add tier-specific knowledge to this file — add it to a
// QualityPreset implementation (or an optional capability interface)
// and pass it in.
package ffmpeg

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
)

// -----------------------------------------------------------------------------
// QualityPreset — broadcast-engineer integration surface.
// -----------------------------------------------------------------------------
//
// Contract:
//
//   - All methods must be safe to call from any goroutine. Implementations
//     should be value types or guarded by their own locks.
//   - Bitrate strings use FFmpeg's k/M suffix convention ("6M", "8000k").
//   - Resolution() returns "WxH" (e.g. "1920x1080"). Width()/Height()
//     drive the `scale=W:H` filter and the capture-size request.
//   - FPSExpr() returns "30", "60", "24000/1001" — passed verbatim to
//     `-r`. Cinema presets MUST return the rational form so 23.976 does
//     not drift to 24.000 over service-length streams.
//   - GOP() returns the frame-count interval between keyframes. For
//     standard live HLS this is FPS() * 2 (two-second segments).
//   - PixelFormat() returns "yuv420p" for SDR HLS; HEVC tiers may return
//     "yuv420p10le". The args builder maps it to the encoder's native
//     layout (p010le on VideoToolbox / NVENC).
//   - Profile() / Level() are profile and level strings ("high" / "4.1").
//     An HEVC "main10" profile is mapped to "high" if the encode falls
//     back to H.264.
//   - VideoEncoderPreset() returns an x264-style speed preset name
//     ("ultrafast".."veryslow"). It is translated per backend (NVENC
//     p1..p7, QSV veryfast..veryslow, ignored on VideoToolbox / VAAPI).
//     NVENC "p1".."p7" are passed through unchanged. Empty means "use
//     our per-encoder default".
//   - Tune() returns "zerolatency", "film", "animation", "", etc. Empty
//     means "do not pass -tune". The live encode path force-overrides
//     this to "zerolatency" when WantsZeroLatency() reports true.
//   - WantsBFrames() lets cinema/archive tiers opt INTO B-frames where
//     consumer live tiers turn them off (-bf 0).
type QualityPreset interface {
	ID() string           // stable identifier ("consumer-1080p30", "broadcast-2160p60", ...)
	Label() string        // human-friendly name shown in UI
	Resolution() string   // "1920x1080"
	Width() int           // numeric pieces of Resolution
	Height() int          //
	FPS() int             // integer FPS rounded down (30, 60). Use FPSExpr for the wire value.
	FPSExpr() string      // "30" | "60" | "24000/1001"
	GOP() int             // keyframe interval in frames
	VideoBitrate() string // "6M" / "8000k"
	AudioBitrate() string // "128k" / "192k"
	PixelFormat() string  // "yuv420p"
	Profile() string      // "high"
	Level() string        // "4.1"
	VideoEncoderPreset() string
	Tune() string           // "" or "zerolatency" etc.
	WantsZeroLatency() bool // forces -tune zerolatency on the live path
	WantsBFrames() bool     // false => emit -bf 0
}

// DefaultPreset is a minimal, conservative QualityPreset used when the
// tier matrix is absent (tests, degraded operation). It targets 1080p30
// at 6 Mbps — the existing easycampus-go "high" quality setting.
type DefaultPreset struct{}

func (DefaultPreset) ID() string                 { return "default-1080p30" }
func (DefaultPreset) Label() string              { return "1080p30 @ 6 Mbps" }
func (DefaultPreset) Resolution() string         { return "1920x1080" }
func (DefaultPreset) Width() int                 { return 1920 }
func (DefaultPreset) Height() int                { return 1080 }
func (DefaultPreset) FPS() int                   { return 30 }
func (DefaultPreset) FPSExpr() string            { return "30" }
func (DefaultPreset) GOP() int                   { return 60 }
func (DefaultPreset) VideoBitrate() string       { return "6000k" }
func (DefaultPreset) AudioBitrate() string       { return "128k" }
func (DefaultPreset) PixelFormat() string        { return "yuv420p" }
func (DefaultPreset) Profile() string            { return "high" }
func (DefaultPreset) Level() string              { return "4.1" }
func (DefaultPreset) VideoEncoderPreset() string { return "veryfast" }
func (DefaultPreset) Tune() string               { return "" }
func (DefaultPreset) WantsZeroLatency() bool     { return true }
func (DefaultPreset) WantsBFrames() bool         { return false }

// SimplePreset is a flexible QualityPreset implementation for the
// "low" / "standard" / "high" / "ultra" buckets used by upload conversion.
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

func (p SimplePreset) ID() string         { return p.IDValue }
func (p SimplePreset) Label() string      { return p.LabelValue }
func (p SimplePreset) Resolution() string { return fmt.Sprintf("%dx%d", p.W, p.H) }
func (p SimplePreset) Width() int         { return p.W }
func (p SimplePreset) Height() int        { return p.H }
func (p SimplePreset) FPS() int           { return p.FPSInt }
func (p SimplePreset) FPSExpr() string {
	if p.FPSString != "" {
		return p.FPSString
	}
	return fmt.Sprintf("%d", p.FPSInt)
}
func (p SimplePreset) GOP() int             { return p.GOPFrames }
func (p SimplePreset) VideoBitrate() string { return p.VBitrate }
func (p SimplePreset) AudioBitrate() string { return p.ABitrate }
func (p SimplePreset) PixelFormat() string {
	if p.PixFmt == "" {
		return "yuv420p"
	}
	return p.PixFmt
}
func (p SimplePreset) Profile() string            { return p.ProfileValue }
func (p SimplePreset) Level() string              { return p.LevelValue }
func (p SimplePreset) VideoEncoderPreset() string { return p.EncoderPreset }
func (p SimplePreset) Tune() string               { return p.TuneValue }
func (p SimplePreset) WantsZeroLatency() bool     { return p.ZeroLatency }
func (p SimplePreset) WantsBFrames() bool         { return p.BFrames }

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
	Kind InputKind
	// VideoDevice is the device path / index / name / file path. For
	// dshow it is the device name; a "video=" prefix is added if missing.
	VideoDevice string
	// AudioDevice is an optional secondary audio source: the
	// avfoundation audio index, or the dshow audio device name (emitted
	// as ":audio=<name>").
	AudioDevice string
	URL         string // when Kind == InputNetwork
	// Framerate is the capture framerate request (avfoundation, v4l2,
	// dshow). avfoundation falls back to the preset FPS when zero;
	// v4l2/dshow omit -framerate when zero (device default).
	Framerate int
	// VideoSize is the capture resolution request ("1280x720"). Empty =
	// the preset resolution on avfoundation / v4l2, device default on
	// dshow (dshow hard-fails on an unsupported size).
	VideoSize string
	// InputFormat is the v4l2 -input_format ("mjpeg", "yuyv422", ...).
	// Many UVC cameras only reach 1080p30 in MJPEG. Empty = driver default.
	InputFormat string
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
// `delete_segments` if HLSDeleteOld is set. The Supervisor sets
// HLSAppend on restarts so the playlist / DVR window survives.
//
// Upload conversion: Mode = ModeUpload, Input.Kind = InputFile or
// InputNetwork. Output is an unbounded HLS playlist; `delete_segments`
// and `append_list` are NOT used — re-running a conversion is a fresh
// transcode, and appending would duplicate the content.
type Config struct {
	Binary string

	Mode    Mode
	Input   Input
	Preset  QualityPreset
	Encoder Encoder // EncoderAuto = probe and pick the best usable encoder
	// HWAccel selects input-side decode acceleration:
	//   HWAccelNone (""): no -hwaccel flag (software decode).
	//   HWAccelAuto ("auto"): pick the hwaccel matching the encoder
	//     (videotoolbox/cuda/vaapi/qsv) from `ffmpeg -hwaccels`, falling
	//     back to `-hwaccel auto` when only a mismatched one exists.
	//   Any other value: passed verbatim as `-hwaccel <value>`.
	HWAccel HWAccel

	// HLS output settings.
	OutputDir         string // directory to write playlist + segments
	HLSSegmentSeconds int    // -hls_time; default 2
	HLSListSize       int    // -hls_list_size; 0 = unlimited (archive default)
	HLSDeleteOld      bool   // live: drop old segments to bound disk usage
	HLSPlaylistType   string // "event" / "vod" / ""
	HLSSegmentType    string // "fmp4" / "mpegts"; default "fmp4" for live and HEVC, "mpegts" for H.264 upload
	HLSSegmentPattern string // overrides the default segment_%06d pattern when set
	HLSInitFile       string // fmp4 init filename ("init.mp4")
	PlaylistFilename  string // defaults to "master.m3u8"
	// HLSAppend (live only) continues an existing playlist instead of
	// overwriting it: `-hls_flags +append_list+discont_start`. The
	// Supervisor sets this on every restart after the first run.
	HLSAppend bool

	// LogLevel passed to `-loglevel`. We emit `-nostats` always so the
	// stderr stream is parse-friendly; progress goes through
	// `-progress pipe:1`. Default: "error".
	LogLevel string

	// DisableAudio drops the audio mapping/encoder entirely.
	DisableAudio bool

	// StreamKey is redacted from logs if it ever appears.
	StreamKey string

	// SourceHasMissingPTS hints that the input lacks valid PTS so the
	// builder adds `-fflags +genpts`.
	SourceHasMissingPTS bool

	// Realtime: when true, use `-re` on file/network sources so the
	// pipeline paces at wallclock (live-emulation).
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
// invalid; never panics. Safe to call from any goroutine. Encoder
// detection (EncoderAuto, HEVC availability) is probed once per process
// and cached, so repeated calls don't spawn ffmpeg.
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
	// `-y` overwrites output without prompting. Each event has its own
	// directory; restarts that must keep the playlist use HLSAppend.
	args = append(args, "-y")

	encoder := c.EffectiveEncoder()
	hwaccel := c.EffectiveHWAccel(encoder)
	plan := c.planVideo(encoder)
	if plan.warning != "" {
		warnOnce(plan.warning)
	}

	// VAAPI encode needs a filter-graph device for hwupload. Global
	// option; emitted before the inputs.
	if plan.family == EncoderVAAPI {
		args = append(args, "-vaapi_device", vaapiRenderNode)
	}

	// Input-side hwaccel. Must precede `-i`.
	if hwaccel != HWAccelNone {
		args = append(args, "-hwaccel", string(hwaccel))
	}

	// SourceHasMissingPTS: opt-in genpts only. Blanket genpts on
	// well-formed inputs can shift A/V sync.
	if c.SourceHasMissingPTS {
		args = append(args, "-fflags", "+genpts")
	}

	// Live capture devices benefit from -thread_queue_size to avoid
	// "real-time buffer too full, frame dropped".
	switch c.Input.Kind {
	case InputAVFoundation, InputV4L2, InputDShow, InputDeckLink, InputNDI:
		args = append(args, "-thread_queue_size", "1024")
	}

	inputArgs, err := c.inputArgs()
	if err != nil {
		return nil, err
	}
	args = append(args, inputArgs...)

	// Video mapping, filter chain, encoder.
	args = append(args, "-map", "0:v:0")
	if vf := c.videoFilterChain(plan); vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args, c.encoderArgs(plan)...)
	if plan.outPixFmt != "" {
		args = append(args, "-pix_fmt", plan.outPixFmt)
	}

	// Output framerate. -fps_mode cfr replaces the deprecated -vsync.
	args = append(args, "-fps_mode", "cfr", "-r", c.Preset.FPSExpr())

	// Force keyframes on the segment boundary so HLS cuts land on IDRs.
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

	args = append(args, c.hlsOutputArgs(plan.hevc)...)
	return args, nil
}

// EffectiveEncoder returns the encoder backend to use. EncoderAuto is
// resolved by DetectEncoders + PickEncoder; the detection (including a
// trial encode per candidate) is cached for the process lifetime, so
// only the first call spawns ffmpeg. An HEVCAware preset may still
// promote the result to the family's HEVC encoder in Args().
func (c Config) EffectiveEncoder() Encoder {
	if c.Encoder != EncoderAuto {
		return c.Encoder
	}
	return PickEncoder(DetectEncoders(c.Binary))
}

// EffectiveHWAccel returns the input hwaccel decision. Pass the encoder
// you'll use so we can match them (videotoolbox decode + videotoolbox
// encode skips a CPU roundtrip). Detection is cached per process.
func (c Config) EffectiveHWAccel(encoder Encoder) HWAccel {
	if c.HWAccel == HWAccelNone {
		return HWAccelNone
	}
	if c.HWAccel != HWAccelAuto {
		return c.HWAccel
	}
	return PickHWAccel(DetectHWAccels(c.Binary), encoder)
}

// EffectiveVideoCodec returns the `-c:v` value Args() will emit, after
// HEVC promotion / fallback. Useful for logging and UI.
func (c Config) EffectiveVideoCodec() string {
	if c.Preset == nil {
		return ""
	}
	return c.planVideo(c.EffectiveEncoder()).codec
}

func (c Config) inputArgs() ([]string, error) {
	in := c.Input
	switch in.Kind {
	case InputAVFoundation:
		fr := in.Framerate
		if fr <= 0 {
			fr = c.Preset.FPS()
		}
		size := c.captureSize()
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
		dev := in.VideoDevice
		if dev == "" {
			dev = "/dev/video0"
		}
		args := []string{"-f", "v4l2"}
		if in.InputFormat != "" {
			args = append(args, "-input_format", in.InputFormat)
		}
		if in.Framerate > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", in.Framerate))
		}
		// v4l2 negotiates the nearest supported size (with a warning)
		// rather than failing, so asking for the preset size is safe.
		args = append(args, "-video_size", c.captureSize(), "-i", dev)
		return args, nil
	case InputDShow:
		args := []string{"-f", "dshow"}
		if in.VideoSize != "" {
			args = append(args, "-video_size", in.VideoSize)
		}
		if in.Framerate > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", in.Framerate))
		}
		args = append(args, "-i", dshowInputSpec(in.VideoDevice, in.AudioDevice, c.DisableAudio))
		return args, nil
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

// captureSize is the capture-resolution request: the explicit
// Input.VideoSize, else the preset's resolution (so the device delivers
// what we encode instead of being upscaled from 720p).
func (c Config) captureSize() string {
	if c.Input.VideoSize != "" {
		return c.Input.VideoSize
	}
	if c.Preset.Width() > 0 && c.Preset.Height() > 0 {
		return fmt.Sprintf("%dx%d", c.Preset.Width(), c.Preset.Height())
	}
	return "1280x720"
}

// dshowInputSpec builds the DirectShow `-i` value:
// "video=<name>[:audio=<name>]". Device names from the scanner come
// without the "video=" prefix, which dshow requires.
func dshowInputSpec(video, audio string, disableAudio bool) string {
	video = strings.TrimSpace(video)
	if video == "" {
		video = "Integrated Camera"
	}
	spec := video
	if !strings.HasPrefix(strings.ToLower(spec), "video=") && !strings.HasPrefix(strings.ToLower(spec), "audio=") {
		spec = "video=" + spec
	}
	audio = strings.TrimSpace(audio)
	if !disableAudio && audio != "" && !strings.Contains(strings.ToLower(spec), ":audio=") {
		audio = strings.TrimPrefix(audio, "audio=")
		spec += ":audio=" + audio
	}
	return spec
}

// networkInputFlags returns the scheme-appropriate input flags for a
// network URL. RTSP wants tcp transport on lossy LANs, HLS wants
// reconnect on intermittent CDNs, MPEG-TS over UDP/RTP wants a big FIFO.
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

func (c Config) hlsOutputArgs(hevc bool) []string {
	segSec := c.hlsSegmentSeconds()
	playlist := c.PlaylistFilename
	if playlist == "" {
		playlist = "master.m3u8"
	}
	segType := c.HLSSegmentType
	if segType == "" {
		// HEVC in HLS requires fMP4 for Apple players.
		if c.Mode == ModeLiveHLS || hevc {
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
	if c.Mode == ModeLiveHLS {
		if c.HLSDeleteOld {
			flags += "+delete_segments"
		}
		// Restart: continue the existing playlist (media sequence and
		// segment numbering carry on, an EXT-X-DISCONTINUITY marks the
		// splice) instead of wiping the DVR window.
		if c.HLSAppend {
			flags += "+append_list+discont_start"
		}
	}
	// ModeUpload: no append_list. A re-run is a full re-transcode;
	// appending would duplicate every segment in the playlist.

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

// RunInitFilename returns the fMP4 init-segment name for supervisor run
// number `run` (0 = first run). Run 0 keeps the configured name; later
// runs get a "-rN" suffix so a restart never overwrites the init segment
// earlier segments were encoded against.
func RunInitFilename(base string, run int) string {
	if base == "" {
		base = "init.mp4"
	}
	if run <= 0 {
		return base
	}
	ext := ""
	if i := strings.LastIndex(base, "."); i > 0 {
		ext = base[i:]
		base = base[:i]
	}
	return fmt.Sprintf("%s-r%d%s", base, run, ext)
}

// -----------------------------------------------------------------------------
// Optional preset capabilities
// -----------------------------------------------------------------------------

// ColorPresetAware is an optional capability interface that
// QualityPreset implementations can satisfy to surface broadcast-grade
// VUI metadata. When present the builder (a) tags frames in the -vf
// chain with `setparams` (FFmpeg 7+ lets frame side data override the
// output-side flags, so the filter is what actually sticks) and (b)
// still emits -color_primaries / -color_trc / -colorspace /
// -color_range before -c:v for the encoder's SPS/VUI.
type ColorPresetAware interface {
	ColorPrimaries() string
	ColorTransfer() string
	ColorSpace() string
	ColorRange() string
}

// RateControlAware lets a preset declare its rate-control envelope
// (CBR vs VBR, plus the VBV ceiling and buffer size).
type RateControlAware interface {
	RateControlMode() string
	VideoMaxBitrate() string
	VideoBufSize() string
}

// BroadcastSafeAware lets a preset opt INTO the broadcast-legal limiter
// (luma 16-235, chroma 16-240, scaled for 10-bit) in the -vf chain.
type BroadcastSafeAware interface {
	BroadcastSafe() bool
}

// HEVCAware lets a preset request HEVC output. When PrefersHEVC() is
// true the encoder family is promoted to its HEVC sibling
// (libx264 -> libx265, h264_videotoolbox -> hevc_videotoolbox,
// h264_nvenc -> hevc_nvenc). If that encoder isn't usable (or the
// family has no HEVC path: VAAPI, QSV), the encode falls back to 8-bit
// H.264 on the same family and a warning is logged.
type HEVCAware interface {
	PrefersHEVC() bool
}

// -----------------------------------------------------------------------------
// Video codec plan
// -----------------------------------------------------------------------------

// videoPlan is the resolved video encode: which codec, which profile,
// which pixel formats. Computed once per Args() call.
type videoPlan struct {
	family    Encoder // H.264 backend family (x264 / vt / nvenc / vaapi / qsv)
	codec     string  // -c:v value
	hevc      bool
	profile   string // -profile:v value
	swPixFmt  string // software format pinned in the -vf chain
	outPixFmt string // -pix_fmt value ("" = omit, VAAPI)
	warning   string // non-empty when we fell back from HEVC
}

func (c Config) planVideo(encoder Encoder) videoPlan {
	family := encoderFamily(encoder)
	if family == EncoderAuto {
		family = EncoderX264
	}
	wantHEVC := isHEVCEncoder(encoder)
	if h, ok := c.Preset.(HEVCAware); ok && h.PrefersHEVC() {
		wantHEVC = true
	}
	presetPixFmt := c.Preset.PixelFormat()
	if presetPixFmt == "" {
		presetPixFmt = "yuv420p"
	}

	if wantHEVC {
		sibling := hevcSibling(family)
		if sibling != "" && ProbeEncoder(c.Binary, string(sibling)) {
			tenBit := isTenBit(presetPixFmt)
			p := videoPlan{
				family:   family,
				codec:    string(sibling),
				hevc:     true,
				profile:  "main",
				swPixFmt: presetPixFmt,
			}
			if tenBit {
				p.profile = "main10"
			}
			p.outPixFmt = presetPixFmt
			if tenBit && (sibling == EncoderHEVCVideoToolbox || sibling == EncoderHEVCNVENC) {
				// VT / NVENC take semi-planar 10-bit, not yuv420p10le.
				p.outPixFmt = "p010le"
			}
			return p
		}
		h264 := c.planH264(family, true)
		missing := string(sibling)
		if missing == "" {
			missing = "an HEVC encoder for " + string(family)
		}
		h264.warning = fmt.Sprintf("ffmpeg: preset %s wants HEVC but %s is not usable; falling back to 8-bit H.264 (%s)",
			c.Preset.ID(), missing, h264.codec)
		return h264
	}
	return c.planH264(family, false)
}

func (c Config) planH264(family Encoder, fallbackFromHEVC bool) videoPlan {
	pix := c.Preset.PixelFormat()
	if pix == "" {
		pix = "yuv420p"
	}
	profile := c.Preset.Profile()
	if fallbackFromHEVC || (family != EncoderX264 && isTenBit(pix)) {
		// Hardware H.264 encoders are 8-bit only, and an HEVC preset's
		// 10-bit format doesn't apply after falling back.
		pix = "yuv420p"
	}
	switch profile {
	case "", "main10", "main12", "rext":
		profile = "high"
	}
	p := videoPlan{
		family:    family,
		codec:     string(family),
		profile:   profile,
		swPixFmt:  pix,
		outPixFmt: pix,
	}
	if family == EncoderVAAPI {
		// Frames are uploaded to the GPU at the end of the -vf chain;
		// -pix_fmt would fight the hw frame format.
		p.outPixFmt = ""
	}
	return p
}

func isTenBit(pixFmt string) bool {
	return strings.Contains(pixFmt, "10le") || strings.Contains(pixFmt, "10be") || strings.HasPrefix(pixFmt, "p010")
}

// videoFilterChain builds the single -vf chain:
//
//	scale=W:H[:out_range=R], format=<sw pix fmt>,
//	setparams=color_primaries=..:color_trc=..:colorspace=..:range=..,
//	limiter (luma) , limiter (chroma),
//	format=nv12,hwupload            (VAAPI only)
func (c Config) videoFilterChain(plan videoPlan) string {
	var parts []string
	cp, colorAware := c.Preset.(ColorPresetAware)

	if w, h := c.Preset.Width(), c.Preset.Height(); w > 0 && h > 0 {
		scale := fmt.Sprintf("scale=%d:%d", w, h)
		if colorAware && cp.ColorRange() != "" {
			// Convert (not just retag) full-range webcam / MJPEG input.
			scale += ":out_range=" + cp.ColorRange()
		}
		parts = append(parts, scale)
	}
	if plan.swPixFmt != "" {
		// Pin the software format so the limiter sees a known bit depth.
		parts = append(parts, "format="+plan.swPixFmt)
	}
	if colorAware {
		sp := []string{}
		if v := cp.ColorPrimaries(); v != "" {
			sp = append(sp, "color_primaries="+v)
		}
		if v := cp.ColorTransfer(); v != "" {
			sp = append(sp, "color_trc="+v)
		}
		if v := cp.ColorSpace(); v != "" {
			sp = append(sp, "colorspace="+v)
		}
		if v := cp.ColorRange(); v != "" {
			sp = append(sp, "range="+v)
		}
		if len(sp) > 0 {
			parts = append(parts, "setparams="+strings.Join(sp, ":"))
		}
	}
	if bs, ok := c.Preset.(BroadcastSafeAware); ok && bs.BroadcastSafe() {
		parts = append(parts, limiterFilters(plan.swPixFmt)...)
	}
	if plan.family == EncoderVAAPI && !plan.hevc {
		parts = append(parts, "format=nv12", "hwupload")
	}
	return strings.Join(parts, ",")
}

// limiterFilters clamps to the broadcast-legal range: luma (plane 0,
// planes=1) 16-235, chroma (planes 1+2, planes=6) 16-240, scaled x4 for
// 10-bit formats.
func limiterFilters(pixFmt string) []string {
	shift := 1
	if isTenBit(pixFmt) {
		shift = 4
	}
	return []string{
		fmt.Sprintf("limiter=min=%d:max=%d:planes=1", 16*shift, 235*shift),
		fmt.Sprintf("limiter=min=%d:max=%d:planes=6", 16*shift, 240*shift),
	}
}

// -----------------------------------------------------------------------------
// Encoder args
// -----------------------------------------------------------------------------

// encoderArgs returns the encoder-specific flags for a resolved plan.
// Each hardware encoder has its own knobs because they don't share
// x264's CLI surface.
func (c Config) encoderArgs(plan videoPlan) []string {
	preset := c.Preset
	level := preset.Level()
	levelNoDot := strings.ReplaceAll(level, ".", "")

	wantsTune := preset.Tune()
	if c.Mode == ModeLiveHLS && preset.WantsZeroLatency() {
		wantsTune = "zerolatency"
	}
	// Upload conversions explicitly do NOT use zerolatency — it disables
	// B-frames and hurts quality at the same bitrate.
	if c.Mode == ModeUpload && wantsTune == "zerolatency" {
		wantsTune = ""
	}

	bfArgs := []string{}
	if !preset.WantsBFrames() {
		bfArgs = []string{"-bf", "0"}
	}

	// Color VUI flags ahead of -c:v so the encoder bakes them into the
	// SPS. The setparams filter in the -vf chain tags the frames.
	var colorArgs []string
	if cp, ok := preset.(ColorPresetAware); ok {
		colorArgs = []string{
			"-color_primaries", cp.ColorPrimaries(),
			"-color_trc", cp.ColorTransfer(),
			"-colorspace", cp.ColorSpace(),
			"-color_range", cp.ColorRange(),
		}
	}

	// Rate control — RateControlAware presets declare CBR/VBR + VBV.
	// Others get the legacy "soft CBR" path (maxrate == bufsize == target).
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
	gop := preset.GOP()

	args := append([]string{}, colorArgs...)
	switch Encoder(plan.codec) {
	case EncoderVideoToolbox, EncoderHEVCVideoToolbox:
		args = append(args, "-c:v", plan.codec, "-profile:v", plan.profile)
		if !plan.hevc {
			args = append(args, "-level:v", level)
		}
		args = append(args, "-allow_sw", "1", "-b:v", preset.VideoBitrate())
		if c.Mode == ModeLiveHLS {
			args = append(args, "-realtime", "1")
			if rcMode == "cbr" {
				args = append(args, "-constant_bit_rate", "1")
			}
		}
		if plan.hevc {
			args = append(args, "-tag:v", "hvc1")
		}
	case EncoderNVENC, EncoderHEVCNVENC:
		nvencRC := "cbr"
		if rcMode == "vbr" || rcMode == "crf" || rcMode == "cqp" {
			nvencRC = "vbr"
		}
		args = append(args,
			"-c:v", plan.codec,
			"-preset", nvencPreset(preset.VideoEncoderPreset()),
			"-profile:v", plan.profile,
		)
		if !plan.hevc {
			args = append(args, "-level:v", level)
		}
		args = append(args,
			"-rc", nvencRC,
			"-no-scenecut", "1",
			"-forced-idr", "1",
			"-strict_gop", "1",
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
		if plan.hevc {
			args = append(args, "-tag:v", "hvc1")
		}
	case EncoderVAAPI:
		args = append(args,
			"-c:v", plan.codec,
			"-profile:v", plan.profile,
			"-level", levelNoDot,
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
	case EncoderQSV:
		args = append(args,
			"-c:v", plan.codec,
			"-preset", qsvPreset(preset.VideoEncoderPreset()),
			"-profile:v", plan.profile,
			"-level", levelNoDot,
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
	case EncoderX265:
		params := []string{
			fmt.Sprintf("keyint=%d", gop),
			fmt.Sprintf("min-keyint=%d", gop),
			"scenecut=0",
			"open-gop=0",
			"log-level=error",
		}
		args = append(args,
			"-c:v", plan.codec,
			"-preset", defaultString(preset.VideoEncoderPreset(), "veryfast"),
			"-profile:v", plan.profile,
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-x265-params", strings.Join(params, ":"),
		)
		if wantsTune != "" {
			args = append(args, "-tune", wantsTune)
		}
		args = append(args, "-tag:v", "hvc1")
	default: // libx264
		args = append(args,
			"-c:v", plan.codec,
			"-preset", defaultString(preset.VideoEncoderPreset(), "veryfast"),
			"-profile:v", plan.profile,
			"-level:v", level,
			"-b:v", preset.VideoBitrate(),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
		)
		// Broadcast CBR: declare HRD parameters in the SPS VUI and emit
		// filler NALs. Gated on RateControlAware so the legacy
		// SimplePreset path keeps its historic argv.
		if rateControlAware && rcMode == "cbr" {
			args = append(args, "-x264-params", fmt.Sprintf(
				"nal-hrd=cbr:filler=1:keyint=%d:min-keyint=%d:scenecut=0:open-gop=0", gop, gop))
		}
		if wantsTune != "" {
			args = append(args, "-tune", wantsTune)
		}
	}
	args = append(args, bfArgs...)
	return args
}

// x264PresetRank orders the x264 speed presets fastest-first.
var x264PresetRank = map[string]int{
	"ultrafast": 0, "superfast": 1, "veryfast": 2, "faster": 3,
	"fast": 4, "medium": 5, "slow": 6, "slower": 7, "veryslow": 8, "placebo": 8,
}

// nvencPreset translates an x264-style preset name to NVENC p1..p7
// (p1 fastest). "p1".."p7" pass through; empty / unknown -> p4.
func nvencPreset(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if len(name) == 2 && name[0] == 'p' && name[1] >= '1' && name[1] <= '7' {
		return name
	}
	switch rank, ok := x264PresetRank[name]; {
	case !ok:
		return "p4"
	case rank <= 0:
		return "p1"
	case rank == 1:
		return "p2"
	case rank <= 3:
		return "p3"
	case rank == 4:
		return "p4"
	case rank == 5:
		return "p5"
	case rank == 6:
		return "p6"
	default:
		return "p7"
	}
}

// qsvPreset maps an x264-style preset to the QSV keyword space
// (veryfast..veryslow). ultrafast/superfast clamp to veryfast; NVENC
// pN names and unknowns map to the default veryfast.
func qsvPreset(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow":
		return name
	case "placebo":
		return "veryslow"
	}
	return "veryfast"
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
// Helpers
// -----------------------------------------------------------------------------

var (
	warnedMu sync.Mutex
	warned   = map[string]bool{}
	// warnf is the warning sink; swapped in tests.
	warnf = log.Printf
)

// warnOnce logs msg the first time it is seen. Args() runs on every
// supervisor restart, so a fallback warning would otherwise repeat.
func warnOnce(msg string) {
	warnedMu.Lock()
	seen := warned[msg]
	warned[msg] = true
	fn := warnf
	warnedMu.Unlock()
	if !seen {
		fn("%s", msg)
	}
}

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
