// Model-copy is the only program in a model image. It runs as an init container and copies the
// model file into a volume the model server reads, because a scratch image has no shell or cp.
// It writes to a temporary name and renames, so a half-copied file never has the final name.
//
// Usage: model-copy SOURCE DESTINATION
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	part := dst + ".part"
	// A copy killed part-way leaves its file behind, and the volume outlives the container.
	if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if err == nil {
		err = out.Chmod(0o444)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(part)
		return n, err
	}
	return n, os.Rename(part, dst)
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: model-copy SOURCE DESTINATION")
		os.Exit(2)
	}
	start := time.Now()
	n, err := copyFile(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "model-copy:", err)
		os.Exit(1)
	}
	fmt.Printf("copied %d bytes to %s in %.1f s\n", n, os.Args[2], time.Since(start).Seconds())
}
