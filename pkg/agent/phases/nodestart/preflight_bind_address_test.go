// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const procTCPHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestPreflightBindAddresses(t *testing.T) {
	goalState := &goalstates.MachineGoalState{
		NodeStart: &goalstates.NodeStart{Containerd: goalstates.Containerd{MetricsAddress: "0.0.0.0:12345"}},
		RootFS:    &goalstates.RootFS{MachineDir: "/var/lib/machines/kube1"},
	}

	checks := Preflight(slog.New(slog.DiscardHandler), config.AgentConfig{}, goalState)

	assert.Equal(t, checkKubeletBindAddressName, checks[0].Name())
	assert.Equal(t, kubeletBindAddress, checks[0].(bindAddressChecker).address)
	assert.Equal(t, checkContainerdMetricsBindAddressName, checks[1].Name())
	assert.Equal(t, "0.0.0.0:12345", checks[1].(bindAddressChecker).address)

	// Bind checks are always ownership-aware, so an interrupted bootstrap can
	// retry while its own node keeps listening.
	for _, check := range checks[:2] {
		assert.NotNil(t, check.(bindAddressChecker).owned)
	}
}

// A partially resolved goal state must degrade to a plain port check instead of
// panicking, because callers build checks before the rootfs stage has run.
func TestPreflightBindAddressesWithoutResolvedRootFS(t *testing.T) {
	goalState := &goalstates.MachineGoalState{NodeStart: &goalstates.NodeStart{
		Containerd: goalstates.Containerd{MetricsAddress: "0.0.0.0:12345"},
	}}

	checks := Preflight(slog.New(slog.DiscardHandler), config.AgentConfig{}, goalState)
	assert.False(t, checks[0].(bindAddressChecker).owned())
}

func TestBindAddressCheckerReportsAvailable(t *testing.T) {
	checker := testBindAddressChecker(func(string) (string, bool, error) { return "", false, nil })

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.Equal(t, "kubelet bind address is available", results[0].Message)
}

func TestBindAddressCheckerReportsForeignOwner(t *testing.T) {
	checker := testBindAddressChecker(func(string) (string, bool, error) {
		return `"kubelet" (PID 123)`, true, nil
	})

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, `kubelet bind address is already in use by process "kubelet" (PID 123)`, results[0].Message)
}

func TestBindAddressCheckerReportsInspectionFailure(t *testing.T) {
	checker := testBindAddressChecker(func(string) (string, bool, error) {
		return "", false, errors.New("inspection failed")
	})

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, "kubelet bind address availability could not be determined", results[0].Message)
}

func TestInspectTCPListenerFindsIPv4Owner(t *testing.T) {
	procRoot := createProcFixture(t,
		procTCPHeader+"   0: 00000000:280A 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 45678\n",
		procTCPHeader,
	)
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "123", "fd"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "123", "comm"), []byte("kubelet\n"), 0o600))
	require.NoError(t, os.Symlink("socket:[45678]", filepath.Join(procRoot, "123", "fd", "4")))

	owner, occupied, err := inspectTCPListener(procRoot, kubeletBindAddress)

	require.NoError(t, err)
	assert.True(t, occupied)
	assert.Equal(t, `"kubelet" (PID 123)`, owner)
}

func TestInspectTCPListenerFindsIPv6Listener(t *testing.T) {
	procRoot := createProcFixture(t,
		procTCPHeader,
		procTCPHeader+"   0: 00000000000000000000000000000000:2811 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 56789\n",
	)

	owner, occupied, err := inspectTCPListener(procRoot, "0.0.0.0:10257")

	require.NoError(t, err)
	assert.True(t, occupied)
	assert.Empty(t, owner)
}

func TestInspectTCPListenerPortAvailable(t *testing.T) {
	procRoot := createProcFixture(t, procTCPHeader, procTCPHeader)

	_, occupied, err := inspectTCPListener(procRoot, kubeletBindAddress)

	require.NoError(t, err)
	assert.False(t, occupied)
}

