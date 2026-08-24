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
			return runNext(cmd.OutOrStdout(), opts, version.Request{
				Mode:                  version.Mode(mode),
				Series:                "",
				Pre:                   pre,
				Version:               explicit,
				AllowConcurrentTrains: concurrent,
			}, branch, major, cmd)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "main", "Branch to cut from: main, or release-X.Y")
	cmd.Flags().StringVar(&mode, "mode", string(version.ModeRelease), "release, prerelease or promote")
	cmd.Flags().BoolVar(&major, "major", false, "main only: cut a major instead of a minor")
	cmd.Flags().StringVar(&pre, "pre", "", "Explicit prerelease suffix, e.g. rc.3")
	cmd.Flags().StringVar(&explicit, "version", "", "promote only: the final version to cut")
	cmd.Flags().BoolVar(&concurrent, "allow-concurrent-trains", false,
		"Permit starting a second live prerelease train")

	return cmd
}

func runNext(out io.Writer, opts *Options, req version.Request, branch string, major bool, cmd *cobra.Command) error {
	if err := opts.validateOutput(); err != nil {
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

	result, err := version.Resolve(opts.repo(cmd.Context()), req)
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
		// Byte-identical to what the shell emitted, so the workflow steps that
		// consume it do not change.
		_, err := fmt.Fprintf(out, "tag=%s\nbase=%s\n", answer.Tag, answer.Base)

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
		if _, err := fmt.Fprintf(out, "\nwarning: %s\n", warning); err != nil {
			return err
		}
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
