package ffmpeg

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readLines(path string) []string {
	data, _ := os.ReadFile(path)
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestSupervisorRequestedRestartAppendsAndIsFree: an explicit Restart
// re-runs ffmpeg with append_list + a per-run init file, doesn't count
// against MaxRestarts, and doesn't fire onRestart.
func TestSupervisorRequestedRestartAppendsAndIsFree(t *testing.T) {
	dir := t.TempDir()
	argLog := filepath.Join(dir, "args")
	bin := writeFakeFFmpeg(t, fmt.Sprintf(`echo "$*" >> %q
i=0
while [ $i -lt 300 ]; do i=$((i+1)); printf 'frame=%%d\nout_time_us=%%d\nprogress=continue\n' $i $i; sleep 0.05; done
`, argLog))
	s := NewSupervisor(quietLogger(), SupervisorConfig{MaxRestarts: 1, RestartInitialDelay: 10 * time.Millisecond})
	crashCallbacks := 0
	s.SetOnRestart(func() { crashCallbacks++ })
	cfg := liveCfg(testPreset, EncoderX264)
	cfg.Binary = bin
	cfg.OutputDir = dir
	if err := s.Start(cfg, "event-123"); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	waitFor(t, "first run", 3*time.Second, func() bool { return len(readLines(argLog)) == 1 })
	for i := 2; i <= 3; i++ { // more requested restarts than MaxRestarts allows crashes
		want := i
		waitFor(t, "running", 3*time.Second, func() bool {
			return s.Status().State == StateRunning && s.Status().LastProgress.Frame > 0
		})
		if !s.Restart("operator") {
			t.Fatal("Restart returned false")
		}
		waitFor(t, fmt.Sprintf("run %d", want), 6*time.Second, func() bool { return len(readLines(argLog)) == want })
	}
	lines := readLines(argLog)
	if strings.Contains(lines[0], "append_list") {
		t.Errorf("first run must not append: %s", lines[0])
	}
	for i, l := range lines[1:] {
		if !strings.Contains(l, "+append_list+discont_start") {
			t.Errorf("restart run %d missing append flags: %s", i+1, l)
		}
		if !strings.Contains(l, fmt.Sprintf("-hls_fmp4_init_filename init-r%d.mp4", i+1)) {
			t.Errorf("restart run %d missing per-run init: %s", i+1, l)
		}
	}
	st := s.Status()
	if st.State == StateFailed {
		t.Fatalf("requested restarts must not exhaust MaxRestarts: %+v", st)
	}
	if st.EventID != "event-123" {
		t.Errorf("EventID changed to %q", st.EventID)
	}
	if crashCallbacks != 0 {
		t.Errorf("onRestart fired %d times for requested restarts", crashCallbacks)
	}
}

// TestSupervisorStallWatchdogFiresOnFrozenFrames: ffmpeg keeps emitting
// progress=continue with a frozen frame counter; the watchdog must
// restart it.
func TestSupervisorStallWatchdogFiresOnFrozenFrames(t *testing.T) {
	dir := t.TempDir()
	argLog := filepath.Join(dir, "args")
	bin := writeFakeFFmpeg(t, fmt.Sprintf(`echo "$*" >> %q
i=0
while [ $i -lt 300 ]; do i=$((i+1)); printf 'frame=42\nout_time_us=1000\nprogress=continue\n'; sleep 0.05; done
`, argLog))
	s := NewSupervisor(quietLogger(), SupervisorConfig{
		ProgressStallAfter:  600 * time.Millisecond,
		RestartInitialDelay: 10 * time.Millisecond,
		MaxRestarts:         10,
	})
	cfg := liveCfg(testPreset, EncoderX264)
	cfg.Binary = bin
	cfg.OutputDir = dir
	if err := s.Start(cfg, "e"); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "stall restart", 8*time.Second, func() bool { return len(readLines(argLog)) >= 2 })
}

