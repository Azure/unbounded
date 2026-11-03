// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Turning LocalDNS off deletes its unit file. Stop and Upgrade derive the stack
// from the unit files, so a unit deleted while running would never be stopped
// and CoreDNS would keep the node's listeners.
func TestPrepareStopsALocalDNSUnitItIsAboutToRemove(t *testing.T) {
	b, rec := newBackend(t)
	unit := filepath.Join(b.SystemdDir, UnitLocalDNS)
	require.NoError(t, os.WriteFile(unit, []byte("[Service]\n"), 0o644))

	b.Units.LocalDNS = false

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	stop, disable := -1, -1

	for i, c := range rec.calls {
		switch c {
		case "systemctl stop " + UnitLocalDNS:
			stop = i
		case "systemctl disable " + UnitLocalDNS:
			disable = i
		}
	}

	require.GreaterOrEqual(t, stop, 0, "the running unit is stopped: %v", rec.calls)
	require.Greater(t, disable, stop, "then disabled")
	require.NoFileExists(t, unit)
}

func TestPrepareDoesNotTouchSystemdForLocalDNSItNeverHad(t *testing.T) {
	b, rec := newBackend(t)
	b.Units.LocalDNS = false

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)

	for _, c := range rec.calls {
		require.NotContains(t, c, UnitLocalDNS)
	}
}

// If the unit cannot be stopped it is not deleted, because deleting it would
// leave it running and unmanaged.
func TestPrepareKeepsTheLocalDNSUnitWhenItCannotBeStopped(t *testing.T) {
	b, rec := newBackend(t)
	unit := filepath.Join(b.SystemdDir, UnitLocalDNS)
	require.NoError(t, os.WriteFile(unit, []byte("[Service]\n"), 0o644))

	b.Units.LocalDNS = false
	rec.fail["systemctl stop "+UnitLocalDNS] = errors.New("stuck")

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.ErrorContains(t, err, "stuck")
	require.FileExists(t, unit)
}

// With LocalDNS on there is nothing to stop.
func TestPrepareWithLocalDNSOnDoesNotStopIt(t *testing.T) {
	b, rec := newBackend(t)
	b.Units.LocalDNS = true

	_, err := b.Prepare(context.Background(), isolation.NodeSpec{Slot: "v1"})
	require.NoError(t, err)
	require.NotContains(t, rec.calls, "systemctl stop "+UnitLocalDNS)
}
