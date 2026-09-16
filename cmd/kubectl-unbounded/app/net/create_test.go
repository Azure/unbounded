// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestHealthCheckFlagsPreserveRuntimeDefaults(t *testing.T) {
	cmd := &cobra.Command{}
	flags := &healthCheckFlags{}
	flags.addToFlags(cmd)
	flags.selectedFrom(cmd)

	if flags.toObject() != nil {
		t.Fatal("omitted health flags must preserve the node's runtime defaults")
	}

	for _, name := range []string{"health-check-transmit-interval", "health-check-receive-interval"} {
		flag := cmd.Flags().Lookup(name)
		if flag.DefValue != "" || !strings.Contains(flag.Usage, "15s") {
			t.Fatalf("flag %s must document the inherited 15s default without serializing it", name)
		}
	}

	if err := cmd.Flags().Set("health-check-transmit-interval", "60s"); err != nil {
		t.Fatal(err)
	}

	flags.selectedFrom(cmd)

	got := flags.toObject()
	if len(got) != 1 || got["transmitInterval"] != "60s" {
		t.Fatalf("explicit interval or partial settings changed: %v", got)
	}
}

func TestCreateSiteUsesSharedSiteAPI(t *testing.T) {
	t.Parallel()

	cmd := newCreateSiteCommand(newPluginRuntime())
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"primary",
		"--node-cidr", "10.0.0.0/16",
		"--pod-cidr-block", "10.244.0.0/16",
		"--dry-run=client",
		"-o", "yaml",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"apiVersion: unbounded-cloud.io/v1alpha3",
		"kind: Site",
		"name: primary",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "apiVersion: net.unbounded-cloud.io/v1alpha1") {
		t.Fatalf("output used old Site API:\n%s", got)
	}
}
