// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// nextResult is what `relctl next` answers.
type nextResult struct {
	Tag         string   `json:"tag"`
	Base        string   `json:"base"`
	Bump        string   `json:"bump"`
	Series      string   `json:"series,omitempty"`
	LatestFinal string   `json:"latestFinal"`
	Live        []string `json:"live,omitempty"`
	Stale       []string `json:"stale,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

func nextCommand(opts *Options) *cobra.Command {
	var (
		branch     string
		mode       string
		major      bool
		pre        string
		explicit   string
		concurrent bool
	)

	cmd := &cobra.Command{
		Use:   "next",
		Short: "Show the version that would be cut, without cutting it",
		Long: `Show the version that would be cut, without cutting it.

Computed from the local clone, so this needs an up-to-date checkout and no
GitHub credential at all. Fetch tags first if the answer looks stale.

The same resolution the release workflow performs, which is the point: the
answer here and the answer there come from one implementation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNext(cmd, opts, version.Request{
				Mode:                  version.Mode(mode),
				Series:                "",
				Pre:                   pre,
				Version:               explicit,
				AllowConcurrentTrains: concurrent,
			}, branch, major)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "",
		"Branch to cut from: main, or release-X.Y (default: the checked-out branch)")
	cmd.Flags().StringVar(&mode, "mode", string(version.ModeRelease), "release, prerelease or promote")
	cmd.Flags().BoolVar(&major, "major", false, "main only: cut a major instead of a minor")
	cmd.Flags().StringVar(&pre, "pre", "", "Explicit prerelease suffix, e.g. rc.3")
	cmd.Flags().StringVar(&explicit, "version", "", "promote only: the final version to cut")
	cmd.Flags().BoolVar(&concurrent, "allow-concurrent-trains", false,
		"Permit starting a second live prerelease train")

	return cmd
}

func runNext(cmd *cobra.Command, opts *Options, req version.Request, branch string, major bool) error {
	if err := opts.validateOutput(); err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	repo := opts.repo(cmd.Context())

	branch, err := resolveBranch(repo, branch, cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	// The branch decides what may be cut before anything is computed: main cuts
	// minors and majors, release-X.Y cuts patches, nothing else releases at all.
	policy, err := version.ForBranch(branch, major)
	if err != nil {
		return err
	}

	req.Bump = policy.Bump
	req.Series = policy.Series

	result, err := version.Resolve(repo, req)
	if err != nil {
		return err
	}

	answer := nextResult{
		Tag:         result.Tag,
		Base:        result.Base,
		Bump:        string(policy.Bump),
		Series:      policy.Series,
		LatestFinal: result.LatestFinal,
		Live:        result.Live,
		Stale:       result.Stale,
		Warnings:    result.Warnings,
	}

	switch opts.Output {
	case OutputJSON:
		return writeJSON(out, answer)

	case OutputGitHub:
		// All four keys, so one call replaces both scripts. release-prepare
		// currently runs bump-for-branch.sh for bump= and series=, then
		// next-version.sh for tag= and base=; relctl already computes the
		// policy internally to resolve at all, so emitting it separately would
		// be reporting a value it had to derive anyway.
		//
		// series is empty on main, and an empty value is still written: a
		// missing key and an empty one differ to a workflow reading it.
		// Warnings go to stderr, so they annotate the run without landing in
		// $GITHUB_OUTPUT, which stdout is redirected into.
		for _, warning := range result.Warnings {
			warn(cmd.ErrOrStderr(), "%s", warning)
		}

		_, err := fmt.Fprintf(out, "tag=%s\nbase=%s\nbump=%s\nseries=%s\n",
			answer.Tag, answer.Base, answer.Bump, answer.Series)

		return err

	default:
		return writeNextText(out, answer, result)
	}
}

func writeNextText(out io.Writer, answer nextResult, result *version.Result) error {
	rows := [][]string{
		{"Tag:", answer.Tag},
		{"Base:", short(answer.Base)},
		{"Bump:", answer.Bump},
	}

	if answer.Series != "" {
		rows = append(rows, []string{"Series:", answer.Series})
	}

	rows = append(rows, []string{"Latest final:", answer.LatestFinal})
	rows = append(rows, []string{"Live trains:", joinOrNone(answer.Live)})

	if len(answer.Stale) > 0 {
		rows = append(rows, []string{"Stale trains:", strings.Join(answer.Stale, " ")})
	}

	if err := table(out, rows); err != nil {
		return err
	}

	for _, warning := range result.Warnings {
		warn(out, "%s", warning)
	}

	return nil
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}

	return strings.Join(items, " ")
}

func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}

	return commit
}

// resolveBranch decides which branch's policy applies.
//
// --branch is POLICY. Tag discovery is separately scoped by reachability from
// local HEAD, so the two are independent inputs and can disagree. Defaulting to
// main regardless meant that on a release-0.3 checkout the bare command applied
// main's minor-bump policy to that branch's history and answered confidently.
//
// The workflow always passes --branch explicitly, so nothing changes there.
// This is about the terminal, where a maintainer patching a release is the
// ordinary case.
func resolveBranch(repo *version.GitRepo, requested string, warnTo io.Writer) (string, error) {
	current, err := repo.CurrentBranch()
	if err != nil {
		// Not fatal on its own: an explicit --branch needs no checkout to agree
		// with, and resolution itself will report a broken repository.
		if requested == "" {
			return "", fmt.Errorf("could not determine the current branch, so --branch is required: %w", err)
		}

		return requested, nil
	}

	if requested == "" {
		if current == "" {
			return "", fmt.Errorf("HEAD is detached, so --branch is required")
		}

		return current, nil
	}

	// Answering about a branch you are not on is legitimate - resolving a
	// hypothetical, or a workflow being explicit - but silence would hide the
	// case where it was a mistake.
	if current != "" && current != requested {
		warn(warnTo, "--branch %s but %s is checked out; versions resolve against the CHECKOUT",
			requested, current)
	}

	return requested, nil
}
