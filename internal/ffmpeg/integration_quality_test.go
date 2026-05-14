// integration_quality_test.go exercises the cross-package wiring
// between internal/quality and internal/ffmpeg.
//
// We declare an external test package (ffmpeg_test) so this file can
// import easycampus/internal/quality without forming an import
// cycle in the production internal/ffmpeg package.
//
// The narrow contract we verify here:
//
//   - quality.FFmpegPreset satisfies the ffmpeg.QualityPreset interface.
//   - quality.FFmpegPreset also satisfies the optional richer
//     interfaces (ColorPresetAware, RateControlAware, BroadcastSafeAware)
//     so the supervisor emits broadcast-grade flags.
//   - Config.Args() produces the expected -color_primaries / -b:v /
//     -profile:v / -level:v / CBR HRD flags when the preset is a
//     consumer-1080p30 wrapped tier preset.
package ffmpeg_test

import (
	"strings"
	"testing"

	"easycampus/internal/ffmpeg"
	"easycampus/internal/quality"
)

// TestFFmpegPresetSatisfiesQualityPreset is a compile-time-style
// guard: the parameter is the interface, so a regression that
// removes a method from FFmpegPreset would fail to compile.
func TestFFmpegPresetSatisfiesQualityPreset(t *testing.T) {
	var _ ffmpeg.QualityPreset = quality.FFmpegPreset{}
	p, ok := quality.ResolveFFmpegPreset("consumer-1080p30")
	if !ok {
		t.Fatal("consumer-1080p30 missing from catalog")
	}
	var qp ffmpeg.QualityPreset = p
	if qp.ID() != "consumer-1080p30" {
		t.Errorf("ID()=%q, want consumer-1080p30", qp.ID())
	}
}

// TestFFmpegPresetSatisfiesColorPresetAware exercises the optional
// interface dispatch. The supervisor type-asserts to this; a missing
// method would mean color VUI flags don't make it into the args.
func TestFFmpegPresetSatisfiesColorPresetAware(t *testing.T) {
	p, _ := quality.ResolveFFmpegPreset("broadcast-2160p30")
	cp, ok := interface{}(p).(ffmpeg.ColorPresetAware)
	if !ok {
		t.Fatal("FFmpegPreset does not satisfy ffmpeg.ColorPresetAware")
	}
	if cp.ColorPrimaries() != "bt2020" {
		t.Errorf("ColorPrimaries=%q, want bt2020", cp.ColorPrimaries())
	}
	if cp.ColorSpace() != "bt2020nc" {
		t.Errorf("ColorSpace=%q, want bt2020nc", cp.ColorSpace())
	}
}

