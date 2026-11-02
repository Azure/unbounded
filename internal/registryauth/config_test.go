// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	testRegistry       = "example.azurecr.io"
	testUpstream       = "upstream_registries:\n  - name: example.azurecr.io\n    endpoint: https://example.azurecr.io\n"
	testClientID       = "11111111-2222-3333-4444-555555555555"
	testTenantID       = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testIdentityPolicy = "mode: workloadIdentity\nclientID: " + testClientID + "\ntenantID: " + testTenantID + "\n"
)

func TestDerive(t *testing.T) {
	cases := []struct {
		name, upstream, policy string
		want                   []string
		wantMode, wantErr      string
	}{
		{name: "no inferred identity", upstream: testUpstream, wantMode: "external"},
		{name: "explicit federated identity", upstream: testUpstream, policy: testIdentityPolicy, want: []string{testRegistry}, wantMode: "workloadIdentity"},
		{name: "late ACR addition", upstream: testUpstream, policy: testIdentityPolicy, want: []string{testRegistry}, wantMode: "workloadIdentity"},
		{name: "placeholder", upstream: "upstream_registries:\n- name: registry.example.com\n  endpoint: https://registry.example.com", policy: testIdentityPolicy, wantMode: "workloadIdentity"},
		{name: "shared identity", upstream: testUpstream + "    credentials_path: /etc/gantry/registry/credentials\n", policy: testIdentityPolicy, wantMode: "workloadIdentity"},
		{name: "external", upstream: testUpstream, policy: "mode: external", wantMode: "external"},
		{name: "unknown mode", upstream: testUpstream, policy: "mode: auto", wantErr: "mode must"},
		{name: "unknown field", upstream: testUpstream, policy: "unknownField: typo", wantErr: "parse node-auth"},
		{name: "missing client", upstream: testUpstream, policy: "mode: workloadIdentity\ntenantID: " + testTenantID, wantErr: "clientID"},
		{name: "missing tenant", upstream: testUpstream, policy: "mode: workloadIdentity\nclientID: " + testClientID, wantErr: "tenantID"},
		{name: "invalid client", upstream: testUpstream, policy: strings.Replace(testIdentityPolicy, testClientID, "not-a-uuid", 1), wantErr: "UUID"},
		{name: "policy registry injection", upstream: testUpstream, policy: testIdentityPolicy + "registries: [other.azurecr.io]", wantErr: "must come from"},
		{name: "insecure endpoint", upstream: strings.ReplaceAll(testUpstream, "https:", "http:"), policy: testIdentityPolicy, wantErr: "matching HTTPS"},
		{name: "mismatched endpoint", upstream: strings.Replace(testUpstream, "https://example.azurecr.io", "https://other.azurecr.io", 1), policy: testIdentityPolicy, wantErr: "matching HTTPS"},
		{name: "userinfo", upstream: strings.Replace(testUpstream, "https://", "https://user:password@", 1), policy: testIdentityPolicy, wantErr: "matching HTTPS"},
		{name: "probe", upstream: testUpstream, policy: testIdentityPolicy + "probeImage: example.azurecr.io/private/image:v1", want: []string{testRegistry}, wantMode: "workloadIdentity"},
		{name: "probe wrong upstream", upstream: testUpstream, policy: testIdentityPolicy + "probeImage: another.azurecr.io/private/image:v1", wantErr: "probeImage"},
		{name: "malformed upstream", upstream: "upstream_registries: [", policy: testIdentityPolicy, wantErr: "parse Gantry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Derive(tc.upstream, tc.policy)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantMode, cfg.Mode)
			require.Equal(t, tc.want, cfg.Registries)
		})
	}
}

func TestDeriveSortsDeduplicatesAndPreservesClientID(t *testing.T) {
	upstream := testUpstream + "  - name: other.azurecr.io\n    endpoint: https://other.azurecr.io\n  - name: example.azurecr.io\n    endpoint: https://example.azurecr.io\n"
	cfg, err := Derive(upstream, testIdentityPolicy)
	require.NoError(t, err)
	require.Equal(t, testClientID, cfg.ClientID)
	require.Equal(t, testTenantID, cfg.TenantID)
	require.Equal(t, []string{testRegistry, "other.azurecr.io"}, cfg.Registries)
	data, err := cfg.ProviderConfig()
	require.NoError(t, err)

	var wire struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Providers  []struct {
			Name        string
			MatchImages []string
			Args        []string
			APIVersion  string
		}
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Equal(t, "kubelet.config.k8s.io/v1", wire.APIVersion)
	require.Equal(t, "CredentialProviderConfig", wire.Kind)
	require.Len(t, wire.Providers, 1)
	require.Equal(t, ProviderName, wire.Providers[0].Name)
	require.Equal(t, cfg.Registries, wire.Providers[0].MatchImages)
	require.Equal(t, providerVersion, wire.Providers[0].APIVersion)
	require.Equal(t, []string{"acr-credential-provider", "--socket=" + BrokerSocketPath, "--registry=" + testRegistry, "--registry=other.azurecr.io"}, wire.Providers[0].Args)
	require.NotContains(t, string(data), "tokenAttributes", "application ServiceAccounts must not be enrolled")
	require.NotContains(t, string(data), testClientID, "the host adapter must not have an Azure identity or assertion")
	require.NotContains(t, string(data), "TOKEN_FILE")
}

func TestACRValidationAndImages(t *testing.T) {
	for _, host := range []string{testRegistry, "example-hash.azurecr.io", "example.eastus2.geo.azurecr.io"} {
		require.True(t, IsACR(host), host)
	}

	for _, host := range []string{"example.azurecr.io.evil.test", "example.azurecr.io:443", "127.0.0.1", "user@example.azurecr.io", "https://example.azurecr.io", "example.azurecr.cn", "a/b.azurecr.io"} {
		require.False(t, IsACR(host), host)
	}

	for _, image := range []string{"example.azurecr.io", "example.azurecr.io/../image:v1", "example.azurecr.io/repo?secret=1", "example.azurecr.io/repo:", "example.azurecr.io//repo:v1", "example.azurecr.io/repo:tag/other"} {
		_, _, _, err := ParseImage(image)
		require.Error(t, err, image)
	}

	host, repo, ref, err := ParseImage("example.azurecr.io/private/image@sha256:" + strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Equal(t, testRegistry, host)
	require.Equal(t, "private/image", repo)
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), ref)
	_, taggedRepo, taggedRef, err := ParseImage("example.azurecr.io/private/image:v1@sha256:" + strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Equal(t, repo, taggedRepo)
	require.Equal(t, ref, taggedRef)
}
