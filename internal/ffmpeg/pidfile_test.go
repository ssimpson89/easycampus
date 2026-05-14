package ffmpeg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPidFile_NoFile(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "nope.pid")}
	got, err := p.ReapOrphan()
	if err != nil || got != 0 {
		t.Fatalf("expected (0, nil) for missing file, got (%d, %v)", got, err)
	}
}

func TestPidFile_StaleDead(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	if err := p.Write(99999999); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReapOrphan()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("expected 0 (dead PID), got %d", got)
	}
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("expected pid file to be removed, got err=%v", err)
	}
}

func TestPidFile_OwnPidNotReaped(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	if err := p.Write(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReapOrphan()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("reaper should NOT have killed our own (non-ffmpeg) process, got %d", got)
	}
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("expected pid file to be removed, got err=%v", err)
	}
}

func TestPidFile_Garbage(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	if err := os.WriteFile(p.Path, []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReapOrphan()
	if err != nil || got != 0 {
		t.Fatalf("expected (0, nil) for garbage file, got (%d, %v)", got, err)
	}
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("expected garbage pid file to be removed")
	}
}

func TestPidFile_WriteAndClear(t *testing.T) {
	dir := t.TempDir()
	p := &PidFile{Path: filepath.Join(dir, "ffmpeg.pid")}
	if err := p.Write(1234); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1234" {
		t.Fatalf("expected '1234', got %q", string(data))
	}
	p.Clear()
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatal("expected pid file to be removed after Clear()")
	}
	p.Clear() // idempotent
}

func TestIsFFmpegCommandLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"ffmpeg -i input.mp4 output.mp4", true},
		{"/usr/local/bin/ffmpeg -y -i in.mov out.mov", true},
		{"ffmpeg", true},
		{"FFMPEG -i x.mov y.mov", true}, // case-insensitive on basename
		{"ffmpeg.test -test.run X", false},
		{"go run main.go", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isFFmpegCommandLine(tc.line)
		if got != tc.want {
			t.Errorf("isFFmpegCommandLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
