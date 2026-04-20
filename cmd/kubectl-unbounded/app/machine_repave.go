// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

// machineRepaveCommand returns a cobra.Command that repaves a Machine via Redfish.
func machineRepaveCommand() *cobra.Command {
	var kubeVersion string
	var ociImage string

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

			return runRepave(ctx, c, args[0], kubeVersion, ociImage)
		},
	}

	cmd.Flags().StringVar(&kubeVersion, "kube-version", "", "Kubernetes version to use for the repaved machine (e.g., \"v1.34.0\")")
	cmd.Flags().StringVar(&ociImage, "oci-image", "", "OCI image reference for the agent (e.g., \"ghcr.io/org/repo:tag\")")

	return cmd
}

func runRepave(ctx context.Context, c client.WithWatch, name, kubeVersion, ociImage string) error {
	machine, err := getMachine(ctx, c, name)
	if err != nil {
		return err
	}

	if kubeVersion != "" {
		machine.Spec.Kubernetes.Version = kubeVersion
	}

	if ociImage != "" {
		if machine.Spec.Agent == nil {
			machine.Spec.Agent = &v1alpha3.AgentSpec{}
		}
		machine.Spec.Agent.Image = ociImage
	}

	machine.Spec.Operations.RepaveCounter++
	machine.Spec.Operations.RebootCounter++
	target := machine.Spec.Operations.RebootCounter

	if err := c.Update(ctx, machine); err != nil {
		return fmt.Errorf("updating Machine: %w", err)
	}

	printStep(fmt.Sprintf("Repaving Machine %s...", name))
	if kubeVersion != "" {
		printConfig("kube-version", kubeVersion)
	}
	if ociImage != "" {
		printConfig("oci-image", ociImage)
	}
	printConfig("target", fmt.Sprintf("%d", target))
	printConfig("repave", fmt.Sprintf("%d", machine.Spec.Operations.RepaveCounter))
	fmt.Println()

	return watchReboot(ctx, c, name, target)
}
