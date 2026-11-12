// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// soakPlan is whether a release soaks on unbounded-stable, and why.
type soakPlan struct {
	Tag  string `json:"tag"`
	Soak bool   `json:"soak"`
	// Reason is one line, because it lands in $GITHUB_OUTPUT and a run summary.
	Reason string `json:"reason"`
	// Candidate is the candidate a final was promoted from, when there is one.
	Candidate string `json:"candidate,omitempty"`
}

// Events release-upgrade runs under.
const (
	eventWorkflowRun      = "workflow_run"
	eventWorkflowDispatch = "workflow_dispatch"
)

func soakPlanCommand(opts *Options) *cobra.Command {
	var event string

	cmd := &cobra.Command{
		Use:   "soak-plan <tag>",
		Short: "Say whether a release soaks on unbounded-stable, and why",
		Long: `Say whether a release soaks on unbounded-stable, and why.

release-upgrade asks this to decide whether to deploy, run Orca and smoke-test.
The answer is no for:

  - anything cut from a release branch. unbounded-stable soaks main only.
  - a final promoted from a candidate that passed its soak, when the build
    triggered it. That candidate is the same commit and already soaked; the
    final is a rebuild of it.

Everything else soaks: every candidate, any final whose candidate did not pass
or cannot be found, and every soak dispatched by hand, which is a request for
one. A result that cannot be read from GitHub soaks too: the safe direction is
the one that touches the cluster, not the one that skips it.

Needs a checkout of the default branch, like classify, and a credential to read
commit statuses when the tag is a final from main.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}

			ctx := cmd.Context()
			repo := opts.repo(ctx)

			if err := requireDefaultBranch(cmd, repo); err != nil {
				return err
			}

			// Lazily, so the cases that need no status, which is most of them,
			// need no credential either.
			status := func(ctx context.Context, commit string) (*gh.Status, error) {
				client, err := opts.client(ctx)
				if err != nil {
					return nil, err
				}

				return client.CandidateStatus(ctx, commit)
			}

			plan, warning, err := planSoak(ctx, repo, args[0], event, status)
			if err != nil {
				return err
			}

			if warning != "" {
				warn(cmd.ErrOrStderr(), "%s", warning)
			}

			out := cmd.OutOrStdout()

			switch opts.Output {
			case OutputJSON:
				return writeJSON(out, plan)
			case OutputGitHub:
				_, err := fmt.Fprintf(out, "soak=%t\nreason=%s\ncandidate=%s\n",
					plan.Soak, oneLine(plan.Reason), plan.Candidate)

				return err
			default:
				return table(out, [][]string{
					{"Tag:", plan.Tag},
					{"Soaks:", yesNo(plan.Soak)},
					{"Why:", plan.Reason},
				})
			}
		},
	}

	cmd.Flags().StringVar(&event, "event", eventWorkflowRun,
		"The event release-upgrade is running under: workflow_run or workflow_dispatch")

	return cmd
}

// planSoak decides whether a release soaks.
//
// The warning is set when a status could not be read and the plan fell back to
// soaking, so the run can say so rather than appear to have decided on merit.
func planSoak(
	ctx context.Context,
	repo version.Repo,
	tag, event string,
	status func(context.Context, string) (*gh.Status, error),
) (plan soakPlan, warning string, err error) {
	switch event {
	case eventWorkflowRun, eventWorkflowDispatch:
	default:
		return soakPlan{}, "", fmt.Errorf("event must be %s or %s, got %q", eventWorkflowRun, eventWorkflowDispatch, event)
	}

	classification, err := version.Classify(repo, tag)
	if err != nil {
		return soakPlan{}, "", err
	}

	plan = soakPlan{Tag: tag, Soak: true}

	switch {
	case !classification.FromMain:
		plan.Soak = false
		plan.Reason = tag + " was cut from a release branch; unbounded-stable soaks main only"

		return plan, "", nil

	case event == eventWorkflowDispatch:
		plan.Reason = "dispatched by hand, which is a request for a soak"

		return plan, "", nil

	case semver.Prerelease(tag) != "":
		plan.Reason = tag + " is a candidate, and a candidate always soaks"

		return plan, "", nil
	}

	// A final from main, built by its tag push. Legacy finals and malformed
	// tags have no candidate to inherit a soak from, and soak.
	if !version.IsFinal(tag) {
		plan.Reason = tag + " is not a canonical final, so it has no candidate to inherit a soak from"

		return plan, "", nil
	}

	candidate, ok, err := version.CandidateAt(repo, tag)
	if err != nil {
		return soakPlan{}, "", err
	}

	if !ok {
		plan.Reason = "no candidate of " + tag + " is on its commit, so nothing has soaked this tree"

		return plan, "", nil
	}

	plan.Candidate = candidate.Tag

	result, err := status(ctx, candidate.Commit)
	if err != nil {
		plan.Reason = "could not read " + candidate.Tag + "'s soak result, so the final soaks"

		return plan, fmt.Sprintf("could not read %s's soak result (%v); soaking %s instead", candidate.Tag, err, tag), nil
	}

	if !result.Passed() {
		state := "no recorded result"
		if result != nil {
			state = result.State
		}

		plan.Reason = "promoted from " + candidate.Tag + ", which has not passed its soak (" + state + "), so the final soaks"

		return plan, "", nil
	}

	plan.Soak = false
	plan.Reason = "promoted from " + candidate.Tag + ", which passed its soak on the same commit"

	return plan, "", nil
}
