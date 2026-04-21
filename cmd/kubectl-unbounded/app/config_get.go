// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

func configGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get [NAME]",
		Short: "List MachineConfigurations",
		Long: `List MachineConfigurations. If a name is provided, show details for
that specific configuration.

Example:
  kubectl unbounded config get
  kubectl unbounded config get my-config`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			if len(args) == 1 {
				return runConfigGetOne(ctx, c, args[0])
			}

			return runConfigGetAll(ctx, c)
		},
	}

	return cmd
}

func runConfigGetAll(ctx context.Context, c client.WithWatch) error {
	var list v1alpha3.MachineConfigurationList
	if err := c.List(ctx, &list); err != nil {
		return fmt.Errorf("listing MachineConfigurations: %w", err)
	}

	if len(list.Items) == 0 {
		fmt.Println("No MachineConfigurations found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tLATEST VERSION\tSTRATEGY\tPRIORITY\tAGE")

	for i := range list.Items {
		mc := &list.Items[i]
		age := formatAge(mc.CreationTimestamp.Time)
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\n",
			mc.Name,
			mc.Status.LatestVersion,
			mc.Spec.UpdateStrategy.Type,
			mc.Spec.Priority,
			age,
		)
	}

	return w.Flush()
}

func runConfigGetOne(ctx context.Context, c client.WithWatch, name string) error {
	mc := &v1alpha3.MachineConfiguration{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, mc); err != nil {
		return fmt.Errorf("getting MachineConfiguration: %w", err)
	}

	printStep(fmt.Sprintf("MachineConfiguration: %s", mc.Name))
	printConfig("latest-version", fmt.Sprintf("%d", mc.Status.LatestVersion))
	printConfig("current-version", fmt.Sprintf("%d", mc.Status.CurrentVersion))
	printConfig("update-strategy", string(mc.Spec.UpdateStrategy.Type))
	printConfig("priority", fmt.Sprintf("%d", mc.Spec.Priority))

	if mc.Spec.Template.Kubernetes != nil {
		k := mc.Spec.Template.Kubernetes
		if k.Version != "" {
			printConfig("k8s-version", k.Version)
		}

		if len(k.NodeLabels) > 0 {
			for lk, lv := range k.NodeLabels {
				printConfig("node-label", fmt.Sprintf("%s=%s", lk, lv))
			}
		}

		if len(k.RegisterWithTaints) > 0 {
			for _, t := range k.RegisterWithTaints {
				printConfig("taint", t)
			}
		}
	}

	if mc.Spec.Template.Agent != nil {
		printConfig("agent-image", mc.Spec.Template.Agent.Image)
	}

	fmt.Println()

	return nil
}
