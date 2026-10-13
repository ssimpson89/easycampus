// Package quality defines the EasyCampus preset matrix and the FFmpeg
// flag fragments each preset translates to.
//
// The package is deliberately self-contained: it has no dependency on the
// ffmpeg supervisor, the HTTP layer, or the configuration loader. Presets
// reach internal/ffmpeg through the FFmpegPreset adapter
// (ffmpeg_adapter.go), which satisfies ffmpeg.QualityPreset plus its
// optional capability interfaces; the argv itself is built by
// ffmpeg.Config.Args(). Keeping the boundary here narrow lets the table
// grow with new tiers without touching the process-management code.
//
// Tier model
//
//   - Consumer:  Mac mini iGPU / USB webcam / residential upload.
//     Works without thinking. Soft CBR is fine, audio at 128 kbps,
//     Rec.709 SDR, integer framerates.
//   - Prosumer:  Capture card / discrete GPU / business fiber.
//     1080p60 reliably with hardware encoders, true CBR via NVENC/QSV,
//     audio at 160 kbps, two-second GOP.
//   - Broadcast: SDI ingest / redundant network / technical operator.
//     True CBR with HRD signalling (libx264 nal-hrd=cbr), Rec.709 VUI
//     baked into the SPS so downstream transcoders don't guess,
//     1-second VBV so the egress rate stays flat, audio at 192 kbps
//     stereo (broadcast-safe baseline). 2160p adds BT.2020 primaries.
//   - Cinema:    23.976 / 24.000 with proper cadence preservation.
//     Higher bitrates per pixel because film grain and shallow-DOF
//     motion eat bits, no scenecut keyframes (closed GOP only), and
//     -fps_mode cfr is mandatory — drop-frame from a 30 fps source
//     will judder. See docs/quality-presets.md for the caveats.
//
// Bitrate / VBV references
//
//   - YouTube live H.264 recommendations:
//     https://support.google.com/youtube/answer/2853702
//   - SMPTE / EBU broadcast practice for 1-second VBV (bufsize == maxrate)
//     in live distribution.
//   - ITU-R BT.709 for HD primaries / transfer / matrix.
//   - ITU-R BT.2020 for UHD (2160p) primaries on the broadcast tier.
package quality

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Tier groups presets by their target operator and infrastructure
// profile. The UI uses this to render presets in sections; the
// recommender uses it to prefer the right tier for a given operator
// constraint set.
type Tier string

const (
	TierConsumer  Tier = "consumer"
	TierProsumer  Tier = "prosumer"
	TierBroadcast Tier = "broadcast"
	TierCinema    Tier = "cinema"
)

// RateControl names the strategy the encoder uses to hit the target
// bitrate. CBR is required by SRT and broadcast distribution; VBR is
// the consumer default; CRF/CQP are archival modes appropriate for
// cinema masters where size variance is acceptable.
type RateControl string

const (
	RateControlCBR RateControl = "cbr"
	RateControlVBR RateControl = "vbr"
	RateControlCRF RateControl = "crf" // libx264/libx265 quality-targeted
	RateControlCQP RateControl = "cqp" // NVENC constant QP, archival
)

// Encoder is the ffmpeg -c:v identifier. We keep this as a string so
// the FFmpeg supervisor can pass through whatever it detected at
// startup without us re-validating.
type Encoder string

const (
	EncoderX264             Encoder = "libx264"
	EncoderX265             Encoder = "libx265"
	EncoderH264VideoToolbox Encoder = "h264_videotoolbox"
	EncoderHEVCVideoToolbox Encoder = "hevc_videotoolbox"
	EncoderH264NVENC        Encoder = "h264_nvenc"
	EncoderHEVCNVENC        Encoder = "hevc_nvenc"
	EncoderH264QSV          Encoder = "h264_qsv"
	EncoderH264VAAPI        Encoder = "h264_vaapi"
)

