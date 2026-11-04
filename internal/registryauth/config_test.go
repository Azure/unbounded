// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	testRegistry = "example.azurecr.io"
	testUpstream = "upstream_registries:\n  - name: example.azurecr.io\n    endpoint: https://example.azurecr.io\n"
	testClientID = "11111111-2222-3333-4444-555555555555"
	testTenantID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

var testIdentityPolicy = "mode: aks-identity-binding\nclientID: " + testClientID + "\ntenantID: " + testTenantID +
	"\nsniName: proxy.example.com\nproxyAddress: 10.0.0.1\naudience: api://AzureADTokenExchange\ncaCert: |\n  " +
	strings.ReplaceAll(strings.TrimSpace(testCertificate()), "\n", "\n  ") + "\n"

func testCertificate() string {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}

	data, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		panic(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: data}))
}

func testManagedConfig(t *testing.T) Config {
	t.Helper()

	cfg, err := Derive(testUpstream, testIdentityPolicy+"registry: "+testRegistry)
	require.NoError(t, err)

	return cfg
}

func TestDerive(t *testing.T) {
	policy := testIdentityPolicy + "registry: " + testRegistry

	cases := []struct{ name, upstream, policy, wantMode, wantErr string }{
		{name: "no inferred identity", upstream: testUpstream, wantMode: ExternalMode},
		{name: "explicit shared identity", upstream: testUpstream, policy: policy, wantMode: IdentityBindingMode},
		{name: "placeholder", upstream: "upstream_registries:\n- name: registry.example.com\n  endpoint: https://registry.example.com", policy: policy, wantErr: "requires its registry"},
		{name: "registry absent from upstreams", upstream: "upstream_registries: []\n", policy: policy, wantErr: "requires its registry"},
		{name: "duplicate upstream keys", upstream: testUpstream + "upstream_registries: []\n", policy: policy, wantErr: "parse Gantry"},
		{name: "non-sequence upstreams", upstream: "upstream_registries: {}\n", policy: policy, wantErr: "parse Gantry"},
		{name: "shared credentials conflict", upstream: testUpstream + "    credentials_path: /shared\n", policy: policy, wantErr: "credentials_path"},
		{name: "external", upstream: "unrelated customer configuration", policy: "mode: external", wantMode: ExternalMode},
		{name: "unknown mode", policy: "mode: auto", wantErr: "mode must"},
		{name: "unknown field", policy: "unknownField: typo", wantErr: "parse node-auth"},
		{name: "missing registry", policy: testIdentityPolicy, wantErr: "explicit registry"},
		{name: "invalid client", policy: strings.Replace(policy, testClientID, "invalid", 1), wantErr: "clientID"},
		{name: "invalid tenant", policy: strings.Replace(policy, testTenantID, "", 1), wantErr: "tenantID"},
		{name: "invalid SNI", policy: strings.Replace(policy, "proxy.example.com", "https://proxy.example.com", 1), wantErr: "SNI"},
		{name: "invalid proxy", policy: strings.Replace(policy, "10.0.0.1", "localhost:443", 1), wantErr: "IP address"},
		{name: "missing audience", policy: strings.Replace(policy, "api://AzureADTokenExchange", "", 1), wantErr: "audience"},
		{name: "missing CA", policy: "mode: aks-identity-binding\nregistry: " + testRegistry + "\nclientID: " + testClientID + "\ntenantID: " + testTenantID + "\nsniName: proxy.example.com\nproxyAddress: 10.0.0.1\naudience: api://test", wantErr: "CA certificate"},
		{name: "registry injection", policy: policy + "\nregistries: [other.azurecr.io]", wantErr: "parse node-auth"},
		{name: "insecure endpoint", upstream: strings.ReplaceAll(testUpstream, "https:", "http:"), policy: policy, wantErr: "matching HTTPS"},
		{name: "mismatched endpoint", upstream: strings.Replace(testUpstream, "https://example", "https://other", 1), policy: policy, wantErr: "matching HTTPS"},
		{name: "userinfo", upstream: strings.Replace(testUpstream, "https://", "https://user@", 1), policy: policy, wantErr: "matching HTTPS"},
		{name: "malformed upstream", upstream: "upstream_registries: [", policy: policy, wantErr: "parse Gantry"},
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

			if cfg.Mode == IdentityBindingMode {
				require.Equal(t, testRegistry, cfg.Registry)
			}
		})
	}
}

func TestProviderConfigurationUsesSelectedIdentityAndRegistry(t *testing.T) {
	cfg := testManagedConfig(t)
	cfg.Registry = "another.azurecr.io"
	cfg.ClientID, cfg.TenantID = testTenantID, testClientID
	data, err := cfg.ProviderConfig()
	require.NoError(t, err)

	var wire struct {
		Providers []struct {
			MatchImages, Args []string
			TokenAttributes   map[string]any
		}
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Len(t, wire.Providers, 1)
	provider := wire.Providers[0]
	require.Equal(t, []string{cfg.Registry}, provider.MatchImages)
	require.Contains(t, provider.Args, "--ib-default-client-id="+cfg.ClientID)
	require.Contains(t, provider.Args, "--ib-default-tenant-id="+cfg.TenantID)
	require.Equal(t, cfg.Audience, provider.TokenAttributes["serviceAccountTokenAudience"])
}

func TestACRValidationAndImages(t *testing.T) {
	for _, host := range []string{testRegistry, "example-hash.azurecr.io", "example.eastus2.geo.azurecr.io"} {
		require.True(t, IsACR(host), host)
	}

	for _, host := range []string{"example.azurecr.io.evil.test", "example.azurecr.io:443", "127.0.0.1", "user@example.azurecr.io", "https://example.azurecr.io", "example.azurecr.cn", "a/b.azurecr.io"} {
		require.False(t, IsACR(host), host)
	}
}
