// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import "github.com/spf13/cobra"

// ControllerCmd runs Metalman's leader-elected Kubernetes control loops.
func ControllerCmd() *cobra.Command {
	return newMetalmanRoleCmd(metalmanRoleController, "Run the Metalman control loops")
}
