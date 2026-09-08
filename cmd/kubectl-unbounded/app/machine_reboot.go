// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// machineRebootCommand returns a cobra.Command that reboots a Machine via Redfish.
func machineRebootCommand() *cobra.Command {
	var ttl int32

	cmd := &cobra.Command{
		Use:   "reboot NAME",
		Short: "Reboot a Machine via a HostReboot operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runReboot(ctx, c, args[0], ttl, cmd.OutOrStdout())
		},
	}
	cmd.Flags().Int32Var(&ttl, "ttl", defaultTTLSeconds,
		"Seconds after completion before the MachineOperation CR is automatically deleted (0 to disable)")

	return cmd
}

func runReboot(ctx context.Context, c client.WithWatch, name string, ttlSeconds int32, out io.Writer) error {
	machine, err := getMachine(ctx, c, name)
	if err != nil {
		return err
	}

	if machine.Spec.Netboot() == nil || machine.Spec.Netboot().Redfish == nil {
		return fmt.Errorf("machine %s has no redfish configuration; reboots require BMC access", name)
	}

	opName := fmt.Sprintf("%s-reboot-%d", name, time.Now().Unix())
	if err := createMachineOperation(ctx, c, name, opName, v1alpha3.OperationHostReboot, ttlSeconds); err != nil {
		return err
	}

	printStep(out, fmt.Sprintf("Rebooting Machine %s...", name))
	printConfig(out, "operation", opName)
	fprintln(out)

	return watchMachineOperation(ctx, c, opName, out)
}
