// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
)

// missingTool reports whether err is a host inspection tool that is not
// installed, as opposed to a tool that ran and failed.
//
// Reset and bootstrap ask the host the same questions for opposite reasons, so
// they need opposite answers here.
//
// Bootstrap must fail closed. A host it cannot inspect is not a host it can
// prove is clean, and assuming otherwise risks building over a running node.
//
// Reset is the other way round. The tools it inspects with, machinectl from
// systemd-container and nft from nftables, are the ones bootstrap installs. A
// bootstrap that died inside host preparation therefore leaves a host where the
// question cannot be asked and the answer is known anyway: nothing of ours is
// running, because the tools needed to start it were never there. Treating that
// as an error leaves an installation record nothing can clear, which is the one
// outcome reset exists to prevent.
func missingTool(err error) bool { return errors.Is(err, exec.ErrNotFound) }

// registeredMachineForCleanup answers RegisteredMachine's question with the
// tolerance described on missingTool. Cleanup uses this; admission must not.
func registeredMachineForCleanup(ctx context.Context, log *slog.Logger, name string) (bool, error) {
	exists, err := RegisteredMachine(ctx, log, name)
	if missingTool(err) {
		log.Warn("machine inventory tool is not installed; nothing of ours can be running", "machine", name)

		return false, nil
	}

	return exists, err
}
