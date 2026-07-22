// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import "github.com/spf13/cobra"

// ServerCmd runs Metalman's replicated artifact and callback server.
func ServerCmd() *cobra.Command {
	return newMetalmanRoleCmd(metalmanRoleServer, "Run the Metalman artifact and control server")
}
