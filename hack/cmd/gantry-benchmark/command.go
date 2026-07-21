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

func writeAll(writer io.Writer, value string) {
	_, _ = io.WriteString(writer, value) //nolint:errcheck // CLI progress output is best effort.
}
