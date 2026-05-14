package ffmpeg

import (
	"testing"
	"time"
)

func TestStatusProgressStalledBeforeFirstProgress(t *testing.T) {
	now := time.Now()
	status := Status{State: StateRunning, StartedAt: now.Add(-30 * time.Second)}
	if !status.progressStalled(20*time.Second, now) {
		t.Fatal("expected startup with no progress to be stalled after threshold")
	}
	if status.progressStalled(40*time.Second, now) {
		t.Fatal("did not expect startup to be stalled before threshold")
	}
}

func TestStatusProgressStalledAfterProgressStops(t *testing.T) {
	now := time.Now()
	status := Status{
		State:     StateRunning,
		StartedAt: now.Add(-2 * time.Minute),
		LastProgress: Progress{
			UpdatedAt: now.Add(-25 * time.Second),
		},
	}
	if !status.progressStalled(20*time.Second, now) {
		t.Fatal("expected stale progress to be stalled")
	}
	status.LastProgress.UpdatedAt = now.Add(-5 * time.Second)
	if status.progressStalled(20*time.Second, now) {
		t.Fatal("fresh progress should not be stalled")
	}
}

func TestClassifyFFmpegError(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"Permission denied", "Permission"},
		{"Device or resource busy", "busy"},
		{"Connection refused", "Could not reach"},
		{"normal log line", ""},
	}
	for _, tc := range cases {
		got := classifyFFmpegError(tc.line)
		if tc.want == "" && got != "" {
			t.Errorf("expected empty hint for %q, got %q", tc.line, got)
		}
		if tc.want != "" && got == "" {
			t.Errorf("expected hint for %q, got empty", tc.line)
		}
	}
}

func TestSupervisorIsRunningWhenIdle(t *testing.T) {
	s := NewSupervisor(nil, SupervisorConfig{})
	if s.IsRunning() {
		t.Fatal("new supervisor should not report running")
	}
}
