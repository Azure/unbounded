// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

func configAssignCommand() *cobra.Command {
	var version int32

	cmd := &cobra.Command{
		Use:   "assign CONFIG_NAME MACHINE_NAME",
		Short: "Assign a MachineConfiguration to a Machine",
		Long: `Set the spec.configurationRef on a Machine to reference a
MachineConfiguration. If --version is specified, that exact version is
pinned; otherwise only the configuration name is set and the controller
will select the latest version.

Example:
  kubectl unbounded config assign my-config my-machine
  kubectl unbounded config assign my-config my-machine --version=3`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			var versionPtr *int32
			if cmd.Flags().Changed("version") {
				versionPtr = ptr.To(version)
			}

			return runConfigAssign(ctx, c, args[0], args[1], versionPtr)
		},
	}

	cmd.Flags().Int32Var(&version, "version", 0, "Pin a specific MachineConfigurationVersion number")

	return cmd
}

func runConfigAssign(ctx context.Context, c client.WithWatch, configName, machineName string, version *int32) error {
	// Verify the MachineConfiguration exists.
	mc := &v1alpha3.MachineConfiguration{}
	if err := c.Get(ctx, client.ObjectKey{Name: configName}, mc); err != nil {
		return fmt.Errorf("getting MachineConfiguration %q: %w", configName, err)
	}

	// If a version is specified, verify the MCV exists.
	if version != nil {
		mcvName := fmt.Sprintf("%s-v%d", configName, *version)
		mcv := &v1alpha3.MachineConfigurationVersion{}

		if err := c.Get(ctx, client.ObjectKey{Name: mcvName}, mcv); err != nil {
			return fmt.Errorf("getting MachineConfigurationVersion %q: %w", mcvName, err)
		}
	}

	// Fetch the Machine.
	machine := &v1alpha3.Machine{}
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, machine); err != nil {
		return fmt.Errorf("getting Machine %q: %w", machineName, err)
	}

	// Set the configurationRef.
	machine.Spec.ConfigurationRef = &v1alpha3.MachineConfigurationRef{
		Name:    configName,
		Version: version,
	}

	if err := c.Update(ctx, machine); err != nil {
		return fmt.Errorf("updating Machine: %w", err)
	}

	printStep(fmt.Sprintf("Assigned %s to Machine %s", configName, machineName))
	printConfig("configuration", configName)

	if version != nil {
		printConfig("version", fmt.Sprintf("%d", *version))
	} else {
		printConfig("version", "(controller will select latest)")
	}

	fmt.Println()

	return nil
}
