package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
