// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"log/slog"
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
	goalState := &goalstates.MachineGoalState{NodeStart: &goalstates.NodeStart{
		Containerd: goalstates.Containerd{MetricsAddress: "0.0.0.0:12345"},
	}}

	checks := Preflight(slog.New(slog.DiscardHandler), config.AgentConfig{}, goalState)

	assert.Equal(t, checkKubeletBindAddressName, checks[0].Name())
	assert.Equal(t, kubeletBindAddress, checks[0].(bindAddressChecker).address)
	assert.Equal(t, checkContainerdMetricsBindAddressName, checks[1].Name())
	assert.Equal(t, "0.0.0.0:12345", checks[1].(bindAddressChecker).address)
}

func TestCheckBindAddressAvailable(t *testing.T) {
	checker := testBindAddressChecker(func(string) (string, bool, error) { return "", false, nil })

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.Equal(t, "kubelet bind address is available", results[0].Message)
}

func TestCheckBindAddressInUseIncludesOwner(t *testing.T) {
	checker := testBindAddressChecker(func(string) (string, bool, error) {
		return `"kubelet" (PID 123)`, true, nil
	})

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, `kubelet bind address is already in use by process "kubelet" (PID 123)`, results[0].Message)
}

func TestCheckBindAddressInspectionFailure(t *testing.T) {
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
