// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallerProxyCAOwnershipAndRotation(t *testing.T) {
	for _, state := range []string{"absent", "same", "different"} {
		t.Run(state, func(t *testing.T) {
			i, _, restarts := installerFixture(t)

			cfg := testManagedConfig(t)
			if state != "absent" {
				require.NoError(t, os.MkdirAll(filepath.Dir(i.path(proxyCAPath)), 0o755))

				cert := cfg.CACert
				if state == "different" {
					cert = testCertificate()
				}

				require.NoError(t, os.WriteFile(i.path(proxyCAPath), []byte(cert), 0o644))
			}

			err := i.Reconcile(t.Context(), cfg)
			if state == "different" {
				require.ErrorContains(t, err, "different certificate")
				require.Zero(t, *restarts)

				return
			}

			require.NoError(t, err)

			if state == "same" {
				require.NoFileExists(t, i.path(ownedCAPath))
				require.NoError(t, i.Reconcile(t.Context(), Config{Mode: ExternalMode}))
				require.FileExists(t, i.path(proxyCAPath), "preexisting shared CA must remain")

				return
			}

			require.FileExists(t, i.path(ownedCAPath))

			cfg.CACert = testCertificate()
			require.NoError(t, i.Reconcile(t.Context(), cfg))
			require.Equal(t, 2, *restarts)

			data, err := os.ReadFile(i.path(proxyCAPath))
			require.NoError(t, err)
			require.Equal(t, cfg.CACert, string(data))
			require.NoError(t, i.Reconcile(t.Context(), Config{Mode: ExternalMode}))
			require.NoFileExists(t, i.path(proxyCAPath))
		})
	}
}

func TestInstallerPodTokenFeatureFlagOverridesConfiguration(t *testing.T) {
	i, _, _ := installerFixture(t)
	require.NoError(t, os.WriteFile(i.path("/var/lib/kubelet/config.yaml"), []byte("featureGates:\n  KubeletServiceAccountTokenForCredentialProviders: false\n"), 0o644))
	require.NoError(t, i.checkPodTokenFeature([]string{
		"--config", "/var/lib/kubelet/config.yaml",
		"--feature-gates", "OtherFeature=true,KubeletServiceAccountTokenForCredentialProviders=true",
	}))
}

func TestInstallerRejectsExplicitlyDisabledPodTokens(t *testing.T) {
	for _, viaConfig := range []bool{false, true} {
		t.Run(fmt.Sprint(viaConfig), func(t *testing.T) {
			i, args, restarts := installerFixture(t)
			if viaConfig {
				require.NoError(t, os.WriteFile(i.path("/var/lib/kubelet/config.yaml"), []byte("featureGates:\n  KubeletServiceAccountTokenForCredentialProviders: false\n"), 0o644))
			} else {
				*args = append(*args, "--feature-gates=KubeletServiceAccountTokenForCredentialProviders=false")
			}

			require.ErrorContains(t, i.Reconcile(t.Context(), testManagedConfig(t)), "explicitly disabled")
			require.Zero(t, *restarts)
			require.NoFileExists(t, i.path(ownerPath))
		})
	}
}

func TestInstallerPreservesForeignEditsToOwnedCA(t *testing.T) {
	i, _, _ := installerFixture(t)
	cfg := testManagedConfig(t)
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.NoError(t, os.WriteFile(i.path(proxyCAPath), []byte(testCertificate()), 0o644))
	require.ErrorContains(t, i.Reconcile(t.Context(), cfg), "different certificate")
	require.ErrorContains(t, i.Reconcile(t.Context(), Config{Mode: ExternalMode}), "changed ownership")
	require.FileExists(t, i.path(proxyCAPath))
}
