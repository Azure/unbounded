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

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
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

// The nspawn flush unit is rendered from the unchanged template. Before the
// isolation modes existed that was the only unit; nspawn and the default mode must keep producing it.
func TestConfigureNFTablesForNSpawnRendersTheOriginalUnit(t *testing.T) {
	t.Parallel()

	var want bytes.Buffer
	require.NoError(t, nftablesFlushServiceTemplate.Execute(&want, map[string]string{"NFTablesClearPath": nftablesClearPath}))

	for _, mode := range []isolation.Mode{"", isolation.ModeNSpawn} {
		c, ok := ConfigureNFTablesFor(slog.New(slog.DiscardHandler), mode).(*configureNFTables)
		require.True(t, ok)
		assert.Empty(t, c.nodeUnits)

		got, err := c.renderFlushUnit()
		require.NoError(t, err)
		assert.Equal(t, want.String(), got.String())
		assert.Contains(t, got.String(), "RequiredBy=systemd-nspawn@.service")
	}

	plain, ok := ConfigureNFTables(slog.New(slog.DiscardHandler)).(*configureNFTables)
	require.True(t, ok)
	assert.Empty(t, plain.nodeUnits)
}

func TestConfigureNFTablesForHostModesOrdersTheFlushBeforeTheNodeUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode  isolation.Mode
		units []string
	}{
		{isolation.ModeNone, []string{none.UnitContainerd, none.UnitKubelet}},
	}

	for _, tt := range tests {
		c, ok := ConfigureNFTablesFor(slog.New(slog.DiscardHandler), tt.mode).(*configureNFTables)
		require.True(t, ok)
		assert.Equal(t, tt.units, c.nodeUnits)

		got, err := c.renderFlushUnit()
		require.NoError(t, err)

		unit := got.String()
		assert.Contains(t, unit, "Before="+strings.Join(tt.units, " "))
		assert.Contains(t, unit, "WantedBy=multi-user.target")
		assert.Contains(t, unit, "After=iptables.service ip6tables.service nftables.service")
		assert.Contains(t, unit, "ExecStart=/usr/sbin/nft -f "+nftablesClearPath)
		assert.NotContains(t, unit, "systemd-nspawn")
	}
}

// The host modes cannot ask machined: their hosts do not install
// systemd-container, which is what failed the none bootstrap. They look at the
// node units instead, and no units active means a fresh host that may flush.
func TestConfigureNFTablesForHostModesDoesNotNeedMachinectl(t *testing.T) {
	for _, mode := range []isolation.Mode{isolation.ModeNone} {
		c, ok := ConfigureNFTablesFor(slog.New(slog.DiscardHandler), mode).(*configureNFTables)
		require.True(t, ok)

		c.machineRegistered = anyUnitActiveWith(c.nodeUnits, func(context.Context, string) (string, error) {
			return "inactive\n", errors.New("exit status 3")
		})

		start, err := c.shouldStartFlush(t.Context())
		require.NoError(t, err)
		assert.True(t, start, "nothing in the node units is active on a fresh host")
	}
}

// Only inactive and failed mean "not running". An active unit, or a systemctl
// that gives no usable answer, keeps the ruleset: the check fails closed.
func TestAnyUnitActiveFailsClosed(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	units := []string{"a.service", "b.service"}

	answer := func(states map[string]string, errs map[string]error) unitStateFunc {
		return func(_ context.Context, unit string) (string, error) { return states[unit], errs[unit] }
	}

	exit3 := errors.New("exit status 3")

	running, err := anyUnitActiveWith(units, answer(map[string]string{"a.service": "inactive\n", "b.service": "failed\n"}, map[string]error{"a.service": exit3, "b.service": exit3}))(t.Context(), log)
	require.NoError(t, err)
	assert.False(t, running)

	for _, state := range []string{"active", "activating", "deactivating", "reloading"} {
		running, err = anyUnitActiveWith(units, answer(map[string]string{"a.service": "inactive", "b.service": state + "\n"}, map[string]error{"a.service": exit3}))(t.Context(), log)
		require.NoError(t, err, state)
		assert.True(t, running, state)
	}

	// systemctl could not be run, or could not reach systemd.
	_, err = anyUnitActiveWith(units, answer(nil, map[string]error{"a.service": errors.New("exec: not found")}))(t.Context(), log)
	require.ErrorContains(t, err, "a.service")

	_, err = anyUnitActiveWith(units, answer(map[string]string{"a.service": "unknown"}, map[string]error{"a.service": errors.New("exit status 4")}))(t.Context(), log)
	require.ErrorContains(t, err, "unknown")

	// An answer that is not a state is an error even with a zero exit.
	_, err = anyUnitActiveWith(units, answer(map[string]string{"a.service": "maybe"}, nil))(t.Context(), log)
	require.Error(t, err)
}
