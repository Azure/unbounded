// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// machineRepaveCommand returns a cobra.Command that repaves a Machine via Redfish.
func machineRepaveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repave NAME",
		Short: "Repave a Machine via Redfish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runRepave(ctx, c, args[0])
		},
	}

	return cmd
}

func runRepave(ctx context.Context, c client.WithWatch, name string) error {
	machine, err := getMachine(ctx, c, name)
	if err != nil {
		return err
	}

	machine.Spec.Operations.RepaveCounter++
	machine.Spec.Operations.RebootCounter++
	target := machine.Spec.Operations.RebootCounter

	if err := c.Update(ctx, machine); err != nil {
		return fmt.Errorf("updating Machine: %w", err)
	}

	printStep(fmt.Sprintf("Repaving Machine %s...", name))
	printConfig("target", fmt.Sprintf("%d", target))
	printConfig("repave", fmt.Sprintf("%d", machine.Spec.Operations.RepaveCounter))
	fmt.Println()

	return watchReboot(ctx, c, name, target)
}
