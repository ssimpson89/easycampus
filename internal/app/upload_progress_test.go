package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	ocffmpeg "easycampus/internal/ffmpeg"
)

func readInfoMap(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read info: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	return m
}

func writeInfoMap(t *testing.T, path string, m map[string]interface{}) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write info: %v", err)
	}
}

// TestUpdateInfoProgressPreservesPIDAndSkipsAfterCompletion checks a
// progress tick keeps fields written by other writers (the PID) and
// that a late tick after completion doesn't overwrite the final state.
func TestUpdateInfoProgressPreservesPIDAndSkipsAfterCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "info.json")
	writeInfoMap(t, path, map[string]interface{}{"status": "processing", "pid": "4242"})

	tick := ocffmpeg.Progress{Frame: 300, FPS: 30, OutTimeUS: 10_000_000, UpdatedAt: time.Now()}
	updateInfoProgress(path, tick, 40)
	m := readInfoMap(t, path)
	if m["pid"] != "4242" {
		t.Errorf("pid lost after progress tick: %v", m["pid"])
	}
	prog, _ := m["progress"].(map[string]interface{})
	if prog == nil || prog["percent"].(float64) != 25 {
		t.Fatalf("progress percent = %v, want 25", prog)
	}

	updateInfoStatus(path, "completed", 0)
	updateInfoProgress(path, ocffmpeg.Progress{Frame: 1, OutTimeUS: 1, UpdatedAt: time.Now()}, 40)
	m = readInfoMap(t, path)
	prog, _ = m["progress"].(map[string]interface{})
	if m["status"] != "completed" || prog == nil || prog["percent"].(float64) != 100 {
		t.Errorf("late tick overwrote completion: status=%v progress=%v", m["status"], prog)
	}
}

// TestFFprobePathPrefersSibling checks ffprobe is taken from the same
// directory as the ffmpeg binary when one is there.
func TestFFprobePathPrefersSibling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix executables only")
	}
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	ffprobe := filepath.Join(dir, "ffprobe")
	for _, p := range []string{ffmpeg, ffprobe} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	got := ffprobePath(ffmpeg)
	want, _ := filepath.EvalSymlinks(ffprobe)
	if got != want && got != ffprobe {
		t.Errorf("ffprobePath(%q) = %q, want %q", ffmpeg, got, ffprobe)
	}

	lone := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(lone, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := ffprobePath(lone); got != "ffprobe" {
		t.Errorf("no sibling: ffprobePath = %q, want plain ffprobe", got)
	}
}
