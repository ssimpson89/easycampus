package ffmpeg

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Encoder + hwaccel detection
// -----------------------------------------------------------------------------

// Encoder selects which video encoder FFmpeg uses. The H.264 constants
// name a hardware/software *backend family*; HEVC presets (see
// HEVCAware) are mapped to the matching HEVC encoder of the same family
// at args-build time (h264_videotoolbox -> hevc_videotoolbox,
// h264_nvenc -> hevc_nvenc, libx264 -> libx265).
type Encoder string

const (
	EncoderAuto         Encoder = ""                  // pick the best available
	EncoderX264         Encoder = "libx264"           // Software (CPU)
	EncoderVideoToolbox Encoder = "h264_videotoolbox" // macOS hardware
	EncoderNVENC        Encoder = "h264_nvenc"        // NVIDIA GPU
	EncoderVAAPI        Encoder = "h264_vaapi"        // Linux Intel/AMD GPU
	EncoderQSV          Encoder = "h264_qsv"          // Intel QuickSync

	// HEVC encoders. Callers normally keep Config.Encoder on the H.264
	// family constant and let an HEVCAware preset promote it; pinning
	// one of these directly also works and implies HEVC output.
	EncoderX265             Encoder = "libx265"
	EncoderHEVCVideoToolbox Encoder = "hevc_videotoolbox"
	EncoderHEVCNVENC        Encoder = "hevc_nvenc"
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

// encoderProbeTimeout bounds one trial encode. Hardware encoders that
// are listed but unusable (h264_nvenc without a GPU) fail in well under
// a second; the bound only matters for a wedged driver.
const encoderProbeTimeout = 8 * time.Second

// vaapiRenderNode is the DRM render node the VAAPI paths open.
const vaapiRenderNode = "/dev/dri/renderD128"

// Detection caches. Encoder/hwaccel availability does not change while
// the process runs (short of a driver install), and Config.Args() is on
// the restart path, so every probe is done at most once per
// (binary, codec) for the process lifetime.
var (
	detectMu       sync.Mutex
	encoderListing = map[string][]EncoderInfo{} // binary -> DetectEncoders result
	hwaccelListing = map[string][]HWAccel{}     // binary -> DetectHWAccels result
	encoderProbes  = map[string]*encoderProbe{} // binary+"\x00"+codec -> probe

	// probeEncoderFunc runs one trial encode. Replaced in tests so the
	// args builder is deterministic without an ffmpeg binary.
	probeEncoderFunc = runEncoderProbe
)

type encoderProbe struct {
	once sync.Once
	ok   bool
}

// ProbeEncoder reports whether `codec` can actually encode a frame with
// this ffmpeg binary. `ffmpeg -encoders` lists encoders that are merely
// compiled in (h264_nvenc appears in most distro builds with no NVIDIA
// GPU present), so availability is confirmed with a tiny trial encode:
//
//	ffmpeg -f lavfi -i nullsrc=s=256x256:d=0.1 -frames:v 1 -c:v <codec> -f null -
//
// The result is cached for the lifetime of the process.
func ProbeEncoder(binary, codec string) bool {
	if binary == "" {
		binary = "ffmpeg"
	}
	if codec == "" {
		return false
	}
	key := binary + "\x00" + codec
	detectMu.Lock()
	p, ok := encoderProbes[key]
	if !ok {
		p = &encoderProbe{}
		encoderProbes[key] = p
	}
	fn := probeEncoderFunc
	detectMu.Unlock()
	p.once.Do(func() { p.ok = fn(binary, codec) })
	return p.ok
}

// runEncoderProbe is the real trial encode. 256x256 rather than 64x64
// because NVENC/QSV reject very small frames (min width is >128 on
// some generations), which would produce a false negative.
func runEncoderProbe(binary, codec string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), encoderProbeTimeout)
	defer cancel()
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	if strings.HasSuffix(codec, "_vaapi") {
		// VAAPI encoders only accept hardware frames.
		args = append(args, "-vaapi_device", vaapiRenderNode)
	}
	args = append(args, "-f", "lavfi", "-i", "nullsrc=s=256x256:d=0.1", "-frames:v", "1")
	if strings.HasSuffix(codec, "_vaapi") {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	args = append(args, "-c:v", codec, "-f", "null", "-")
	cmd := exec.CommandContext(ctx, binary, args...)
	return cmd.Run() == nil
}

