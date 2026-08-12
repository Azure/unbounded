// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

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

			c, err := newMachineClientWithKubeconfig(kubeconfig)
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

	// The operator copies a cluster singleton's row onto every Site, because
	// `kubectl get site` would otherwise hide the commonest case entirely. Here
	// that turns one pinned image into one row per Site, so identical rows are
	// collapsed into a single "(all sites)" entry.
	shared := sharedWorkloads(ordered)

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

		own := make([]v1alpha3.OverriddenWorkload, 0, len(status.Workloads))

		for _, workload := range status.Workloads {
			if !shared[workloadKey(workload)] {
				own = append(own, workload)
			}
		}

		if len(own) == 0 {
			table.Rows = append(table.Rows, metav1.TableRow{Cells: []any{
				site.Name, status.Phase, "-", "-", "-",
			}})

			continue
		}

		for _, workload := range own {
			table.Rows = append(table.Rows, metav1.TableRow{Cells: []any{
				site.Name,
				status.Phase,
				workload.Kind + "/" + workload.Name,
				describeApplied(workload),
				orDash(workload.VersionDrift),
			}})
		}
	}

	for _, workload := range sortedSharedWorkloads(ordered, shared) {
		table.Rows = append(table.Rows, metav1.TableRow{Cells: []any{
			"(all sites)",
			"-",
			workload.Kind + "/" + workload.Name,
			describeApplied(workload),
			orDash(workload.VersionDrift),
		}})
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

	// The docs send users here to confirm an apply worked, which is only useful
	// if it can gate a CI step. validate already exits non-zero on a document
	// it rejects; a Site the operator is reporting Degraded is the same answer
	// arriving later.
	if len(degraded) > 0 {
		return fmt.Errorf("%d Site(s) report overrides Degraded", len(degraded))
	}

	return nil
}

// describeApplied says what happened to one workload's override.
//
// It reads the state the operator recorded rather than inferring one from the
// hashes. Inferring was wrong in the case that matters most: a workload whose
// override failed has no applied hash and, before the operator published a
// desired hash on that path, no desired hash either, so the failure rendered as
// "no override", the exact opposite of the truth.
//
// The hash comparison remains as a fallback for a status written by an operator
// that predates the state field, or one a lagging CRD pruned it from. Both
// hashes are computed over the same contributor set for the same workload, so
// they stay directly comparable.
func describeApplied(workload v1alpha3.OverriddenWorkload) string {
	switch workload.State {
	case v1alpha3.OverrideStateApplied:
		return "yes"
	case v1alpha3.OverrideStatePending:
		return "pending"
	case v1alpha3.OverrideStateWithheld:
		return "withheld"
	case v1alpha3.OverrideStateFailed:
		return "failed"
	}

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

// workloadKey identifies a workload row independently of the Site carrying it.
func workloadKey(workload v1alpha3.OverriddenWorkload) string {
	return workload.Kind + "/" + workload.Name
}

// sharedWorkloads returns the workloads that appear identically on every Site
// reporting overrides, which is what a cluster singleton looks like from here.
//
// Identity includes the state and the drift, so a singleton that resolved
// differently on different Sites is deliberately not collapsed: that is a real
// divergence and hiding it would be worse than repeating it.
func sharedWorkloads(sites []v1alpha3.Site) map[string]bool {
	counts := map[string]int{}
	distinct := map[string]map[v1alpha3.OverriddenWorkload]bool{}
	reporting := 0

	for i := range sites {
		status := sites[i].Status.Overrides
		if status == nil || status.Phase == v1alpha3.OverridePhaseNone {
			continue
		}

		reporting++

		for _, workload := range status.Workloads {
			key := workloadKey(workload)
			counts[key]++

			if distinct[key] == nil {
				distinct[key] = map[v1alpha3.OverriddenWorkload]bool{}
			}

			distinct[key][workload] = true
		}
	}

	// With one Site there is nothing to collapse, and calling its rows
	// "(all sites)" would be a strange way to say "edge-west".
	if reporting < 2 {
		return map[string]bool{}
	}

	out := map[string]bool{}

	for key, count := range counts {
		if count == reporting && len(distinct[key]) == 1 {
			out[key] = true
		}
	}

	return out
}

// sortedSharedWorkloads returns one copy of each collapsed workload, in a
// stable order.
func sortedSharedWorkloads(sites []v1alpha3.Site, shared map[string]bool) []v1alpha3.OverriddenWorkload {
	seen := map[string]bool{}

	var out []v1alpha3.OverriddenWorkload

	for i := range sites {
		status := sites[i].Status.Overrides
		if status == nil {
			continue
		}

		for _, workload := range status.Workloads {
			key := workloadKey(workload)
			if !shared[key] || seen[key] {
				continue
			}

			seen[key] = true

			out = append(out, workload)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}

		return out[i].Name < out[j].Name
	})

	return out
}

func reportObservedVersions(sites []v1alpha3.Site, out io.Writer) {
	versions := map[string]bool{}

	for i := range sites {
		status := sites[i].Status.Overrides

		// Only Sites that appear in the table above. A Site reporting no
		// overrides is not part of the disagreement being described.
		if status == nil || status.Phase == v1alpha3.OverridePhaseNone {
			continue
		}

		if status.ObservedResourceVersion != "" {
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

	// resourceVersions are opaque, but they are decimal in every apiserver in
	// practice, so ordering them numerically reads correctly and lexicographic
	// ordering does not: 100 would sort before 9.
	sort.Slice(ordered, func(i, j int) bool {
		left, lok := strconv.ParseUint(ordered[i], 10, 64)
		right, rok := strconv.ParseUint(ordered[j], 10, 64)

		if lok == nil && rok == nil {
			return left < right
		}

		return ordered[i] < ordered[j]
	})

	fprintf(out, "\nNote: Sites observed different ConfigMap versions (%s). This is transient;\n",
		strings.Join(ordered, ", "))
	fprintln(out, "      a newer version always triggers another reconcile.")
}

func reportDegraded(degraded []v1alpha3.Site, out io.Writer) {
	if len(degraded) == 0 {
		return
	}

	fprintln(out, "\nDegraded Sites:")

	// A document-level failure is written verbatim to every Site, so printing
	// per Site repeated a message that can run to two kilobytes once per Site.
	// Identical messages are reported once, naming the Sites that share them.
	byMessage := map[string][]string{}
	order := []string{}

	for i := range degraded {
		message := degraded[i].Status.Overrides.Message
		if _, seen := byMessage[message]; !seen {
			order = append(order, message)
		}

		byMessage[message] = append(byMessage[message], degraded[i].Name)
	}

	for _, message := range order {
		sites := byMessage[message]

		who := strings.Join(sites, ", ")
		if len(sites) == len(degraded) && len(sites) > 1 {
			who = "all " + strconv.Itoa(len(sites)) + " Sites"
		}

		fprintf(out, "  %s:\n", who)

		// Validation reports every problem it finds, so a message is routinely
		// several lines. Indenting the continuations keeps them from reading as
		// further Sites.
		for _, line := range strings.Split(message, "\n") {
			fprintf(out, "    %s\n", strings.TrimSpace(line))
		}
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

			// The key deliberately omits the Site. A cluster singleton is
			// reported on every Site, so including it made this dedup
			// unreachable and turned one pinned image into one line per Site.
			key := workloadKey(workload) + "=" + workload.VersionDrift
			if seen[key] {
				continue
			}

			seen[key] = true

			drifted = append(drifted, fmt.Sprintf("  %s/%s: %s",
				workload.Kind, workload.Name, workload.VersionDrift))
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
