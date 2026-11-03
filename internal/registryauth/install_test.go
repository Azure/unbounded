// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testUnit = "[Service]\nExecStart=/usr/local/bin/kubelet \\\n --config=/var/lib/kubelet/config.yaml \\\n $KUBELET_NODE_CONFIG_ARGS $KUBELET_EXTRA_ARGS\n"

func installerFixture(t *testing.T) (*Installer, *[]string, *int) {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, path), []byte(content), 0o644))
	}
	write(unitPath, testUnit)
	write("/var/lib/kubelet/config.yaml", "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\n")
	write("/source-operator", "static-binary-fixture")

	args := []string{"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}
	restarts := 0
	i := &Installer{Root: root, Executable: filepath.Join(root, "source-operator"), KubeletArgs: func() ([]string, error) { return args, nil }}
	i.HostCommand = func(_ context.Context, command ...string) error {
		if command[0] == "restart" {
			restarts++

			args = []string{"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}
			if _, err := os.Stat(i.path(dropInPath)); err == nil {
				args = append(args, "--image-credential-provider-config="+providerConfigPath, "--image-credential-provider-bin-dir="+installDir+"/bin")
			}
		}

		return nil
	}

	return i, &args, &restarts
}

func TestInstallerLateInstallationIdempotencyAndDrift(t *testing.T) {
	i, args, restarts := installerFixture(t)
	cfg := testManagedConfig(t)
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.True(t, hasProviderArgs(*args))
	require.Equal(t, 1, *restarts)

	unit, err := os.ReadFile(i.path(unitPath))
	require.NoError(t, err)
	require.Equal(t, testUnit, string(unit), "agent-owned kubelet unit must not change")

	dropIn, err := os.ReadFile(i.path(dropInPath))
	require.NoError(t, err)
	require.Contains(t, string(dropIn), "$KUBELET_NODE_CONFIG_ARGS $KUBELET_EXTRA_ARGS")
	require.Contains(t, string(dropIn), managedMarker)

	binary, err := os.ReadFile(i.path(providerPath))
	require.NoError(t, err)
	require.Equal(t, "static-binary-fixture", string(binary))

	info, err := os.Stat(i.path(providerPath))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.Equal(t, 1, *restarts, "unchanged reconciliation must not restart kubelet")
	require.NoError(t, os.WriteFile(i.path(providerConfigPath), []byte("drift"), 0o644))
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.Equal(t, 2, *restarts)

	cfg.Registry = "other.azurecr.io"
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.Equal(t, 3, *restarts)

	data, err := os.ReadFile(i.path(providerConfigPath))
	require.NoError(t, err)
	require.Contains(t, string(data), "other.azurecr.io")
	require.NotContains(t, string(data), "access_token")
}

func TestInstallerRefusesConflictingOwnersAndProviders(t *testing.T) {
	for _, conflict := range []string{"provider", "directory", "drop-in", "other-drop-in", "symlink"} {
		t.Run(conflict, func(t *testing.T) {
			i, args, restarts := installerFixture(t)

			switch conflict {
			case "provider":
				*args = append(*args, "--image-credential-provider-config=/etc/kubernetes/external.json")
			case "directory":
				require.NoError(t, os.MkdirAll(i.path(installDir), 0o755))
				require.NoError(t, os.WriteFile(i.path(installDir+"/foreign"), []byte("preserve"), 0o644))
			case "drop-in":
				require.NoError(t, os.MkdirAll(filepath.Dir(i.path(dropInPath)), 0o755))
				require.NoError(t, os.WriteFile(i.path(dropInPath), []byte("foreign"), 0o644))
			case "other-drop-in":
				require.NoError(t, os.MkdirAll(filepath.Dir(i.path(dropInPath)), 0o755))
				require.NoError(t, os.WriteFile(i.path(filepath.Dir(dropInPath)+"/99-external.conf"), []byte("[Service]\nExecStart=/custom/kubelet\n"), 0o644))
			case "symlink":
				require.NoError(t, os.MkdirAll(i.path("/opt/unbounded"), 0o755))
				require.NoError(t, os.Symlink(t.TempDir(), i.path(installDir)))
			}

			require.Error(t, i.Reconcile(t.Context(), testManagedConfig(t)))
			require.Zero(t, *restarts)
		})
	}
}

func TestInstallerDoesNotOverwriteDropInAfterOwnershipChanges(t *testing.T) {
	i, _, _ := installerFixture(t)
	cfg := testManagedConfig(t)
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.NoError(t, os.WriteFile(i.path(dropInPath), []byte("foreign"), 0o644))
	require.ErrorContains(t, i.Reconcile(t.Context(), cfg), "unowned")
	require.ErrorContains(t, i.Reconcile(t.Context(), Config{Mode: "external"}), "unowned")
	data, err := os.ReadFile(i.path(dropInPath))
	require.NoError(t, err)
	require.Equal(t, "foreign", string(data))
}

func TestExternalCleanupWithoutOwnershipPreservesCustomerCASymlink(t *testing.T) {
	i, _, restarts := installerFixture(t)
	target := i.path("/customer-ca.crt")
	require.NoError(t, os.WriteFile(target, []byte("customer CA"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Dir(i.path(proxyCAPath)), 0o755))
	require.NoError(t, os.Symlink(target, i.path(proxyCAPath)))
	require.NoError(t, i.Reconcile(t.Context(), Config{Mode: ExternalMode}))
	require.Zero(t, *restarts)

	got, err := os.Readlink(i.path(proxyCAPath))
	require.NoError(t, err)
	require.Equal(t, target, got)
}

func TestInstallerRemovalAndReenableWithoutRejoin(t *testing.T) {
	i, args, restarts := installerFixture(t)
	cfg := testManagedConfig(t)
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.NoError(t, i.Reconcile(t.Context(), Config{Mode: "external"}))
	require.False(t, hasProviderArgs(*args))
	require.Equal(t, 2, *restarts)
	require.NoFileExists(t, i.path(providerPath))
	require.NoFileExists(t, i.path(dropInPath))
	require.NoError(t, i.Reconcile(t.Context(), Config{Mode: "external"}))
	require.Equal(t, 2, *restarts)
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.Equal(t, 3, *restarts)
}

func TestInstallerRecoversFromRestartFailure(t *testing.T) {
	i, _, restarts := installerFixture(t)
	run := i.HostCommand
	i.HostCommand = func(_ context.Context, args ...string) error { return errors.New("systemd unavailable") }
	cfg := testManagedConfig(t)
	require.ErrorContains(t, i.Reconcile(t.Context(), cfg), "systemd unavailable")
	require.FileExists(t, i.path(providerPath))
	i.HostCommand = run
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.Equal(t, 1, *restarts)
}

func TestInstallerTracksAgentUnitChangesAndRejectsUnsupportedUnits(t *testing.T) {
	i, _, restarts := installerFixture(t)
	cfg := testManagedConfig(t)
	require.NoError(t, i.Reconcile(t.Context(), cfg))

	updated := strings.Replace(testUnit, "/usr/local/bin/kubelet", "/usr/local/bin/new-kubelet", 1)
	require.NoError(t, os.WriteFile(i.path(unitPath), []byte(updated), 0o644))
	require.NoError(t, i.Reconcile(t.Context(), cfg))
	require.Equal(t, 2, *restarts)

	data, err := os.ReadFile(i.path(dropInPath))
	require.NoError(t, err)
	require.Contains(t, string(data), "/usr/local/bin/new-kubelet")
	require.NoError(t, os.WriteFile(i.path(unitPath), []byte("[Service]\nExecStart=/custom/kubelet\n"), 0o644))
	require.ErrorContains(t, i.Reconcile(t.Context(), cfg), "unsupported kubelet unit")
}

func TestRunningKubeletArgs(t *testing.T) {
	root := t.TempDir()
	_, err := RunningKubeletArgs(root)
	require.Error(t, err)

	for _, pid := range []string{"100", "200"} {
		dir := filepath.Join(root, "proc", pid)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "comm"), []byte("kubelet\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/usr/local/bin/kubelet\x00--config=/config\x00"), 0o644))

		args, err := RunningKubeletArgs(root)
		if pid == "100" {
			require.NoError(t, err)
			require.Equal(t, []string{"/usr/local/bin/kubelet", "--config=/config"}, args)
		} else {
			require.ErrorContains(t, err, "multiple kubelet")
		}
	}
}
