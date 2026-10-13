package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func parsePID(val string) (int, error) {
	pid, err := strconv.Atoi(val)
	if err != nil || pid <= 0 {
		return 0, errors.New("invalid pid")
	}
	return pid, nil
}

func terminateProcess(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		return process.Kill()
	}

	_ = process.Signal(os.Interrupt)
	time.Sleep(500 * time.Millisecond)
	return process.Kill()
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// On Windows FindProcess opens a handle via OpenProcess and fails
		// when no such process exists, so success is the liveness signal.
		// (Signalling os.Interrupt is unsupported on Windows and always
		// errors, which used to mark every upload as failed immediately.)
		_ = process.Release()
		return true
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func padInt(value, width int) string {
	return fmt.Sprintf("%0*d", width, value)
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// idPattern is the single allow-list for every identifier that ends up
// in a filesystem path (event IDs, cache IDs, upload IDs, bookmark IDs).
// No dots, no separators, so filepath.Join can never escape the parent.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validID reports whether id is safe to use as a single path component.
func validID(id string) bool {
	return id != "" && len(id) <= 128 && idPattern.MatchString(id)
}

// validHTTPURL reports whether raw is an absolute http(s) URL with a host.
func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// safeSegmentName reports whether an HLS playlist entry is a plain file
// name that can be joined under the cache directory. Rejects separators,
// parent references, absolute paths and anything with a URL scheme (or a
// Windows drive letter) — all of which come from an untrusted peer.
func safeSegmentName(name string) bool {
	if name == "" || name == "." {
		return false
	}
	if strings.ContainsAny(name, `/\:`) || strings.Contains(name, "..") {
		return false
	}
	return !filepath.IsAbs(name)
}

// newEventID returns a timestamped ID with a short random suffix so two
// events created within the same second don't collide.
func newEventID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return time.Now().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}

// createEventDir creates a fresh, previously non-existent directory under
// base and returns its ID and path. os.Mkdir (not MkdirAll) guarantees
// we never adopt an existing event's directory.
func createEventDir(base string) (string, string, error) {
	if err := os.MkdirAll(base, 0755); err != nil {
		return "", "", err
	}
	for range 5 {
		id := newEventID()
		dir := filepath.Join(base, id)
		err := os.Mkdir(dir, 0755)
		if err == nil {
			return id, dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
	}
	return "", "", errors.New("could not allocate a unique event id")
}

// keyedMutex hands out one mutex per key. Used to serialize
// read-modify-write cycles on per-event files (bookmarks.json).
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*sync.Mutex)
	}
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}