// TestConfigArgsConsumerH264 builds the live args for consumer-1080p30
// on libx264 and verifies the expected color / bitrate / profile /
// level flags appear in argv. This is the end-to-end smoke test the
// task spec calls out.
func TestConfigArgsConsumerH264(t *testing.T) {
	preset, ok := quality.ResolveFFmpegPreset("consumer-1080p30")
	if !ok {
		t.Fatal("consumer-1080p30 missing from catalog")
	}
	cfg := ffmpeg.Config{
		Binary: "ffmpeg",
		Mode:   ffmpeg.ModeLiveHLS,
		Input: ffmpeg.Input{
			Kind:        ffmpeg.InputAVFoundation,
			VideoDevice: "0",
			AudioDevice: "0",
			Framerate:   30,
			VideoSize:   "1920x1080",
		},
		Preset:    preset,
		Encoder:   ffmpeg.EncoderX264,
		HWAccel:   ffmpeg.HWAccelNone,
		OutputDir: "/tmp/test-event",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	joined := strings.Join(args, " ")

	// Color VUI metadata — surfaced because FFmpegPreset implements
	// ColorPresetAware. Order matters: must precede -c:v.
	mustContain(t, joined,
		"-color_primaries bt709",
		"-color_trc bt709",
		"-colorspace bt709",
		"-color_range tv",
	)
	primariesIdx := strings.Index(joined, "-color_primaries")
	cvIdx := strings.Index(joined, "-c:v ")
	if primariesIdx < 0 || cvIdx < 0 {
		t.Fatalf("missing -color_primaries (%d) or -c:v (%d)", primariesIdx, cvIdx)
	}
	if primariesIdx > cvIdx {
		t.Errorf("color VUI must precede -c:v (color@%d, cv@%d)", primariesIdx, cvIdx)
	}

	// Bitrate / profile / level — consumer-1080p30 = 6 Mbps high@4.1.
	mustContain(t, joined,
		"-c:v libx264",
		"-profile:v high",
		"-level:v 4.1",
		"-b:v 6000k",
		"-maxrate 7500k", // consumer VBR ceiling
		"-bufsize 12000k",
		"-pix_fmt yuv420p",
		"-fps_mode cfr",
		"-r 30",
	)
	// Consumer tier is VBR — must NOT declare HRD CBR.
	if strings.Contains(joined, "nal-hrd=cbr") {
		t.Errorf("consumer preset should not declare HRD CBR, got args: %s", joined)
	}
	// Consumer tier opts INTO zerolatency on the live path.
	if !strings.Contains(joined, "-tune zerolatency") {
		t.Errorf("consumer live args missing -tune zerolatency: %s", joined)
	}
	// Consumer tier turns off B-frames.
	if !strings.Contains(joined, "-bf 0") {
		t.Errorf("consumer args missing -bf 0: %s", joined)
	}
}

// TestConfigArgsBroadcastH264CBR builds the live args for the
// broadcast 1080p30 preset on libx264. Broadcast presets satisfy
// RateControlAware so the supervisor emits the nal-hrd=cbr / filler=1
// SPS hints and pins maxrate == bufsize == target (1-second VBV).
func TestConfigArgsBroadcastH264CBR(t *testing.T) {
	preset, ok := quality.ResolveFFmpegPreset("broadcast-1080p30")
	if !ok {
		t.Fatal("broadcast-1080p30 missing from catalog")
	}
	cfg := ffmpeg.Config{
		Binary: "ffmpeg",
		Mode:   ffmpeg.ModeLiveHLS,
		Input: ffmpeg.Input{
			Kind:        ffmpeg.InputAVFoundation,
			VideoDevice: "0",
			AudioDevice: "0",
			Framerate:   30,
			VideoSize:   "1920x1080",
		},
		Preset:    preset,
		Encoder:   ffmpeg.EncoderX264,
		HWAccel:   ffmpeg.HWAccelNone,
		OutputDir: "/tmp/test-event",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-b:v 8000k",
		"-maxrate 8000k", // CBR: maxrate == target
		"-bufsize 8000k", // 1-second VBV
		"-color_primaries bt709",
	)
	// Broadcast x264 must carry the SPS HRD parameters.
	if !strings.Contains(joined, "nal-hrd=cbr") {
		t.Errorf("broadcast x264 missing nal-hrd=cbr: %s", joined)
	}
	if !strings.Contains(joined, "filler=1") {
		t.Errorf("broadcast x264 missing filler=1: %s", joined)
	}
	// BroadcastSafe wires the limiter video filter.
	if !strings.Contains(joined, "limiter=min=16:max=235") {
		t.Errorf("broadcast preset missing -vf limiter (broadcast-safe): %s", joined)
	}
	// Broadcast tier keeps B-frames; must NOT emit -bf 0.
	if strings.Contains(joined, "-bf 0") {
		t.Errorf("broadcast tier should keep B-frames, found -bf 0: %s", joined)
	}
	// Broadcast keeps the encoder default cadence — no -tune zerolatency
	// (that would disable B-frames and tank quality at the same bitrate).
	if strings.Contains(joined, "-tune zerolatency") {
		t.Errorf("broadcast preset should not force zerolatency: %s", joined)
	}
}

// TestConfigArgsBroadcast2160BT2020 verifies the 2160p broadcast
// preset emits BT.2020 primaries / colorspace and the 10-bit pixel
// format. Tagging is load-bearing — downstream players default to
// BT.601 on unspecified UHD.
func TestConfigArgsBroadcast2160BT2020(t *testing.T) {
	preset, ok := quality.ResolveFFmpegPreset("broadcast-2160p30")
	if !ok {
		t.Fatal("broadcast-2160p30 missing from catalog")
	}
	cfg := ffmpeg.Config{
		Binary: "ffmpeg",
		Mode:   ffmpeg.ModeLiveHLS,
		Input: ffmpeg.Input{
			Kind: ffmpeg.InputTest,
		},
		Preset:    preset,
		Encoder:   ffmpeg.EncoderX264, // pin to x264 so we don't depend on host
		HWAccel:   ffmpeg.HWAccelNone,
		OutputDir: "/tmp/test-event",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-color_primaries bt2020",
		"-colorspace bt2020nc",
		"-pix_fmt yuv420p10le",
	)
}

// mustContain is a small helper that asserts every needle appears as
// a contiguous substring of haystack.
func mustContain(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("expected to contain %q in: %s", n, haystack)
		}
	}
}