// Rational represents a frame rate as a fraction. 23.976 must be
// expressed exactly as 24000/1001 — using the floating-point 23.976
// instead drifts ~3.6 seconds per hour and the muxer eventually drops
// a frame on a service-length stream. Den=1 covers integer rates
// (24, 25, 30, 50, 60).
type Rational struct {
	Num int `json:"num"`
	Den int `json:"den"`
}

func (r Rational) Float() float64 {
	if r.Den <= 0 {
		return float64(r.Num)
	}
	return float64(r.Num) / float64(r.Den)
}

// IsFractional reports whether the rate needs the num/den form on -r.
// NTSC-cinema 23.976 = 24000/1001 is the canonical fractional rate;
// 29.97 = 30000/1001 and 59.94 = 60000/1001 would qualify too if we
// ever ship them. Integer rates emit a plain decimal.
func (r Rational) IsFractional() bool { return r.Den > 1 }

// Expr returns the form ffmpeg accepts on -r. Integer rates render
// as plain decimals; fractional rates render as "num/den" so the
// muxer maintains exact PTS cadence across long streams.
func (r Rational) Expr() string {
	if r.Den <= 1 {
		return strconv.Itoa(r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// Int returns the rounded integer rate. Used wherever ffmpeg
// requires an integer (the avfoundation -framerate probe, scale
// filter rate constants). For everywhere the exact ratio matters,
// use Expr() instead.
func (r Rational) Int() int {
	if r.Den <= 0 {
		return r.Num
	}
	return int(math.Round(float64(r.Num) / float64(r.Den)))
}

// Common rational rates used across the preset table.
var (
	FPS2398 = Rational{Num: 24000, Den: 1001} // NTSC-cinema 23.976
	FPS24   = Rational{Num: 24, Den: 1}       // true 24.000
	FPS25   = Rational{Num: 25, Den: 1}       // PAL
	FPS30   = Rational{Num: 30, Den: 1}       // integer 30 (NB: not 29.97)
	FPS50   = Rational{Num: 50, Den: 1}
	FPS60   = Rational{Num: 60, Den: 1}
)

// HDRMetadata carries cinema-tier transfer-curve hints. Nil means
// SDR (Rec.709 transfer). Populated only on presets that actually
// emit HDR — we don't speculatively flip ColorPrimaries to BT.2020
// for SDR sources, because mistagged SDR clips dramatically on
// HDR-aware decoders.
type HDRMetadata struct {
	// MaxLuminance is the mastering-display peak in cd/m². 1000 is the
	// HDR10 reference; 600 is common for HLG broadcast.
	MaxLuminance int `json:"maxLuminance"`
	// MinLuminance is the mastering-display floor in cd/m² × 10000.
	MinLuminance int `json:"minLuminance"`
}

// Preset is the unit of operator-facing quality choice. One Preset
// produces a self-consistent set of ffmpeg flags: a fixed resolution,
// frame rate, and bitrate envelope, plus the color and HRD signalling
// required by its tier. Presets do not carry input-side concerns
// (capture format, audio device) — those belong to the input layer.
type Preset struct {
	ID    string `json:"id"`    // stable identifier (e.g. "broadcast-1080p60")
	Tier  Tier   `json:"tier"`  // grouping for UI / recommender
	Label string `json:"label"` // human-readable, shown in UI

	Width  int      `json:"width"`
	Height int      `json:"height"`
	FPS    Rational `json:"fps"`

	// VideoBitrate is the target rate the encoder aims to hit, in
	// kilobits per second. Used as -b:v on every encoder.
	VideoBitrate int `json:"videoBitrate"`
	// VideoMaxBitrate is the VBV ceiling (-maxrate). For CBR presets
	// equals VideoBitrate; for VBR it leaves a small headroom.
	VideoMaxBitrate int `json:"videoMaxBitrate"`
	// VideoBufSize is -bufsize. For broadcast/SRT (true CBR) this
	// equals VideoBitrate, producing a 1-second VBV — the strictest
	// rate-control envelope a live distributor will accept. For
	// VBR consumer/prosumer presets it's typically 2x target so the
	// encoder can save bits on easy scenes and spend them on hard
	// ones without violating the ceiling.
	VideoBufSize int         `json:"videoBufSize"`
	RateControl  RateControl `json:"rateControl"`

	// PixFmt is the pixel format the encoder accepts. yuv420p is the
	// universal compatible format; yuv422p10le on broadcast 1080p+
	// gives 10-bit chroma headroom for chroma keyer / downstream
	// grading without ballooning bitrate the way 4:4:4 would.
	PixFmt string `json:"pixFmt"`

	// Color signalling is emitted via -color_primaries / -color_trc
	// / -colorspace BEFORE -c:v so hardware encoders bake the VUI
	// hints into their SPS at init. Without explicit color tagging
	// downstream transcoders guess, and a Rec.709 SDR stream
	// occasionally arrives at the CDN tagged as unspecified —
	// players then default to BT.601 on some hardware decoders and
	// the colors clip.
	ColorPrimaries string `json:"colorPrimaries"` // bt709 / bt2020
	ColorTransfer  string `json:"colorTransfer"`  // bt709 / smpte2084 / arib-std-b67
	ColorSpace     string `json:"colorSpace"`     // bt709 / bt2020nc
	ColorRange     string `json:"colorRange"`     // tv / pc — broadcast is always tv

	AudioBitrate    int `json:"audioBitrate"`    // kbps
	AudioSampleRate int `json:"audioSampleRate"` // Hz, 48000 is the broadcast default
	AudioChannels   int `json:"audioChannels"`   // 2 = stereo, 6 = 5.1

	// H264Level is the level emitted in the SPS. The macroblock-rate
	// cap is per-level: 1080p30 fits in 4.1 (245.76 k MB/s); 1080p60
	// requires 4.2 (522 k MB/s); 2160p30 needs 5.1. Strict hardware
	// decoders reject non-conformant SPS, so each preset declares its
	// own and the encoder paths honor it.
	H264Level string `json:"h264Level"`
	// MinUploadKbps is the honest minimum upload bandwidth the
	// preset requires, including audio + 20% protocol overhead.
	// The recommender uses this to filter the catalog by what the
	// operator's actual connection can sustain.
	MinUploadKbps int `json:"minUploadKbps"`

	// RecommendedHW orders encoder preferences from best fit to
	// software fallback. The supervisor walks this list against the
	// list of locally-available encoders and picks the first match.
	RecommendedHW []Encoder `json:"recommendedHW"`

	// HDR is non-nil only for cinema-tier presets that actually
	// emit HDR transfer characteristics.
	HDR *HDRMetadata `json:"hdr,omitempty"`

	// BroadcastSafe applies a video filter that limits luma to the
	// 16-235 range and chroma to 16-240 (the Rec.709 broadcast-safe
	// envelope). Set on broadcast-tier presets so a source with
	// out-of-range whites (a confidence monitor, a Decklink card
	// passing through PC-range graphics) doesn't trip a downstream
	// QC system. SDR consumer/prosumer leave this off — applying it
	// to an already-legal source crushes mid-tones.
	BroadcastSafe bool `json:"broadcastSafe,omitempty"`

	// Notes is a one-line operator-facing summary surfaced in the UI.
	Notes string `json:"notes,omitempty"`
}

// Resolution returns the canonical "WxH" string. Used wherever a
// human-readable form is preferable to "%dx%d" inline.
func (p Preset) Resolution() string { return fmt.Sprintf("%dx%d", p.Width, p.Height) }

// GOP returns the keyframe interval in frames, sized for a 2-second
// GOP (YouTube live spec, broadcast HLS practice). Rounds to the
// nearest frame count so 23.976 produces 48 (~2.003 s — within the
// ≤4 s YouTube tolerance) and 30 produces 60.
func (p Preset) GOP() int {
	n := 2.0 * float64(p.FPS.Num)
	if p.FPS.Den > 0 {
		n /= float64(p.FPS.Den)
	}
	return int(math.Round(n))
}

// VideoBitrateStr renders the target bitrate in the "Nk" form ffmpeg
// expects for -b:v and -maxrate.
func (p Preset) VideoBitrateStr() string { return fmt.Sprintf("%dk", p.VideoBitrate) }

// VideoMaxBitrateStr renders -maxrate. CBR presets emit the target;
// VBR presets emit the ceiling.
func (p Preset) VideoMaxBitrateStr() string {
	r := p.VideoMaxBitrate
	if r == 0 {
		r = p.VideoBitrate
	}
	return fmt.Sprintf("%dk", r)
}

// VideoBufSizeStr renders -bufsize. CBR presets emit a 1-second VBV
// (bufsize == maxrate); VBR presets typically emit 2x target.
func (p Preset) VideoBufSizeStr() string {
	b := p.VideoBufSize
	if b == 0 {
		b = p.VideoBitrate
	}
	return fmt.Sprintf("%dk", b)
}

// AudioBitrateStr renders -b:a in ffmpeg's "Nk" form.
func (p Preset) AudioBitrateStr() string { return fmt.Sprintf("%dk", p.AudioBitrate) }

// Level returns the H.264 SPS profile-level with a sane default
// when the preset doesn't declare one explicitly.
func (p Preset) Level() string {
	if p.H264Level == "" {
		return "4.1"
	}
	return p.H264Level
}

// Presets is the complete catalog ordered roughly by ascending
// bitrate within each tier. The UI groups by Tier; the recommender
// walks the table top to bottom and picks the highest-quality preset
// that fits the operator's constraints.
//
// Bitrate justification per preset is inline. Two rules guide the
// numbers:
//
//  1. SDR live consumer/prosumer numbers land within YouTube's
//     published H.264 live recommendations (Google's table).
//  2. Broadcast presets clamp to 1-second VBV (bufsize == maxrate)
//     and use the upper end of YouTube's range — broadcast operators
//     have the bandwidth and the QoE downside of bursting is worse.
var Presets = []Preset{
	// =========================================================
	// CONSUMER — Mac mini, USB webcam, ~5-10 Mbps residential up.
	// =========================================================

	{
		ID:    "consumer-480p30",
		Tier:  TierConsumer,
		Label: "Consumer 480p30 (emergency)",
		Width: 854, Height: 480,
		FPS:             FPS30,
		VideoBitrate:    1500, // YT 480p: 0.5-2 Mbps. Low end of the band; the goal is "stream survives an 8 Mbps consumer line under load".
		VideoMaxBitrate: 2000, // 33% VBR headroom for hard scenes.
		VideoBufSize:    3000, // 2x target — standard VBR buffer.
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    96, // Speech-optimised baseline; saves 64 kbps for video.
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "3.1",
		MinUploadKbps:   2500,
		RecommendedHW:   []Encoder{EncoderH264VideoToolbox, EncoderH264NVENC, EncoderH264QSV, EncoderX264},
		Notes:           "Fallback for failing upload. Below most live distributor minimums; not for primary use.",
	},
	{
		ID:    "consumer-720p30",
		Tier:  TierConsumer,
		Label: "Consumer 720p30",
		Width: 1280, Height: 720,
		FPS:             FPS30,
		VideoBitrate:    3500, // YT 720p30: 2.5-5 Mbps. Mid-band — leaves margin for the volunteer operator who doesn't tune anything.
		VideoMaxBitrate: 4500,
		VideoBufSize:    7000, // 2x target — VBR can save and spend.
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    128,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "3.2",
		MinUploadKbps:   5500,
		RecommendedHW:   []Encoder{EncoderH264VideoToolbox, EncoderH264NVENC, EncoderH264QSV, EncoderX264},
		Notes:           "The default for a volunteer-operated church-on-a-budget stream.",
	},
	{
		ID:    "consumer-1080p30",
		Tier:  TierConsumer,
		Label: "Consumer 1080p30",
		Width: 1920, Height: 1080,
		FPS:             FPS30,
		VideoBitrate:    6000, // YT 1080p30: 4.5-9 Mbps. Lower end — consumer uplinks rarely sustain >8 Mbps committed.
		VideoMaxBitrate: 7500,
		VideoBufSize:    12000,
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    128,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.1",
		MinUploadKbps:   8500,
		RecommendedHW:   []Encoder{EncoderH264VideoToolbox, EncoderH264NVENC, EncoderH264QSV, EncoderX264},
		Notes:           "1080p30 on a consumer line. Step down if upload sustains <8 Mbps.",
	},

	// =========================================================
	// PROSUMER — capture card, decent GPU, business fiber.
	// =========================================================

	{
		ID:    "prosumer-720p60",
		Tier:  TierProsumer,
		Label: "Prosumer 720p60",
		Width: 1280, Height: 720,
		FPS:             FPS60,
		VideoBitrate:    6000, // YT 720p60: 3.5-7 Mbps. Top of band — prosumer can spare the bits and 60p deserves them.
		VideoMaxBitrate: 7000,
		VideoBufSize:    12000,
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    160,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.1",
		MinUploadKbps:   8000,
		RecommendedHW:   []Encoder{EncoderH264NVENC, EncoderH264VideoToolbox, EncoderH264QSV, EncoderX264},
		Notes:           "Smooth motion at 720p — good fit for music/movement when bandwidth is moderate.",
	},
	{
		ID:    "prosumer-1080p30-high",
		Tier:  TierProsumer,
		Label: "Prosumer 1080p30 (high)",
		Width: 1920, Height: 1080,
		FPS:             FPS30,
		VideoBitrate:    9000, // YT 1080p30 top: 9 Mbps. Headroom for capture-card-clean 1080p without going into broadcast territory.
		VideoMaxBitrate: 10000,
		VideoBufSize:    18000,
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    160,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.1",
		MinUploadKbps:   11000,
		RecommendedHW:   []Encoder{EncoderH264NVENC, EncoderH264VideoToolbox, EncoderH264QSV, EncoderX264},
		Notes:           "1080p30 with capture-card-grade headroom. The mid-tier default for a prosumer rig.",
	},
	{
		ID:    "prosumer-1080p60",
		Tier:  TierProsumer,
		Label: "Prosumer 1080p60",
		Width: 1920, Height: 1080,
		FPS:             FPS60,
		VideoBitrate:    12000, // YT 1080p60: 6-12 Mbps. Top of band — 60p doubles temporal data and capture-card sources have the cleanliness to reward bitrate.
		VideoMaxBitrate: 13000,
		VideoBufSize:    24000,
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    160,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.2", // 1080p60 exceeds 4.1's macroblock-rate cap (245.76 k MB/s); 4.2 caps at 522 k which covers it.
		MinUploadKbps:   14600, // (12000+160)*1.2 = 14592, rounded up
		RecommendedHW:   []Encoder{EncoderH264NVENC, EncoderH264VideoToolbox, EncoderH264QSV, EncoderX264},
		Notes:           "1080p60 with NVENC/VideoToolbox. Requires sustained 15+ Mbps upload.",
	},

	// =========================================================
	// BROADCAST — SDI ingest, redundant net, true CBR + Rec.709 VUI.
	// =========================================================

	{
		ID:    "broadcast-1080p30",
		Tier:  TierBroadcast,
		Label: "Broadcast 1080p30 (CBR)",
		Width: 1920, Height: 1080,
		FPS:             FPS30,
		VideoBitrate:    8000, // Broadcast 1080p30 H.264 CBR — within EBU Tech 3326 guidance for HD live distribution at the 6-10 Mbps tier.
		VideoMaxBitrate: 8000, // CBR: maxrate == target.
		VideoBufSize:    8000, // 1-second VBV — the strict envelope SRT/RTMP distributors expect.
		RateControl:     RateControlCBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    192, // Broadcast stereo baseline. Headroom for music + speech without dipping into AAC artifacts.
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.1",
		MinUploadKbps:   11000,
		RecommendedHW:   []Encoder{EncoderX264, EncoderH264NVENC, EncoderH264QSV, EncoderH264VideoToolbox},
		BroadcastSafe:   true,
		Notes:           "True CBR 1080p30 with 1-second VBV and Rec.709 VUI baked into the SPS.",
	},
	{
		ID:    "broadcast-1080p60",
		Tier:  TierBroadcast,
		Label: "Broadcast 1080p60 (CBR)",
		Width: 1920, Height: 1080,
		FPS:             FPS60,
		VideoBitrate:    15000, // 1080p60 broadcast — doubles temporal data vs 30p. EBU live HD upper end.
		VideoMaxBitrate: 15000,
		VideoBufSize:    15000,
		RateControl:     RateControlCBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    192,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.2",
		MinUploadKbps:   18300, // (15000+192)*1.2 = 18230, rounded up
		RecommendedHW:   []Encoder{EncoderX264, EncoderH264NVENC, EncoderH264QSV, EncoderH264VideoToolbox},
		BroadcastSafe:   true,
		Notes:           "1080p60 broadcast CBR. Pairs with a 25 Mbps committed-rate uplink.",
	},
	{
		ID:    "broadcast-2160p30",
		Tier:  TierBroadcast,
		Label: "Broadcast 2160p30 (HEVC, BT.2020)",
		Width: 3840, Height: 2160,
		FPS:             FPS30,
		VideoBitrate:    35000, // YT 2160p30: 20-51 Mbps. Mid-upper band — HEVC at 35 Mbps is broadcaster-clean 2160p30.
		VideoMaxBitrate: 35000,
		VideoBufSize:    35000,
		RateControl:     RateControlCBR,
		PixFmt:          "yuv420p10le", // 10-bit chroma — BT.2020 requires 10-bit per ITU-R BT.2020-2 §3.
		ColorPrimaries:  "bt2020",
		ColorTransfer:   "bt709", // SDR-2020: BT.2020 primaries but Rec.709 transfer. HDR variants would flip this to smpte2084 / arib-std-b67.
		ColorSpace:      "bt2020nc",
		ColorRange:      "tv",
		AudioBitrate:    192,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "5.1", // 2160p30 = ~245.76 k MB/s × 4 panels = beyond H.264 4.1; SPS gets HEVC equivalent on x265/hevc_nvenc paths, level is here for argv consistency.
		MinUploadKbps:   42300, // (35000+192)*1.2 = 42230, rounded up
		RecommendedHW:   []Encoder{EncoderHEVCNVENC, EncoderHEVCVideoToolbox, EncoderX265},
		BroadcastSafe:   true,
		Notes:           "2160p30 HEVC. BT.2020 primaries; 10-bit chroma. Requires 50+ Mbps committed uplink.",
	},

	// =========================================================
	// CINEMA — 24p / 23.976 cadence, archive-quality, closed GOP.
	// =========================================================

	{
		ID:    "cinema-1080p2398",
		Tier:  TierCinema,
		Label: "Cinema 1080p 23.976",
		Width: 1920, Height: 1080,
		FPS:             FPS2398,
		VideoBitrate:    12000, // YT 1080p24-30: 4.5-9 Mbps recommended; cinema sits above because film grain + shallow DOF eat bits.
		VideoMaxBitrate: 14000, // Small headroom — CRF would be better for true archive but the operator wants a predictable distribution rate.
		VideoBufSize:    24000, // 2-second VBV — cinema cadence is forgiving on burst tolerance.
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    192,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.0",
		MinUploadKbps:   16000,
		RecommendedHW:   []Encoder{EncoderX264, EncoderH264NVENC, EncoderH264VideoToolbox},
		Notes:           "NTSC-cinema 23.976 (24000/1001 — exact, not rounded). Source must be 24p; drop-frame 30p sources will judder.",
	},
	{
		ID:    "cinema-1080p24",
		Tier:  TierCinema,
		Label: "Cinema 1080p 24.000",
		Width: 1920, Height: 1080,
		FPS:             FPS24,
		VideoBitrate:    12000, // Same target as 23.976 — bitrate-per-frame parity.
		VideoMaxBitrate: 14000,
		VideoBufSize:    24000,
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p",
		ColorPrimaries:  "bt709",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt709",
		ColorRange:      "tv",
		AudioBitrate:    192,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "4.0",
		MinUploadKbps:   16000,
		RecommendedHW:   []Encoder{EncoderX264, EncoderH264NVENC, EncoderH264VideoToolbox},
		Notes:           "True 24.000 fps for cinema cameras shooting integer 24. Not interchangeable with 23.976.",
	},
	{
		ID:    "cinema-2160p2398",
		Tier:  TierCinema,
		Label: "Cinema 2160p 23.976 (HEVC)",
		Width: 3840, Height: 2160,
		FPS:             FPS2398,
		VideoBitrate:    40000, // YT 2160p24-30: 20-51 Mbps. Cinema sits near the top because grain at 2160p eats bits.
		VideoMaxBitrate: 48000,
		VideoBufSize:    80000,
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p10le",
		ColorPrimaries:  "bt2020",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt2020nc",
		ColorRange:      "tv",
		AudioBitrate:    192,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "5.1",
		MinUploadKbps:   55000,
		RecommendedHW:   []Encoder{EncoderHEVCNVENC, EncoderHEVCVideoToolbox, EncoderX265},
		Notes:           "2160p 23.976 HEVC cinema. BT.2020 primaries, 10-bit chroma, archive-grade.",
	},
	{
		ID:    "cinema-2160p24",
		Tier:  TierCinema,
		Label: "Cinema 2160p 24.000 (HEVC)",
		Width: 3840, Height: 2160,
		FPS:             FPS24,
		VideoBitrate:    40000,
		VideoMaxBitrate: 48000,
		VideoBufSize:    80000,
		RateControl:     RateControlVBR,
		PixFmt:          "yuv420p10le",
		ColorPrimaries:  "bt2020",
		ColorTransfer:   "bt709",
		ColorSpace:      "bt2020nc",
		ColorRange:      "tv",
		AudioBitrate:    192,
		AudioSampleRate: 48000,
		AudioChannels:   2,
		H264Level:       "5.1",
		MinUploadKbps:   55000,
		RecommendedHW:   []Encoder{EncoderHEVCNVENC, EncoderHEVCVideoToolbox, EncoderX265},
		Notes:           "True 24.000 fps 2160p HEVC. Cinema-camera native rate; not interchangeable with 23.976.",
	},
}

// Default returns the safest mid-tier preset for first-run callers
// that have no operator input yet. Consumer 1080p30 is the right
// choice: works on a Mac mini, hits 1080p, and 30p is forgiving on
// upload variance.
func Default() Preset {
	p, _ := ByID("consumer-1080p30")
	return p
}

// ByID looks up a preset by stable identifier. Returns the zero
// value and false when the id doesn't exist; callers must check the
// bool before consuming the preset.
func ByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// ByTier returns all presets in a tier, in catalog order. Used by
// the UI to render tier-grouped pickers.
func ByTier(tier Tier) []Preset {
	out := make([]Preset, 0, 4)
	for _, p := range Presets {
		if p.Tier == tier {
			out = append(out, p)
		}
	}
	return out
}

// Constraints captures the operator-provided inputs the recommender
// uses to pick a preset. All fields are optional — zero means "no
// constraint on this dimension".
type Constraints struct {
	// UploadKbps is the sustained upload bandwidth the operator can
	// commit to. Zero disables the bandwidth filter (caller is
	// confident or testing locally).
	UploadKbps int
	// AvailableEncoders names the -c:v encoders this build of FFmpeg
	// can actually use. Empty falls back to libx264 only.
	AvailableEncoders []Encoder
	// PreferredTier biases the recommender. Empty picks the highest
	// quality tier the bandwidth supports.
	PreferredTier Tier
	// CinemaCadence requests a cinema-tier preset specifically. The
	// recommender will not silently downgrade out of cinema (a 24p
	// → 30p flip is more disruptive than any bitrate cut).
	CinemaCadence bool
	// PreferHEVC opts into the HEVC presets when the operator has an
	// HEVC-capable encoder and a destination that accepts HEVC.
	PreferHEVC bool
	// TargetFPS biases toward presets at this rate. Use FPS60 to
	// prefer 60p tiers; FPS2398 / FPS24 push the recommender at
	// the cinema cadence tiers.
	TargetFPS Rational
}

// Recommend picks the best-fitting preset for the operator's
// constraints. The algorithm:
//
//  1. Build a candidate set filtered by UploadKbps and
//     AvailableEncoders and (when set) PreferredTier / CinemaCadence.
//  2. If TargetFPS is set, prefer presets matching that rate.
//  3. If PreferHEVC and an HEVC encoder is available, prefer presets
//     whose RecommendedHW lists an HEVC encoder.
//  4. Walk the filtered set in catalog order and return the
//     highest-VideoBitrate preset that fits — bitrate is a reliable
//     proxy for resolution × framerate × quality within a tier.
//
// Returns Default() and false when nothing fits.
func Recommend(c Constraints) (Preset, bool) {
	candidates := make([]Preset, 0, len(Presets))
	for _, p := range Presets {
		if c.UploadKbps > 0 && p.MinUploadKbps > c.UploadKbps {
			continue
		}
		if len(c.AvailableEncoders) > 0 && !hasAnyEncoder(p.RecommendedHW, c.AvailableEncoders) {
			continue
		}
		if c.PreferredTier != "" && p.Tier != c.PreferredTier {
			continue
		}
		if c.CinemaCadence && p.Tier != TierCinema {
			continue
		}
		// When the operator has not asked for cinema, don't silently
		// hand them a cinema-cadence preset. A 24p flip mid-stream is
		// worse than a bitrate cut, and the recommender shouldn't
		// surprise the volunteer operator with film cadence.
		if !c.CinemaCadence && p.Tier == TierCinema {
			continue
		}
		// HEVC filter: skip HEVC presets unless explicitly asked for.
		// Most live destinations still want H.264; HEVC is opt-in.
		if isHEVCPreset(p) && !c.PreferHEVC {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return Default(), false
	}
	// FPS preference: if set, narrow to exact-match first, then
	// fall back to the full candidate set if nothing matched.
	if c.TargetFPS.Num != 0 {
		matched := make([]Preset, 0, len(candidates))
		for _, p := range candidates {
			if p.FPS.Num == c.TargetFPS.Num && p.FPS.Den == c.TargetFPS.Den {
				matched = append(matched, p)
			}
		}
		if len(matched) > 0 {
			candidates = matched
		}
	}
	// Pick highest-quality (bitrate proxy) among candidates.
	best := candidates[0]
	for _, p := range candidates[1:] {
		if p.VideoBitrate > best.VideoBitrate {
			best = p
		}
	}
	return best, true
}

// hasAnyEncoder reports whether any of `want` appears in `have`.
// Order-independent — the supervisor will re-rank by RecommendedHW
// when it actually invokes ffmpeg.
func hasAnyEncoder(want, have []Encoder) bool {
	for _, w := range want {
		for _, h := range have {
			if w == h {
				return true
			}
		}
	}
	return false
}

// isHEVCPreset reports whether a preset's hardware recommendations
// are HEVC-only (HDR / 2160p tiers). Used by the recommender to
// avoid silently flipping into HEVC when the operator hasn't asked.
func isHEVCPreset(p Preset) bool {
	if len(p.RecommendedHW) == 0 {
		return false
	}
	for _, e := range p.RecommendedHW {
		if !strings.HasPrefix(string(e), "hevc_") && e != EncoderX265 {
			return false
		}
	}
	return true
}

// PreferredEncoder picks the first encoder from p.RecommendedHW that
// also appears in `available`. Returns the software fallback (x264
// for H.264 presets, x265 for HEVC presets) when nothing matches.
// Used by the supervisor to bridge the preset's encoder preference
// to whatever the local FFmpeg build actually supports.
func PreferredEncoder(p Preset, available []Encoder) Encoder {
	for _, want := range p.RecommendedHW {
		for _, have := range available {
			if want == have {
				return want
			}
		}
	}
	// Software fallback — HEVC presets to x265, H.264 to x264.
	if isHEVCPreset(p) {
		return EncoderX265
	}
	return EncoderX264
}
