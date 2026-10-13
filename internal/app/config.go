package app

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"easycampus/internal/atomicfile"
	"easycampus/internal/playback"
	ocquality "easycampus/internal/quality"

	"gopkg.in/yaml.v3"
)

type AppSettings struct {
	// --- General Settings ---
	NodeType    string `yaml:"node_type"`    // "encoder" or "decoder"
	EncoderHost string `yaml:"encoder_host"` // IP/Hostname of the encoder (for decoders)
	Port        int    `yaml:"port"`

	// --- Media Settings ---
	InputDevice string `yaml:"input_device"`
	InputFormat string `yaml:"input_format"`
	// DefaultBitrate is the legacy video bitrate in bits per second
	// (4000000 = 4 Mbps). Only used when QualityPreset is empty.
	DefaultBitrate   int  `yaml:"default_bitrate"`
	DefaultFramerate int  `yaml:"default_framerate"`
	DisableAudio     bool `yaml:"disable_audio"`

	// QualityPreset is the stable identifier of an internal/quality
	// Preset (e.g. "consumer-1080p30", "broadcast-1080p60"). Empty
	// means "use legacy DefaultBitrate / DefaultFramerate" — the
	// supervisor falls back to the pre-preset fields when this is
	// unset so existing configs keep working. See internal/quality
	// for the catalog and tier definitions.
	QualityPreset string `yaml:"quality_preset"`

	// PlaybackProfile is the stable identifier of an internal/playback
	// Profile (e.g. "projector-30", "tv-50-pal", "decklink-broadcast").
	// Drives the decoder's mpv --video-sync / --interpolation /
	// --display-fps-override / --hwdec / --vo flags. Empty defaults to
	// "auto" — mpv's prior unconfigured behavior, no flag changes.
	// The operator's last UI choice is persisted here so it survives a
	// restart. See internal/playback for the catalog.
	PlaybackProfile string `yaml:"playback_profile"`

	// AdaptiveBitrate gates the AdaptiveController. When true, the
	// supervisor watches FFmpeg's progress / restart rate and steps
	// the active preset DOWN (within the same tier) on sustained
	// degradation, then steps it back UP after a stable window.
	//
	// Default: false. Broadcasters often hate mid-service auto-
	// stepping because it changes the upstream packet rate visible
	// to monitoring — operators opt in explicitly.
	AdaptiveBitrate bool `yaml:"adaptive_bitrate"`

	// --- Advanced HLS Settings ---
	HLSSegmentDuration int  `yaml:"hls_segment_duration"`
	HLSListSize        int  `yaml:"hls_list_size"`
	HLSDeleteOld       bool `yaml:"hls_delete_old"`

	// --- System Paths ---
	// Empty values mean "derive": BaseDir defaults to the directory
	// holding config.yaml (~/.easycampus), EventsDir/CacheDir default to
	// <BaseDir>/<NodeType>/{events,cache}.
	BaseDir   string `yaml:"base_dir"`
	EventsDir string `yaml:"events_dir"`
	CacheDir  string `yaml:"cache_dir"`

	// --- Logging / Legacy ---
	// LogLevel is the FFmpeg -loglevel source (debug|info|warning|error).
	// The daemon's own slog level comes from LOG_LEVEL, not this field.
	LogLevel          string `yaml:"log_level"`
	DefaultEncoderURL string `yaml:"default_encoder_url"` // Legacy, use encoder_host

	// --- CORS ---
	// AllowedOrigins extends the default CORS allow-list (which always
	// includes http://127.0.0.1:<port> and http://localhost:<port>) with
	// additional concrete origins. No wildcards. Add the encoder's LAN
	// URL here when the operator needs to reach the daemon from another
	// host. The server never echoes back "*" — every preflight returns
	// the matched concrete origin.
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// Validation bounds. Out-of-range values (from the file or the
// environment) are rejected with a warning and replaced by the default;
// we never clamp, so an operator typo can't silently become a different
// valid-looking number.
const (
	defaultPort               = 8000
	defaultBitrateBPS         = 4_000_000
	minBitrateBPS             = 100_000
	defaultFramerate          = 30
	defaultHLSSegmentDuration = 2
	minHLSSegmentDuration     = 1
	maxHLSSegmentDuration     = 10
	defaultHLSListSize        = 12
	defaultLogLevel           = "error"
)

// envLookup matches os.LookupEnv; injected so tests can supply a fixed
// environment without mutating the process.
type envLookup func(string) (string, bool)

// LoadConfig loads the AppSettings from the standard layered sources:
// defaults -> ~/.easycampus/config.yaml -> environment variables.
// The logger is required so file/parse/env warnings surface in the
// structured journal alongside everything else.
func LoadConfig(logger *slog.Logger) AppSettings {
	return loadConfig(logger, getConfigPath(), os.LookupEnv)
}

// defaultSettings returns the built-in defaults. Paths are left empty
// and resolved at the end of loadConfig.
func defaultSettings() AppSettings {
	inputFormat, inputDevice := platformDefaults()
	return AppSettings{
		Port:               defaultPort,
		LogLevel:           defaultLogLevel,
		DefaultBitrate:     defaultBitrateBPS,
		DefaultFramerate:   defaultFramerate,
		HLSSegmentDuration: defaultHLSSegmentDuration,
		HLSListSize:        defaultHLSListSize,
		InputFormat:        inputFormat,
		InputDevice:        inputDevice,
		DefaultEncoderURL:  "http://localhost:8000",
		NodeType:           "decoder",
		EncoderHost:        "localhost",
		// QualityPreset / PlaybackProfile empty: legacy bitrate fields
		// and playback.Default() ("auto") respectively.
		// AdaptiveBitrate off: broadcasters opt in explicitly.
	}
}

func loadConfig(logger *slog.Logger, configPath string, lookup envLookup) AppSettings {
	settings := defaultSettings()

	// 1. Config file: read it, or create a commented default template.
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			logger.Info("loading config", "path", configPath)
			fromFile := settings
			if err := yaml.Unmarshal(data, &fromFile); err != nil {
				logger.Warn("config parse failed; using defaults", "path", configPath, "err", err)
			} else {
				settings = fromFile
			}
		case errors.Is(err, os.ErrNotExist):
			if out, err := renderConfig(settings); err != nil {
				logger.Warn("config render failed", "err", err)
			} else if err := atomicfile.Write(configPath, out, 0644); err != nil {
				logger.Warn("config write failed", "path", configPath, "err", err)
			} else {
				logger.Info("created default config", "path", configPath)
			}
		default:
			logger.Warn("config read failed; using defaults", "path", configPath, "err", err)
		}
	}

	// 2. Environment overrides.
	applyEnv(logger, &settings, lookup)

	// 3. Validation (applies to file and env values alike).
	validateSettings(logger, &settings)

	// 4. Derived paths. Explicit values win; empty means derive.
	if settings.BaseDir == "" {
		settings.BaseDir = defaultBaseDir(configPath)
	}
	if settings.EventsDir == "" {
		settings.EventsDir = filepath.Join(settings.BaseDir, settings.NodeType, "events")
	}
	if settings.CacheDir == "" {
		settings.CacheDir = filepath.Join(settings.BaseDir, settings.NodeType, "cache")
	}
	return settings
}