func TestInspectTCPListenerFailsWhenTCPTableUnreadable(t *testing.T) {
	_, _, err := inspectTCPListener(t.TempDir(), kubeletBindAddress)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tcp socket table")
}

func testBindAddressChecker(inspect func(string) (string, bool, error)) bindAddressChecker {
	return bindAddressChecker{
		name:        checkKubeletBindAddressName,
		address:     kubeletBindAddress,
		description: "kubelet bind address",
		log:         slog.New(slog.DiscardHandler),
		inspect:     inspect,
	}
}

func createProcFixture(t *testing.T, tcp, tcp6 string) string {
	t.Helper()

	procRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "net", "tcp"), []byte(tcp), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "net", "tcp6"), []byte(tcp6), 0o600))

	return procRoot
}

func TestListenerOwnershipRequiresRootAndExecutableForEverySocket(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"owned", "foreign-root", "foreign-executable", "unknown-owner", "shared-with-foreign"} {
		t.Run(mode, func(t *testing.T) {
			proc := createProcFixture(t, procTCPHeader+"0: 00000000:280A 00000000:0000 0A 0 0 0 0 0 45678\n", procTCPHeader)
			root := t.TempDir()
			exe := filepath.Join(root, "kubelet")
			require.NoError(t, os.WriteFile(exe, []byte("executable"), 0o755))

			processRoot, processExe := root, exe
			if mode == "foreign-root" {
				processRoot = t.TempDir()
			}

			if mode == "foreign-executable" {
				processExe = filepath.Join(t.TempDir(), "kubelet")
				require.NoError(t, os.WriteFile(processExe, []byte("executable"), 0o755))
			}

			if mode != "unknown-owner" {
				require.NoError(t, os.MkdirAll(filepath.Join(proc, "123", "fd"), 0o755))
				require.NoError(t, os.Symlink("socket:[45678]", filepath.Join(proc, "123", "fd", "4")))
				require.NoError(t, os.Symlink(processRoot, filepath.Join(proc, "123", "root")))
				require.NoError(t, os.Symlink(processExe, filepath.Join(proc, "123", "exe")))
			}

			if mode == "shared-with-foreign" {
				require.NoError(t, os.MkdirAll(filepath.Join(proc, "456", "fd"), 0o755))
				require.NoError(t, os.Symlink("socket:[45678]", filepath.Join(proc, "456", "fd", "4")))
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(proc, "456", "root")))
				require.NoError(t, os.Symlink(exe, filepath.Join(proc, "456", "exe")))
			}

			require.Equal(t, mode == "owned", listenerOwnedByRoot(proc, kubeletBindAddress, root, "kubelet"))
		})
	}
}

// TestCheckBindAddressRejectsAnyListener exercises the exported constructor
// rather than the checker type the other tests here build directly.
//
// It exists because nothing inside the agent calls CheckBindAddress: Preflight
// is ownership-aware and uses checkOwnedBindAddress. A sweep for symbols with no
// caller therefore reads it as dead and removes it, which breaks callers outside
// the repository that compose their own preflight sets. This test is the caller
// that keeps it honest, and it pins the behavior those callers rely on: the
// unowned constructor rejects any listener at all, where the owned variant
// accepts one it can prove belongs to this installation.
func TestCheckBindAddressRejectsAnyListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	address := listener.Addr().String()
	log := slog.New(slog.DiscardHandler)

	occupied := CheckBindAddress(log, "test-bind-address", address, "test bind address")
	require.Equal(t, "test-bind-address", occupied.Name())

	results := occupied.Check(context.Background())
	require.NotEmpty(t, results)
	assert.Equal(t, preflight.SeverityError, results[0].Severity,
		"a listener this installation cannot claim must fail the unowned check")

	require.NoError(t, listener.Close())

	free := CheckBindAddress(log, "test-bind-address", address, "test bind address")
	assert.Equal(t, preflight.SeverityOK, free.Check(context.Background())[0].Severity)
}
