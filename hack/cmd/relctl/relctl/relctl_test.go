// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootRejectsAnUnknownSubcommand pins behavior that was silently wrong.
//
// cobra skips argument validation entirely for a command it cannot run, so
// Args: cobra.NoArgs did nothing on a root with no RunE, and `relctl bogus`
// printed help and exited zero. A mistyped release command must not look like
// success, and this is the kind of fault nothing else would catch: the tool
// appears to work, having done nothing.
func TestRootRejectsAnUnknownSubcommand(t *testing.T) {
	t.Parallel()

	cmd := Root()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"bogus-subcommand"})

	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("ExecuteContext: want an error for an unknown subcommand")
	}

	if !strings.Contains(err.Error(), "bogus-subcommand") {
		t.Errorf("error = %q, want it to name the offending argument", err)
	}
}

// TestRootWithNoArgumentsShowsHelp keeps the ordinary invocation friendly: no
// arguments is a request for help, not a mistake.
func TestRootWithNoArgumentsShowsHelp(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("output does not look like help:\n%s", out.String())
	}
}

// TestRootDefaultsToTheUnboundedRepo covers the flag every command reads.
func TestRootDefaultsToTheUnboundedRepo(t *testing.T) {
	t.Parallel()

	cmd := Root()

	flag := cmd.PersistentFlags().Lookup("repo")
	if flag == nil {
		t.Fatal("no --repo flag")
	}

	if flag.DefValue != "Azure/unbounded" {
		t.Errorf("--repo default = %q, want Azure/unbounded", flag.DefValue)
	}
}
