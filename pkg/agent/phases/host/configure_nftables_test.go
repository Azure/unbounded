// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func nftablesTask(registered bool, err error) *configureNFTables {
	return &configureNFTables{
		log: slog.New(slog.DiscardHandler),
		machineRegistered: func(context.Context, *slog.Logger) (bool, error) {
			return registered, err
		},
	}
}

// TestFlushNotStartedWhileMachineRegistered is the replay guard.
//
// Starting the flush unit applies `flush ruleset`, and the nspawn container
// shares the host network namespace, so on a host with a running node that
// erases kube-proxy, CNI and LocalDNS rules at once. Only kube-proxy resyncs;
// LocalDNS's NOTRACK table is reinstated by a unit that runs when the machine
// starts, and systemd ordering does not pull that unit in when this one is
// started on its own.
func TestFlushNotStartedWhileMachineRegistered(t *testing.T) {
	t.Parallel()

	start, err := nftablesTask(true, nil).shouldStartFlush(t.Context())
	require.NoError(t, err)
	require.False(t, start, "must not flush a ruleset a running node depends on")
}

// TestFlushStartedOnFreshHost keeps the original behavior where it is correct:
// no machine is registered, so no running node can lose rules.
func TestFlushStartedOnFreshHost(t *testing.T) {
	t.Parallel()

	start, err := nftablesTask(false, nil).shouldStartFlush(t.Context())
	require.NoError(t, err)
	require.True(t, start)
}

// TestFlushFailsClosedOnUninspectableHost keeps an inspection failure from
// being read as "nothing registered", which would flush a ruleset that may
// belong to a running node.
func TestFlushFailsClosedOnUninspectableHost(t *testing.T) {
	t.Parallel()

	injected := errors.New("machinectl unavailable")

	start, err := nftablesTask(false, injected).shouldStartFlush(t.Context())
	require.ErrorIs(t, err, injected)
	require.False(t, start)
}
