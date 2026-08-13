// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import "github.com/spf13/cobra"

func overridesCommandGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "overrides",
		Short: "Inspect user-supplied workload overrides",
		Long: `Inspect the component workload overrides the operator reads from the
unbounded-component-overrides ConfigMap.

Overrides customize the Deployments and DaemonSets the operator generates.
Write access to that ConfigMap is equivalent to root on every node in every
affected Site: the workloads it patches already run privileged and
host-networked, so changing a container image, its arguments, or adding a
sidecar is arbitrary code execution. Treat it like a ClusterRoleBinding.`,
	}

	cmd.AddCommand(
		overridesValidateCommand(),
		overridesListCommand(),
		overridesStatusCommand(),
	)

	return cmd
}
