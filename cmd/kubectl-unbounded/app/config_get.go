// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/printers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func configGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get [NAME]",
		Short: "List MachineConfigurations",
		Long: `List MachineConfigurations. If a name is provided, show details for
that specific configuration.

Example:
  kubectl unbounded machine config get
  kubectl unbounded machine config get my-config`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			if len(args) == 1 {
				return runConfigGetOne(ctx, c, args[0], cmd.OutOrStdout())
			}

			return runConfigGetAll(ctx, c, cmd.OutOrStdout())
		},
	}

	return cmd
}

func runConfigGetAll(ctx context.Context, c client.WithWatch, out io.Writer) error {
	var list v1alpha3.MachineConfigurationList
	if err := c.List(ctx, &list); err != nil {
		return fmt.Errorf("listing MachineConfigurations: %w", err)
	}

	if len(list.Items) == 0 {
		fprintln(out, "No MachineConfigurations found")
		return nil
	}

	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Latest Version", Type: "integer"},
			{Name: "Strategy", Type: "string"},
			{Name: "Priority", Type: "integer"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range list.Items {
		mc := &list.Items[i]
		age := duration.HumanDuration(time.Since(mc.CreationTimestamp.Time))

		table.Rows = append(table.Rows, metav1.TableRow{Cells: []interface{}{
			mc.Name,
			mc.Status.LatestVersion,
			mc.Spec.UpdateStrategy.Type,
			mc.Spec.Priority,
			age,
		}})
	}

	return printers.NewTablePrinter(printers.PrintOptions{}).PrintObj(table, out)
}

func runConfigGetOne(ctx context.Context, c client.WithWatch, name string, out io.Writer) error {
	mc := &v1alpha3.MachineConfiguration{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, mc); err != nil {
		return fmt.Errorf("getting MachineConfiguration: %w", err)
	}

	printStep(out, fmt.Sprintf("MachineConfiguration: %s", mc.Name))
	printConfig(out, "latest-version", fmt.Sprintf("%d", mc.Status.LatestVersion))
	printConfig(out, "current-version", fmt.Sprintf("%d", mc.Status.CurrentVersion))
	printConfig(out, "update-strategy", string(mc.Spec.UpdateStrategy.Type))
	printConfig(out, "priority", fmt.Sprintf("%d", mc.Spec.Priority))

	if mc.Spec.Template.Kubernetes != nil {
		k := mc.Spec.Template.Kubernetes
		if k.Version != "" {
			printConfig(out, "k8s-version", k.Version)
		}

		if len(k.NodeLabels) > 0 {
			for lk, lv := range k.NodeLabels {
				printConfig(out, "node-label", fmt.Sprintf("%s=%s", lk, lv))
			}
		}

		if len(k.RegisterWithTaints) > 0 {
			for _, t := range k.RegisterWithTaints {
				printConfig(out, "taint", formatTaint(t))
			}
		}
	}

	if mc.Spec.Template.Agent != nil {
		printConfig(out, "agent-image", mc.Spec.Template.Agent.Image)
	}

	fprintln(out)

	return nil
}
