package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type AppSettings struct {
	// --- General Settings ---
	AppName     string `yaml:"app_name"`
	NodeType    string `yaml:"node_type"`    // "encoder" or "decoder"
	EncoderHost string `yaml:"encoder_host"` // IP/Hostname of the encoder (for decoders)
	Port        int    `yaml:"port"`

	// --- Media Settings ---
	InputDevice      string `yaml:"input_device"`
	InputFormat      string `yaml:"input_format"`
	DefaultBitrate   int    `yaml:"default_bitrate"`
	DefaultFramerate int    `yaml:"default_framerate"`
	DisableAudio     bool   `yaml:"disable_audio"`

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
	BaseDir   string `yaml:"base_dir"`
	EventsDir string `yaml:"events_dir"`
	CacheDir  string `yaml:"cache_dir"`

	// --- Debug / Legacy ---
	Debug             bool   `yaml:"debug"`
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

// LoadConfig loads the AppSettings from the standard layered sources:
// defaults -> ~/.easycampus/config.yaml -> environment variables.
// The logger is required so file/parse/env warnings surface in the
// structured journal alongside everything else.
func LoadConfig(logger *slog.Logger) AppSettings {
	return loadConfigInternal(logger)
}

func loadConfigInternal(logger *slog.Logger) AppSettings {
	// 1. Set Defaults
	cwd, _ := os.Getwd()
	// Default baseDir is still cwd for fallback, but ideally it should be user home.
	// We will adjust this after checking for config file/env vars or creating default config.
	baseDir := cwd

	inputFormat, inputDevice := platformDefaults()

	settings := AppSettings{
		AppName:  "EasyCampus",
		Debug:    true,
		Port:     8000,
		LogLevel: "INFO",
		BaseDir:  baseDir,
		// EventsDir and CacheDir will be set after determining BaseDir
		DefaultBitrate:     4000000,
		DefaultFramerate:   30,
		HLSSegmentDuration: 2,
		HLSListSize:        12,
		HLSDeleteOld:       false,
		DisableAudio:       false,
		InputFormat:        inputFormat,
		InputDevice:        inputDevice,
		DefaultEncoderURL:  "http://localhost:8000",
		NodeType:           "decoder", // Default to decoder as requested
		EncoderHost:        "localhost",
		// Empty default — the supervisor uses the legacy
		// DefaultBitrate/DefaultFramerate fields when QualityPreset
		// is unset. Operators opt into the preset system via the
		// UI / config file / env var.
		QualityPreset: "",
		// PlaybackProfile defaults to empty — the handler resolves
		// empty to playback.Default() ("auto"), which emits no
		// --video-sync / --interpolation / --display-fps-override
		// flags. Existing decoder deployments see no change.
		PlaybackProfile: "",
		// AdaptiveBitrate stays OFF by default. Broadcasters opt in
		// when their upstream is variable enough to want fallback.
		AdaptiveBitrate: false,
	}

	// 2. Load from Config File (if exists) or Create Default
	configPath := getConfigPath()
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			// File exists, read it
			if data, err := os.ReadFile(configPath); err == nil {
				logger.Info("loading config", "path", configPath)
				if err := yaml.Unmarshal(data, &settings); err != nil {
					logger.Warn("config parse failed; using defaults", "err", err)
				}
			}
		} else {
			// File does not exist, create it with defaults
			// First, ensure the directory exists
			configDir := filepath.Dir(configPath)
			if err := os.MkdirAll(configDir, 0755); err == nil {
				// We update the BaseDir to be the config dir for the default file
				// actually, user wanted ~/.easycampus/encoder or decoder
				// Let's set the BaseDir relative to the home dir if possible
				homeDir, _ := os.UserHomeDir()
				if homeDir != "" {
					settings.BaseDir = filepath.Join(homeDir, ".easycampus")
				} else {
					settings.BaseDir = configDir
				}

				// Marshal defaults to YAML
				if data, err := yaml.Marshal(&settings); err == nil {
					if err := os.WriteFile(configPath, data, 0644); err == nil {
						logger.Info("created default config", "path", configPath)
					} else {
						logger.Warn("config write failed", "path", configPath, "err", err)
					}
				}
			} else {
				logger.Warn("config dir create failed", "dir", configDir, "err", err)
			}
		}
	}

	// 3. Override with Environment Variables
	if val := os.Getenv("EASYCAMPUS_PORT"); val != "" {
		if p, err := strconv.Atoi(val); err == nil {
			settings.Port = p
		}
	}
	if val := os.Getenv("EASYCAMPUS_LOG_LEVEL"); val != "" {
		settings.LogLevel = val
	}
	if val := os.Getenv("EASYCAMPUS_DEFAULT_BITRATE"); val != "" {
		if b, err := strconv.Atoi(val); err == nil {
			settings.DefaultBitrate = b
		}
	}
	if val := os.Getenv("EASYCAMPUS_DEFAULT_FRAMERATE"); val != "" {
		if f, err := strconv.Atoi(val); err == nil {
			settings.DefaultFramerate = f
		}
	}
	if val := os.Getenv("EASYCAMPUS_HLS_SEGMENT_DURATION"); val != "" {
		if d, err := strconv.Atoi(val); err == nil {
			settings.HLSSegmentDuration = d
		}
	}
	if val := os.Getenv("EASYCAMPUS_HLS_LIST_SIZE"); val != "" {
		if s, err := strconv.Atoi(val); err == nil {
			settings.HLSListSize = s
		}
	}
	if val := os.Getenv("EASYCAMPUS_HLS_DELETE_OLD"); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			settings.HLSDeleteOld = b
		}
	}
	if val := os.Getenv("EASYCAMPUS_DISABLE_AUDIO"); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			settings.DisableAudio = b
		}
	}
	if val := os.Getenv("EASYCAMPUS_INPUT_FORMAT"); val != "" {
		settings.InputFormat = val
	}
	if val := os.Getenv("EASYCAMPUS_INPUT_DEVICE"); val != "" {
		settings.InputDevice = val
	}
	if val := os.Getenv("EASYCAMPUS_DEFAULT_ENCODER_URL"); val != "" {
		settings.DefaultEncoderURL = val
	}
	if val := os.Getenv("EASYCAMPUS_NODE_TYPE"); val != "" {
		settings.NodeType = val
	}
	if val := os.Getenv("EASYCAMPUS_ENCODER_HOST"); val != "" {
		settings.EncoderHost = val
	}
	if val := os.Getenv("EASYCAMPUS_QUALITY_PRESET"); val != "" {
		settings.QualityPreset = val
	}
	if val := os.Getenv("EASYCAMPUS_PLAYBACK_PROFILE"); val != "" {
		settings.PlaybackProfile = val
	}
	if val := os.Getenv("EASYCAMPUS_ALLOWED_ORIGINS"); val != "" {
		// Comma-separated, trim each entry. Empty entries are skipped at
		// the CORS layer.
		parts := strings.Split(val, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		settings.AllowedOrigins = parts
	}
	if val := os.Getenv("EASYCAMPUS_ADAPTIVE_BITRATE"); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			settings.AdaptiveBitrate = b
		}
	}

	// 4. Resolve Derived Paths (Events/Cache) based on NodeType
	// Logic: ~/.easycampus/<NodeType>/events

	// If the user config has specific EventsDir/CacheDir, respect them.
	// We only set defaults if they are empty.

	// Ensure NodeType is valid
	if settings.NodeType != "encoder" && settings.NodeType != "decoder" {
		logger.Warn("invalid node_type; defaulting to 'decoder'",
			"received", settings.NodeType)
		settings.NodeType = "decoder"
	}

	nodeDir := settings.NodeType
	if settings.EventsDir == "" {
		settings.EventsDir = filepath.Join(settings.BaseDir, nodeDir, "events")
	}
	if settings.CacheDir == "" {
		settings.CacheDir = filepath.Join(settings.BaseDir, nodeDir, "cache")
	}

	return settings
}

