package main

import (
	"bytes"
	"log"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every log line goes through logf, which redacts it.
func TestLogfRedacts(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	logf("model server: 500: %s", "contact jane.doe@example.com or +49 30 12345678")
	if out := buf.String(); strings.Contains(out, "jane.doe") || strings.Contains(out, "12345678") ||
		!strings.Contains(out, "[email address removed]") {
		t.Errorf("logged %q", out)
	}
}

// Nothing in the gateway logs except through logf.
func TestOnlyLogfLogs(t *testing.T) {
	direct := regexp.MustCompile(`\blog\.(Print|Printf|Println|Fatal|Fatalf|Panic|Panicf)\(`)
	for _, f := range []string{"main.go", "chat.go", "pages.go", "auth.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if direct.MatchString(line) && !strings.Contains(line, "redact.String") && !strings.Contains(line, "log.Fatal(err)") {
				t.Errorf("%s:%d logs directly: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
