package ffmpeg

import (
	"strings"
	"testing"
)

// preset used across tests; mirrors the broadcast-engineer-supplied
// QualityPreset interface without depending on their (in-progress) tier
// table.
var testPreset = SimplePreset{
	IDValue:       "test-1080p30",
	LabelValue:    "Test 1080p30",
	W:             1920,
	H:             1080,
	FPSInt:        30,
	GOPFrames:     60,
	VBitrate:      "6000k",
	ABitrate:      "128k",
	PixFmt:        "yuv420p",
	ProfileValue:  "high",
	LevelValue:    "4.1",
	EncoderPreset: "veryfast",
	ZeroLatency:   true,
	BFrames:       false,
}

func TestArgsLiveAVFoundation(t *testing.T) {
	cfg := Config{
		Binary: "ffmpeg",
		Mode:   ModeLiveHLS,
		Input: Input{
			Kind:        InputAVFoundation,
			VideoDevice: "0",
			AudioDevice: "0",
			Framerate:   30,
			VideoSize:   "1280x720",
		},
		Preset:    testPreset,
		Encoder:   EncoderX264, // pin so the test doesn't depend on the host
		HWAccel:   HWAccelNone,
		OutputDir: "/tmp/test-event",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-progress pipe:1",
		"-nostats",
		"-y",
		"-thread_queue_size 1024",
		"-f avfoundation",
		"-framerate 30",
		"-video_size 1280x720",
		"-i 0:0",
		"-c:v libx264",
		"-pix_fmt yuv420p",
		"-fps_mode cfr",
		"-r 30",
		`-force_key_frames expr:gte(t,n_forced*2)`,
		"-g 60",
		"-keyint_min 60",
		"-tune zerolatency",
		"-bf 0",
		"-hls_time 2",
		"-hls_segment_type fmp4",
		"-hls_fmp4_init_filename init.mp4",
		"independent_segments",
		"-hls_flags",
		"master.m3u8",
	)
	mustNotContain(t, joined, "delete_segments")
}

func TestArgsLiveAVFoundationDeleteOld(t *testing.T) {
	cfg := Config{
		Binary:       "ffmpeg",
		Mode:         ModeLiveHLS,
		Input:        Input{Kind: InputAVFoundation, VideoDevice: "0", AudioDevice: "0"},
		Preset:       testPreset,
		Encoder:      EncoderX264,
		HWAccel:      HWAccelNone,
		OutputDir:    "/tmp/x",
		HLSDeleteOld: true,
		HLSListSize:  10,
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined, "delete_segments", "-hls_list_size 10")
	mustNotContain(t, joined, "-hls_playlist_type event")
}

func TestArgsUploadHLS(t *testing.T) {
	cfg := Config{
		Binary:    "ffmpeg",
		Mode:      ModeUpload,
		Input:     Input{Kind: InputFile, VideoDevice: "/tmp/input.mov"},
		Preset:    testPreset,
		Encoder:   EncoderX264,
		HWAccel:   HWAccelNone,
		OutputDir: "/tmp/out",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-i /tmp/input.mov",
		"-pix_fmt yuv420p",
		"-hls_segment_type mpegts",
		"-hls_list_size 0",
		"-c:a",
	)
	mustNotContain(t, joined,
		// Upload re-runs are fresh transcodes: appending would
		// duplicate every segment.
		"append_list",
		// Upload conversions should NOT have zerolatency or live-only flags.
		"-tune zerolatency",
		"delete_segments",
		"-realtime",
		"-thread_queue_size",
	)
}

func TestArgsExternalNetworkSourceRTSP(t *testing.T) {
	cfg := Config{
		Binary:    "ffmpeg",
		Mode:      ModeUpload,
		Input:     Input{Kind: InputNetwork, URL: "rtsp://admin:secret@cam.local/stream"},
		Preset:    testPreset,
		Encoder:   EncoderX264,
		HWAccel:   HWAccelNone,
		OutputDir: "/tmp/out",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-rtsp_transport tcp",
		"-fflags discardcorrupt+genpts",
		"-analyzeduration 1000000",
		"-i rtsp://admin:secret@cam.local/stream",
	)
}

