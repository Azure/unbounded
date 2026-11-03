// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

type fakeNoneNode struct {
	params   noneops.Params
	calls    []string
	restart  error
	upgrade  error
	pruneErr error
}

func (f *fakeNoneNode) Restart(context.Context) error {
	f.calls = append(f.calls, "restart")
	return f.restart
}

func (f *fakeNoneNode) Upgrade(context.Context) error {
	f.calls = append(f.calls, "upgrade")
	return f.upgrade
}
func (f *fakeNoneNode) PruneVersions() error { f.calls = append(f.calls, "prune"); return f.pruneErr }

// stubNone replaces the host-touching seams and returns the recorder.
func stubNone(t *testing.T) (*fakeNoneNode, *[]string) {
	t.Helper()

	node := &fakeNoneNode{}
	events := &[]string{}

	oldNew := newNoneNode

	t.Cleanup(func() { newNoneNode = oldNew })

	stubNodeSeams(t, events)

	newNoneNode = func(p noneops.Params) noneNode {
		node.params = p
		return node
	}
	persistNodeConfig = func(_ context.Context, _ *slog.Logger, _ string, cfg *provision.AgentConfig) error {
		*events = append(*events, "persist:"+cfg.Isolation+":"+cfg.Cluster.Version)
		return nil
	}

	return node, events
}

func noneActive() *ActiveMachine {
	cfg := baseConfig()
	cfg.Isolation = "none"

	return &ActiveMachine{Name: "kube1", Config: cfg, Mode: isolation.ModeNone}
}

func TestNoneOperatorIsRegisteredAndDispatched(t *testing.T) {
	op, ok := newNodeOperator().(*dispatchOperator)
	require.True(t, ok)

	got, err := op.forMode(isolation.ModeNone)
	require.NoError(t, err)
	assert.IsType(t, noneNodeOperator{}, got)

	assert.Panics(t, func() { registerNodeOperator(isolation.ModeNone, func() nodeOperator { return noneNodeOperator{} }) })
}

func TestNoneOperatorRestartNode(t *testing.T) {
	node, _ := stubNone(t)

	active := noneActive()
	require.NoError(t, noneNodeOperator{}.RestartNode(t.Context(), discardLogger(), active))
	assert.Equal(t, []string{"restart"}, node.calls)
	assert.Equal(t, "none", node.params.Config.Isolation)
	assert.Equal(t, "1.33.1", node.params.Config.Cluster.Version, "the applied config the node runs from")

	node.restart = errors.New("kubelet not active")
	err := noneNodeOperator{}.RestartNode(t.Context(), discardLogger(), active)
	require.ErrorContains(t, err, "kubelet not active")
	require.ErrorContains(t, err, "restart node")
}

func TestNoneOperatorRestartDoesNotMutateTheAppliedConfig(t *testing.T) {
	stubNone(t)

	active := noneActive()
	before := *active.Config

	require.NoError(t, noneNodeOperator{}.RestartNode(t.Context(), discardLogger(), active))
	assert.Equal(t, before, *active.Config)
}

func TestNoneOperatorRepaveOrderAndAppliedConfigMode(t *testing.T) {
	node, events := stubNone(t)

	newCfg := &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}
	newCfg.Cluster.Version = "1.34.0"
	// The desired config asks for another mode; the repave reconciler handles
	// that before it gets here, so the record must keep the running mode.
	newCfg.Isolation = "container"

	require.NoError(t, noneNodeOperator{}.RepaveNode(t.Context(), discardLogger(), noneActive(), newCfg))

	assert.Equal(t, []string{"resolve", "stage", "persist:none:1.34.0"}, *events)
	assert.Equal(t, []string{"upgrade", "prune"}, node.calls)
	assert.Equal(t, "none", node.params.Config.Isolation)
	assert.Equal(t, "container", newCfg.Isolation, "the caller's desired config is not modified")
	require.NotNil(t, node.params.Downloads)
}

