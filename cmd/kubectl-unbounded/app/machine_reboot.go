package app

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// machineRebootCommand returns a cobra.Command that reboots a Machine via Redfish.
func machineRebootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reboot NAME",
		Short: "Reboot a Machine via Redfish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runReboot(ctx, c, args[0])
		},
	}

	return cmd
}

func runReboot(ctx context.Context, c client.WithWatch, name string) error {
	machine, err := getMachine(ctx, c, name)
	if err != nil {
		return err
	}

	machine.Spec.Operations.RebootCounter++
	target := machine.Spec.Operations.RebootCounter

	if err := c.Update(ctx, machine); err != nil {
		return fmt.Errorf("updating Machine: %w", err)
	}

	printStep(fmt.Sprintf("Rebooting Machine %s...", name))
	printConfig("target", fmt.Sprintf("%d", target))
	fmt.Println()

	return watchReboot(ctx, c, name, target)
}
