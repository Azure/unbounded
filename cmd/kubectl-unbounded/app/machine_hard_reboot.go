// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func machineHardRebootCommand() *cobra.Command {
	var ttl int32

	cmd := &cobra.Command{
		Use:   "hard-reboot NAME",
		Short: "Hard-reboot a machine through MachineOperation",
		Long: `Hard-reboot creates a MachineOperation CR requesting a full host
power cycle. The machina controller or cloud controller processes the
operation and updates MachineOperation status to "Complete" or "Failed".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runHardReboot(ctx, c, args[0], ttl)
		},
	}

	cmd.Flags().Int32Var(&ttl, "ttl", defaultTTLSeconds,
		"Seconds after completion before the MachineOperation CR is automatically deleted (0 to disable)")

	return cmd
}

func runHardReboot(ctx context.Context, c client.WithWatch, name string, ttlSeconds int32) error {
	opName := fmt.Sprintf("%s-hard-reboot-%d", name, time.Now().Unix())

	if err := createMachineOperation(ctx, c, name, opName, v1alpha3.OperationHardReboot, ttlSeconds); err != nil {
		return err
	}

	printStep(fmt.Sprintf("Hard-rebooting Machine %s...", name))
	printConfig("operation", opName)
	fmt.Println()

	return watchMachineOperation(ctx, c, opName)
}
