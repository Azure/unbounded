// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type machineOperationAliasOptions struct {
	kind              v1alpha3.OperationKind
	operationNamePart string
	machine           string
	operationName     string
	parameters        map[string]string
	ttlSeconds        int32
	wait              bool
	timeout           time.Duration
	kubeconfig        string
	clientFactory     machineClientFactory
}

type machineOperationParametersKey struct{}

func newMachineNodeRebootCommand(rt *machineCommandRuntime) *cobra.Command {
	return newMachineOperationAliasCommand(
		rt,
		"node-reboot NAME",
		"Reboot the nspawn-backed Kubernetes node through MachineOperation",
		v1alpha3.OperationNodeReboot,
		"node-reboot",
	)
}

func newMachineHostRebootCommand(rt *machineCommandRuntime) *cobra.Command {
	return newMachineOperationAliasCommand(
		rt,
		"host-reboot NAME",
		"Reboot the host through MachineOperation",
		v1alpha3.OperationHostReboot,
		"host-reboot",
	)
}

func newMachinePowerOffCommand(rt *machineCommandRuntime) *cobra.Command {
	return newMachineOperationAliasCommand(
		rt,
		"power-off NAME",
		"Power off the host through MachineOperation",
		v1alpha3.OperationHostPowerOff,
		"power-off",
	)
}

func newMachinePowerOnCommand(rt *machineCommandRuntime) *cobra.Command {
	return newMachineOperationAliasCommand(
		rt,
		"power-on NAME",
		"Power on the host through MachineOperation",
		v1alpha3.OperationHostPowerOn,
		"power-on",
	)
}

func newMachineAgentUpgradeCommand(rt *machineCommandRuntime) *cobra.Command {
	var downloadURL string

	cmd := newMachineOperationAliasCommand(
		rt,
		"agent-upgrade NAME",
		"Upgrade the host-side unbounded-agent through MachineOperation",
		v1alpha3.OperationAgentUpgrade,
		"agent-upgrade",
	)

	cmd.Flags().StringVar(&downloadURL, "download-url", "", "URL of the unbounded-agent release tarball")

	oldRunE := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if downloadURL == "" {
			return fmt.Errorf("--download-url is required")
		}

		cmd.SetContext(context.WithValue(cmd.Context(), machineOperationParametersKey{}, map[string]string{
			"downloadURL": downloadURL,
		}))

		return oldRunE(cmd, args)
	}

	return cmd
}

func newMachineAgentResetCommand(rt *machineCommandRuntime) *cobra.Command {
	var force bool

	cmd := newMachineOperationAliasCommand(
		rt,
		"agent-reset NAME",
		"Reset the host-side unbounded-agent through MachineOperation",
		v1alpha3.OperationAgentReset,
		"agent-reset",
	)

	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation for destructive agent reset")

	oldRunE := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !force {
			if err := confirmAgentReset(args[0], os.Stdin, os.Stderr); err != nil {
				return err
			}
		}

		return oldRunE(cmd, args)
	}

	return cmd
}

func newMachineOperationAliasCommand(rt *machineCommandRuntime, use, short string, kind v1alpha3.OperationKind, operationNamePart string) *cobra.Command {
	o := &machineOperationAliasOptions{
		kind:              kind,
		operationNamePart: operationNamePart,
		ttlSeconds:        defaultTTLSeconds,
		wait:              true,
		clientFactory:     rt.clientWithKubeconfig,
	}

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.machine = args[0]
			if params, ok := cmd.Context().Value(machineOperationParametersKey{}).(map[string]string); ok {
				o.parameters = params
			}

			return runMachineOperationAlias(cmd.Context(), *o, cmd.OutOrStdout())
		},
	}

	addMachineOperationAliasFlags(cmd, o)

	return cmd
}

func addMachineOperationAliasFlags(cmd *cobra.Command, o *machineOperationAliasOptions) {
	cmd.Flags().StringVar(&o.operationName, "operation-name", "", "Name for the MachineOperation resource")
	cmd.Flags().Int32Var(&o.ttlSeconds, "ttl", defaultTTLSeconds, "Seconds after completion before cleanup (0 keeps indefinitely)")
	cmd.Flags().BoolVar(&o.wait, "wait", true, "Wait until the operation reaches Complete or Failed")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 0, "Time to wait for completion when --wait is set (0 waits indefinitely)")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
}

func runMachineOperationAlias(ctx context.Context, o machineOperationAliasOptions, out io.Writer) error {
	return newMachineOperationAliasCreateOptions(o, out, time.Now()).run(ctx)
}

func newMachineOperationAliasCreateOptions(o machineOperationAliasOptions, out io.Writer, now time.Time) *machineOperationCreateOptions {
	opName := o.operationName
	if opName == "" {
		opName = generateMachineOperationName(o.machine, o.operationNamePart, now)
	}

	return &machineOperationCreateOptions{
		name:          opName,
		kind:          o.kind,
		machine:       o.machine,
		parameters:    o.parameters,
		ttlSeconds:    o.ttlSeconds,
		wait:          o.wait,
		timeout:       o.timeout,
		output:        operationOutputName,
		dryRun:        dryRunNone,
		fieldManager:  fieldManagerID,
		kubeconfig:    o.kubeconfig,
		clientFactory: o.clientFactory,
		out:           out,
		printCreated:  true,

		ownerReferenceMachine: true,
	}
}