func TestNoneOperatorRepaveFailureDoesNotPersistOrPrune(t *testing.T) {
	node, events := stubNone(t)
	node.upgrade = errors.New("upgrade to 1.34.0: kubelet crashlooping (rolled back to 1.33.1)")

	err := noneNodeOperator{}.RepaveNode(t.Context(), discardLogger(), noneActive(), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()})
	require.ErrorContains(t, err, "rolled back")
	assert.Equal(t, []string{"upgrade"}, node.calls)
	assert.Equal(t, []string{"resolve", "stage"}, *events, "the record still describes what is running")
}

func TestNoneOperatorRepavePruneFailureIsNotAnUpgradeFailure(t *testing.T) {
	node, events := stubNone(t)
	node.pruneErr = errors.New("disk")

	require.NoError(t, noneNodeOperator{}.RepaveNode(t.Context(), discardLogger(), noneActive(), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}))
	assert.Contains(t, strings.Join(*events, ","), "persist")
}

func TestNoneOperatorRepaveSetupFailures(t *testing.T) {
	_, _ = stubNone(t)

	resolveNodeDownloads = func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
		return nil, nil, errors.New("bad offline bundle")
	}

	err := noneNodeOperator{}.RepaveNode(t.Context(), discardLogger(), noneActive(), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()})
	require.ErrorContains(t, err, "resolve download overrides")

	node, _ := stubNone(t)
	stageNodeArchives = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		return errors.New("staging failed")
	}

	err = noneNodeOperator{}.RepaveNode(t.Context(), discardLogger(), noneActive(), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()})
	require.ErrorContains(t, err, "staging failed")
	assert.Empty(t, node.calls, "nothing is stopped when staging fails")

	_, _ = stubNone(t)
	persistNodeConfig = func(context.Context, *slog.Logger, string, *provision.AgentConfig) error {
		return errors.New("read-only /etc")
	}

	err = noneNodeOperator{}.RepaveNode(t.Context(), discardLogger(), noneActive(), &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()})
	require.ErrorContains(t, err, "read-only /etc")
}

func TestNoneOperatorLifecycleMigrationIsANoOp(t *testing.T) {
	require.NoError(t, noneNodeOperator{}.EnsureLifecycleMigration(t.Context(), discardLogger(), noneActive()))
}

func TestNoneOperatorMigrationToAnUnknownTargetNamesTheMode(t *testing.T) {
	desired := &provision.UnboundedAgentConfig{AgentConfig: *baseConfig()}
	desired.Isolation = "bogus"

	err := noneNodeOperator{}.MigrateIsolation(t.Context(), discardLogger(), noneActive(), desired, migrate.Strategy("reprovision"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none")
}

func TestDispatchRoutesNoneMachinesToTheNoneOperator(t *testing.T) {
	node, _ := stubNone(t)

	require.NoError(t, newNodeOperator().RestartNode(t.Context(), discardLogger(), noneActive()))
	assert.Equal(t, []string{"restart"}, node.calls)
}

func TestResetResourcesChoosesTheNodeTeardownByHost(t *testing.T) {
	old := hostIsNone

	t.Cleanup(func() { hostIsNone = old })

	hostIsNone = func() bool { return false }

	nspawn := resetResources(discardLogger()).Name()
	assert.Contains(t, nspawn, "parallel(stop-machine, stop-machine)")
	assert.NotContains(t, nspawn, "none")

	hostIsNone = func() bool { return true }

	name := resetResources(discardLogger()).Name()
	assert.NotContains(t, name, "stop-machine")
	assert.NotContains(t, name, "remove-bpffs-mount")

	// The order of the nspawn reset is preserved: daemon unit, stop, WireGuard
	// keys, config, root, network and routes, bootstrap unit, artifacts, reload.
	order := []string{
		"remove-daemon-unit", "stop-none-node", "remove-wireguard-keys", "remove-none-config", "remove-none-node",
		"cleanup-routes", "remove-first-boot-unit", "remove-agent-artifacts", "reload-systemd",
	}

	last := -1

	for _, step := range order {
		idx := strings.Index(name, step)
		require.GreaterOrEqual(t, idx, 0, "%s missing from %s", step, name)
		require.Greater(t, idx, last, "%s out of order in %s", step, name)

		last = idx
	}
}
