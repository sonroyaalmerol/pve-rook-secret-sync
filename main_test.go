package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"sync", "extra"}, io.Discard, &stderr, nil); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Fatalf("unexpected stderr %q", stderr.String())
	}
}

func TestPreferredConfigPath(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.json")
	local := filepath.Join(dir, "local.json")
	if got := preferredConfigPath(shared, local); got != local {
		t.Fatalf("path = %q, want %q", got, local)
	}
	if err := os.WriteFile(shared, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := preferredConfigPath(shared, local); got != shared {
		t.Fatalf("path = %q, want %q", got, shared)
	}
}

func TestRunRejectsNegativeInterval(t *testing.T) {
	var stderr bytes.Buffer
	args := []string{"sync", "-config", "unused", "-interval", "-1s"}
	if code := run(context.Background(), args, io.Discard, &stderr, nil); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-interval must not be negative") {
		t.Fatalf("unexpected stderr %q", stderr.String())
	}
}

func TestRunBootstrapHelp(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"bootstrap", "-h"}, io.Discard, &stderr, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "-dry-run") {
		t.Fatalf("unexpected help %q", stderr.String())
	}
}

func TestRunMigrationCommandHelp(t *testing.T) {
	for _, command := range []string{"bundle", "provision", "rotate"} {
		t.Run(command, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(context.Background(), []string{command, "-h"}, io.Discard, &stderr, nil); code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if stderr.Len() == 0 {
				t.Fatal("help was empty")
			}
		})
	}
}

func TestWatchRunsImmediatelyAndOnSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	trigger := make(chan os.Signal, 1)
	calls := make(chan struct{}, 2)
	done := make(chan struct{})
	var stderr bytes.Buffer
	go func() {
		watch(ctx, time.Hour, trigger, func(context.Context) error {
			calls <- struct{}{}
			return errors.New("temporary failure")
		}, &stderr)
		close(done)
	}()

	for i := range 2 {
		if i == 1 {
			trigger <- syscall.SIGHUP
		}
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("synchronization did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
	if !strings.Contains(stderr.String(), "temporary failure") {
		t.Fatalf("unexpected stderr %q", stderr.String())
	}
}