// defaultBaseDir is the directory holding the config file (normally
// ~/.easycampus), made absolute so a relative fallback path doesn't
// drift with the working directory.
func defaultBaseDir(configPath string) string {
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func applyEnv(logger *slog.Logger, s *AppSettings, lookup envLookup) {
	get := func(key string) (string, bool) {
		v, ok := lookup(key)
		if !ok || v == "" {
			return "", false
		}
		return v, true
	}
	envInt := func(key string, dst *int) {
		if v, ok := get(key); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				logger.Warn("ignoring invalid integer env var", "var", key, "value", v, "err", err)
				return
			}
			*dst = n
		}
	}
	envBool := func(key string, dst *bool) {
		if v, ok := get(key); ok {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				logger.Warn("ignoring invalid boolean env var", "var", key, "value", v, "err", err)
				return
			}
			*dst = b
		}
	}
	envString := func(key string, dst *string) {
		if v, ok := get(key); ok {
			*dst = v
		}
	}

	envInt("EASYCAMPUS_PORT", &s.Port)
	envString("EASYCAMPUS_LOG_LEVEL", &s.LogLevel)
	envInt("EASYCAMPUS_DEFAULT_BITRATE", &s.DefaultBitrate)
	envInt("EASYCAMPUS_DEFAULT_FRAMERATE", &s.DefaultFramerate)
	envInt("EASYCAMPUS_HLS_SEGMENT_DURATION", &s.HLSSegmentDuration)
	envInt("EASYCAMPUS_HLS_LIST_SIZE", &s.HLSListSize)
	envBool("EASYCAMPUS_HLS_DELETE_OLD", &s.HLSDeleteOld)
	envBool("EASYCAMPUS_DISABLE_AUDIO", &s.DisableAudio)
	envString("EASYCAMPUS_INPUT_FORMAT", &s.InputFormat)
	envString("EASYCAMPUS_INPUT_DEVICE", &s.InputDevice)
	envString("EASYCAMPUS_DEFAULT_ENCODER_URL", &s.DefaultEncoderURL)
	envString("EASYCAMPUS_NODE_TYPE", &s.NodeType)
	envString("EASYCAMPUS_ENCODER_HOST", &s.EncoderHost)
	envString("EASYCAMPUS_QUALITY_PRESET", &s.QualityPreset)
	envString("EASYCAMPUS_PLAYBACK_PROFILE", &s.PlaybackProfile)
	envBool("EASYCAMPUS_ADAPTIVE_BITRATE", &s.AdaptiveBitrate)
	if v, ok := get("EASYCAMPUS_ALLOWED_ORIGINS"); ok {
		// Comma-separated, trim each entry. Empty entries are skipped at
		// the CORS layer.
		parts := strings.Split(v, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		s.AllowedOrigins = parts
	}
}

