// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func newMachineOperationWaitCommand(rt *machineCommandRuntime) *cobra.Command {
	var (
		timeout    time.Duration
		kubeconfig string
	)

	cmd := &cobra.Command{
		Use:   "wait NAME",
		Short: "Wait for a MachineOperation to complete",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			baseCtx := rt.context(cmd.Context())

			ctx, cancel := contextWithOptionalTimeout(baseCtx, timeout)
			defer cancel()

			c, err := rt.clientWithKubeconfig(kubeconfig)
			if err != nil {
				return err
			}

			return waitForMachineOperation(ctx, c, args[0], cmd.OutOrStdout())
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Time to wait for completion (0 waits indefinitely)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")

	return cmd
}

func waitForMachineOperation(ctx context.Context, c client.WithWatch, opName string, out io.Writer) error {
	return watchMachineOperation(ctx, c, opName, out)
}

func finishMachineOperationWait(op *v1alpha3.MachineOperation, out io.Writer) error {
	if op.Status.Phase == v1alpha3.OperationPhaseFailed {
		return fmt.Errorf("operation failed: %s", op.Status.Message)
	}

	printReady(out)

	return nil
}
