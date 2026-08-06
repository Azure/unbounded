// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// signalOnFirstWrite creates syncPath as soon as any output arrives, which lets
// a child process block until it observes that the parent already received it.
type signalOnFirstWrite struct {
	syncPath string
	mu       sync.Mutex
	buffer   bytes.Buffer
	signaled bool
}

func (w *signalOnFirstWrite) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buffer.Write(p)

	if !w.signaled {
		w.signaled = true
		if err := os.WriteFile(w.syncPath, []byte("go\n"), 0o600); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

func (w *signalOnFirstWrite) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buffer.String()
}

// The child only emits its second line after the parent has already consumed
// the first, so this deadlocks and times out if output is buffered until exit.
func TestRunStreamingDeliversOutputBeforeProcessExits(t *testing.T) {
	directory := t.TempDir()
	syncPath := filepath.Join(directory, "received.flag")
	progress := &signalOnFirstWrite{syncPath: syncPath}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	script := fmt.Sprintf(
		"echo first; while [ ! -f %q ]; do sleep 0.01; done; echo second",
		syncPath,
	)

	output, err := execCommandRunner{directory: directory}.RunStreaming(ctx, nil, progress, "sh", "-c", script)
	if err != nil {
		t.Fatalf("RunStreaming: %v", err)
	}

	for _, want := range []string{"first", "second"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("returned output %q missing %q", string(output), want)
		}

		if !strings.Contains(progress.String(), want) {
			t.Fatalf("streamed output %q missing %q", progress.String(), want)
		}
	}
}

func TestRunStreamingCapturesStderrAndReportsFailure(t *testing.T) {
	var progress bytes.Buffer

	output, err := execCommandRunner{directory: t.TempDir()}.RunStreaming(
		context.Background(), nil, &progress, "sh", "-c", "echo to-stderr >&2; exit 3",
	)
	if err == nil {
		t.Fatal("RunStreaming succeeded, want failure")
	}

	if !strings.Contains(string(output), "to-stderr") {
		t.Fatalf("returned output = %q, want stderr content", string(output))
	}

	if !strings.Contains(progress.String(), "to-stderr") {
		t.Fatalf("streamed output = %q, want stderr content", progress.String())
	}
}

func TestPrefixWriterSplitsProgressRedrawsAndFlushesRemainder(t *testing.T) {
	var target bytes.Buffer

	writer := &prefixWriter{target: &target, prefix: "  [push] "}

	// Carriage returns are how push progress bars redraw in place.
	if _, err := writer.Write([]byte("Copying blob 10%\rCopying blob 60%\rCopying blob 100%\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := writer.Write([]byte("trailing without newline")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if strings.Contains(target.String(), "trailing") {
		t.Fatalf("partial line emitted before Flush: %q", target.String())
	}

	writer.Flush()

	want := []string{
		"  [push] Copying blob 10%",
		"  [push] Copying blob 60%",
		"  [push] Copying blob 100%",
		"  [push] trailing without newline",
	}
	if got := strings.Split(strings.TrimRight(target.String(), "\n"), "\n"); !slicesEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestPrefixWriterSkipsBlankLines(t *testing.T) {
	var target bytes.Buffer

	writer := &prefixWriter{target: &target, prefix: "> "}

	if _, err := writer.Write([]byte("\n\r\n   \nreal\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if target.String() != "> real\n" {
		t.Fatalf("output = %q, want %q", target.String(), "> real\n")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
