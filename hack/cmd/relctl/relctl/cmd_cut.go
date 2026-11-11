// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// prepareRequest is everything a release-prepare dispatch can carry.
type prepareRequest struct {
	mode       string
	branch     string
	major      bool
	pre        string
	version    string
	candidate  string
	concurrent bool
	force      bool
	reason     string
}

// inputs builds release-prepare's dispatch inputs.
//
// Booleans are sent as booleans. The REST API accepts them either way, but
// release-prepare compares dry_run against both true and 'true' precisely
// because a string is truthy, and sending the right type keeps that guard from
// being the only thing standing between a dry run and a tag.
func (p prepareRequest) inputs() map[string]any {
	inputs := map[string]any{
		"mode":   p.mode,
		"branch": p.branch,
		"major":  p.major,
	}

	if p.pre != "" {
		inputs["pre"] = p.pre
	}

	if p.version != "" {
		inputs["version"] = p.version
	}

	if p.candidate != "" {
		inputs["candidate"] = p.candidate
	}

	if p.concurrent {
		inputs["allow_concurrent_trains"] = true
	}

	if p.force {
		inputs["force"] = true
		inputs["reason"] = p.reason
	}

	return inputs
}

// cutCommand is what `relctl cut` became.
//
// Kept as a command rather than deleted, so the muscle memory and every old
// runbook get an explanation instead of "unknown command". Flags are accepted
// and ignored for the same reason: `relctl cut --branch main` should say why,
// not complain about a flag.
func cutCommand(_ *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "cut",
		Short: "Retired: cut a candidate with 'rc', then finalize it with 'promote'",
		Long: `Retired.

'cut' minted a final version straight from HEAD, before anything had shown the
tree could be built or deployed, so every failure that needed a code change cost
a version number. A final is now only ever a promoted candidate:

  relctl rc        cut a candidate (the schedule also cuts them)
  relctl promote   finalize the newest candidate that passed its soak`,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return version.CheckMode(version.ModeRelease)
		},
	}
}

func rcCommand(opts *Options) *cobra.Command {
	var (
		req    = prepareRequest{mode: string(version.ModePrerelease)}
		yes    bool
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "rc",
		Short: "Cut a release candidate",
		Long: `Cut a release candidate.

A train in flight is continued automatically and the next rc taken, so running
this repeatedly is the normal way to iterate. Starting a SECOND train alongside
an existing one needs --allow-concurrent-trains, because promote then requires
an explicit version and a forgotten train is how v0.1.24 was orphaned at rc.18.

release-prepare also cuts candidates on a schedule. Cutting one by hand is for
when you need one now: a fix that has just merged, or a retry of a commit whose
candidate failed for a reason that has since been fixed. Unlike the schedule,
this cuts even when HEAD already carries a candidate.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveBranch(opts.repo(cmd.Context()), req.branch, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			req.branch = resolved

			tag, warnings := preview(cmd, opts, resolved, version.Request{
				Mode:                  version.ModePrerelease,
				Pre:                   req.pre,
				AllowConcurrentTrains: req.concurrent,
			}, req.major)

			summary := fmt.Sprintf("Dispatching a candidate from %s.", resolved)
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
				Inputs:   req.inputs(),
				Summary:  summary,
				Warnings: warnings,
			}.run(cmd.Context(), cmd, opts, yes, dryRun)
		},
	}

	cmd.Flags().StringVar(&req.branch, "branch", "", "Branch to cut from (default: the checked-out branch)")
	cmd.Flags().BoolVar(&req.major, "major", false, "main only: start a major train instead of a minor")
	cmd.Flags().StringVar(&req.pre, "pre", "", "Explicit rc suffix, e.g. rc.3 (blank takes the next)")
	cmd.Flags().BoolVar(&req.concurrent, "allow-concurrent-trains", false,
		"Permit starting a second live train alongside an existing one")
	addDispatchFlags(cmd, &yes, &dryRun)

	return cmd
}

func promoteCommand(opts *Options) *cobra.Command {
	var (
		req    = prepareRequest{mode: string(version.ModePromote)}
		yes    bool
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Promote the newest passing candidate to a final release",
		Long: `Promote a candidate to a final release.

Finalizes the NEWEST CANDIDATE THAT PASSED ITS SOAK, which is often not the
highest: with scheduled candidates the highest may still be building, or may
have failed where the one below it did not. --candidate names one instead, and
it must have passed too.

The tag goes on that candidate's commit, not on HEAD, so anything merged after
it is not in the release. That is the point: the tree that was built, soaked and
smoke-tested is the tree that ships. The final is rebuilt from it, and skips its
own soak because its candidate already passed one.

With more than one live train this refuses rather than guessing, and asks for
--version. Guessing is what orphaned v0.1.24.

BREAK GLASS: --force promotes a candidate that has not passed, for when the soak
cluster rather than the release is the problem. It needs --reason and a typed
confirmation, runs behind the unbounded-reviewers environment, and the final
then soaks; if that cannot pass either, 'relctl publish' is the next step.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPromote(cmd, opts, req, yes, dryRun)
		},
	}

	cmd.Flags().StringVar(&req.branch, "branch", "", "Branch to promote on (default: the checked-out branch)")
	cmd.Flags().StringVar(&req.version, "version", "",
		"The final version to promote; blank takes the single live train")
	cmd.Flags().StringVar(&req.candidate, "candidate", "",
		"The rc to finalize, e.g. v0.5.0-rc.3 (default: the newest that passed its soak)")
	cmd.Flags().BoolVar(&req.force, "force", false,
		"BREAK GLASS: promote a candidate that has not passed its soak")
	cmd.Flags().StringVar(&req.reason, "reason", "", "Why --force is needed (required with it, recorded on the run)")
	addDispatchFlags(cmd, &yes, &dryRun)

	return cmd
}

