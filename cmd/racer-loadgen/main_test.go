// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/version"
)

func TestCLIProcess(t *testing.T) {
	if os.Getenv("RACER_LOADGEN_TEST_CLI") != "1" {
		return
	}

	os.Args = append([]string{"racer-loadgen"}, os.Args[3:]...)

	main()
	os.Exit(0)
}

func TestCLI(t *testing.T) {
	t.Setenv("RACER_LOADGEN_TEST_CLI", "1")

	for _, tc := range []struct {
		name       string
		args       []string
		wantError  bool
		wantStderr string
	}{
		{name: "subcommand", args: []string{"version"}},
		{name: "short-flag", args: []string{"-version"}},
		{name: "long-flag", args: []string{"--version"}},
		{name: "explicit-true", args: []string{"-version=true"}},
		{name: "before-network", args: []string{"-endpoint=invalid", "-listen=invalid", "--version"}},
		{name: "before-validation", args: []string{"-footprint=invalid", "--version"}},
		{name: "explicit-false", args: []string{"-version=false", "-cache-uid=cli-uid", "-footprint=invalid"}, wantError: true, wantStderr: "footprint:"},
		{name: "unknown-flag", args: []string{"--version", "-unknown"}, wantError: true, wantStderr: "flag provided but not defined"},
		{name: "invalid-boolean", args: []string{"--version=invalid"}, wantError: true, wantStderr: "invalid boolean value"},
		{name: "unexpected-argument", args: []string{"--version", "unexpected"}, wantError: true, wantStderr: "unexpected arguments"},
		{name: "help", args: []string{"-h"}, wantStderr: "print version and exit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, tc.args...)...)

			var stdout, stderr bytes.Buffer

			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()

			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}

			if (err != nil) != tc.wantError {
				t.Fatalf("exit error = %v, stderr = %q", err, stderr.String())
			}

			wantStdout := version.String() + "\n"
			if tc.wantStderr != "" {
				wantStdout = ""

				if !strings.Contains(stderr.String(), tc.wantStderr) {
					t.Fatalf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("unexpected stderr: %s", &stderr)
			}

			if stdout.String() != wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
			}
		})
	}
}
