package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// richPreset satisfies every optional capability interface, mirroring
// quality.FFmpegPreset without importing it.
type richPreset struct {
	SimplePreset
	primaries, trc, space, rng string
	broadcastSafe              bool
	hevc                       bool
	rc                         string
}

func (p richPreset) ColorPrimaries() string  { return p.primaries }
func (p richPreset) ColorTransfer() string   { return p.trc }
func (p richPreset) ColorSpace() string      { return p.space }
func (p richPreset) ColorRange() string      { return p.rng }
func (p richPreset) BroadcastSafe() bool     { return p.broadcastSafe }
func (p richPreset) PrefersHEVC() bool       { return p.hevc }
func (p richPreset) RateControlMode() string { return p.rc }
func (p richPreset) VideoMaxBitrate() string { return p.VBitrate }
func (p richPreset) VideoBufSize() string    { return p.VBitrate }

func uhdPreset() richPreset {
	return richPreset{
		SimplePreset: SimplePreset{
			IDValue: "test-2160p30", W: 3840, H: 2160, FPSInt: 30, GOPFrames: 60,
			VBitrate: "35000k", ABitrate: "192k", PixFmt: "yuv420p10le",
			ProfileValue: "main10", LevelValue: "5.1", BFrames: true,
		},
		primaries: "bt2020", trc: "bt709", space: "bt2020nc", rng: "tv",
		broadcastSafe: true, hevc: true, rc: "cbr",
	}
}

func hdBroadcastPreset() richPreset {
	return richPreset{
		SimplePreset: SimplePreset{
			IDValue: "test-bcast-1080p30", W: 1920, H: 1080, FPSInt: 30, GOPFrames: 60,
			VBitrate: "8000k", ABitrate: "192k", PixFmt: "yuv420p",
			ProfileValue: "high", LevelValue: "4.1", BFrames: true,
		},
		primaries: "bt709", trc: "bt709", space: "bt709", rng: "tv",
		broadcastSafe: true, rc: "cbr",
	}
}

func liveCfg(p QualityPreset, enc Encoder) Config {
	return Config{
		Binary:    "ffmpeg",
		Mode:      ModeLiveHLS,
		Input:     Input{Kind: InputTest},
		Preset:    p,
		Encoder:   enc,
		HWAccel:   HWAccelNone,
		OutputDir: "/tmp/x",
	}
}

