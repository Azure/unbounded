// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// candidateStaleAfter is how old a passing candidate may be and still count,
// once main has moved past it.
//
// Age alone is not staleness: a candidate of an unchanged main is the current
// tree however old it is. What goes stale is a candidate main has left behind,
// because promoting it ships a tree from days ago without everything since.
// Two days allows for a weekend, matching what the nightly was held to.
const candidateStaleAfter = 48 * time.Hour

// preflightResult answers whether a branch is releasable.
type preflightResult struct {
	Branch     string `json:"branch"`
	Releasable bool   `json:"releasable"`
	// CandidateOK says whether there is a passing candidate worth promoting.
	CandidateOK bool `json:"candidateOk"`
	// Candidate is what promote would finalize: the newest that passed.
	Candidate *candidateInfo `json:"candidate,omitempty"`
	// Newest is the newest candidate, when it is not the one that passed.
	Newest *candidateInfo `json:"newest,omitempty"`
	// Behind counts commits on the checkout since Candidate.
	Behind     int      `json:"behind,omitempty"`
	CIOK       bool     `json:"ciOk"`
	LiveTrains []string `json:"liveTrains,omitempty"`
	Blockers   []string `json:"blockers,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

func preflightCommand(opts *Options) *cobra.Command {
	var branch string

	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Check whether a branch is releasable",
		Long: `Check whether a branch is releasable.

Releasable means there is a candidate to promote: the newest candidate that
passed its soak, recent enough to be worth shipping, with CI green on the
branch. Says RELEASABLE or NOT RELEASABLE with the reason.

A newer candidate that failed is reported but does not block: promote ships the
one that passed, not the one that failed. Read why it failed before promoting
past it all the same.

Candidates are read from the local clone and their results from GitHub, so this
needs an up-to-date checkout and a credential.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPreflight(cmd.Context(), cmd.OutOrStdout(), opts, branch)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "main", "Branch to check")

	return cmd
}

func runPreflight(ctx context.Context, out io.Writer, opts *Options, branch string) error {
	if err := opts.validateOutput(); err != nil {
		return err
	}

	if opts.Output == OutputGitHub {
		return fmt.Errorf("preflight has no github output; use json")
	}

	client, err := opts.client(ctx)
	if err != nil {
		return err
	}

	result := preflightResult{Branch: branch, Releasable: true}
	repo := opts.repo(ctx)

	if branch != "main" {
		result.Notes = append(result.Notes,
			"candidates cut from "+branch+" are verified but not soaked: unbounded-stable soaks main only")
	}

	if current, err := repo.CurrentBranch(); err == nil && current != "" && current != branch {
		result.Notes = append(result.Notes,
			"candidates are read from the checkout, which is "+current+", not "+branch)
	}

	if err := judgeCandidates(ctx, client, repo, &result, time.Now()); err != nil {
		return err
	}

	ci, err := client.Runs(ctx, gh.ListRuns{Workflow: gh.WorkflowCI, Branch: branch, Limit: 5})
	if err != nil {
		return err
	}

	switch {
	case len(ci) == 0:
		result.Blockers = append(result.Blockers, "no CI runs found for "+branch)
	case !ci[0].Done():
		result.Blockers = append(result.Blockers, "CI is still running on "+branch)
	case !ci[0].Succeeded():
		result.Blockers = append(result.Blockers, "CI is "+ci[0].State()+" on "+branch)
	default:
		result.CIOK = true
	}

	result.Releasable = len(result.Blockers) == 0

	if opts.Output == OutputJSON {
		return writeJSON(out, result)
	}

	return writePreflightText(out, result, time.Now())
}