// validateSettings rejects out-of-range values, logging a warning and
// restoring the default for each one. Unknown catalog IDs are warned
// about but kept: the runtime already falls through to legacy/auto
// behavior on an unknown ID, and keeping the value preserves the
// operator's intent if a later build adds that ID.
func validateSettings(logger *slog.Logger, s *AppSettings) {
	reject := func(field string, got any, want string, def any) {
		logger.Warn("config value out of range; using default",
			"field", field, "value", got, "valid", want, "default", def)
	}

	s.NodeType = normalizeNodeType(s.NodeType)
	if s.NodeType != "encoder" && s.NodeType != "decoder" {
		logger.Warn("invalid node_type; defaulting to 'decoder'", "received", s.NodeType)
		s.NodeType = "decoder"
	}
	if s.Port < 1 || s.Port > 65535 {
		reject("port", s.Port, "1-65535", defaultPort)
		s.Port = defaultPort
	}
	if s.HLSListSize < 1 {
		reject("hls_list_size", s.HLSListSize, ">=1", defaultHLSListSize)
		s.HLSListSize = defaultHLSListSize
	}
	if s.HLSSegmentDuration < minHLSSegmentDuration || s.HLSSegmentDuration > maxHLSSegmentDuration {
		reject("hls_segment_duration", s.HLSSegmentDuration,
			fmt.Sprintf("%d-%d seconds", minHLSSegmentDuration, maxHLSSegmentDuration),
			defaultHLSSegmentDuration)
		s.HLSSegmentDuration = defaultHLSSegmentDuration
	}
	if s.DefaultBitrate < minBitrateBPS {
		reject("default_bitrate", s.DefaultBitrate,
			fmt.Sprintf(">=%d bits/s", minBitrateBPS), defaultBitrateBPS)
		s.DefaultBitrate = defaultBitrateBPS
	}

	s.QualityPreset = strings.TrimSpace(s.QualityPreset)
	if s.QualityPreset != "" {
		if _, ok := ocquality.ByID(s.QualityPreset); !ok {
			logger.Warn("unknown quality_preset; legacy bitrate/framerate will be used",
				"quality_preset", s.QualityPreset)
		}
	}
	s.PlaybackProfile = strings.TrimSpace(s.PlaybackProfile)
	if s.PlaybackProfile != "" {
		if _, ok := playback.ByID(s.PlaybackProfile); !ok {
			logger.Warn("unknown playback_profile; the default profile will be used",
				"playback_profile", s.PlaybackProfile, "default", playback.DefaultID)
		}
	}
}

