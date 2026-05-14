package devices

import "testing"

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
	output := `[decklink @ 0x7f8e6e0] Blackmagic DeckLink SDK
[decklink @ 0x7f8e6e0] 'DeckLink Mini Recorder HD'
[decklink @ 0x7f8e6e0] 'DeckLink Quad HDMI Recorder (2)'
`
	list := parseDeckLink(output)
	if len(list.Video) != 2 {
		t.Fatalf("want 2 got %d", len(list.Video))
	}
	if list.Video[0].Name != "DeckLink Mini Recorder HD" {
		t.Errorf("got %q", list.Video[0].Name)
	}
	if list.Video[0].Backend != "decklink" {
		t.Errorf("backend = %q", list.Video[0].Backend)
	}
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
