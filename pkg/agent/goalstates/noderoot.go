// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
)

// Root returns the node root phases use to place files in the node stack's
// filesystem. Phases call Path on it instead of joining the machine directory
// themselves. This file is the intentional bridge from the nspawn goal state
// fields to the isolation seam.
func (n *NodeStart) Root() isolation.NodeRoot {
	return nspawn.RootAt(n.MachineName, n.MachineDir)
}

// Exec runs args inside the node stack and returns its stdout.
func (n *NodeStart) Exec(ctx context.Context, log *slog.Logger, args ...string) (string, error) {
	return nspawn.Exec(ctx, log, n.MachineName, args...)
}

// Root returns the node root phases use to place files in the node stack's
// filesystem. The slot is the base name of the machine directory.
func (r *RootFS) Root() isolation.NodeRoot {
	return nspawn.RootAt(filepath.Base(r.MachineDir), r.MachineDir)
}
