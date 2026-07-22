// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import "github.com/spf13/cobra"

// EdgeCmd runs Metalman's provisioning-network protocol edge.
func EdgeCmd() *cobra.Command {
	return newMetalmanRoleCmd(metalmanRoleEdge, "Run the Metalman provisioning protocol edge")
}
