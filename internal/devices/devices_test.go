package devices

import (
	"strings"
	"testing"
)

// TestClassifyCaptureCards pins device classification against real device
// names we expect to see in production church / broadcast / cinema setups.
func TestClassifyCaptureCards(t *testing.T) {
	cases := []struct {
		name string
		want DeviceType
	}{
		// SDI / HDMI USB capture (most common in church installs)
		{"Magewell USB Capture HDMI 4K Plus", TypeCaptureCard},
		{"Magewell Pro Capture SDI", TypeCaptureCard},
		{"INOGENI 4K HDMI to USB", TypeCaptureCard},
		{"INOGENI SDI to USB", TypeCaptureCard},
		{"AJA U-TAP HDMI", TypeCaptureCard},
		{"AJA U-TAP SDI", TypeCaptureCard},
		{"Elgato Cam Link 4K", TypeCaptureCard},
		{"Elgato HD60 S+", TypeCaptureCard},
		{"AVerMedia Live Gamer 4K", TypeCaptureCard},
		{"Epiphan AV.io HD", TypeCaptureCard},
		{"YUAN SDI Capture", TypeCaptureCard},
		// Cameras
		{"FaceTime HD Camera", TypeCamera},
		{"Studio Display Camera", TypeCamera},
		{"OBS Virtual Camera", TypeCamera},
		{"Stephen Desk View Camera", TypeCamera},
		{"iPhone (2)", TypeCamera},
		// Screen capture
		{"Capture screen 0", TypeScreen},
	}
	for _, tc := range cases {
		got := classifyVideoDevice(Device{Name: tc.name, Backend: "avfoundation"})
		if got != tc.want {
			t.Errorf("classifyVideoDevice(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestClassifyDeckLinkBackendIsSDI confirms BlackMagic devices come through
// the dedicated SDI backend regardless of name.
func TestClassifyDeckLinkBackendIsSDI(t *testing.T) {
	got := classifyVideoDevice(Device{Name: "DeckLink Mini Recorder HD", Backend: "decklink"})
	if got != TypeSDI {
		t.Errorf("DeckLink should classify as SDI, got %q", got)
	}
}

func TestClassifyNDIBackendIsNDI(t *testing.T) {
	got := classifyVideoDevice(Device{Name: "Studio (CAM1)", Backend: "libndi_newtek"})
	if got != TypeNDI {
		t.Errorf("NDI device should classify as NDI, got %q", got)
	}
}

func TestParseAVFoundationVideoAndAudio(t *testing.T) {
	// FFmpeg writes the listing to stderr; the lines are interspersed with
	// "[AVFoundation indev @ ...]" prefixes. Our parser uses section keywords.
	output := `[AVFoundation indev @ 0x7f8e6e0] AVFoundation video devices:
[AVFoundation indev @ 0x7f8e6e0] [0] FaceTime HD Camera
[AVFoundation indev @ 0x7f8e6e0] [1] Capture screen 0
[AVFoundation indev @ 0x7f8e6e0] AVFoundation audio devices:
[AVFoundation indev @ 0x7f8e6e0] [0] MacBook Pro Microphone
`
	list := parseAVFoundation(output)
	if len(list.Video) != 2 {
		t.Fatalf("video: want 2 got %d (%v)", len(list.Video), list.Video)
	}
	if list.Video[0].Name != "FaceTime HD Camera" {
		t.Errorf("video[0].Name = %q", list.Video[0].Name)
	}
	if list.Video[0].Index != "0" {
		t.Errorf("video[0].Index = %q", list.Video[0].Index)
	}
	if len(list.Audio) != 1 {
		t.Fatalf("audio: want 1 got %d", len(list.Audio))
	}
	if list.Audio[0].Name != "MacBook Pro Microphone" {
		t.Errorf("audio[0].Name = %q", list.Audio[0].Name)
	}
}

func TestParseDeckLink(t *testing.T) {
	// Real libavdevice output: a header line, then one tab-indented
	// quoted name per card, each carrying the av_log prefix.
	output := "[decklink @ 0x7f8e6e0] Blackmagic DeckLink input devices:\n" +
		"[decklink @ 0x7f8e6e0] \t'DeckLink Mini Recorder HD'\n" +
		"[decklink @ 0x7f8e6e0] \t'DeckLink Quad HDMI Recorder (2)'\n" +
		"[in#0 @ 0x600] Error opening input: Immediate exit requested\n" +
		"Error opening input file dummy.\n"
	list := parseDeckLink(output)
	if len(list.Video) != 2 {
		t.Fatalf("want 2 got %d (%v)", len(list.Video), list.Video)
	}
	if list.Video[0].Name != "DeckLink Mini Recorder HD" {
		t.Errorf("got %q", list.Video[0].Name)
	}
	if list.Video[1].Name != "DeckLink Quad HDMI Recorder (2)" {
		t.Errorf("got %q", list.Video[1].Name)
	}
	if list.Video[0].Backend != "decklink" {
		t.Errorf("backend = %q", list.Video[0].Backend)
	}
}

func TestParseDeckLinkIgnoresQuotedLinesWithoutHeader(t *testing.T) {
	output := "[decklink @ 0x1] 'not a device'\n[decklink @ 0x1] Could not create DeckLink iterator\n"
	if list := parseDeckLink(output); len(list.Video) != 0 {
		t.Fatalf("want 0 got %v", list.Video)
	}
}

func TestParseDshowModernIncludingCombinedDevices(t *testing.T) {
	output := `[dshow @ 000001f8] "Integrated Camera" (video)
[dshow @ 000001f8]   Alternative name "@device_pnp_\\?\usb#vid_04f2"
[dshow @ 000001f8] "Microphone (Realtek(R) Audio)" (audio)
[dshow @ 000001f8]   Alternative name "@device_cm_{33D9A762}"
[dshow @ 000001f8] "Decklink Video Capture" (audio, video)
[dshow @ 000001f8] "Some Filter" (none)
`
	list := parseDshow(output)
	video := names(list.Video)
	audio := names(list.Audio)
	if strings.Join(video, "|") != "Integrated Camera|Decklink Video Capture" {
		t.Errorf("video = %v", video)
	}
	if strings.Join(audio, "|") != "Microphone (Realtek(R) Audio)|Decklink Video Capture" {
		t.Errorf("audio = %v", audio)
	}
}

func TestParseDshowLegacySections(t *testing.T) {
	output := `[dshow @ 0x1] DirectShow video devices (some may be both video and audio devices)
[dshow @ 0x1]  "USB Video Device"
[dshow @ 0x1]     Alternative name "@device_pnp_x"
[dshow @ 0x1] DirectShow audio devices
[dshow @ 0x1]  "Line In (Cam Link 4K)"
`
	list := parseDshow(output)
	if v := names(list.Video); len(v) != 1 || v[0] != "USB Video Device" {
		t.Errorf("video = %v", v)
	}
	if a := names(list.Audio); len(a) != 1 || a[0] != "Line In (Cam Link 4K)" {
		t.Errorf("audio = %v", a)
	}
}

func TestParseNDIOnlySourceLines(t *testing.T) {
	output := `[libndi_newtek @ 0x7f] -------------- 2 NDI Sources--------------------
[libndi_newtek @ 0x7f] 'STUDIO (CAM1)'	'192.168.1.20:5961'
[libndi_newtek @ 0x7f] 'STUDIO (Slides)'	'192.168.1.21:5961'
[libndi_newtek @ 0x7f] Error: 'dummy' not found
[in#0 @ 0x7f] Error opening input: Input/output error
`
	list := parseNDI(output)
	if got := strings.Join(names(list.Video), "|"); got != "STUDIO (CAM1)|STUDIO (Slides)" {
		t.Errorf("NDI sources = %q", got)
	}
	// No header: the libndi_newtek log prefix alone must not match.
	if l := parseNDI("[libndi_newtek @ 0x7f] 'x' something\n"); len(l.Video) != 0 {
		t.Errorf("matched without header: %v", l.Video)
	}
}

func names(ds []Device) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

func TestPlatformBackendReturnsKnownValue(t *testing.T) {
	got := PlatformBackend()
	switch got {
	case "avfoundation", "dshow", "v4l2":
		// ok
	default:
		t.Errorf("unexpected platform backend %q", got)
	}
}
