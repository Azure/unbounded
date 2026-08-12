// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/printers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/override"
	"github.com/Azure/unbounded/internal/unbounded"
)

func overridesListCommand() *cobra.Command {
	var (
		namespace  string
		kubeconfig string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the override entries the operator reads",
		Long: `List the override entries in the unbounded-component-overrides ConfigMap,
as authored.

This shows what was asked for. To see what the operator actually did with it,
use 'kubectl unbounded overrides status'.

Example:
  kubectl unbounded overrides list`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClientWithKubeconfig(kubeconfig)
			if err != nil {
				return err
			}

			return runOverridesList(ctx, c, namespace, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", unbounded.SystemNamespace(),
		"Namespace holding the overrides ConfigMap")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")

	return cmd
}

func runOverridesList(ctx context.Context, c client.Client, namespace string, out io.Writer) error {
	configMap, found, err := getOverridesConfigMap(ctx, c, namespace)
	if err != nil {
		return err
	}

	if !found {
		fprintf(out, "No overrides ConfigMap found in namespace %q.\n", namespace)
		fprintln(out, "If the operator is installed elsewhere, pass --namespace.")

		return nil
	}

	entries, problems, err := override.Parse(configMap.Data)
	if err != nil {
		return err
	}

	problems = append(problems, override.Validate(entries)...)

	if len(entries) == 0 && len(problems) == 0 {
		fprintln(out, "The overrides ConfigMap exists but declares no entries.")

		return nil
	}

	// A Site list failure must not cost the user the table. The Sites are only
	// needed for an advisory warning about selectors that match nothing, so
	// saying the check could not run is better than printing nothing at all.
	var (
		sites     v1alpha3.SiteList
		sitesRead = true
	)

	if err := c.List(ctx, &sites); err != nil {
		sitesRead = false

		fprintf(out, "Warning: could not list Sites, so Site selectors were not checked: %v\n\n", err)
	}

	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Source", Type: "string"},
			{Name: "Component", Type: "string"},
			{Name: "Kind", Type: "string"},
			{Name: "Sites", Type: "string"},
			{Name: "Changes", Type: "string"},
		},
	}

	for _, entry := range entries {
		table.Rows = append(table.Rows, metav1.TableRow{Cells: []any{
			entry.Source.String(),
			entry.Entry.Component,
			entry.Entry.Kind,
			describeSiteSelector(entry.Entry.Sites),
			describeChanges(entry.Entry),
		}})
	}

	if err := printers.NewTablePrinter(printers.PrintOptions{}).PrintObj(table, out); err != nil {
		return fmt.Errorf("print overrides: %w", err)
	}

	if sitesRead {
		reportUnknownSites(entries, sites.Items, out)
	}

	fprintf(out, "\nObserved ConfigMap resourceVersion: %s\n", configMap.ResourceVersion)
	fprintln(out, "Run 'kubectl unbounded overrides status' to see what the operator applied.")

	// Listing what a document declares is not the same as saying it works.
	// Parsing alone accepted a document the operator rejects outright, so this
	// printed a clean table while the operator was Degraded and had stopped
	// reconciling the workloads the document names.
	if len(problems) > 0 {
		fprintln(out, "")

		return override.ProblemsError(problems)
	}

	return nil
}

// describeSiteSelector renders a Site selector, distinguishing an absent
// selector from a listed one because the two mean different things.
func describeSiteSelector(sites []string) string {
	if sites == nil {
		return "(all)"
	}

	return strings.Join(sites, ",")
}

// describeChanges summarizes what an entry changes, so the table is useful
// without dumping whole patches.
func describeChanges(entry override.Entry) string {
	var parts []string

	if len(entry.Patch) > 0 {
		parts = append(parts, "patch")
	}

	if len(entry.ExtraArgs) > 0 {
		containers := make([]string, 0, len(entry.ExtraArgs))
		for name := range entry.ExtraArgs {
			containers = append(containers, name)
		}

		sort.Strings(containers)

		parts = append(parts, "extraArgs("+strings.Join(containers, ",")+")")
	}

	if len(entry.AddContainers) > 0 {
		parts = append(parts, "add("+strings.Join(entry.AddContainers, ",")+")")
	}

	if len(entry.AddInitContainers) > 0 {
		parts = append(parts, "addInit("+strings.Join(entry.AddInitContainers, ",")+")")
	}

	return joinNonEmpty(parts, " ")
}

// reportUnknownSites warns about Site names that match nothing.
//
// These are inert rather than an error: a document may legitimately be written
// before its Site exists, and deleting a Site must not retroactively invalidate
// an unrelated override. Saying so is still useful, because the other
// explanation is a typo.
//
// The matching itself comes from the override package rather than being
// repeated here, so the CLI and the operator cannot disagree about what a Site
// selector means.
func reportUnknownSites(entries []override.SourcedEntry, sites []v1alpha3.Site, out io.Writer) {
	known := make([]string, 0, len(sites))
	for i := range sites {
		known = append(known, sites[i].Name)
	}

	unknown := override.UnmatchedSites(entries, known)
	if len(unknown) == 0 {
		return
	}

	fprintf(out, "\nWarning: these Site names match no Site and are inert: %s\n", strings.Join(unknown, ", "))
	fprintln(out, "         This is expected if the Site has not been created yet.")
}
