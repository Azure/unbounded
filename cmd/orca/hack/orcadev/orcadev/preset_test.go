// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/Azure/unbounded/internal/unbounded"
)

func TestValidatePreset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		wantErr bool
	}{
		{PresetDev, false},
		{"none", false},
		{"", true},
		{"prod", true},
		{"kind-dev", true}, // old name; intentionally not accepted
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			err := validatePreset(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validatePreset(%q) = nil; want error", tt.in)
				}

				return
			}

			if err != nil {
				t.Errorf("validatePreset(%q) = %v; want nil", tt.in, err)
			}
		})
	}
}

// TestApplyPresetDevAzureblob asserts the azureblob-flavor defaults
// produced by the dev preset. This is the out-of-the-box path that
// `bin/orcadev <verb>` exercises.
func TestApplyPresetDevAzureblob(t *testing.T) {
	t.Parallel()

	g := &globalFlags{}
	applyPresetDev(g)

	if g.preset != "" {
		t.Errorf("applyPresetDev should not set preset itself (set by caller); got %q", g.preset)
	}

	if g.orcaURL != devOrcaURL {
		t.Errorf("orcaURL = %q want %q", g.orcaURL, devOrcaURL)
	}

	if g.originDriver != "azureblob" {
		t.Errorf("originDriver = %q want azureblob", g.originDriver)
	}

	if g.originID != "azureblob-azurite" {
		t.Errorf("originID = %q want azureblob-azurite", g.originID)
	}

	if g.originBucket != "orca-test" {
		t.Errorf("originBucket = %q want orca-test", g.originBucket)
	}

	if g.originEndpoint != devOriginAzuriteEndpoint {
		t.Errorf("originEndpoint = %q want %q", g.originEndpoint, devOriginAzuriteEndpoint)
	}

	if g.originAccount != "devstoreaccount1" {
		t.Errorf("originAccount = %q want devstoreaccount1", g.originAccount)
	}

	if g.originAccountKey != azuriteWellKnownDevKey {
		t.Errorf("originAccountKey should be Azurite well-known dev key")
	}

	if g.cachestoreEndpoint != devCachestoreEndpoint {
		t.Errorf("cachestoreEndpoint = %q want %q", g.cachestoreEndpoint, devCachestoreEndpoint)
	}

	if g.cachestoreBucket != "orca-cache" {
		t.Errorf("cachestoreBucket = %q want orca-cache", g.cachestoreBucket)
	}

	if !g.autoPortForward {
		t.Error("autoPortForward should default true under preset=dev")
	}

	if g.kubeContext != "" {
		t.Errorf("kubeContext should default to empty (current context); got %q", g.kubeContext)
	}
}

// TestApplyPresetDevAWSS3 covers the awss3 origin variant of the dev
// preset. The applyPresetDevAWSS3 path is what
// `bin/orcadev --origin-driver=awss3 ...` triggers when the operator
// did not override the origin-* fields individually.
func TestApplyPresetDevAWSS3(t *testing.T) {
	t.Parallel()

	g := defaultGlobalFlags()
	g.originDriver = "awss3"

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("origin-id", "", "")
	flags.String("origin-bucket", "", "")
	flags.String("origin-endpoint", "", "")
	flags.String("origin-region", "", "")
	flags.String("origin-access-key", "", "")
	flags.String("origin-secret-key", "", "")
	flags.Bool("origin-use-path-style", false, "")

	applyPresetDevAWSS3(g, flags)

	if g.originID != "awss3-garage" {
		t.Errorf("originID = %q want awss3-garage", g.originID)
	}

	if g.originBucket != "orca-origin" {
		t.Errorf("originBucket = %q want orca-origin", g.originBucket)
	}

	if g.originEndpoint != devOriginAWSS3Endpoint {
		t.Errorf("originEndpoint = %q want %q", g.originEndpoint, devOriginAWSS3Endpoint)
	}

	if !g.originUsePathStyle {
		t.Error("originUsePathStyle should be true under awss3 dev preset")
	}
}

// TestApplyPresetDevAWSS3RespectsOverrides verifies that
// applyPresetDevAWSS3 honors flags.Changed: an operator-supplied
// origin-bucket wins over the awss3 dev default.
func TestApplyPresetDevAWSS3RespectsOverrides(t *testing.T) {
	t.Parallel()

	g := defaultGlobalFlags()
	g.originDriver = "awss3"
	g.originBucket = "custom-bucket"

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("origin-id", "", "")
	flags.String("origin-bucket", "", "")
	flags.String("origin-endpoint", "", "")
	flags.String("origin-region", "", "")
	flags.String("origin-access-key", "", "")
	flags.String("origin-secret-key", "", "")
	flags.Bool("origin-use-path-style", false, "")

	if err := flags.Set("origin-bucket", "custom-bucket"); err != nil {
		t.Fatalf("flags.Set: %v", err)
	}

	applyPresetDevAWSS3(g, flags)

	if g.originBucket != "custom-bucket" {
		t.Errorf("origin-bucket override lost; got %q want custom-bucket", g.originBucket)
	}

	if g.originID != "awss3-garage" {
		t.Errorf("originID should still pick up dev default; got %q", g.originID)
	}
}

// TestDefaultPresetMatchesDevDefaults asserts the long-standing
// guarantee that `bin/orcadev <verb>` works zero-config against a
// fresh dev install: the default globalFlags are the dev preset.
func TestDefaultPresetMatchesDevDefaults(t *testing.T) {
	t.Parallel()

	g := defaultGlobalFlags()

	if g.preset != PresetDev {
		t.Errorf("default preset = %q want %q", g.preset, PresetDev)
	}

	if g.namespace != unbounded.SystemNamespace() {
		t.Errorf("default namespace = %q want %q", g.namespace, unbounded.SystemNamespace())
	}

	// Cross-check a few high-value preset fields so a future
	// refactor that splits applyPresetDev from defaultGlobalFlags
	// can't silently change behavior.
	if g.originDriver != "azureblob" {
		t.Errorf("default originDriver = %q want azureblob", g.originDriver)
	}

	if g.cachestoreEndpoint != devCachestoreEndpoint {
		t.Errorf("default cachestoreEndpoint = %q want %q", g.cachestoreEndpoint, devCachestoreEndpoint)
	}
}

// TestSupportedPresetsListed makes sure validatePreset's accepted
// set is exactly what we document in the help text.
func TestSupportedPresetsListed(t *testing.T) {
	t.Parallel()

	want := []string{PresetDev, "none"}

	if len(supportedPresets) != len(want) {
		t.Fatalf("supportedPresets = %v want %v", supportedPresets, want)
	}

	for i, w := range want {
		if supportedPresets[i] != w {
			t.Errorf("supportedPresets[%d] = %q want %q", i, supportedPresets[i], w)
		}
	}
}

// TestPresetUnknownProducesActionableError is a sanity check that
// validatePreset's error text names the offending value and lists
// the accepted ones, so an operator can fix the typo from the
// terminal output alone.
func TestPresetUnknownProducesActionableError(t *testing.T) {
	t.Parallel()

	err := validatePreset("dvelopment")
	if err == nil {
		t.Fatal("expected error for unknown preset")
	}

	msg := err.Error()
	if !strings.Contains(msg, "dvelopment") || !strings.Contains(msg, PresetDev) {
		t.Errorf("error %q should name the bad value and the supported set", msg)
	}
}
