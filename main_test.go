package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"sync", "extra"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Fatalf("unexpected stderr %q", stderr.String())
	}
}
