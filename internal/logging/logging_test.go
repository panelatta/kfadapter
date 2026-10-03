package logging

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestErrorIsSingleBoundedLine(t *testing.T) {
	if Error(nil) != "" {
		t.Fatal("nil error rendered")
	}
	if got := Error(errors.New("first\nsecond\t third")); got != "first second third" {
		t.Fatalf("got %q", got)
	}
	long := Error(errors.New(strings.Repeat("界", 400)))
	if len(long) > maxErrorBytes+len("…") || !strings.HasSuffix(long, "…") {
		t.Fatalf("long error not truncated: %d bytes", len(long))
	}
}

func TestNewWritesText(t *testing.T) {
	var buffer bytes.Buffer
	New(&buffer).Info("ready", "proxy", "socks5://127.0.0.1:10808")
	if !strings.Contains(buffer.String(), "msg=ready") {
		t.Fatalf("log = %q", buffer.String())
	}
}