func normalizeNodeType(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// SetNodeType rewrites node_type in the on-disk config file and returns
// the path written. It operates on the raw file only: environment
// overrides and derived paths (events_dir/cache_dir) are never folded
// in, and every other key, value and comment in the file is preserved.
// When no config file exists, a default template is written first.
// Used by the --set-type flag.
func SetNodeType(nodeType string) (string, error) {
	path := getConfigPath()
	return path, setNodeTypeAt(path, nodeType)
}

func setNodeTypeAt(path, nodeType string) error {
	nodeType = normalizeNodeType(nodeType)
	if nodeType != "encoder" && nodeType != "decoder" {
		return fmt.Errorf("invalid node type %q (expected encoder|decoder)", nodeType)
	}

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var doc yaml.Node
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			// Refuse to clobber a file we can't parse; the operator
			// would lose every other setting.
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if doc.Kind == 0 {
		// Missing or empty file: start from the commented template.
		def := defaultSettings()
		def.NodeType = nodeType
		out, err := renderConfig(def)
		if err != nil {
			return err
		}
		return atomicfile.Write(path, out, 0644)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("parse %s: top level is not a mapping", path)
	}
	setMappingScalar(doc.Content[0], "node_type", nodeType)

	out, err := encodeYAML(&doc)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, out, 0644)
}