func runPromote(cmd *cobra.Command, opts *Options, req prepareRequest, yes, dryRun bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	if req.force && req.reason == "" {
		return fmt.Errorf("--reason is required with --force: it is recorded on the run")
	}

	if !req.force && req.reason != "" {
		return fmt.Errorf("--reason only applies with --force")
	}

	repo := opts.repo(ctx)

	resolved, err := resolveBranch(repo, req.branch, cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	req.branch = resolved

	var warnings []string

	// Which candidate passed is on GitHub, so this needs a credential even to
	// preview. Without one, a dry run still describes the dispatch; a real
	// promote never gets here, because dispatching needs the same credential.
	if !req.force {
		client, err := opts.client(ctx)

		switch {
		case err != nil && dryRun:
			warnings = append(warnings, "could not check which candidate passed: "+err.Error())
		case err != nil:
			return err
		default:
			picked, results, err := pickCandidate(ctx, client, repo, req.version, req.candidate)
			if err != nil {
				writeCandidateResults(out, results)

				return err
			}

			// Sent explicitly, so the workflow promotes what was previewed
			// rather than whatever is newest by the time it runs.
			req.candidate = picked.Tag
		}
	}

	tag, previewWarnings := preview(cmd, opts, resolved, version.Request{
		Mode:      version.ModePromote,
		Version:   req.version,
		Candidate: req.candidate,
	}, false)

	warnings = append(warnings, previewWarnings...)

	summary := fmt.Sprintf("This will promote from %s.", resolved)

	switch {
	case tag != "" && req.candidate != "":
		summary = fmt.Sprintf("This will promote %s to %s on %s.", req.candidate, tag, resolved)
	case tag != "":
		summary = fmt.Sprintf("This will cut %s from %s.", tag, resolved)
	}

	confirm := ""

	if req.force {
		target := tag
		if target == "" {
			target = "the live train"
		}

		// Typed rather than --yes, like every other break-glass path.
		confirm = "promote " + target + " unsoaked"

		warnings = append(warnings,
			"the candidate has NOT passed its soak; the final will soak instead, and may need 'relctl publish' if it cannot pass either",
			"reason: "+req.reason)
	}

	return dispatch{
		Workflow: gh.WorkflowPrepare,
		Ref:      "main",
		Inputs:   req.inputs(),
		Summary:  summary,
		Warnings: warnings,
		Confirm:  confirm,
	}.run(ctx, cmd, opts, yes, dryRun)
}

// writeCandidateResults shows what each candidate recorded, so a refusal says
// what it found rather than only that it found nothing usable.
//
//nolint:errcheck // context for an error already being returned
func writeCandidateResults(out io.Writer, results []candidateInfo) {
	if len(results) == 0 {
		return
	}

	rows := [][]string{{"  CANDIDATE", "RESULT"}}
	for _, result := range results {
		rows = append(rows, []string{"  " + result.Tag, result.State})
	}

	fmt.Fprintln(out, "Candidates, newest first:")
	table(out, rows)
	fmt.Fprintln(out)
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
pull request to it would be unmergeable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Checked here as well as in the workflow. "v0.4" is the obvious
			// thing to type given every other argument takes a tag, and
			// learning otherwise should not cost a dispatch and a minute.
			if err := version.CheckSeries(args[0]); err != nil {
				return err
			}

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
