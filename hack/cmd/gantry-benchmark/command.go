// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type commandRunner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// streamingCommandRunner is an optional capability. Long image builds and
// pushes emit progress for many minutes, so callers stream it live instead of
// buffering until the process exits.
type streamingCommandRunner interface {
	RunStreaming(ctx context.Context, stdin []byte, progress io.Writer, name string, args ...string) ([]byte, error)
}

type execCommandRunner struct {
	directory string
}

func (r execCommandRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)

	command.Dir = r.directory
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}

	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}

	return output, nil
}

func (r execCommandRunner) RunStreaming(
	ctx context.Context,
	stdin []byte,
	progress io.Writer,
	name string,
	args ...string,
) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)

	command.Dir = r.directory
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}

	var output bytes.Buffer

	// os/exec serializes writes when Stdout and Stderr are the same value.
	sink := io.Writer(io.MultiWriter(&output, progress))
	command.Stdout = sink
	command.Stderr = sink

	if err := command.Run(); err != nil {
		return output.Bytes(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(output.String()))
	}

	return output.Bytes(), nil
}

// prefixWriter reproduces child-process output one line at a time with a
// prefix. Carriage returns terminate a line so redrawn progress bars stream
// rather than accumulating into a single unbounded line.
type prefixWriter struct {
	target  io.Writer
	prefix  string
	pending []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)

	for {
		index := bytes.IndexAny(w.pending, "\n\r")
		if index < 0 {
			break
		}

		line := w.pending[:index]
		w.pending = w.pending[index+1:]

		w.emit(line)
	}

	return len(p), nil
}

func (w *prefixWriter) Flush() {
	if len(w.pending) == 0 {
		return
	}

	line := w.pending
	w.pending = nil

	w.emit(line)
}

func (w *prefixWriter) emit(line []byte) {
	trimmed := strings.TrimRight(string(line), " \t")
	if strings.TrimSpace(trimmed) == "" {
		return
	}

	writeAll(w.target, w.prefix+trimmed+"\n")
}

func writeAll(writer io.Writer, value string) {
	if writer == nil {
		return
	}

	_, _ = io.WriteString(writer, value) //nolint:errcheck // CLI progress output is best effort.
}