func TestArgsExternalNetworkSourceHLS(t *testing.T) {
	cfg := Config{
		Binary:    "ffmpeg",
		Mode:      ModeUpload,
		Input:     Input{Kind: InputNetwork, URL: "https://example.com/live/stream.m3u8"},
		Preset:    testPreset,
		Encoder:   EncoderX264,
		HWAccel:   HWAccelNone,
		OutputDir: "/tmp/out",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-reconnect 1",
		"-reconnect_streamed 1",
		"-reconnect_delay_max 5",
		"-multiple_requests 1",
		"-i https://example.com/live/stream.m3u8",
	)
}

func TestArgsVideoToolboxLive(t *testing.T) {
	cfg := Config{
		Binary:    "ffmpeg",
		Mode:      ModeLiveHLS,
		Input:     Input{Kind: InputAVFoundation, VideoDevice: "0", AudioDevice: "0"},
		Preset:    testPreset,
		Encoder:   EncoderVideoToolbox,
		HWAccel:   HWAccelVideoToolbox,
		OutputDir: "/tmp/x",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-hwaccel videotoolbox",
		"-c:v h264_videotoolbox",
		"-allow_sw 1",
		"-realtime 1",
		"-constant_bit_rate 1",
	)
}

func TestArgsVideoToolboxUploadOmitsRealtime(t *testing.T) {
	cfg := Config{
		Binary:    "ffmpeg",
		Mode:      ModeUpload,
		Input:     Input{Kind: InputFile, VideoDevice: "/tmp/in.mov"},
		Preset:    testPreset,
		Encoder:   EncoderVideoToolbox,
		HWAccel:   HWAccelVideoToolbox,
		OutputDir: "/tmp/x",
	}
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	mustContain(t, joined,
		"-hwaccel videotoolbox",
		"-c:v h264_videotoolbox",
	)
	mustNotContain(t, joined,
		"-realtime 1",
		"-constant_bit_rate 1", // soft CBR only on live; upload uses VBR
	)
}

func TestArgsValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "no binary",
			cfg:     Config{},
			wantErr: "binary",
		},
		{
			name:    "no preset",
			cfg:     Config{Binary: "ffmpeg"},
			wantErr: "preset",
		},
		{
			name: "no output dir",
			cfg: Config{
				Binary: "ffmpeg",
				Mode:   ModeLiveHLS,
				Input:  Input{Kind: InputAVFoundation},
				Preset: testPreset,
			},
			wantErr: "OutputDir",
		},
		{
			name: "upload network with no URL",
			cfg: Config{
				Binary:    "ffmpeg",
				Mode:      ModeUpload,
				Input:     Input{Kind: InputNetwork},
				Preset:    testPreset,
				OutputDir: "/tmp/x",
			},
			wantErr: "URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Args()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestArgsGenPTSOpt(t *testing.T) {
	cfg := Config{
		Binary:              "ffmpeg",
		Mode:                ModeUpload,
		Input:               Input{Kind: InputFile, VideoDevice: "/tmp/i.mp4"},
		Preset:              testPreset,
		Encoder:             EncoderX264,
		HWAccel:             HWAccelNone,
		OutputDir:           "/tmp/x",
		SourceHasMissingPTS: true,
	}
	args, _ := cfg.Args()
	joined := strings.Join(args, " ")
	mustContain(t, joined, "-fflags +genpts")
}

func TestArgsRealtimeFlag(t *testing.T) {
	cfg := Config{
		Binary:    "ffmpeg",
		Mode:      ModeUpload,
		Input:     Input{Kind: InputFile, VideoDevice: "/tmp/i.mp4"},
		Preset:    testPreset,
		Encoder:   EncoderX264,
		HWAccel:   HWAccelNone,
		OutputDir: "/tmp/x",
		Realtime:  true,
	}
	args, _ := cfg.Args()
	joined := strings.Join(args, " ")
	mustContain(t, joined, "-re -i /tmp/i.mp4")
}