// judgeCandidates decides whether there is a candidate worth promoting.
//
// A failure to read the local state is a blocker rather than a note, which is
// where this differs from the nightly it replaces. The nightly was its own
// signal and the train view decorated it; here the train view IS the signal,
// and "could not tell" must not read as releasable.
func judgeCandidates(
	ctx context.Context,
	client *gh.Client,
	repo *version.GitRepo,
	result *preflightResult,
	now time.Time,
) error {
	state, err := version.Discover(repo)
	if err != nil {
		result.Blockers = append(result.Blockers, "could not read candidates from the checkout: "+err.Error())

		return nil
	}

	result.LiveTrains = state.Live

	if len(state.Live) == 0 {
		result.Blockers = append(result.Blockers,
			"no candidate has been cut since "+state.LatestFinal+
				"; cut one with relctl rc, or wait for the scheduled one")

		return nil
	}

	// The highest train is the one anyone is about to promote. With more than
	// one, promote asks which, and saying so here saves a refusal there.
	train := slices.MaxFunc(state.Live, semver.Compare)
	if len(state.Live) > 1 {
		result.Notes = append(result.Notes,
			fmt.Sprintf("several trains are live; this reports %s, and promote will need --version", train))
	}

	candidates, err := version.Candidates(repo, train)
	if err != nil {
		return err
	}

	results, err := candidateResults(ctx, client, candidates)
	if err != nil {
		return err
	}

	passing, ok := newestPassing(results)

	if len(results) > 0 && (!ok || results[0].Tag != passing.Tag) {
		newest := results[0]
		result.Newest = &newest
	}

	if !ok {
		result.Blockers = append(result.Blockers,
			"no candidate of "+train+" has passed its soak; the newest, "+results[0].Tag+", has "+describeState(results[0]))

		return nil
	}

	result.CandidateOK = true
	result.Candidate = &passing

	if result.Newest != nil {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"the newest candidate, %s, has %s; promote ships %s, so read why before promoting past it",
			result.Newest.Tag, describeState(*result.Newest), passing.Tag))
	}

	behind, err := repo.CountCommits(passing.Commit)
	if err != nil {
		return err
	}

	result.Behind = behind

	if behind == 0 {
		return nil
	}

	if passing.At != nil && now.Sub(*passing.At) > candidateStaleAfter {
		result.CandidateOK = false
		result.Blockers = append(result.Blockers, fmt.Sprintf(
			"%s passed %s ago and %d commit(s) have merged since; cut a fresh candidate rather than ship a tree that old",
			passing.Tag, elapsed(*passing.At, now), behind))

		return nil
	}

	result.Notes = append(result.Notes, fmt.Sprintf(
		"%d commit(s) merged since %s are not in it; promote ships %s as it was soaked", behind, passing.Tag, passing.Tag))

	return nil
}

func writePreflightText(out io.Writer, result preflightResult, now time.Time) error {
	verdict := "RELEASABLE"
	if !result.Releasable {
		verdict = "NOT RELEASABLE"
	}

	rows := [][]string{
		{"Branch:", result.Branch},
		{"Verdict:", verdict},
		{"Candidate:", tick(result.CandidateOK) + candidateSuffix(result.Candidate, now)},
		{"CI:", tick(result.CIOK)},
	}

	if len(result.LiveTrains) > 0 {
		rows = append(rows, []string{"Live trains:", joinOrNone(result.LiveTrains)})
	}

	if err := table(out, rows); err != nil {
		return err
	}

	for _, blocker := range result.Blockers {
		if _, err := fmt.Fprintf(out, "\nblocker: %s\n", blocker); err != nil {
			return err
		}
	}

	for _, note := range result.Notes {
		if _, err := fmt.Fprintf(out, "note: %s\n", note); err != nil {
			return err
		}
	}

	return nil
}

func tick(ok bool) string {
	if ok {
		return "ok"
	}

	return "FAILED"
}

func candidateSuffix(candidate *candidateInfo, now time.Time) string {
	if candidate == nil {
		return ""
	}

	suffix := " " + candidate.Tag
	if when := recordedAgo(*candidate, now); when != "" {
		suffix += " (passed " + when + ")"
	}

	return suffix
}
