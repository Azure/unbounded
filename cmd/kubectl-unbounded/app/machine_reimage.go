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

// machineReimageCommand returns a cobra.Command that reimages a Machine via Redfish.
func machineReimageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reimage NAME",
		Short: "Reimage a Machine via Redfish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runReimage(ctx, c, args[0])
		},
	}

	return cmd
}

func runReimage(ctx context.Context, c client.WithWatch, name string) error {
	machine, err := getMachine(ctx, c, name)
	if err != nil {
		return err
	}

	machine.Spec.Operations.ReimageCounter++
	machine.Spec.Operations.RebootCounter++
	target := machine.Spec.Operations.RebootCounter

	if err := c.Update(ctx, machine); err != nil {
		return fmt.Errorf("updating Machine: %w", err)
	}

	printStep(fmt.Sprintf("Reimaging Machine %s...", name))
	printConfig("target", fmt.Sprintf("%d", target))
	printConfig("reimage", fmt.Sprintf("%d", machine.Spec.Operations.ReimageCounter))
	fmt.Println()

	return watchReboot(ctx, c, name, target)
}
