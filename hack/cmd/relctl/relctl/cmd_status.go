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
	LatestRelease  string `json:"latestRelease,omitempty"`
	LatestFinalTag string `json:"latestFinalTag,omitempty"`
	// NextFromLocal is the next version for whatever is CHECKED OUT, which is
	// not necessarily main. Named for what it is rather than what it usually
	// is, because on a release branch checkout the two differ.
	NextFromLocal string `json:"nextFromLocal,omitempty"`
	// LocalError is set when version resolution failed, so an absent train list
	// cannot be misread as an empty one.
	LocalError      string       `json:"localError,omitempty"`
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

	// Resolved locally, and a failure here is reported rather than rendered as
	// a fact. "I could not tell" and "there are none" are different answers,
	// and for the command whose whole purpose is being the dashboard, guessing
	// the second is the worst thing it could do. Ordinary causes are a stale
	// checkout, a wrong --repo-path, or running outside a clone.
	resolved, err := version.Resolve(opts.repo(ctx), version.Request{
		Mode: version.ModeRelease,
		Bump: version.BumpMinor,
	})
	if err != nil {
		result.LocalError = err.Error()
	} else {
		result.LatestFinalTag = resolved.LatestFinal
		result.NextFromLocal = resolved.Tag
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

	drafts, err := client.Drafts(ctx)
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

	workflows := []string{
		gh.WorkflowPrepare,
		gh.WorkflowRelease,
		gh.WorkflowUpgrade,
		gh.WorkflowBranch,
	}

	for _, workflow := range workflows {
		// Filtered server-side rather than by fetching a page and discarding
		// the finished ones: ten completed runs newer than a still-running one
		// would otherwise hide it entirely, which is what concurrent tag
		// pushes produce.
		for _, status := range []string{"in_progress", "queued"} {
			runs, err := client.Runs(ctx, gh.ListRuns{
				Workflow: workflow,
				Status:   status,
				Limit:    20,
			})
			if err != nil {
				return nil, err
			}

			for _, run := range runs {
				summaries = append(summaries, runSummary{
					Workflow: workflow,
					Ref:      run.HeadBranch,
					State:    run.State(),
					URL:      run.URL,
				})
			}
		}
	}

	return summaries, nil
}

func writeStatusText(out io.Writer, result statusResult) error {
	rows := [][]string{{"Latest release:", orNone(result.LatestRelease)}}

	if result.LocalError != "" {
		// Every local answer is unknown, and saying so once beats printing
		// "(none)" four times for facts nobody established.
		rows = append(rows, []string{"Local state:", "UNKNOWN (" + result.LocalError + ")"})
	} else {
		rows = append(rows,
			[]string{"Latest final tag:", orNone(result.LatestFinalTag)},
			[]string{"Next from checkout:", orNone(result.NextFromLocal)},
			[]string{"Live trains:", joinOrNone(result.Live)},
		)

		if len(result.Stale) > 0 {
			rows = append(rows, []string{"Stale trains:", strings.Join(result.Stale, " ")})
		}
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