// TestSupervisorSwapPresetKeepsEventAndRestarts swaps the preset on a
// running stream; the next run uses the new preset and appends.
func TestSupervisorSwapPresetKeepsEventAndRestarts(t *testing.T) {
	dir := t.TempDir()
	argLog := filepath.Join(dir, "args")
	bin := writeFakeFFmpeg(t, fmt.Sprintf(`echo "$*" >> %q
i=0
while [ $i -lt 300 ]; do i=$((i+1)); printf 'frame=%%d\nprogress=continue\n' $i; sleep 0.05; done
`, argLog))
	s := NewSupervisor(quietLogger(), SupervisorConfig{RestartInitialDelay: 10 * time.Millisecond})
	cfg := liveCfg(testPreset, EncoderX264)
	cfg.Binary = bin
	cfg.OutputDir = dir
	if err := s.Start(cfg, "event-9"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first run", 3*time.Second, func() bool {
		return len(readLines(argLog)) == 1 && s.Status().LastProgress.Frame > 0
	})
	gen := s.Generation()

	lower := testPreset
	lower.IDValue, lower.W, lower.H = "test-720p30", 1280, 720
	if err := s.SwapPresetIfGeneration(lower, "bandwidth", gen); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second run", 6*time.Second, func() bool { return len(readLines(argLog)) == 2 })
	second := readLines(argLog)[1]
	if !strings.Contains(second, "scale=1280:720") || !strings.Contains(second, "append_list") {
		t.Errorf("swapped run args wrong: %s", second)
	}
	if st := s.Status(); st.PresetID != "test-720p30" || st.EventID != "event-9" {
		t.Errorf("status after swap: preset=%q event=%q", st.PresetID, st.EventID)
	}

	s.Stop()
	if err := s.SwapPresetIfGeneration(testPreset, "late", gen); err != ErrNotRunning {
		t.Fatalf("swap after Stop: err=%v want ErrNotRunning", err)
	}
	if s.IsRunning() {
		t.Fatal("swap after Stop resurrected the stream")
	}
	// A new stream bumps the generation; a stale decision is refused.
	if err := s.Start(cfg, "event-10"); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.SwapPresetIfGeneration(lower, "stale", gen); err != ErrNotRunning {
		t.Fatalf("stale-generation swap: err=%v want ErrNotRunning", err)
	}
}

// TestSupervisorMaxRestartsResetsAfterStableRun: crash budget is for
// crash storms; a run that lasted StableAfter resets it.
func TestSupervisorMaxRestartsResetsAfterStableRun(t *testing.T) {
	dir := t.TempDir()
	argLog := filepath.Join(dir, "args")
	// Every run lives ~1.5s (> StableAfter) then crashes.
	bin := writeFakeFFmpeg(t, fmt.Sprintf(`echo "$*" >> %q
i=0
while [ $i -lt 15 ]; do i=$((i+1)); printf 'frame=%%d\nprogress=continue\n' $i; sleep 0.1; done
exit 1
`, argLog))
	if testing.Short() {
		t.Skip("slow")
	}
	s := NewSupervisor(quietLogger(), SupervisorConfig{
		MaxRestarts:         1,
		StableAfter:         time.Second,
		RestartInitialDelay: 10 * time.Millisecond,
	})
	cfg := liveCfg(testPreset, EncoderX264)
	cfg.Binary = bin
	cfg.OutputDir = dir
	if err := s.Start(cfg, "e"); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "third run", 20*time.Second, func() bool { return len(readLines(argLog)) >= 3 })
	if st := s.Status(); st.State == StateFailed {
		t.Fatalf("stable runs should reset the crash budget: %+v", st)
	}
}

// TestJobCapturesFinalStderrLines: the last lines FFmpeg writes before
// exiting (usually the real error) must reach the log file.
func TestJobCapturesFinalStderrLines(t *testing.T) {
	bin := writeFakeFFmpeg(t, `i=0
while [ $i -lt 200 ]; do i=$((i+1)); echo "noise line $i" >&2; done
printf 'Error opening output file: rtsp://u:SECRET@h/x\n' >&2
printf 'FINAL-FATAL-LINE-NO-NEWLINE' >&2
exit 1
`)
	logPath := filepath.Join(t.TempDir(), "job.log")
	cfg := Config{Binary: bin, Mode: ModeUpload, Input: Input{Kind: InputFile, VideoDevice: "/tmp/i.mp4"},
		Preset: testPreset, Encoder: EncoderX264, OutputDir: t.TempDir()}
	for i := 0; i < 5; i++ { // repeat: the race only shows up intermittently
		err := RunOnce(cfg, JobOptions{LogPath: logPath})
		if err == nil {
			t.Fatal("expected non-nil exit error")
		}
		data, _ := os.ReadFile(logPath)
		log := string(data)
		if !strings.Contains(log, "FINAL-FATAL-LINE-NO-NEWLINE") || !strings.Contains(log, "noise line 200") {
			t.Fatalf("iteration %d: final stderr lines missing from log:\n...%s", i, log[max(0, len(log)-200):])
		}
		if strings.Contains(log, "SECRET") {
			t.Fatalf("credential leaked to job log")
		}
	}
}
