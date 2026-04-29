// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/printers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func configVersionsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "versions NAME",
		Short: "List MachineConfigurationVersions for a MachineConfiguration",
		Long: `List all MachineConfigurationVersions belonging to a specific
MachineConfiguration, ordered by version number.

Example:
  kubectl unbounded config versions my-config`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runConfigVersions(ctx, c, args[0])
		},
	}

	return cmd
}

func runConfigVersions(ctx context.Context, c client.WithWatch, name string) error {
	var list v1alpha3.MachineConfigurationVersionList
	if err := c.List(ctx, &list,
		client.MatchingLabels{v1alpha3.MCVConfigurationLabelKey: name},
	); err != nil {
		return fmt.Errorf("listing MachineConfigurationVersions: %w", err)
	}

	if len(list.Items) == 0 {
		fmt.Printf("No MachineConfigurationVersions found for %s\n", name)
		return nil
	}

	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Version", Type: "integer"},
			{Name: "Deployed", Type: "boolean"},
			{Name: "Machines", Type: "integer"},
			{Name: "K8s Version", Type: "string"},
			{Name: "Agent Image", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range list.Items {
		mcv := &list.Items[i]

		k8sVersion := ""
		agentImage := ""

		if mcv.Spec.Template.Kubernetes != nil {
			k8sVersion = mcv.Spec.Template.Kubernetes.Version
		}

		if mcv.Spec.Template.Agent != nil {
			agentImage = mcv.Spec.Template.Agent.Image
		}

		age := duration.HumanDuration(time.Since(mcv.CreationTimestamp.Time))

		table.Rows = append(table.Rows, metav1.TableRow{Cells: []interface{}{
			mcv.Name,
			mcv.Spec.Version,
			mcv.Status.Deployed,
			mcv.Status.DeployedMachines,
			k8sVersion,
			agentImage,
			age,
		}})
	}

	return printers.NewTablePrinter(printers.PrintOptions{}).PrintObj(table, os.Stdout)
}
