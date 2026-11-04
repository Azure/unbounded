// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestExplicitRegistryOnboardingPreservesOtherRuntimeConfiguration(t *testing.T) {
	original := "# administrator comment\nmirror_listen: 0.0.0.0:5000\ncustom_setting: preserve\nupstream_registries:\n  - name: other.example\n    endpoint: https://other.example\n    ns_alias: mirror.example\n"
	policy := testIdentityPolicy + "registry: " + testRegistry + "\n"
	merged, changed, err := EnsureUpstream(original, policy)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, merged, "# administrator comment")

	var got map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(merged), &got))
	require.Equal(t, "preserve", got["custom_setting"])
	require.Equal(t, "0.0.0.0:5000", got["mirror_listen"])
	entries := got["upstream_registries"].([]any)
	require.Len(t, entries, 2)
	require.Equal(t, "other.example", entries[0].(map[string]any)["name"])
	require.Equal(t, "mirror.example", entries[0].(map[string]any)["ns_alias"])
	require.Equal(t, testRegistry, entries[1].(map[string]any)["name"])
	require.Equal(t, "https://"+testRegistry, entries[1].(map[string]any)["endpoint"])

	again, changed, err := EnsureUpstream(merged, policy)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, merged, again)

	cfg, err := Derive(merged, policy)
	require.NoError(t, err)
	require.Equal(t, testRegistry, cfg.Registry, "the auth plan must use the registry actually attached to Gantry")
}

func TestExplicitRegistryReplacesOnlyUntouchedDefaultPlaceholder(t *testing.T) {
	original := "mirror_listen: 0.0.0.0:5000\nupstream_registries:\n- name: registry.example.com\n  endpoint: https://registry.example.com\n"
	merged, changed, err := EnsureUpstream(original, testIdentityPolicy+"registry: "+testRegistry)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotContains(t, merged, "registry.example.com")
	require.Contains(t, merged, testRegistry)
}

func TestExplicitRegistryDoesNotEnrollUnrelatedACRUpstreams(t *testing.T) {
	runtime := testUpstream + "  - name: other.azurecr.io\n    endpoint: https://other.azurecr.io\n"
	policy := testIdentityPolicy + "registry: " + testRegistry
	cfg, err := Derive(runtime, policy)
	require.NoError(t, err)
	require.Equal(t, testRegistry, cfg.Registry)
	provider, err := cfg.ProviderConfig()
	require.NoError(t, err)
	require.NotContains(t, string(provider), "other.azurecr.io")

	unchanged, changed, err := EnsureUpstream(runtime, policy)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, runtime, unchanged)
}

func TestRegistryOnboardingRefusesAmbiguousOrConflictingConfiguration(t *testing.T) {
	policy := testIdentityPolicy + "registry: " + testRegistry
	for _, runtime := range []string{"", "upstream_registries: {}", "upstream_registries: []\nupstream_registries: []", testUpstream + "    credentials_path: /shared/credentials\n"} {
		_, _, err := EnsureUpstream(runtime, policy)
		require.Error(t, err, runtime)
	}

	original := testUpstream
	merged, changed, err := EnsureUpstream(original, "")
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, original, merged)
}

func TestRegistryAttachmentDoesNotOverrideInheritedUpstreams(t *testing.T) {
	runtime := "<<: &defaults\n  upstream_registries:\n  - name: other.example\n    endpoint: https://other.example\n"
	_, _, err := EnsureUpstream(runtime, testIdentityPolicy+"registry: "+testRegistry)
	require.ErrorContains(t, err, "make inherited upstream_registries explicit")
}

func TestRegistryAttachmentResolvesMergedAndAliasedNames(t *testing.T) {
	for _, alias := range []string{"", "  name: *registry\n"} {
		runtime := "upstream_registries:\n- <<: &entry\n    name: &registry " + testRegistry + "\n    endpoint: https://" + testRegistry + "\n" + alias
		merged, changed, err := EnsureUpstream(runtime, testIdentityPolicy+"registry: "+testRegistry)
		require.NoError(t, err)
		require.False(t, changed, "an effective existing name must not be duplicated")
		require.Equal(t, runtime, merged)
	}
}

func TestRegistryAttachmentRejectsUpstreamAliasCollisions(t *testing.T) {
	for _, mode := range []string{ExternalMode, IdentityBindingMode} {
		t.Run(mode, func(t *testing.T) {
			for _, entry := range []string{
				"- name: other.example\n  endpoint: https://other.example\n  ns_alias: " + testRegistry + "\n",
				"- <<: &upstream\n    name: other.example\n    endpoint: https://other.example\n    ns_alias: " + testRegistry + "\n",
			} {
				_, changed, err := EnsureUpstream("upstream_registries:\n"+entry, "mode: "+mode+"\nregistry: "+testRegistry)
				require.ErrorContains(t, err, "ns_alias")
				require.False(t, changed)
			}
		})
	}
}

func TestManagedRegistryOnboardingRejectsInvalidExistingEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://" + testRegistry, "https://other.azurecr.io", "https://" + testRegistry + "/path", "https://user@" + testRegistry} {
		t.Run(endpoint, func(t *testing.T) {
			runtime := "upstream_registries:\n- name: " + testRegistry + "\n  endpoint: " + endpoint + "\n"
			_, _, err := EnsureUpstream(runtime, testIdentityPolicy+"registry: "+testRegistry)
			require.ErrorContains(t, err, "matching HTTPS endpoint")
			unchanged, changed, err := EnsureUpstream(runtime, "mode: external\nregistry: "+testRegistry)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, runtime, unchanged, "external authentication must preserve customer endpoints")
		})
	}
}
