// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"bytes"
	"strings"
	"testing"
)

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
