// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// prepareInputs builds release-prepare's dispatch inputs.
//
// Booleans are sent as booleans. The REST API accepts them either way, but
// release-prepare compares dry_run against both true and 'true' precisely
// because a string is truthy, and sending the right type keeps that guard from
// being the only thing standing between a dry run and a tag.
func prepareInputs(mode, branch string, major bool, pre, explicit string, concurrent bool) map[string]any {
	inputs := map[string]any{
		"mode":   mode,
		"branch": branch,
		"major":  major,
	}

	if pre != "" {
		inputs["pre"] = pre
	}

	if explicit != "" {
		inputs["version"] = explicit
	}

	if concurrent {
		inputs["allow_concurrent_trains"] = true
	}

	return inputs
}

// prepareCommand builds cut, rc and promote, which differ only in mode.
func prepareCommand(opts *Options, use, mode, short, long string) *cobra.Command {
	var (
		branch     string
		major      bool
		pre        string
		explicit   string
		concurrent bool
		yes        bool
		dryRun     bool
	)

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveBranch(opts.repo(cmd.Context()), branch, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			if explicit != "" && mode != string(version.ModePromote) {
				return fmt.Errorf("--version is only valid with promote (this is %s)", mode)
			}

			tag, warnings := preview(cmd, opts, resolved, version.Request{
				Mode:                  version.Mode(mode),
				Pre:                   pre,
				Version:               explicit,
				AllowConcurrentTrains: concurrent,
			}, major)

			summary := fmt.Sprintf("Dispatching %s from %s.", mode, resolved)
			if tag != "" {
				summary = fmt.Sprintf("This will cut %s from %s.", tag, resolved)
			}

			return dispatch{
				Workflow: gh.WorkflowPrepare,
				// The workflow itself must come from the default branch even
				// when cutting from a release branch: it takes its tooling from
				// main deliberately, and dispatching it on the branch would run
				// that branch's copy of the workflow.
				Ref:      "main",
				Inputs:   prepareInputs(mode, resolved, major, pre, explicit, concurrent),
				Summary:  summary,
				Warnings: warnings,
			}.run(cmd.Context(), cmd, opts, yes, dryRun)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "Branch to cut from (default: the checked-out branch)")
	cmd.Flags().BoolVar(&major, "major", false, "main only: cut a major instead of a minor")
	addDispatchFlags(cmd, &yes, &dryRun)

	if mode == string(version.ModePrerelease) {
		cmd.Flags().StringVar(&pre, "pre", "", "Explicit rc suffix, e.g. rc.3 (blank takes the next)")
		cmd.Flags().BoolVar(&concurrent, "allow-concurrent-trains", false,
			"Permit starting a second live train alongside an existing one")
	}

	if mode == string(version.ModePromote) {
		cmd.Flags().StringVar(&explicit, "version", "",
			"The final version to promote; blank takes the single live train")
	}

	return cmd
}

func cutCommand(opts *Options) *cobra.Command {
	return prepareCommand(opts, "cut", string(version.ModeRelease),
		"Cut a release",
		`Cut a release.

Shows the version it will mint before asking, resolved from the local clone by
the same code release-prepare runs. That is a preview rather than a guess, but
the workflow resolves against its own checkout, so a stale clone can differ.`)
}

func rcCommand(opts *Options) *cobra.Command {
	return prepareCommand(opts, "rc", string(version.ModePrerelease),
		"Cut a release candidate",
		`Cut a release candidate.

A train in flight is continued automatically and the next rc taken, so running
this repeatedly is the normal way to iterate. Starting a SECOND train alongside
an existing one needs --allow-concurrent-trains, because promote then requires
an explicit version and a forgotten train is how v0.1.24 was orphaned at rc.18.`)
}

func promoteCommand(opts *Options) *cobra.Command {
	return prepareCommand(opts, "promote", string(version.ModePromote),
		"Promote a candidate to a final release",
		`Promote a candidate to a final release.

Tags the LAST CANDIDATE'S COMMIT, not HEAD, so anything merged after that
candidate is not in the release. That is the point: the tree that was built,
soaked and smoke-tested is the tree that ships.

With more than one live train this refuses rather than guessing, and asks for
--version. Guessing is what orphaned v0.1.24.`)
}

func branchCommand(opts *Options) *cobra.Command {
	var (
		yes    bool
		dryRun bool
	)

	create := &cobra.Command{
		Use:   "create <series>",
		Short: "Open a release-X.Y maintenance branch",
		Long: `Open a release-X.Y maintenance branch.

The branch point is derived rather than given: the newest release in the series.
Take <series> as X.Y, without the leading v.

A branch cut from a tag that predates the release-* CI triggers will not run CI
on its pull requests, and the release-* ruleset requires those checks, so every
pull request to it would be unmergeable. See RELEASING.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch{
				Workflow: gh.WorkflowBranch,
				Ref:      "main",
				Inputs:   map[string]any{"series": args[0]},
				Summary:  fmt.Sprintf("This will open release-%s at the newest release in that series.", args[0]),
			}.run(cmd.Context(), cmd, opts, yes, dryRun)
		},
	}

	addDispatchFlags(create, &yes, &dryRun)

	group := &cobra.Command{
		Use:   "branch",
		Short: "Work with release branches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	group.AddCommand(create)

	return group
}
