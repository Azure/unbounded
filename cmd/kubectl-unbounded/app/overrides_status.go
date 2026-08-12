// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/printers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func overridesStatusCommand() *cobra.Command {
	var kubeconfig string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what the operator did with the overrides",
		Long: `Show the override state the operator recorded on each Site.

Every value here was computed and written by the running operator: desired
hashes come from Site status, applied hashes from the workloads themselves.
This command performs no rendering, no merging and no hashing of its own, so
its answer stays correct when the plugin and the operator are different
versions.

Example:
  kubectl unbounded overrides status`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClientWithKubeconfig(getKubeconfigPath(kubeconfig))
			if err != nil {
				return err
			}

			return runOverridesStatus(ctx, c, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")

	return cmd
}

func runOverridesStatus(ctx context.Context, c client.Client, out io.Writer) error {
	var sites v1alpha3.SiteList
	if err := c.List(ctx, &sites); err != nil {
		return fmt.Errorf("list Sites: %w", err)
	}

	if len(sites.Items) == 0 {
		fprintln(out, "No Sites found.")

		return nil
	}

	ordered := make([]v1alpha3.Site, len(sites.Items))
	copy(ordered, sites.Items)

	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Site", Type: "string"},
			{Name: "Phase", Type: "string"},
			{Name: "Workload", Type: "string"},
			{Name: "Applied", Type: "string"},
			{Name: "Drift", Type: "string"},
		},
	}

	var (
		degraded    []v1alpha3.Site
		anyOverride bool
	)

	for i := range ordered {
		site := ordered[i]

		status := site.Status.Overrides
		if status == nil || status.Phase == v1alpha3.OverridePhaseNone {
			continue
		}

		anyOverride = true

		if status.Phase == v1alpha3.OverridePhaseDegraded {
			degraded = append(degraded, site)
		}

		if len(status.Workloads) == 0 {
			table.Rows = append(table.Rows, metav1.TableRow{Cells: []any{
				site.Name, status.Phase, "-", "-", "-",
			}})

			continue
		}

		for _, workload := range status.Workloads {
			table.Rows = append(table.Rows, metav1.TableRow{Cells: []any{
				site.Name,
				status.Phase,
				workload.Kind + "/" + workload.Name,
				describeApplied(workload),
				orDash(workload.VersionDrift),
			}})
		}
	}

	if !anyOverride {
		fprintln(out, "No overrides are in effect on any Site.")

		return nil
	}

	if err := printers.NewTablePrinter(printers.PrintOptions{}).PrintObj(table, out); err != nil {
		return fmt.Errorf("print override status: %w", err)
	}

	reportObservedVersions(ordered, out)
	reportDegraded(degraded, out)
	reportDrift(ordered, out)

	return nil
}

// describeApplied compares the hash a workload carries against the one the
// operator wanted.
//
// Both were computed over the same contributor set for the same workload, so
// they are directly comparable. Reporting the comparison rather than the raw
// hashes is what makes the output readable; the hashes themselves are in Site
// status for anyone who needs them.
func describeApplied(workload v1alpha3.OverriddenWorkload) string {
	switch {
	case workload.DesiredHash == "":
		return "no override"
	case workload.AppliedHash == "":
		return "not applied"
	case workload.AppliedHash == workload.DesiredHash:
		return "yes"
	default:
		return "stale"
	}
}

func reportObservedVersions(sites []v1alpha3.Site, out io.Writer) {
	versions := map[string]bool{}

	for i := range sites {
		if status := sites[i].Status.Overrides; status != nil && status.ObservedResourceVersion != "" {
			versions[status.ObservedResourceVersion] = true
		}
	}

	if len(versions) <= 1 {
		return
	}

	// Different Sites acting on different ConfigMap versions is transient
	// rather than wrong: a pass records the version it saw, and a newer version
	// always triggers another pass. Saying so avoids it looking like a fault.
	ordered := make([]string, 0, len(versions))
	for version := range versions {
		ordered = append(ordered, version)
	}

	sort.Strings(ordered)

	fprintf(out, "\nNote: Sites observed different ConfigMap versions (%v). This is transient;\n", ordered)
	fprintln(out, "      a newer version always triggers another reconcile.")
}

func reportDegraded(degraded []v1alpha3.Site, out io.Writer) {
	if len(degraded) == 0 {
		return
	}

	fprintln(out, "\nDegraded Sites:")

	for i := range degraded {
		status := degraded[i].Status.Overrides
		fprintf(out, "  %s: %s\n", degraded[i].Name, status.Message)
	}

	fprintln(out, "\nWhile overrides are unusable the operator leaves the affected workloads")
	fprintln(out, "as they are rather than reverting them, so drift on those workloads is not")
	fprintln(out, "corrected until the document is fixed.")
}

// reportDrift calls out image overrides, which break version lockstep with the
// operator and survive its upgrades.
func reportDrift(sites []v1alpha3.Site, out io.Writer) {
	seen := map[string]bool{}

	var drifted []string

	for i := range sites {
		status := sites[i].Status.Overrides
		if status == nil {
			continue
		}

		for _, workload := range status.Workloads {
			if workload.VersionDrift == "" {
				continue
			}

			line := fmt.Sprintf("  %s %s/%s: %s", sites[i].Name, workload.Kind, workload.Name, workload.VersionDrift)
			if seen[line] {
				continue
			}

			seen[line] = true

			drifted = append(drifted, line)
		}
	}

	if len(drifted) == 0 {
		return
	}

	sort.Strings(drifted)

	fprintln(out, "\nVersion drift: these workloads no longer run the operator's own image.")
	fprintln(out, "They will not be updated by an operator upgrade.")

	for _, line := range drifted {
		fprintln(out, line)
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}

	return value
}
