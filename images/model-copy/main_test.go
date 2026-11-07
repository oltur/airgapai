package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "model.gguf")
	want := bytes.Repeat([]byte("gguf"), 1<<16)
	if err := os.WriteFile(src, want, 0o444); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out", "model.gguf")
	n, err := copyFile(src, dst)
	if err != nil || n != int64(len(want)) {
		t.Fatalf("copyFile = %d, %v", n, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("copy differs from source: %v", err)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

func TestCopyFileReplacesLeftoverPart(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(src, []byte("model"), 0o444); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.gguf")
	// What a copy killed part-way leaves behind, read-only as the finished file is.
	if err := os.WriteFile(dst+".part", []byte("half"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile after a leftover part: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("finished file: %v, %v", info, err)
	}
}

func TestCopyFileMissingSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "model.gguf")
	if _, err := copyFile(filepath.Join(dir, "absent"), dst); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("destination exists after a failed copy")
	}
}
