package app

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

func envMap(m map[string]string) envLookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDefaultsCreateTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	s := loadConfig(quietLogger(), path, envMap(nil))

	if s.Port != defaultPort || s.NodeType != "decoder" || s.LogLevel != "error" {
		t.Fatalf("unexpected defaults: port=%d node=%q log=%q", s.Port, s.NodeType, s.LogLevel)
	}
	if s.BaseDir != dir {
		t.Errorf("BaseDir=%q want %q", s.BaseDir, dir)
	}
	if want := filepath.Join(dir, "decoder", "events"); s.EventsDir != want {
		t.Errorf("EventsDir=%q want %q", s.EventsDir, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("template not written: %v", err)
	}
	text := string(data)
	for _, want := range []string{"# --- General Settings ---", "allowed_origins:", "node_type: decoder"} {
		if !strings.Contains(text, want) {
			t.Errorf("template missing %q:\n%s", want, text)
		}
	}
	// Derived paths must not be persisted.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("template does not parse: %v", err)
	}
	if raw["events_dir"] != "" || raw["cache_dir"] != "" || raw["base_dir"] != "" {
		t.Errorf("template persisted derived paths: %v %v %v", raw["base_dir"], raw["events_dir"], raw["cache_dir"])
	}
}

func TestConfigFileAndEnvOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "node_type: encoder\nport: 9000\nhls_list_size: 6\nallowed_origins: [\"http://a.local\"]\n")
	s := loadConfig(quietLogger(), path, envMap(map[string]string{
		"EASYCAMPUS_PORT":            "9100",
		"EASYCAMPUS_NODE_TYPE":       "  Decoder ",
		"EASYCAMPUS_ALLOWED_ORIGINS": "http://b.local , http://c.local",
		"EASYCAMPUS_HLS_DELETE_OLD":  "true",
	}))
	if s.Port != 9100 {
		t.Errorf("Port=%d want 9100", s.Port)
	}
	if s.NodeType != "decoder" {
		t.Errorf("NodeType=%q want decoder (trimmed/lowercased)", s.NodeType)
	}
	if s.HLSListSize != 6 || !s.HLSDeleteOld {
		t.Errorf("HLSListSize=%d HLSDeleteOld=%v", s.HLSListSize, s.HLSDeleteOld)
	}
	if want := []string{"http://b.local", "http://c.local"}; !reflect.DeepEqual(s.AllowedOrigins, want) {
		t.Errorf("AllowedOrigins=%v want %v", s.AllowedOrigins, want)
	}
}

func TestConfigInvalidEnvWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	logger, buf := captureLogger()
	s := loadConfig(logger, path, envMap(map[string]string{
		"EASYCAMPUS_PORT":            "eighty",
		"EASYCAMPUS_DISABLE_AUDIO":   "nope",
		"EASYCAMPUS_HLS_LIST_SIZE":   "1.5",
		"EASYCAMPUS_DEFAULT_BITRATE": "4M",
	}))
	if s.Port != defaultPort || s.DisableAudio || s.HLSListSize != defaultHLSListSize || s.DefaultBitrate != defaultBitrateBPS {
		t.Errorf("invalid env values should be ignored: %+v", s)
	}
	for _, key := range []string{"EASYCAMPUS_PORT", "EASYCAMPUS_DISABLE_AUDIO", "EASYCAMPUS_HLS_LIST_SIZE", "EASYCAMPUS_DEFAULT_BITRATE"} {
		if !strings.Contains(buf.String(), key) {
			t.Errorf("no warning logged for %s; log:\n%s", key, buf.String())
		}
	}
}

func TestConfigValidationRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		check func(AppSettings) bool
	}{
		{"port zero", map[string]string{"EASYCAMPUS_PORT": "0"}, func(s AppSettings) bool { return s.Port == defaultPort }},
		{"port high", map[string]string{"EASYCAMPUS_PORT": "70000"}, func(s AppSettings) bool { return s.Port == defaultPort }},
		{"port ok", map[string]string{"EASYCAMPUS_PORT": "65535"}, func(s AppSettings) bool { return s.Port == 65535 }},
		{"list size", map[string]string{"EASYCAMPUS_HLS_LIST_SIZE": "0"}, func(s AppSettings) bool { return s.HLSListSize == defaultHLSListSize }},
		{"segment low", map[string]string{"EASYCAMPUS_HLS_SEGMENT_DURATION": "0"}, func(s AppSettings) bool { return s.HLSSegmentDuration == defaultHLSSegmentDuration }},
		{"segment high", map[string]string{"EASYCAMPUS_HLS_SEGMENT_DURATION": "11"}, func(s AppSettings) bool { return s.HLSSegmentDuration == defaultHLSSegmentDuration }},
		{"segment ok", map[string]string{"EASYCAMPUS_HLS_SEGMENT_DURATION": "10"}, func(s AppSettings) bool { return s.HLSSegmentDuration == 10 }},
		{"bitrate kbps typo", map[string]string{"EASYCAMPUS_DEFAULT_BITRATE": "4000"}, func(s AppSettings) bool { return s.DefaultBitrate == defaultBitrateBPS }},
		{"bitrate ok", map[string]string{"EASYCAMPUS_DEFAULT_BITRATE": "100000"}, func(s AppSettings) bool { return s.DefaultBitrate == 100000 }},
		{"bad node type", map[string]string{"EASYCAMPUS_NODE_TYPE": "relay"}, func(s AppSettings) bool { return s.NodeType == "decoder" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := loadConfig(quietLogger(), filepath.Join(t.TempDir(), "config.yaml"), envMap(tc.env))
			if !tc.check(s) {
				t.Errorf("unexpected settings: %+v", s)
			}
		})
	}
}

func TestConfigValidationAppliesToFileValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "port: -1\nhls_segment_duration: 30\n")
	s := loadConfig(quietLogger(), path, envMap(nil))
	if s.Port != defaultPort || s.HLSSegmentDuration != defaultHLSSegmentDuration {
		t.Errorf("file values not validated: port=%d seg=%d", s.Port, s.HLSSegmentDuration)
	}
}

func TestConfigUnknownCatalogIDsWarn(t *testing.T) {
	logger, buf := captureLogger()
	s := loadConfig(logger, filepath.Join(t.TempDir(), "config.yaml"), envMap(map[string]string{
		"EASYCAMPUS_QUALITY_PRESET":   "consumer-9000p",
		"EASYCAMPUS_PLAYBACK_PROFILE": "hologram",
	}))
	if !strings.Contains(buf.String(), "unknown quality_preset") || !strings.Contains(buf.String(), "unknown playback_profile") {
		t.Errorf("missing catalog warnings:\n%s", buf.String())
	}
	// Kept as-is; the runtime falls through on unknown IDs.
	if s.QualityPreset != "consumer-9000p" || s.PlaybackProfile != "hologram" {
		t.Errorf("catalog IDs should be preserved: %q %q", s.QualityPreset, s.PlaybackProfile)
	}

	logger, buf = captureLogger()
	loadConfig(logger, filepath.Join(t.TempDir(), "config.yaml"), envMap(map[string]string{
		"EASYCAMPUS_QUALITY_PRESET":   "consumer-1080p30",
		"EASYCAMPUS_PLAYBACK_PROFILE": "projector-30",
	}))
	if strings.Contains(buf.String(), "unknown") {
		t.Errorf("valid catalog IDs warned:\n%s", buf.String())
	}
}

func TestConfigParseFailureKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "port: 9000\nnode_type: [unclosed\n")
	s := loadConfig(quietLogger(), path, envMap(nil))
	if s.Port != defaultPort {
		t.Errorf("partial parse leaked into settings: port=%d", s.Port)
	}
}

func TestRenderConfigRoundTripsAwkwardStrings(t *testing.T) {
	in := defaultSettings()
	in.BaseDir = `C:\Users\op\easycampus`
	in.InputDevice = `video="Cam: 1" #2`
	in.EncoderHost = "yes" // would decode as bool if left unquoted
	in.AllowedOrigins = []string{"http://encoder.local:8000"}
	out, err := renderConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	var got AppSettings
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("rendered config does not parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v\n%s", got, in, out)
	}
}

func TestSetTypeRoundTripPreservesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := `# operator note: do not touch
node_type: "decoder"   # set by installer
port: 9000
base_dir: 'C:\Users\op\ec'
allowed_origins:
  - http://projector.local:8000
quality_preset: broadcast-1080p60
custom_key: kept
`
	writeFile(t, path, original)

	// Env overrides must not leak into the written file.
	t.Setenv("EASYCAMPUS_PORT", "1234")
	t.Setenv("EASYCAMPUS_NODE_TYPE", "decoder")

	if err := setNodeTypeAt(path, " Encoder "); err != nil {
		t.Fatalf("setNodeTypeAt: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# operator note: do not touch", "# set by installer", "custom_key: kept"} {
		if !strings.Contains(text, want) {
			t.Errorf("lost %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"events_dir", "cache_dir", "1234"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("wrote %q into file:\n%s", unwanted, text)
		}
	}

	s := loadConfig(quietLogger(), path, envMap(nil))
	if s.NodeType != "encoder" || s.Port != 9000 || s.BaseDir != `C:\Users\op\ec` || s.QualityPreset != "broadcast-1080p60" {
		t.Errorf("round trip lost values: %+v", s)
	}
	if !reflect.DeepEqual(s.AllowedOrigins, []string{"http://projector.local:8000"}) {
		t.Errorf("allowed_origins lost: %v", s.AllowedOrigins)
	}
	if want := filepath.Join(`C:\Users\op\ec`, "encoder", "events"); s.EventsDir != want {
		t.Errorf("EventsDir=%q want %q (derived from new node type)", s.EventsDir, want)
	}
}

func TestSetTypeCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := setNodeTypeAt(path, "encoder"); err != nil {
		t.Fatalf("setNodeTypeAt: %v", err)
	}
	s := loadConfig(quietLogger(), path, envMap(nil))
	if s.NodeType != "encoder" {
		t.Errorf("NodeType=%q want encoder", s.NodeType)
	}
}

func TestSetTypeAppendsMissingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "port: 9000\n")
	if err := setNodeTypeAt(path, "encoder"); err != nil {
		t.Fatal(err)
	}
	s := loadConfig(quietLogger(), path, envMap(nil))
	if s.NodeType != "encoder" || s.Port != 9000 {
		t.Errorf("got node=%q port=%d", s.NodeType, s.Port)
	}
}

func TestSetTypeRejectsInvalidAndUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := setNodeTypeAt(path, "relay"); err == nil {
		t.Error("expected error for invalid node type")
	}
	broken := "node_type: [unclosed\n"
	writeFile(t, path, broken)
	if err := setNodeTypeAt(path, "encoder"); err == nil {
		t.Error("expected parse error")
	}
	data, _ := os.ReadFile(path)
	if string(data) != broken {
		t.Errorf("unparseable file was overwritten:\n%s", data)
	}
}
