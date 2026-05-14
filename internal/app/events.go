package app

import (
	"encoding/json"
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

func deleteEvent(settings AppSettings, eventID string) error {
	eventDir := filepath.Join(settings.EventsDir, eventID)
	if _, err := os.Stat(eventDir); err != nil {
		return err
	}

	infoPath := filepath.Join(eventDir, "info.json")
	if infoData, err := os.ReadFile(infoPath); err == nil {
		var info EventInfoFile
		if err := json.Unmarshal(infoData, &info); err == nil {
			if info.PID != "" {
				if pid, err := parsePID(info.PID); err == nil {
					_ = terminateProcess(pid)
				}
			}
		}
	}

	return os.RemoveAll(eventDir)
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