func TestArgsDisableAudio(t *testing.T) {
	cfg := Config{
		Binary:       "ffmpeg",
		Mode:         ModeLiveHLS,
		Input:        Input{Kind: InputAVFoundation, VideoDevice: "0"},
		Preset:       testPreset,
		Encoder:      EncoderX264,
		HWAccel:      HWAccelNone,
		OutputDir:    "/tmp/x",
		DisableAudio: true,
	}
	args, _ := cfg.Args()
	joined := strings.Join(args, " ")
	mustContain(t, joined, "-i 0:none", "-an")
	mustNotContain(t, joined, "-c:a")
}

func TestArgsLogLevelDefault(t *testing.T) {
	cfg := Config{
		Binary:    "ffmpeg",
		Mode:      ModeLiveHLS,
		Input:     Input{Kind: InputAVFoundation, VideoDevice: "0", AudioDevice: "0"},
		Preset:    testPreset,
		Encoder:   EncoderX264,
		HWAccel:   HWAccelNone,
		OutputDir: "/tmp/x",
	}
	args, _ := cfg.Args()
	joined := strings.Join(args, " ")
	mustContain(t, joined, "-loglevel error")
}

func TestPickEncoderPlatformPreferences(t *testing.T) {
	// Simulate macOS with VideoToolbox available.
	allAvail := []EncoderInfo{
		{ID: EncoderX264, Available: true},
		{ID: EncoderVideoToolbox, Available: true},
		{ID: EncoderNVENC, Available: true},
		{ID: EncoderQSV, Available: true},
		{ID: EncoderVAAPI, Available: true},
	}
	got := PickEncoder(allAvail)
	// We don't pin to runtime.GOOS here, but we expect a hardware encoder
	// to win over libx264 when one is available.
	if got == EncoderX264 {
		t.Errorf("PickEncoder should prefer HW over x264 when available, got %v", got)
	}
}

func TestPickEncoderFallsBackToX264(t *testing.T) {
	got := PickEncoder([]EncoderInfo{{ID: EncoderX264, Available: true}})
	if got != EncoderX264 {
		t.Errorf("expected x264 fallback, got %v", got)
	}
}

func TestPickHWAccelMatchesEncoder(t *testing.T) {
	avail := []HWAccel{HWAccelVideoToolbox, HWAccelCUDA, HWAccelVAAPI, HWAccelQSV}
	cases := []struct {
		encoder Encoder
		want    HWAccel
	}{
		{EncoderVideoToolbox, HWAccelVideoToolbox},
		{EncoderNVENC, HWAccelCUDA},
		{EncoderVAAPI, HWAccelVAAPI},
		{EncoderQSV, HWAccelQSV},
	}
	for _, tc := range cases {
		got := PickHWAccel(avail, tc.encoder)
		if got != tc.want {
			t.Errorf("PickHWAccel(%v) = %v, want %v", tc.encoder, got, tc.want)
		}
	}
}

func TestPickHWAccelNoneAvailable(t *testing.T) {
	got := PickHWAccel(nil, EncoderX264)
	if got != HWAccelNone {
		t.Errorf("expected HWAccelNone with no hwaccels, got %v", got)
	}
}

func TestNetworkInputFlagsScheme(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want []string
	}{
		{"rtsp", "rtsp://cam.local/stream", []string{"-rtsp_transport", "tcp"}},
		{"rtsps", "rtsps://cam.local/stream", []string{"-rtsp_transport", "tcp"}},
		{"udp", "udp://239.0.0.1:1234", []string{"-fifo_size", "1000000", "-overrun_nonfatal", "1"}},
		{"http", "http://example.com/stream.m3u8", []string{"-reconnect", "1", "-multiple_requests", "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := networkInputFlags(tc.url)
			joined := strings.Join(args, " ")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("missing %q in %q", w, joined)
				}
			}
		})
	}
}

// --- helpers ---

func mustContain(t *testing.T, joined string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(joined, n) {
			t.Errorf("missing %q in args:\n%s", n, joined)
		}
	}
}

func mustNotContain(t *testing.T, joined string, banned ...string) {
	t.Helper()
	for _, b := range banned {
		if strings.Contains(joined, b) {
			t.Errorf("unexpected %q in args:\n%s", b, joined)
		}
	}
}
