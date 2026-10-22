// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// fakeCommands stands in for the agent and init implementations and records
// what the root command dispatched to them.
type fakeCommands struct {
	agentErr error
	initErr  error

	calls     []string
	agentArgs []string
	initDir   string
}

// execute runs the root command with args and returns what it printed.
func (f *fakeCommands) execute(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()

	root := newCmdRoot(
		func(args []string) error {
			f.calls = append(f.calls, "agent")
			f.agentArgs = args

			return f.agentErr
		},
		func(dir string, logger *slog.Logger) error {
			f.calls = append(f.calls, "init")
			f.initDir = dir

			if logger == nil {
				t.Error("init got no logger")
			}

			return f.initErr
		},
	)

	var out bytes.Buffer

	// A nil argument list would make cobra read the test binary's own flags.
	root.SetArgs(append([]string{}, args...))
	root.SetOut(&out)
	root.SetErr(&out)

	err = root.Execute()

	return out.String(), err
}

func TestAgentCommandPassesArgumentsThrough(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "no flags"},
		{name: "config file", args: []string{"--config=/etc/gantry/config.yaml"}},
		{name: "single-dash flags", args: []string{"-config", "/etc/gantry/config.yaml", "-log-level", "debug"}},
		{name: "help reaches the agent's parser", args: []string{"--help"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeCommands{}

			if _, err := fake.execute(t, append([]string{"agent"}, test.args...)...); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if !slices.Equal(fake.calls, []string{"agent"}) || !slices.Equal(fake.agentArgs, test.args) {
				t.Fatalf("calls = %q with args %q, want agent with %q", fake.calls, fake.agentArgs, test.args)
			}
		})
	}
}

func TestAgentCommandReturnsAgentErrors(t *testing.T) {
	// main exits quietly on flag.ErrHelp because the agent's own parser has
	// already printed its usage, so the error must come back unchanged.
	for _, want := range []error{flag.ErrHelp, errors.New("agent failed")} {
		fake := &fakeCommands{agentErr: want}

		if _, err := fake.execute(t, "agent"); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	}
}

func TestInitCommandPreparesIdentityDirectory(t *testing.T) {
	fake := &fakeCommands{}

	if _, err := fake.execute(t, "init"); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !slices.Equal(fake.calls, []string{"init"}) || fake.initDir != identityDir {
		t.Fatalf("calls = %q with directory %q, want init with %q", fake.calls, fake.initDir, identityDir)
	}

	failure := errors.New("init failed")
	fake = &fakeCommands{initErr: failure}

	if _, err := fake.execute(t, "init"); !errors.Is(err, failure) {
		t.Fatalf("err = %v, want %v", err, failure)
	}
}

func TestInitCommandTakesNoArguments(t *testing.T) {
	for _, args := range [][]string{
		{"init", "extra"},
		{"init", "--config=/etc/gantry/config.yaml"},
	} {
		fake := &fakeCommands{}

		if _, err := fake.execute(t, args...); err == nil {
			t.Fatalf("%q succeeded, want an error", args)
		}

		if len(fake.calls) != 0 {
			t.Fatalf("%q ran %q, want nothing", args, fake.calls)
		}
	}
}

func TestVersionCommandAndFlag(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		fake := &fakeCommands{}

		stdout, err := fake.execute(t, args...)
		if err != nil {
			t.Fatalf("%q: %v", args, err)
		}

		if want := buildInfo() + "\n"; stdout != want || len(fake.calls) != 0 {
			t.Fatalf("%q printed %q and ran %q, want %q and nothing", args, stdout, fake.calls, want)
		}
	}
}

func TestRootRejectsMissingAndUnknownCommands(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "missing", wantErr: "subcommand required"},
		{name: "unknown", args: []string{"bogus"}, wantErr: `unknown command "bogus"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeCommands{}

			_, err := fake.execute(t, test.args...)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, test.wantErr)
			}

			if len(fake.calls) != 0 {
				t.Fatalf("ran %q, want nothing", fake.calls)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "lists commands", args: []string{"help"}, want: []string{"agent", "init", "snapshotter", "version"}},
		// The agent parses its own flags, so its help must list them itself.
		{name: "agent flags", args: []string{"help", "agent"}, want: []string{"Usage: gantry agent [flags]", "-config", "-libp2p-identity-path"}},
		{name: "snapshotter flags", args: []string{"help", "snapshotter"}, want: []string{"Usage: gantry snapshotter [flags]", "-address", "-fallback-snapshotter"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeCommands{}

			stdout, err := fake.execute(t, test.args...)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}

			for _, want := range test.want {
				if !strings.Contains(stdout, want) {
					t.Fatalf("help output is missing %q:\n%s", want, stdout)
				}
			}

			if len(fake.calls) != 0 {
				t.Fatalf("ran %q, want nothing", fake.calls)
			}
		})
	}
}
