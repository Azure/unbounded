// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// nightlyStaleAfter is how old a green nightly may be and still count.
//
// RELEASING.md asks for the nightly to be "green, and green recently" without
// saying how recent. Two days allows for a weekend gap in scheduling while
// still refusing a week-old result, which tells you about a tree nobody has
// released since.
const nightlyStaleAfter = 48 * time.Hour

// preflightResult answers whether a branch is releasable.
type preflightResult struct {
	Branch     string   `json:"branch"`
	Releasable bool     `json:"releasable"`
	NightlyOK  bool     `json:"nightlyOk"`
	NightlyAge string   `json:"nightlyAge,omitempty"`
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

RELEASING.md section 1 lists what to look at and says "There is no single
dashboard." This is it: the nightly, CI on the branch, and any train already in
flight.

A red nightly is a release blocker until it is understood. It deploys the same
component images the release will, to the same shape of cluster, so a nightly
failure is a release failure you have not had yet.`,
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

	// The nightly only ever runs on the default branch, so it says nothing
	// about a release branch. Saying so is better than reporting a green tick
	// that refers to somewhere else.
	if branch == "main" {
		nightlies, err := client.Runs(ctx, gh.ListRuns{Workflow: gh.WorkflowNightly, Limit: 5})
		if err != nil {
			return err
		}

		result.NightlyOK, result.NightlyAge = judgeNightly(nightlies, &result)
	} else {
		result.NightlyOK = true
		result.Notes = append(result.Notes,
			"nightly only runs on the default branch, so it says nothing about "+branch)
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

	// A train already in flight is not a blocker, but cutting past it strands
	// it, so it belongs in the report. A failure to resolve is reported rather
	// than swallowed: silently omitting the note would read as "no train
	// exists", which is the answer that gets one stranded.
	resolved, err := version.Resolve(opts.repo(ctx), version.Request{
		Mode: version.ModeRelease,
		Bump: version.BumpMinor,
	})

	switch {
	case err != nil:
		result.Notes = append(result.Notes,
			"could not resolve local state, so live trains are unknown: "+err.Error())
	case len(resolved.Live) > 0:
		result.LiveTrains = resolved.Live
		result.Notes = append(result.Notes,
			"a live train exists; mode=release cuts past it and strands it, mode=promote finishes it")
	}

	result.Releasable = len(result.Blockers) == 0

	if opts.Output == OutputJSON {
		return writeJSON(out, result)
	}

	return writePreflightText(out, result)
}

// judgeNightly decides whether the nightly signal is good enough.
func judgeNightly(runs []gh.Run, result *preflightResult) (bool, string) {
	for _, run := range runs {
		if !run.Done() {
			continue
		}

		age := time.Since(run.CreatedAt).Round(time.Hour).String()

		if !run.Succeeded() {
			result.Blockers = append(result.Blockers,
				"the most recent completed nightly is "+run.State()+
					"; a red nightly is a release blocker until it is understood")

			return false, age
		}

		if time.Since(run.CreatedAt) > nightlyStaleAfter {
			result.Blockers = append(result.Blockers,
				"the last green nightly is "+age+" old; it should be green recently")

			return false, age
		}

		return true, age
	}

	result.Blockers = append(result.Blockers, "no completed nightly run found")

	return false, ""
}

func writePreflightText(out io.Writer, result preflightResult) error {
	verdict := "RELEASABLE"
	if !result.Releasable {
		verdict = "NOT RELEASABLE"
	}

	rows := [][]string{
		{"Branch:", result.Branch},
		{"Verdict:", verdict},
		{"Nightly:", tick(result.NightlyOK) + nightlySuffix(result.NightlyAge)},
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

func nightlySuffix(age string) string {
	if age == "" {
		return ""
	}

	return " (" + age + " ago)"
}
