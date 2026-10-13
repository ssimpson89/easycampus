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
		// progress=continue blocks still arriving (fresh UpdatedAt) but
		// the frame counter stopped moving 25s ago: that is a stall.
		LastProgress: Progress{
			Frame:     900,
			UpdatedAt: now.Add(-1 * time.Second),
		},
		LastAdvanceAt: now.Add(-25 * time.Second),
	}
	if !status.progressStalled(20*time.Second, now) {
		t.Fatal("expected frozen frame counter to be stalled despite fresh progress blocks")
	}
	status.LastAdvanceAt = now.Add(-5 * time.Second)
	if status.progressStalled(20*time.Second, now) {
		t.Fatal("recently advancing progress should not be stalled")
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

func TestRecordProgressTracksAdvanceOnly(t *testing.T) {
	s := NewSupervisor(nil, SupervisorConfig{})
	t0 := time.Now().Add(-time.Minute).UTC()
	s.recordProgress(Progress{Frame: 10, OutTimeUS: 1000, UpdatedAt: t0})
	if got := s.Status().LastAdvanceAt; !got.Equal(t0) {
		t.Fatalf("first advancing block: LastAdvanceAt=%v want %v", got, t0)
	}
	// Frozen frame/out_time: UpdatedAt moves, LastAdvanceAt must not.
	s.recordProgress(Progress{Frame: 10, OutTimeUS: 1000, UpdatedAt: t0.Add(30 * time.Second)})
	if got := s.Status().LastAdvanceAt; !got.Equal(t0) {
		t.Fatalf("frozen block moved LastAdvanceAt to %v", got)
	}
	// out_time advancing alone counts (audio-only / dup frames).
	t2 := t0.Add(40 * time.Second)
	s.recordProgress(Progress{Frame: 10, OutTimeUS: 2000, UpdatedAt: t2})
	if got := s.Status().LastAdvanceAt; !got.Equal(t2) {
		t.Fatalf("out_time advance: LastAdvanceAt=%v want %v", got, t2)
	}
}

func TestFrozenProgressDoesNotClearDegraded(t *testing.T) {
	s := NewSupervisor(nil, SupervisorConfig{})
	s.recordProgress(Progress{Frame: 10, UpdatedAt: time.Now()})
	s.markDegraded("stalled")
	s.recordProgress(Progress{Frame: 10, UpdatedAt: time.Now()})
	if st := s.status.State; st != StateDegraded {
		t.Fatalf("frozen progress flipped state to %q", st)
	}
	s.recordProgress(Progress{Frame: 11, UpdatedAt: time.Now()})
	if st := s.status.State; st != StateRunning {
		t.Fatalf("advancing progress should recover, state=%q", st)
	}
}

func TestSwapPresetRefusesWhenStopped(t *testing.T) {
	s := NewSupervisor(nil, SupervisorConfig{})
	if err := s.SwapPreset(testPreset, "x"); err != ErrNotRunning {
		t.Fatalf("SwapPreset on idle supervisor: err=%v want ErrNotRunning", err)
	}
	if s.IsRunning() {
		t.Fatal("SwapPreset must never start a stream")
	}
}

func TestRunInitFilename(t *testing.T) {
	cases := []struct {
		base string
		run  int
		want string
	}{
		{"", 0, "init.mp4"},
		{"", 1, "init-r1.mp4"},
		{"init.mp4", 3, "init-r3.mp4"},
		{"hdr", 2, "hdr-r2"},
	}
	for _, c := range cases {
		if got := RunInitFilename(c.base, c.run); got != c.want {
			t.Errorf("RunInitFilename(%q,%d)=%q want %q", c.base, c.run, got, c.want)
		}
	}
}
