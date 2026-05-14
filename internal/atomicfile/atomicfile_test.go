package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesFileAndDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "file.txt")
	if err := Write(target, []byte("hello"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q want %q", data, "hello")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("mode: got %v want 0644", info.Mode().Perm())
	}
}

func TestWriteOverwrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := Write(target, []byte("first"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Write(target, []byte("second"), 0644); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "second" {
		t.Fatalf("got %q want second", data)
	}
}

func TestWriteCleansTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := Write(target, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "f" {
		t.Fatalf("expected only target file, got %v", entries)
	}
}
