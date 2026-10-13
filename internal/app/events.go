package app

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"easycampus/internal/atomicfile"
)

type EventInfoFile struct {
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	StoppedAt  string `json:"stopped_at,omitempty"`
	Status     string `json:"status"`
	Type       string `json:"type"`
	SourceURL  string `json:"source_url,omitempty"`
	SourceFile string `json:"source_file,omitempty"`
	Quality    string `json:"quality,omitempty"`
	PID        string `json:"pid,omitempty"`
	// FPS / QualityPreset are recorded at stream start for live events.
	FPS           float64 `json:"fps,omitempty"`
	QualityPreset string  `json:"quality_preset,omitempty"`
}

// EnsureDirs creates the per-node data directories if they don't exist.
// Called once at startup before constructing the Server so handler code
// can assume the directories are present.
func EnsureDirs(settings AppSettings) error {
	return ensureDirs(settings)
}

func ensureDirs(settings AppSettings) error {
	if err := os.MkdirAll(settings.EventsDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(settings.CacheDir, 0755); err != nil {
		return err
	}
	return nil
}

func listEvents(settings AppSettings) ([]EventInfo, error) {
	entries, err := os.ReadDir(settings.EventsDir)
	if err != nil {
		return nil, err
	}

	events := make([]EventInfo, 0)
	now := time.Now()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		eventID := entry.Name()
		info, err := readEventInfo(settings, eventID)
		if err != nil {
			continue
		}

		createdAt, err := time.Parse(time.RFC3339, info.CreatedAt)
		if err != nil {
			continue
		}

		var stoppedAt *time.Time
		if info.StoppedAt != "" {
			parsed, err := time.Parse(time.RFC3339, info.StoppedAt)
			if err == nil {
				stoppedAt = &parsed
			}
		}

		duration := int(now.Sub(createdAt).Seconds())
		if stoppedAt != nil {
			duration = int(stoppedAt.Sub(createdAt).Seconds())
		}

		eventDir := filepath.Join(settings.EventsDir, eventID)
		sizeBytes := dirSize(eventDir, func(path string) bool {
			return filepath.Base(path) != "ffmpeg.log"
		})

		events = append(events, EventInfo{
			ID:              eventID,
			Name:            info.Name,
			Status:          info.Status,
			Type:            defaultString(info.Type, "recording"),
			CreatedAt:       createdAt,
			StoppedAt:       stoppedAt,
			DurationSeconds: duration,
			FPS:             info.FPS,
			SizeBytes:       sizeBytes,
			StreamURL:       "/events/" + eventID + "/master.m3u8",
			LogURL:          "/events/" + eventID + "/ffmpeg.log",
		})
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].CreatedAt.After(events[j].CreatedAt)
	})

	return events, nil
}

func readEventInfo(settings AppSettings, eventID string) (EventInfoFile, error) {
	infoPath := filepath.Join(settings.EventsDir, eventID, "info.json")
	infoData, err := os.ReadFile(infoPath)
	if err != nil {
		return EventInfoFile{}, err
	}
	var info EventInfoFile
	if err := json.Unmarshal(infoData, &info); err != nil {
		return EventInfoFile{}, err
	}
	return info, nil
}

// deleteEvent removes an event directory. If info.json records a PID,
// killPID is offered it; callers pass a function that only acts on PIDs
// this process is tracking (a PID read from disk may belong to an
// unrelated process after a restart). Returns an fs.ErrNotExist error
// when the event doesn't exist.
func deleteEvent(settings AppSettings, eventID string, killPID func(pid int)) error {
	if !validID(eventID) {
		return errors.New("invalid event id")
	}
	eventDir := filepath.Join(settings.EventsDir, eventID)
	if _, err := os.Stat(eventDir); err != nil {
		return err
	}

	if info, err := readEventInfo(settings, eventID); err == nil && info.PID != "" && killPID != nil {
		if pid, err := parsePID(info.PID); err == nil {
			killPID(pid)
		}
	}

	return os.RemoveAll(eventDir)
}

// reconcileStaleEvents marks live recordings left "running" by a previous
// process (crash, power loss, kill -9) as "stopped". Called once at
// startup, before any stream can be started, so nothing it touches can
// actually be live.
func reconcileStaleEvents(settings AppSettings) int {
	entries, err := os.ReadDir(settings.EventsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := readEventInfo(settings, entry.Name())
		if err != nil || info.Status != "running" {
			continue
		}
		updateInfoStatus(filepath.Join(settings.EventsDir, entry.Name(), "info.json"), "stopped", 0)
		n++
	}
	return n
}

func writeEventInfo(path string, info EventInfoFile) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	// Atomic — a crash mid-write would otherwise yield a truncated
	// info.json that parses as zero values and silently drops the
	// recording from the events list (see listEvents continue at the
	// readEventInfo error branch).
	return atomicfile.Write(path, data, 0644)
}

func dirSize(dir string, include func(path string) bool) int64 {
	var size int64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if include != nil && !include(path) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			size += info.Size()
		}
		return nil
	})
	return size
}

func defaultString(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}
