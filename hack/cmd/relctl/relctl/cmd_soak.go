// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
)

// The break-glass paths.
//
// Both take a typed confirmation rather than accepting --yes, so neither can be
// reached by reflex or by a script that passes --yes everywhere. Making an
// unsoaked publish one keystroke shorter is not a convenience worth having, and
// the phrase is what forces the operator to read what they are about to do.

func soakCommand(opts *Options) *cobra.Command {
	var (
		forceInit   bool
		maxNotReady int
		yes         bool
		dryRun      bool
	)

	cmd := &cobra.Command{
		Use:   "soak <tag>",
		Short: "Re-run the soak for a tag",
		Long: `Re-run the soak for a tag.

The ordinary use is retrying a soak that failed for a reason since fixed, which
needs no override at all.

--max-notready-nodes raises the tolerance ceiling when a known outage takes out
more nodes than the default allows. It raises the ceiling ONLY: a shortfall must
still be entirely explained by NotReady nodes, and anything unhealthy on a
reachable node still fails. State the number deliberately, because it is a claim
someone can review.

--force-init runs 'site init' instead of upgrade-apply, which is for a
first-ever bootstrap and not for recovery.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tag := args[0]
			inputs := map[string]any{"tag": tag}

			// Changed() rather than a zero check: --max-notready-nodes=0
			// disables tolerance entirely, which is a legitimate thing to ask
			// for and the opposite of leaving the flag alone.
			maxNotReadySet := cmd.Flags().Changed("max-notready-nodes")

			var (
				warnings []string
				confirm  string
			)

			if maxNotReadySet {
				// A string, while force_init below is a bool. That matches the
				// workflow: max_notready_nodes is declared `type: string` and
				// force_init `type: boolean`, and a dispatch whose input types
				// disagree with the declaration is rejected.
				inputs["max_notready_nodes"] = fmt.Sprint(maxNotReady)
				warnings = append(warnings,
					fmt.Sprintf("tolerating up to %d NotReady nodes; the release will not be validated on them", maxNotReady))
			}

			if forceInit {
				inputs["force_init"] = true
				// Typed rather than --yes: site init on a cluster that is
				// already initialized is not a retry.
				confirm = "init " + tag

				warnings = append(warnings,
					"force_init runs 'site init', not upgrade-apply; this is for a first-ever bootstrap")
			}

			return dispatch{
				Workflow: gh.WorkflowUpgrade,
				Ref:      "main",
				Inputs:   inputs,
				Summary:  fmt.Sprintf("This will re-run the soak for %s on unbounded-stable.", tag),
				Warnings: warnings,
				Confirm:  confirm,
			}.run(cmd.Context(), cmd, opts, yes, dryRun)
		},
	}

	cmd.Flags().BoolVar(&forceInit, "force-init", false, "Run 'site init' instead of upgrade-apply")
	cmd.Flags().IntVar(&maxNotReady, "max-notready-nodes", 0, "Raise the NotReady node ceiling")
	addDispatchFlags(cmd, &yes, &dryRun)

	return cmd
}

func publishCommand(opts *Options) *cobra.Command {
	var (
		reason string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "publish <tag>",
		Short: "BREAK GLASS: publish without a passing soak",
		Long: `BREAK GLASS: publish a release whose soak did not pass.

For when the soak CLUSTER is the problem and the release must ship. If the
release is the problem, this is the wrong tool.

  - The reason is required, and is written into the release body where it stays
    visible long after the CI logs expire.
  - Artifact signatures and the release BOM are still verified. Forcing means
    "we accept an unsoaked release", never "we accept an unverified one".
  - The deploy and smoke jobs still run, so their diagnostics are preserved.

There is no --yes. The confirmation must be typed, because a publish that
skipped its soak should not be reachable by reflex or by a script that passes
--yes everywhere.

If you use this, file a follow-up to fix whatever made it necessary.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tag := args[0]

			if reason == "" {
				return fmt.Errorf("--reason is required: it is recorded on the release")
			}

			return dispatch{
				Workflow: gh.WorkflowUpgrade,
				Ref:      "main",
				Inputs: map[string]any{
					"tag":           tag,
					"force_publish": true,
					"reason":        reason,
				},
				Summary: fmt.Sprintf("This will publish %s WITHOUT a passing soak.", tag),
				Warnings: []string{
					"the release will ship having been deployed nowhere successfully",
					"signatures and the BOM are still verified; only the soak is bypassed",
				},
				Confirm: "publish " + tag + " unsoaked",
			}.run(cmd.Context(), cmd, opts, false, dryRun)
		},
	}

	cmd.Flags().StringVar(&reason, "reason", "", "Why the gate is being bypassed (required, recorded on the release)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be dispatched and stop")

	return cmd
}