func mustArgs(t *testing.T, c Config) (string, []string) {
	t.Helper()
	args, err := c.Args()
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	return strings.Join(args, " "), args
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func countFlag(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

func TestArgsScaleChainFromPreset(t *testing.T) {
	_, args := mustArgs(t, liveCfg(testPreset, EncoderX264))
	if countFlag(args, "-vf") != 1 {
		t.Fatalf("want exactly one -vf, args=%v", args)
	}
	if got := argAfter(args, "-vf"); got != "scale=1920:1080,format=yuv420p" {
		t.Errorf("-vf = %q", got)
	}
}

func TestArgsColorSetparamsAndLimiter8bit(t *testing.T) {
	_, args := mustArgs(t, liveCfg(hdBroadcastPreset(), EncoderX264))
	want := "scale=1920:1080:out_range=tv,format=yuv420p," +
		"setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv," +
		"limiter=min=16:max=235:planes=1,limiter=min=16:max=240:planes=6"
	if got := argAfter(args, "-vf"); got != want {
		t.Errorf("-vf =\n %q\nwant\n %q", got, want)
	}
	// Output-side flags are still emitted for the SPS/VUI.
	joined := strings.Join(args, " ")
	mustContain(t, joined, "-color_primaries bt709", "-color_trc bt709", "-colorspace bt709", "-color_range tv")
}

func TestArgsHEVCPromotionPerFamily(t *testing.T) {
	restore := StubEncoderProbe(func(string) bool { return true })
	defer restore()
	cases := []struct {
		family      Encoder
		codec       string
		pixFmt      string
		extra       []string
		bannedExtra []string
	}{
		{EncoderX264, "libx265", "yuv420p10le", []string{"-x265-params keyint=60:min-keyint=60:scenecut=0:open-gop=0"}, []string{"-level:v"}},
		{EncoderVideoToolbox, "hevc_videotoolbox", "p010le", []string{"-realtime 1", "-constant_bit_rate 1"}, []string{"-level:v"}},
		{EncoderNVENC, "hevc_nvenc", "p010le", []string{"-rc cbr", "-strict_gop 1"}, []string{"-level:v"}},
	}
	for _, c := range cases {
		t.Run(string(c.family), func(t *testing.T) {
			joined, args := mustArgs(t, liveCfg(uhdPreset(), c.family))
			mustContain(t, joined,
				"-c:v "+c.codec,
				"-profile:v main10",
				"-tag:v hvc1",
				"-pix_fmt "+c.pixFmt,
				"-hls_segment_type fmp4",
			)
			mustContain(t, joined, c.extra...)
			mustNotContain(t, joined, c.bannedExtra...)
			vf := argAfter(args, "-vf")
			if !strings.Contains(vf, "format=yuv420p10le") ||
				!strings.Contains(vf, "limiter=min=64:max=940:planes=1") ||
				!strings.Contains(vf, "limiter=min=64:max=960:planes=6") {
				t.Errorf("10-bit chain wrong: %q", vf)
			}
		})
	}
}

func TestArgsHEVCFallbackToH264When8BitUnavailable(t *testing.T) {
	restore := StubEncoderProbe(func(string) bool { return false })
	defer restore()
	var logged []string
	warnedMu.Lock()
	prevWarn := warnf
	warnf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	warned = map[string]bool{}
	warnedMu.Unlock()
	defer func() { warnedMu.Lock(); warnf = prevWarn; warnedMu.Unlock() }()

	for _, fam := range []Encoder{EncoderX264, EncoderVideoToolbox, EncoderNVENC, EncoderQSV} {
		joined, args := mustArgs(t, liveCfg(uhdPreset(), fam))
		mustContain(t, joined, "-c:v "+string(fam), "-profile:v high", "-pix_fmt yuv420p ")
		mustNotContain(t, joined, "main10", "hvc1", "yuv420p10le", "p010le")
		if vf := argAfter(args, "-vf"); !strings.Contains(vf, "limiter=min=16:max=235:planes=1") {
			t.Errorf("%s: 8-bit fallback should use 8-bit limiter: %q", fam, vf)
		}
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "falling back to 8-bit H.264") {
		t.Errorf("expected fallback warning, got %v", logged)
	}
	// Deduped: a second Args() call doesn't re-log.
	n := len(logged)
	mustArgs(t, liveCfg(uhdPreset(), EncoderX264))
	if len(logged) != n {
		t.Errorf("fallback warning should be logged once per message")
	}
}

func TestArgsVAAPIUploadsAndDropsPixFmt(t *testing.T) {
	joined, args := mustArgs(t, liveCfg(hdBroadcastPreset(), EncoderVAAPI))
	vf := argAfter(args, "-vf")
	if !strings.HasSuffix(vf, ",format=nv12,hwupload") {
		t.Errorf("VAAPI chain must end in format=nv12,hwupload: %q", vf)
	}
	if strings.Contains(joined, "-pix_fmt") {
		t.Errorf("VAAPI must not pass -pix_fmt: %s", joined)
	}
	if strings.Index(joined, "-vaapi_device") > strings.Index(joined, " -i ") {
		t.Errorf("-vaapi_device must precede inputs: %s", joined)
	}
	mustContain(t, joined, "-c:v h264_vaapi")
}

func TestNVENCPresetTranslation(t *testing.T) {
	cases := map[string]string{
		"": "p4", "ultrafast": "p1", "superfast": "p2", "veryfast": "p3", "faster": "p3",
		"fast": "p4", "medium": "p5", "slow": "p6", "slower": "p7", "veryslow": "p7",
		"p2": "p2", "P7": "p7", "bogus": "p4",
	}
	for in, want := range cases {
		if got := nvencPreset(in); got != want {
			t.Errorf("nvencPreset(%q)=%q want %q", in, got, want)
		}
	}
	joined, _ := mustArgs(t, liveCfg(testPreset, EncoderNVENC)) // testPreset uses "veryfast"
	mustContain(t, joined, "-c:v h264_nvenc -preset p3")
	mustNotContain(t, joined, "-preset veryfast")

	if got := qsvPreset("ultrafast"); got != "veryfast" {
		t.Errorf("qsvPreset(ultrafast)=%q", got)
	}
	if got := qsvPreset("slow"); got != "slow" {
		t.Errorf("qsvPreset(slow)=%q", got)
	}
}

func TestArgsLiveAppendOnRestart(t *testing.T) {
	c := liveCfg(testPreset, EncoderX264)
	joined, _ := mustArgs(t, c)
	mustNotContain(t, joined, "append_list", "discont_start")

	c.HLSAppend = true
	c.HLSInitFile = RunInitFilename("", 2)
	_, args := mustArgs(t, c)
	if got := argAfter(args, "-hls_flags"); got != "independent_segments+temp_file+append_list+discont_start" {
		t.Errorf("-hls_flags=%q", got)
	}
	if got := argAfter(args, "-hls_fmp4_init_filename"); got != "init-r2.mp4" {
		t.Errorf("init filename=%q", got)
	}

	// Uploads never append, even if a caller sets the flag.
	u := Config{Binary: "ffmpeg", Mode: ModeUpload, Input: Input{Kind: InputFile, VideoDevice: "/tmp/i.mp4"},
		Preset: testPreset, Encoder: EncoderX264, OutputDir: "/tmp/o", HLSAppend: true}
	joined, _ = mustArgs(t, u)
	mustNotContain(t, joined, "append_list")
}

func TestArgsCaptureSizeFromPreset(t *testing.T) {
	c := liveCfg(testPreset, EncoderX264)
	c.Input = Input{Kind: InputAVFoundation, VideoDevice: "0"}
	joined, _ := mustArgs(t, c)
	mustContain(t, joined, "-video_size 1920x1080", "-framerate 30")

	c.Input = Input{Kind: InputV4L2, VideoDevice: "/dev/video2", Framerate: 60, InputFormat: "mjpeg"}
	joined, _ = mustArgs(t, c)
	mustContain(t, joined, "-f v4l2 -input_format mjpeg -framerate 60 -video_size 1920x1080 -i /dev/video2")

	c.Input = Input{Kind: InputV4L2, VideoSize: "1280x720"}
	joined, _ = mustArgs(t, c)
	mustContain(t, joined, "-f v4l2 -video_size 1280x720 -i /dev/video0")
	mustNotContain(t, joined, "-input_format", "-framerate")
}

func TestArgsDShowInputSpec(t *testing.T) {
	cases := []struct {
		in    Input
		noAud bool
		want  string
	}{
		{Input{Kind: InputDShow, VideoDevice: "Cam Link 4K", AudioDevice: "Microphone (Cam Link 4K)"}, false,
			"-f dshow -i video=Cam Link 4K:audio=Microphone (Cam Link 4K)"},
		{Input{Kind: InputDShow, VideoDevice: "video=Integrated Camera"}, false, "-f dshow -i video=Integrated Camera"},
		{Input{Kind: InputDShow, VideoDevice: "Cam", AudioDevice: "audio=Mic"}, false, "-i video=Cam:audio=Mic"},
		{Input{Kind: InputDShow, VideoDevice: "Cam", AudioDevice: "Mic"}, true, "-i video=Cam -map"},
		{Input{Kind: InputDShow, VideoDevice: "Cam", VideoSize: "1920x1080", Framerate: 30}, false,
			"-f dshow -video_size 1920x1080 -framerate 30 -i video=Cam"},
	}
	for _, c := range cases {
		cfg := liveCfg(testPreset, EncoderX264)
		cfg.Input = c.in
		cfg.DisableAudio = c.noAud
		joined, _ := mustArgs(t, cfg)
		mustContain(t, joined, c.want)
	}
}

// writeFakeFFmpeg writes a shell script standing in for ffmpeg and
// returns its path. Unix only.
func writeFakeFFmpeg(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake ffmpeg is unix-only")
	}
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDetectEncodersProbesListedAndCaches(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	bin := writeFakeFFmpeg(t, fmt.Sprintf(`echo x >> %q
cat <<'EOF'
Encoders:
 V..... = Video
 ------
 V....D libx264              libx264 H.264
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V....D h264_videotoolbox    VideoToolbox H.264 Encoder (codec h264)
EOF
`, counter))
	var (
		probeMu sync.Mutex
		probed  []string
	)
	restore := StubEncoderProbe(func(codec string) bool {
		probeMu.Lock()
		probed = append(probed, codec)
		probeMu.Unlock()
		return codec == "h264_videotoolbox" // nvenc is listed but no GPU
	})
	defer restore()

	got := DetectEncoders(bin)
	avail := map[Encoder]bool{}
	for _, e := range got {
		avail[e.ID] = e.Available
	}
	if avail[EncoderNVENC] {
		t.Error("h264_nvenc listed but failed probe must be unavailable")
	}
	if !avail[EncoderVideoToolbox] || !avail[EncoderX264] {
		t.Errorf("availability wrong: %+v", got)
	}
	if avail[EncoderQSV] || avail[EncoderVAAPI] {
		t.Errorf("unlisted encoders must not be probed/available: %+v", got)
	}
	_ = DetectEncoders(bin)
	_ = Config{Binary: bin, Encoder: EncoderAuto}.EffectiveEncoder()
	data, _ := os.ReadFile(counter)
	if n := strings.Count(string(data), "x"); n != 1 {
		t.Errorf("ffmpeg -encoders spawned %d times, want 1 (cached)", n)
	}
	if len(probed) != 2 {
		t.Errorf("probes run %v, want exactly nvenc + videotoolbox once", probed)
	}
}

func TestPickEncoderSkipsUnusableNVENC(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("NVENC preference applies to linux/windows")
	}
	got := PickEncoder([]EncoderInfo{{ID: EncoderX264, Available: true}, {ID: EncoderNVENC, Available: false}})
	if got != EncoderX264 {
		t.Errorf("got %v", got)
	}
}