// DetectEncoders lists the H.264 encoder families this ffmpeg build can
// use. An encoder is Available only when it is both listed by
// `ffmpeg -encoders` AND passes ProbeEncoder. libx264 is always
// reported available (it is the universal fallback). Results are cached
// per binary for the process lifetime; a failed listing (binary missing)
// is not cached so a later install is picked up.
func DetectEncoders(binary string) []EncoderInfo {
	if binary == "" {
		binary = "ffmpeg"
	}
	detectMu.Lock()
	if cached, ok := encoderListing[binary]; ok {
		detectMu.Unlock()
		return append([]EncoderInfo(nil), cached...)
	}
	detectMu.Unlock()

	result := make([]EncoderInfo, len(knownEncoders))
	copy(result, knownEncoders)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return result
	}
	listed := listedEncoders(string(out))

	var wg sync.WaitGroup
	for i := range result {
		if result[i].ID == EncoderX264 {
			result[i].Available = true
			continue
		}
		if !listed[string(result[i].ID)] {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result[i].Available = ProbeEncoder(binary, string(result[i].ID))
		}(i)
	}
	wg.Wait()

	detectMu.Lock()
	encoderListing[binary] = append([]EncoderInfo(nil), result...)
	detectMu.Unlock()
	return result
}

// listedEncoders parses `ffmpeg -encoders` output into a set of encoder
// names. Lines look like " V....D h264_nvenc   NVIDIA NVENC H.264 encoder".
func listedEncoders(output string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// First field is the capability flags column (e.g. "V....D").
		if len(fields[0]) != 6 {
			continue
		}
		names[fields[1]] = true
	}
	return names
}

// DetectHWAccels probes `ffmpeg -hwaccels` for usable input acceleration.
// Cached per binary for the process lifetime (failures are not cached).
func DetectHWAccels(binary string) []HWAccel {
	if binary == "" {
		binary = "ffmpeg"
	}
	detectMu.Lock()
	if cached, ok := hwaccelListing[binary]; ok {
		detectMu.Unlock()
		return append([]HWAccel(nil), cached...)
	}
	detectMu.Unlock()

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
	detectMu.Lock()
	hwaccelListing[binary] = append([]HWAccel(nil), avail...)
	detectMu.Unlock()
	return avail
}

// resetDetectionCache clears every detection cache. Test-only.
func resetDetectionCache() {
	detectMu.Lock()
	encoderListing = map[string][]EncoderInfo{}
	hwaccelListing = map[string][]HWAccel{}
	encoderProbes = map[string]*encoderProbe{}
	detectMu.Unlock()
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
	switch encoderFamily(encoder) {
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

// encoderFamily maps an HEVC encoder back to its H.264 backend family
// (hevc_nvenc -> h264_nvenc). H.264 encoders map to themselves.
func encoderFamily(e Encoder) Encoder {
	switch e {
	case EncoderX265:
		return EncoderX264
	case EncoderHEVCVideoToolbox:
		return EncoderVideoToolbox
	case EncoderHEVCNVENC:
		return EncoderNVENC
	}
	return e
}

// hevcSibling returns the HEVC encoder for a backend family, or "" when
// we have no HEVC path for it (VAAPI / QSV fall back to H.264).
func hevcSibling(family Encoder) Encoder {
	switch family {
	case EncoderX264:
		return EncoderX265
	case EncoderVideoToolbox:
		return EncoderHEVCVideoToolbox
	case EncoderNVENC:
		return EncoderHEVCNVENC
	}
	return ""
}

func isHEVCEncoder(e Encoder) bool {
	return e == EncoderX265 || e == EncoderHEVCVideoToolbox || e == EncoderHEVCNVENC
}
