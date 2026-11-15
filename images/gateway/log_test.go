package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// Every log line passes through the redacting writer that main installs.
func TestLogsAreRedacted(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(redacting{&buf})
	defer log.SetOutput(os.Stderr)
	logf("model server: 500: %s", "contact jane.doe@example.com or +49 30 12345678")
	log.Print("an http.Server error with 4111 1111 1111 1111") // as the HTTP server logs, not through logf
	if out := buf.String(); strings.Contains(out, "jane.doe") || strings.Contains(out, "12345678") ||
		strings.Contains(out, "4111") || strings.Count(out, "[") != 3 {
		t.Errorf("logged %q", out)
	}
	src, err := os.ReadFile("main.go")
	if err != nil || !strings.Contains(string(src), "log.SetOutput(redacting{os.Stderr})") {
		t.Error("main does not install the redacting writer")
	}
}