// SaveConfig writes the settings to the standard config file location
// with hand-rolled comments. Exported for the --set-type flag path in
// main; everything else mutates settings in memory only.
func SaveConfig(settings AppSettings) error {
	return saveConfig(settings)
}

// saveConfig writes the settings to the config file with comments.
func saveConfig(settings AppSettings) error {
	configPath := getConfigPath()

	// Ensure directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	// We construct a commented YAML manually for better user experience
	// The standard yaml.Marshal doesn't support comments well enough for a config file template

	// Constructing the YAML string
	content := fmt.Sprintf(`# EasyCampus Configuration File
# This file is automatically generated and updated.

# --- General Settings ---
app_name: "%s"
node_type: "%s"             # "encoder" (source) or "decoder" (player)
encoder_host: "%s"          # IP address or hostname of the encoder (e.g., 192.168.1.50)
port: %d                    # Web interface port (default: 8000)

# --- Media Settings ---
input_device: "%s"          # Camera/Device ID (e.g., /dev/video0 or 0)
input_format: "%s"          # v4l2, avfoundation, dshow
default_bitrate: %d         # Video bitrate in bps (e.g., 4000000 = 4Mbps) — used when quality_preset is empty
default_framerate: %d       # Target FPS — used when quality_preset is empty
disable_audio: %t           # Set true to stream video only

# quality_preset selects an internal/quality preset by ID. When set,
# it overrides default_bitrate and default_framerate (the supervisor
# uses the preset's resolution/framerate/bitrate envelope instead).
# Catalog: consumer-{480p30,720p30,1080p30},
#          prosumer-{720p60,1080p30-high,1080p60},
#          broadcast-{1080p30,1080p60,2160p30},
#          cinema-{1080p2398,1080p24,2160p2398,2160p24}.
# Empty disables the preset system (legacy behaviour).
quality_preset: "%s"

# playback_profile selects an internal/playback profile by ID for the
# decoder's mpv child. Drives --video-sync, --interpolation,
# --display-fps-override, --hwdec, and --vo. Empty defaults to "auto"
# (mpv's unconfigured defaults — current behaviour, no change).
# Catalog: auto, projector-{24,30,60}, tv-60, tv-50-pal,
#          decklink-broadcast.
# See docs/playback-profiles.md for selection guidance.
playback_profile: "%s"

# adaptive_bitrate gates the AdaptiveController. When true, the
# supervisor steps the active preset down (within tier) on sustained
# degradation and back up after a stable window. Default off; many
# broadcasters prefer a fixed envelope during a service.
adaptive_bitrate: %t

# --- Advanced HLS Settings ---
hls_segment_duration: %d    # Duration of each video segment in seconds
hls_list_size: %d           # Number of segments to keep in the playlist
hls_delete_old: %t          # Delete old segments to save space (true for live-only)

# --- System Paths ---
# Leave these empty to use default locations (~/.easycampus/<node_type>/...)
base_dir: "%s"
events_dir: "%s"
cache_dir: "%s"

# --- Debug / Legacy ---
debug: %t
log_level: "%s"
default_encoder_url: "%s"   # Legacy setting
`,
		settings.AppName,
		settings.NodeType,
		settings.EncoderHost,
		settings.Port,
		settings.InputDevice,
		settings.InputFormat,
		settings.DefaultBitrate,
		settings.DefaultFramerate,
		settings.DisableAudio,
		settings.QualityPreset,
		settings.PlaybackProfile,
		settings.AdaptiveBitrate,
		settings.HLSSegmentDuration,
		settings.HLSListSize,
		settings.HLSDeleteOld,
		settings.BaseDir,
		settings.EventsDir,
		settings.CacheDir,
		settings.Debug,
		settings.LogLevel,
		settings.DefaultEncoderURL,
	)

	// Write file
	return os.WriteFile(configPath, []byte(content), 0644)
}

// getConfigPath returns the path to the config file to use.

// It prioritizes ~/.easycampus/config.yaml per user request.
func getConfigPath() string {
	// 1. Check user-suggested location: ~/.easycampus/config.yaml
	// This is now the primary location for creation too.
	homeDir, err := os.UserHomeDir()
	if err == nil {
		userDir := filepath.Join(homeDir, ".easycampus")
		return filepath.Join(userDir, "config.yaml")
	}

	// Fallback to local directory
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
