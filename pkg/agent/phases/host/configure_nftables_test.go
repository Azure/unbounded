// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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

// TestNFTablesFlushUnitDoesNotOrderTheMachines guards the removed Before= on
// the nspawn template; see the comment in the unit.
func TestNFTablesFlushUnitDoesNotOrderTheMachines(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nftablesFlushServiceTemplate.Execute(&buf, map[string]string{"NFTablesClearPath": nftablesClearPath}))
	assert.NotContains(t, buf.String(), "Before=systemd-nspawn@.service")
}

// TestNFTablesFlushUnitOutranksTheImageFirewall pins the boot ordering that
// makes the flush mean anything on an image with a firewall of its own.
//
// The flush hands a clean ruleset to a node that has not started yet. That only
// holds if nothing reinstalls rules after it. Azure Container Linux enables
// iptables.service, which loads an INPUT policy of DROP, and it was starting
// after the flush: the flush ran, iptables.service put the policy back, and the
// node came up with kubelet unreachable from the control plane on every boot.
// Nothing fails at install time and the node still reaches Ready, so only this
// test keeps a later edit to the unit from quietly dropping the ordering. The
// machines order themselves after the flush in their service override; see
// TestNFTablesFlushUnitDoesNotOrderTheMachines.
func TestNFTablesFlushUnitOutranksTheImageFirewall(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, nftablesFlushServiceTemplate.Execute(&buf, map[string]string{
		"NFTablesClearPath": nftablesClearPath,
	}))

	unit := buf.String()

	after := ""

	for line := range strings.SplitSeq(unit, "\n") {
		if strings.HasPrefix(line, "After=") {
			after = line
		}
	}

	require.NotEmpty(t, after, "the unit must order itself after the image's firewall units")

	for _, other := range []string{"iptables.service", "ip6tables.service", "nftables.service"} {
		assert.Contains(t, after, other,
			"a firewall unit starting after the flush undoes it")
	}

	// Ordering only, never a dependency: pulling these in would start a
	// firewall on a host that had deliberately disabled one.
	for _, dependency := range []string{"Wants=", "Requires=", "BindsTo="} {
		assert.NotContains(t, unit, dependency+"iptables.service")
	}
}
