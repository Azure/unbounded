// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// statusResult is the whole picture, in one place.
type statusResult struct {
	LatestRelease   string       `json:"latestRelease,omitempty"`
	LatestFinalTag  string       `json:"latestFinalTag"`
	NextFromMain    string       `json:"nextFromMain"`
	Live            []string     `json:"live,omitempty"`
	Stale           []string     `json:"stale,omitempty"`
	Drafts          []string     `json:"drafts,omitempty"`
	ReleaseBranches []string     `json:"releaseBranches,omitempty"`
	InFlight        []runSummary `json:"inFlight,omitempty"`
}

type runSummary struct {
	Workflow string `json:"workflow"`
	Ref      string `json:"ref"`
	State    string `json:"state"`
	URL      string `json:"url"`
}

func statusCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show release state: trains, drafts, branches and what is in flight",
		Long: `Show release state in one place.

RELEASING.md says of deciding whether main is releasable: "There is no single
dashboard." This is that, for the release itself.

Live and stale trains come from the local clone, so tags need to be current.
Everything else comes from the API and needs a credential.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout(), opts)
		},
	}

	return cmd
}

func runStatus(ctx context.Context, out io.Writer, opts *Options) error {
	if err := opts.validateOutput(); err != nil {
		return err
	}

	if opts.Output == OutputGitHub {
		return fmt.Errorf("status has no github output; use json")
	}

	result := statusResult{}

	// Local first, so the version half still reports when the API is
	// unreachable or uncredentialed.
	resolved, err := version.Resolve(opts.repo(ctx), version.Request{
		Mode: version.ModeRelease,
		Bump: version.BumpMinor,
	})
	if err == nil {
		result.LatestFinalTag = resolved.LatestFinal
		result.NextFromMain = resolved.Tag
		result.Live = resolved.Live
		result.Stale = resolved.Stale
	}

	client, err := opts.client(ctx)
	if err != nil {
		return err
	}

	latest, err := client.LatestRelease(ctx)
	if err != nil {
		return err
	}

	if latest != nil {
		result.LatestRelease = latest.Tag
	}

	drafts, err := client.Drafts(ctx, 0)
	if err != nil {
		return err
	}

	for _, draft := range drafts {
		result.Drafts = append(result.Drafts, draft.Tag)
	}

	result.ReleaseBranches, err = client.ReleaseBranches(ctx)
	if err != nil {
		return err
	}

	result.InFlight, err = inFlight(ctx, client)
	if err != nil {
		return err
	}

	if opts.Output == OutputJSON {
		return writeJSON(out, result)
	}

	return writeStatusText(out, result)
}

// inFlight lists release-related runs that have not finished.
func inFlight(ctx context.Context, client *gh.Client) ([]runSummary, error) {
	var summaries []runSummary

	for _, workflow := range []string{gh.WorkflowPrepare, gh.WorkflowRelease, gh.WorkflowUpgrade, gh.WorkflowBranch} {
		runs, err := client.Runs(ctx, gh.ListRuns{Workflow: workflow, Limit: 10})
		if err != nil {
			return nil, err
		}

		for _, run := range runs {
			if run.Done() {
				continue
			}

			summaries = append(summaries, runSummary{
				Workflow: workflow,
				Ref:      run.HeadBranch,
				State:    run.State(),
				URL:      run.URL,
			})
		}
	}

	return summaries, nil
}

func writeStatusText(out io.Writer, result statusResult) error {
	rows := [][]string{
		{"Latest release:", orNone(result.LatestRelease)},
		{"Latest final tag:", orNone(result.LatestFinalTag)},
		{"Next from main:", orNone(result.NextFromMain)},
		{"Live trains:", joinOrNone(result.Live)},
	}

	if len(result.Stale) > 0 {
		rows = append(rows, []string{"Stale trains:", strings.Join(result.Stale, " ")})
	}

	rows = append(rows,
		[]string{"Release branches:", joinOrNone(result.ReleaseBranches)},
		[]string{"Drafts:", joinOrNone(result.Drafts)},
	)

	if err := table(out, rows); err != nil {
		return err
	}

	// A draft is a release that built and never shipped, and the usual cause is
	// a soak that failed. Saying so beats leaving a bare list.
	if len(result.Drafts) > 0 {
		if _, err := fmt.Fprintf(out,
			"\n%d draft(s): built but not published, usually a soak that failed.\n",
			len(result.Drafts)); err != nil {
			return err
		}
	}

	if len(result.InFlight) == 0 {
		_, err := fmt.Fprintln(out, "\nNothing in flight.")

		return err
	}

	if _, err := fmt.Fprintln(out, "\nIn flight:"); err != nil {
		return err
	}

	flight := make([][]string, 0, len(result.InFlight)+1)
	flight = append(flight, []string{"  WORKFLOW", "REF", "STATE", "URL"})

	for _, run := range result.InFlight {
		flight = append(flight, []string{"  " + run.Workflow, run.Ref, run.State, run.URL})
	}

	return table(out, flight)
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}

	return value
}
