package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortTempSocketPath returns a path short enough for AF_UNIX (108
// chars on Linux, 104 on macOS). t.TempDir() commonly nests under
// /var/folders/... on macOS which can already exceed the limit before
// we append a filename, so we fall back to /tmp + a registered
// cleanup when the natural temp dir is too long.
func shortTempSocketPath(t *testing.T, name string) string {
	t.Helper()
	candidate := filepath.Join(t.TempDir(), name)
	if len(candidate) <= 100 {
		return candidate
	}
	dir, err := os.MkdirTemp("/tmp", "mpvtest")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// TestWaitForMPVSocketSucceedsImmediately constructs a Unix socket
// listener at a temp path BEFORE calling waitForMPVSocket. The poll
// loop should return nil on the first iteration (well under 50ms);
// the bound we assert is 200ms to give CI machines slack without
// hiding a regression that re-introduces the old 1s sleep.
func TestWaitForMPVSocketSucceedsImmediately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket test")
	}

	socketPath := shortTempSocketPath(t, "mpv.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	// Need a non-nil *exec.Cmd to pass through the poll loop's
	// ProcessState check. A "sleep 10" is the cheapest portable way
	// to get a started-but-still-running child without depending on
	// mpv being installed in the test environment.
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	start := time.Now()
	if err := waitForMPVSocket(context.Background(), cmd, socketPath, 5*time.Second); err != nil {
		t.Fatalf("waitForMPVSocket: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("waitForMPVSocket took %s; should return immediately when socket is ready", elapsed)
	}
}

// TestWaitForMPVSocketTimesOut confirms the poll exits with an error
// when no socket ever appears. Bound at 500ms with a 200ms timeout
// argument so the test stays fast.
func TestWaitForMPVSocketTimesOut(t *testing.T) {
	socketPath := shortTempSocketPath(t, "never.sock")

	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	start := time.Now()
	err := waitForMPVSocket(context.Background(), cmd, socketPath, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("waitForMPVSocket: expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not come up") {
		t.Errorf("error message=%q; want it to mention timeout", err.Error())
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("waitForMPVSocket returned in %s; should respect the timeout", elapsed)
	}
	// Generous upper bound — the poll cadence is 50ms so we should
	// see at most one extra tick of slop.
	if elapsed > 500*time.Millisecond {
		t.Errorf("waitForMPVSocket took %s; expected ~200ms", elapsed)
	}
}

// TestWaitForMPVSocketDetectsChildExit confirms the loop bails fast
// when the supervised process has already exited — no point polling
// for a socket the now-dead child will never create. This case maps
// to "mpv exited because of an unknown CLI flag" or "mpv crashed
// before opening the IPC server"; the user-visible error should
// surface within a tick rather than waiting the full timeout.
func TestWaitForMPVSocketDetectsChildExit(t *testing.T) {
	socketPath := shortTempSocketPath(t, "never.sock")

	// `true` exits immediately with status 0.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	// After Run() the ProcessState is set; waitForMPVSocket should
	// observe that on its first iteration and return promptly.
	start := time.Now()
	err := waitForMPVSocket(context.Background(), cmd, socketPath, 5*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("waitForMPVSocket: expected exited-process error, got nil")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error message=%q; want it to mention process exit", err.Error())
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("waitForMPVSocket took %s; should bail fast on exited child", elapsed)
	}
}

// TestSendIPCHonoursContextDeadline confirms that sendIPC observes a
// tight ctx deadline against a slow-responding fake mpv IPC server.
//
// The fake listener accepts the connection and writes nothing — so the
// scanner in sendIPC would block on the read deadline (~3s) without
// ctx wiring. With ctx plumbed through (item #7), the call must return
// promptly: a 50ms deadline maps to ~50-100ms wall time end-to-end.
//
// Before this change the read deadline was a hardcoded 3s and there
// was no DialContext, so a 50ms ctx was simply ignored — this test
// would have taken 3000ms to fail.
func TestSendIPCHonoursContextDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket test")
	}

	socketPath := shortTempSocketPath(t, "mpv.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	// Accept loop holds each connection open without writing anything —
	// the fake "mpv" server that processes the command but never replies.
	// Close on stop so leaked goroutines don't keep the listener busy.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				<-stop
				_ = c.Close()
			}(conn)
		}
	}()
	defer func() {
		close(stop)
		_ = listener.Close()
		wg.Wait()
	}()

	// Build a minimal MPVService pointed at the fake socket. We bypass
	// Start by injecting a fake "process" that signal-0 reports as
	// alive — IsRunning short-circuits the IPC otherwise.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := &MPVService{
		socketPath: socketPath,
		logger:     logger,
	}
	// Use `sleep 30` so signal-0 in IsRunning returns nil for the
	// duration of the test.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	svc.process = cmd

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = svc.sendIPC(ctx, map[string]interface{}{"command": []interface{}{"get_property", "time-pos"}}, true)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("sendIPC: expected error, got nil after %s", elapsed)
	}
	// Tight upper bound — 200ms gives ample CI slack for the goroutine
	// watchdog + scanner wakeup; the hard ceiling proves ctx is wired
	// through instead of the prior 3s read deadline ruling the wait.
	if elapsed > 200*time.Millisecond {
		t.Errorf("sendIPC took %s; want <200ms (ctx deadline 50ms)", elapsed)
	}
}
