// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"io"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// containerMigrationBackend adapts a containerops.Node to isolation.Backend for
// an in-place migration. The container backend's own Start only starts the
// unit; bringing the node up as the only running slot (reset the shared
// kubelet state, write configuration, start, wait for kubelet) is
// Node.Activate, and starting a slot that was stopped again is Node.Resume.
type containerMigrationBackend struct {
	node *containerops.Node
	// source is true for the end that already runs: it is only stopped, and
	// started again on rollback.
	source bool
}

var _ isolation.Backend = (*containerMigrationBackend)(nil)

// newContainerMigrationEndpoint builds the backend and node root of a
// container end of an in-place migration.
func newContainerMigrationEndpoint(log *slog.Logger, ep migrationEndpoint) (isolation.Backend, isolation.SlotRef, error) {
	node := containerops.New(containerops.Params{Log: log, Config: ep.Config, Downloads: ep.Downloads, Slot: ep.Slot})

	root, err := node.Backend().NodeRoot(ep.Slot)
	if err != nil {
		return nil, nil, err
	}

	return &containerMigrationBackend{node: node, source: ep.Source}, root, nil
}

func (*containerMigrationBackend) Mode() isolation.Mode { return isolation.ModeContainer }

func (b *containerMigrationBackend) Requirements() isolation.Requirements {
	return b.node.Backend().Requirements()
}

func (b *containerMigrationBackend) Prepare(ctx context.Context, _ isolation.NodeSpec) (isolation.NodeRoot, error) {
	return b.node.Prepare(ctx)
}

func (b *containerMigrationBackend) Start(ctx context.Context, _ isolation.SlotRef) error {
	if b.source {
		return b.node.Resume(ctx)
	}

	return b.node.Activate(ctx)
}

func (b *containerMigrationBackend) Stop(ctx context.Context, _ isolation.SlotRef) error {
	return b.node.Stop(ctx)
}

func (b *containerMigrationBackend) Restart(ctx context.Context, _ isolation.SlotRef) error {
	return b.node.Restart(ctx)
}

func (b *containerMigrationBackend) Destroy(ctx context.Context, _ isolation.SlotRef) error {
	return b.node.Destroy(ctx)
}

func (b *containerMigrationBackend) Status(ctx context.Context, r isolation.SlotRef) (isolation.Status, error) {
	return b.node.Backend().Status(ctx, r)
}

func (b *containerMigrationBackend) Exec(ctx context.Context, r isolation.SlotRef, cmd isolation.ExecSpec) (isolation.ExecResult, error) {
	return b.node.Backend().Exec(ctx, r, cmd)
}

func (b *containerMigrationBackend) Logs(ctx context.Context, r isolation.SlotRef, opts isolation.LogOptions) (io.ReadCloser, error) {
	return b.node.Backend().Logs(ctx, r, opts)
}
