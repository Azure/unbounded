// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func newMachineReplaceCommand(rt *machineCommandRuntime) *cobra.Command {
	var (
		ttl           int32
		force         bool
		wait          bool
		timeout       time.Duration
		operationName string
		kubeconfig    string
	)

	cmd := &cobra.Command{
		Use:   "replace NAME",
		Short: "Destructively replace a machine through MachineOperation",
		Long: `Replace creates a MachineOperation CR requesting a host VM replacement.
The machine-ops-controller processes the operation through the external
provider by deleting and recreating the host VM with fresh bootstrap data.
Host-local OS disk state is destroyed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := rt.context(cmd.Context())

			c, err := rt.clientWithKubeconfig(kubeconfig)
			if err != nil {
				return err
			}

			return runReplaceWithOptions(ctx, c, args[0], machineReplaceOptions{
				ttlSeconds:    ttl,
				force:         force,
				wait:          wait,
				timeout:       timeout,
				operationName: operationName,
			}, cmd.OutOrStdout())
		},
	}

	cmd.Flags().Int32Var(&ttl, "ttl", defaultTTLSeconds,
		"Seconds after completion before the MachineOperation CR is automatically deleted (0 to disable)")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation for destructive VM replacement")
	cmd.Flags().StringVar(&operationName, "operation-name", "", "Name for the MachineOperation resource")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait until the operation reaches Complete or Failed")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Time to wait for completion when --wait is set (0 waits indefinitely)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")

	return cmd
}

type machineReplaceOptions struct {
	ttlSeconds    int32
	force         bool
	wait          bool
	timeout       time.Duration
	operationName string
}

func runReplace(ctx context.Context, c client.WithWatch, name string, ttlSeconds int32, force bool, out io.Writer) error {
	return runReplaceWithOptions(ctx, c, name, machineReplaceOptions{
		ttlSeconds: ttlSeconds,
		force:      force,
		wait:       true,
	}, out)
}

func runReplaceWithOptions(ctx context.Context, c client.WithWatch, name string, opts machineReplaceOptions, out io.Writer) error {
	if !opts.force {
		if err := confirmReplace(name, os.Stdin, os.Stderr); err != nil {
			return err
		}
	}

	opName := opts.operationName
	if opName == "" {
		opName = generateMachineOperationName(name, "replace", time.Now())
	}

	createOptions := &machineOperationCreateOptions{
		name:         opName,
		kind:         v1alpha3.OperationHostReplace,
		machine:      name,
		ttlSeconds:   opts.ttlSeconds,
		output:       operationOutputName,
		dryRun:       dryRunNone,
		fieldManager: fieldManagerID,
		printCreated: false,
		out:          out,
	}
	if err := createOptions.validate(); err != nil {
		return err
	}

	op, err := createOptions.build()
	if err != nil {
		return err
	}

	if err := addMachineOperationOwnerReference(ctx, c, op, name); err != nil {
		return err
	}

	if err := createOptions.runWithClient(ctx, c, op); err != nil {
		return err
	}

	printStep(out, fmt.Sprintf("Replacing Machine %s...", name))
	printConfig(out, "operation", opName)
	fprintln(out)

	if !opts.wait {
		return nil
	}

	waitCtx, cancel := contextWithOptionalTimeout(ctx, opts.timeout)
	defer cancel()

	return waitForMachineOperation(waitCtx, c, opName, out)
}

func confirmReplace(name string, in *os.File, out io.Writer) error {
	return confirmReplaceWithTerminal(name, in, out, isTerminal(in))
}

func confirmReplaceWithTerminal(name string, in io.Reader, out io.Writer, terminal bool) error {
	if !terminal {
		return fmt.Errorf("replace deletes and recreates the host VM; rerun with --force to confirm")
	}

	if _, err := fmt.Fprintf(out, "This will delete and recreate host VM %q, destroying OS disk state. Type the machine name to continue: ", name); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w; rerun with --force to confirm", err)
	}

	if strings.TrimSpace(line) != name {
		return fmt.Errorf("confirmation did not match machine name %q", name)
	}

	return nil
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}
