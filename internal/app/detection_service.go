package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// DetectionService probes the host for display attachments and Decklink
// support. Each call shells out to system-specific tools (system_profiler,
// xrandr, powershell, mpv --vo=help). Results are not cached — at the time
// of writing the daemon doesn't hammer these endpoints; if that changes,
// add a 5s-TTL cache like devices.Scanner.
type DetectionService struct {
	logger *slog.Logger
}

func NewDetectionService(logger *slog.Logger) *DetectionService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DetectionService{logger: logger}
}

// ListDisplays enumerates attached displays. ctx bounds the shell-out
// (system_profiler / xrandr / powershell) so a wedged display driver
// can't hang the handler — each call is sub-second normally but on a
// bad day the underlying tools can stall for tens of seconds.
//
// The historical name was GetAvailableDisplays; the new name aligns with
// the rest of the service surface (ListInputDevices etc.).
func (d *DetectionService) ListDisplays(ctx context.Context) []map[string]interface{} {
	switch runtime.GOOS {
	case "darwin":
		return d.detectMacDisplays(ctx)
	case "linux":
		return d.detectLinuxDisplays(ctx)
	case "windows":
		return d.detectWindowsDisplays(ctx)
	default:
		return []map[string]interface{}{{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true}}
	}
}

// CheckDecklinkAvailable runs `mpv --vo=help` and inspects the output
// for decklink support. ctx bounds the shell-out — the previous version
// could hang indefinitely if mpv itself was wedged (mpv occasionally
// stalls during plugin probing on macOS with a stale CoreAudio device).
func (d *DetectionService) CheckDecklinkAvailable(ctx context.Context) map[string]interface{} {
	cmd := exec.CommandContext(ctx, "mpv", "--vo=help")
	output, err := cmd.Output()
	if err != nil {
		return map[string]interface{}{"available": false, "formats": []string{}, "devices": []string{}}
	}
	if !strings.Contains(string(output), "decklink") {
		return map[string]interface{}{"available": false, "formats": []string{}, "devices": []string{}}
	}

	formats := []string{"720p50", "720p59.94", "720p60", "1080p23.98", "1080p24", "1080p25", "1080p29.97", "1080p30", "1080p50", "1080p59.94", "1080p60", "2160p23.98", "2160p24", "2160p25", "2160p29.97", "2160p30"}
	devices := d.detectDecklinkDevices()
	return map[string]interface{}{"available": true, "formats": formats, "devices": devices}
}

func (d *DetectionService) detectMacDisplays(ctx context.Context) []map[string]interface{} {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPDisplaysDataType", "-json")
	output, err := cmd.Output()
	if err != nil {
		return []map[string]interface{}{{"index": 0, "name": "Built-in Display", "resolution": "Unknown", "is_primary": true}}
	}

	var data map[string]interface{}
	if err := json.Unmarshal(output, &data); err != nil {
		return []map[string]interface{}{{"index": 0, "name": "Built-in Display", "resolution": "Unknown", "is_primary": true}}
	}

	displays := make([]map[string]interface{}, 0)
	items, _ := data["SPDisplaysDataType"].([]interface{})
	for _, entry := range items {
		entryMap, _ := entry.(map[string]interface{})
		drivers, _ := entryMap["spdisplays_ndrvs"].([]interface{})
		for _, driver := range drivers {
			driverMap, _ := driver.(map[string]interface{})
			name, _ := driverMap["_name"].(string)
			resolution, _ := driverMap["_spdisplays_resolution"].(string)
			displays = append(displays, map[string]interface{}{
				"index":      len(displays),
				"name":       name,
				"resolution": resolution,
				"is_primary": len(displays) == 0,
			})
		}
	}
	if len(displays) == 0 {
		displays = append(displays, map[string]interface{}{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true})
	}
	return displays
}

func (d *DetectionService) detectLinuxDisplays(ctx context.Context) []map[string]interface{} {
	cmd := exec.CommandContext(ctx, "xrandr", "--listmonitors")
	output, err := cmd.Output()
	if err != nil {
		return []map[string]interface{}{{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true}}
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) <= 1 {
		return []map[string]interface{}{{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true}}
	}

	displays := make([]map[string]interface{}, 0)
	for i, line := range lines[1:] {
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		name := parts[len(parts)-1]
		resolution := parts[2]
		displays = append(displays, map[string]interface{}{
			"index":      i,
			"name":       name,
			"resolution": resolution,
			"is_primary": i == 0,
		})
	}
	if len(displays) == 0 {
		displays = append(displays, map[string]interface{}{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true})
	}
	return displays
}

func (d *DetectionService) detectWindowsDisplays(ctx context.Context) []map[string]interface{} {
	cmd := exec.CommandContext(ctx, "powershell", "-Command", "Get-WmiObject -Namespace root\\cimv2 -Class Win32_VideoController | Select-Object Name, VideoModeDescription | ConvertTo-Json")
	output, err := cmd.Output()
	if err != nil {
		return []map[string]interface{}{{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true}}
	}

	var data interface{}
	if err := json.Unmarshal(output, &data); err != nil {
		return []map[string]interface{}{{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true}}
	}

	displays := make([]map[string]interface{}, 0)
	switch cast := data.(type) {
	case []interface{}:
		for i, entry := range cast {
			entryMap, _ := entry.(map[string]interface{})
			name, _ := entryMap["Name"].(string)
			resolution, _ := entryMap["VideoModeDescription"].(string)
			displays = append(displays, map[string]interface{}{
				"index":      i,
				"name":       name,
				"resolution": resolution,
				"is_primary": i == 0,
			})
		}
	case map[string]interface{}:
		name, _ := cast["Name"].(string)
		resolution, _ := cast["VideoModeDescription"].(string)
		displays = append(displays, map[string]interface{}{
			"index":      0,
			"name":       name,
			"resolution": resolution,
			"is_primary": true,
		})
	}

	if len(displays) == 0 {
		displays = append(displays, map[string]interface{}{"index": 0, "name": "Primary Display", "resolution": "Unknown", "is_primary": true})
	}
	return displays
}

func (d *DetectionService) detectDecklinkDevices() []string {
	devices := make([]string, 0)
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/dev")
		if err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "blackmagic") {
					devices = append(devices, "/dev/"+entry.Name())
				}
			}
		}
	}
	if len(devices) == 0 {
		devices = append(devices, "auto")
	}
	return devices
}
