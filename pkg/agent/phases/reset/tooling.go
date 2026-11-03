// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
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
// Reset is the other way round. The tools it inspects with, the machine
// manager from systemd-container and nft from nftables, are the ones bootstrap
// installs. A bootstrap that died inside host preparation therefore leaves a host where the
// question cannot be asked and the answer is known anyway: nothing of ours is
// running, because the tools needed to start it were never there. Treating that
// as an error leaves an installation record nothing can clear, which is the one
// outcome reset exists to prevent.
func missingTool(err error) bool { return nspawn.MissingTool(err) }