// setMappingScalar sets key to a plain string scalar in a mapping node,
// keeping any comments on the existing key/value. Appends the key when
// absent.
func setMappingScalar(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			v.Kind = yaml.ScalarNode
			v.Tag = "!!str"
			v.Style = 0
			v.Value = value
			v.Content = nil
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func encodeYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// configField is one key in the rendered config template.
type configField struct {
	key   string
	value any
	head  string // comment block above the key (section headers etc.)
	line  string // trailing comment
}

// renderConfig produces the commented config template for s. Values
// are encoded through yaml.v3 so any string (Windows paths, quotes,
// colons) round-trips. Paths are written exactly as they appear in s:
// callers pass raw (non-derived) settings so empty stays empty.
func renderConfig(s AppSettings) ([]byte, error) {
	origins := s.AllowedOrigins
	if origins == nil {
		origins = []string{}
	}
	fields := []configField{
		{key: "node_type", value: s.NodeType,
			head: "--- General Settings ---",
			line: `"encoder" (source) or "decoder" (player)`},
		{key: "encoder_host", value: s.EncoderHost, line: "IP address or hostname of the encoder (e.g. 192.168.1.50)"},
		{key: "port", value: s.Port, line: "Web interface port, 1-65535 (default: 8000)"},

		{key: "input_device", value: s.InputDevice,
			head: "--- Media Settings ---",
			line: "Camera/device ID (e.g. /dev/video0 or 0)"},
		{key: "input_format", value: s.InputFormat, line: "v4l2, avfoundation, dshow"},
		{key: "default_bitrate", value: s.DefaultBitrate, line: "Video bitrate in bits/s, >= 100000 (4000000 = 4 Mbps); used when quality_preset is empty"},
		{key: "default_framerate", value: s.DefaultFramerate, line: "Target FPS; used when quality_preset is empty"},
		{key: "disable_audio", value: s.DisableAudio, line: "Set true to stream video only"},

		{key: "quality_preset", value: s.QualityPreset, head: strings.Join([]string{
			"quality_preset selects an internal/quality preset by ID. When set,",
			"it overrides default_bitrate and default_framerate (the supervisor",
			"uses the preset's resolution/framerate/bitrate envelope instead).",
			"See docs/quality-presets.md or GET /quality for the catalog.",
			"Empty disables the preset system (legacy behaviour).",
		}, "\n")},
		{key: "playback_profile", value: s.PlaybackProfile, head: strings.Join([]string{
			"playback_profile selects an internal/playback profile by ID for the",
			"decoder's mpv child. Drives --video-sync, --interpolation,",
			"--display-fps-override, --hwdec, and --vo. Empty defaults to \"auto\"",
			"(mpv's unconfigured defaults).",
			"See docs/playback-profiles.md or GET /playback-profiles for the catalog.",
		}, "\n")},
		{key: "adaptive_bitrate", value: s.AdaptiveBitrate, head: strings.Join([]string{
			"adaptive_bitrate gates the AdaptiveController. When true, the",
			"supervisor steps the active preset down (within tier) on sustained",
			"degradation and back up after a stable window. Default off; many",
			"broadcasters prefer a fixed envelope during a service.",
		}, "\n")},

		{key: "hls_segment_duration", value: s.HLSSegmentDuration,
			head: "--- Advanced HLS Settings ---",
			line: "Segment duration in seconds, 1-10"},
		{key: "hls_list_size", value: s.HLSListSize, line: "Segments kept in the playlist, >= 1"},
		{key: "hls_delete_old", value: s.HLSDeleteOld, line: "Delete old segments to save space (true for live-only)"},

		{key: "base_dir", value: s.BaseDir, head: strings.Join([]string{
			"--- System Paths ---",
			"Leave these empty to use default locations:",
			"base_dir defaults to the directory holding this file (~/.easycampus),",
			"events_dir/cache_dir default to <base_dir>/<node_type>/{events,cache}.",
		}, "\n")},
		{key: "events_dir", value: s.EventsDir},
		{key: "cache_dir", value: s.CacheDir},

		{key: "allowed_origins", value: origins, head: strings.Join([]string{
			"--- CORS ---",
			"Extra concrete origins allowed to call the API, in addition to",
			"http://127.0.0.1:<port> and http://localhost:<port>. No wildcards.",
			"Example: [\"http://encoder.local:8000\"]",
		}, "\n")},

		{key: "log_level", value: s.LogLevel,
			head: "--- Logging / Legacy ---",
			line: "FFmpeg -loglevel: debug, info, warning, error (daemon log level is the LOG_LEVEL env var)"},
		{key: "default_encoder_url", value: s.DefaultEncoderURL, line: "Legacy setting; prefer encoder_host"},
	}

	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, f := range fields {
		k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.key, HeadComment: f.head}
		v := &yaml.Node{}
		if err := v.Encode(f.value); err != nil {
			return nil, fmt.Errorf("encode %s: %w", f.key, err)
		}
		v.LineComment = f.line
		if v.Kind == yaml.SequenceNode {
			v.Style = yaml.FlowStyle
		}
		mapping.Content = append(mapping.Content, k, v)
	}
	doc := &yaml.Node{
		Kind:        yaml.DocumentNode,
		HeadComment: "EasyCampus Configuration File\nWritten by easycampus on first run and by --set-type.",
		Content:     []*yaml.Node{mapping},
	}
	return encodeYAML(doc)
}

// getConfigPath returns the path to the config file to use:
// ~/.easycampus/config.yaml, falling back to ./config.yaml when the
// home directory can't be resolved.
func getConfigPath() string {
	homeDir, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(homeDir, ".easycampus", "config.yaml")
	}
	return "config.yaml"
}

func platformDefaults() (string, string) {
	switch runtime.GOOS {
	case "darwin":
		return "avfoundation", "0:0"
	case "windows":
		return "dshow", "video=Integrated Camera"
	default:
		return "v4l2", "/dev/video0"
	}
}
