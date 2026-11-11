// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

func candidateCommand(opts *Options) *cobra.Command {
	group := &cobra.Command{
		Use:   "candidate",
		Short: "Work with release candidates and their soak results",
		Long: `Work with release candidates and their soak results.

A candidate's soak result is recorded as the ` + gh.CandidateContext + ` commit
status on its commit, not by publishing it. These are mostly for the release
workflows; 'pick' is also useful by hand, to see what 'promote' would ship.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	group.AddCommand(
		candidatePickCommand(opts),
		candidateRecordCommand(opts),
		candidateDueCommand(opts),
	)

	return group
}

// pickResult is what `relctl candidate pick` answers.
type pickResult struct {
	Candidate string `json:"candidate"`
	Commit    string `json:"commit"`
	// Results are the candidates examined, newest first.
	Results []candidateInfo `json:"results"`
}

func candidatePickCommand(opts *Options) *cobra.Command {
	var (
		requested string
		explicit  string
	)

	cmd := &cobra.Command{
		Use:   "pick",
		Short: "Show which candidate promote would finalize",
		Long: `Show which candidate promote would finalize: the newest of the train that
passed its soak, or --candidate, which must have passed.

Refuses, and exits non-zero, when there is none. release-prepare runs this before
it tags a promote, so a final can only come from a candidate that passed.

Reads the train from the local clone and the results from GitHub, so it needs an
up-to-date checkout and a credential.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}

			ctx := cmd.Context()

			client, err := opts.client(ctx)
			if err != nil {
				return err
			}

			picked, results, err := pickCandidate(ctx, client, opts.repo(ctx), requested, explicit)
			if err != nil {
				// Only in text: in github or json output stdout is a contract,
				// and a refusal must leave it empty.
				if opts.Output == OutputText {
					writeCandidateResults(cmd.OutOrStdout(), results)
				}

				return err
			}

			answer := pickResult{Candidate: picked.Tag, Commit: picked.Commit, Results: results}
			out := cmd.OutOrStdout()

			switch opts.Output {
			case OutputJSON:
				return writeJSON(out, answer)
			case OutputGitHub:
				_, err := fmt.Fprintf(out, "candidate=%s\ncommit=%s\n", answer.Candidate, answer.Commit)

				return err
			default:
				writeCandidateResults(out, results)

				_, err := fmt.Fprintf(out, "Promote would finalize %s (%s).\n", answer.Candidate, short(answer.Commit))

				return err
			}
		},
	}

	cmd.Flags().StringVar(&requested, "version", "", "The train, as its final version (default: the single live train)")
	cmd.Flags().StringVar(&explicit, "candidate", "", "A specific candidate, which must have passed")

	return cmd
}

func candidateRecordCommand(opts *Options) *cobra.Command {
	var (
		state       string
		description string
		targetURL   string
	)

	cmd := &cobra.Command{
		Use:   "record <candidate>",
		Short: "Record a candidate's soak result on its commit",
		Long: `Record a candidate's soak result as the ` + gh.CandidateContext + ` commit status.

For release-upgrade, which calls this when a candidate's soak finishes. By hand
it overrides what the soak recorded, which is what 'relctl promote --force' is
for instead: that path is gated and leaves a record.

Needs a credential with statuses: write, and no clone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tag := args[0]

			if _, _, ok := version.ParseCandidate(tag); !ok {
				return fmt.Errorf("%s is not a candidate; only vX.Y.Z-rc.N tags record a soak result", tag)
			}

			ctx := cmd.Context()

			client, err := opts.client(ctx)
			if err != nil {
				return err
			}

			commit, err := client.CommitOf(ctx, tag)
			if err != nil {
				return err
			}

			if description == "" {
				description = defaultRecordDescription(tag, state)
			}

			if err := client.SetCandidateStatus(ctx, commit, state, description, targetURL); err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s=%s on %s (%s): %s\n",
				gh.CandidateContext, state, short(commit), tag, description)

			return err
		},
	}

	cmd.Flags().StringVar(&state, "state", "", "success or failure (required)")
	cmd.Flags().StringVar(&description, "description", "", "Caption for the status (default: says what happened)")
	cmd.Flags().StringVar(&targetURL, "target-url", "", "The run that produced the result")

	if err := cmd.MarkFlagRequired("state"); err != nil {
		panic(err)
	}

	return cmd
}

func defaultRecordDescription(tag, state string) string {
	if state == gh.StatusSuccess {
		return tag + " passed its soak"
	}

	return tag + " did not pass its soak"
}

// dueResult is what `relctl candidate due` answers.
type dueResult struct {
	Due    bool   `json:"due"`
	Reason string `json:"reason"`
}

// inFlightStatuses are the run states that mean a build or soak has not
// finished.
//
// Broader than the dashboard's in_progress and queued, because a soak waiting
// on the cluster's concurrency group reports pending, and that is exactly the
// run a new candidate would cancel: GitHub keeps one pending run per group.
var inFlightStatuses = []string{"in_progress", "queued", "waiting", "pending"}

func candidateDueCommand(opts *Options) *cobra.Command {
	var schedule string

	cmd := &cobra.Command{
		Use:   "due",
		Short: "Say whether the schedule should cut a candidate now",
		Long: `Say whether the schedule should cut a candidate now.

For release-prepare's scheduled runs. A candidate is due unless:

  - scheduled candidates are switched off (--schedule off, from RC_SCHEDULE);
  - HEAD already carries a release tag, candidate or final, so nothing has
    changed since the last one; or
  - a release build or soak is still running. A new one would queue behind it
    and could cancel a soak that is waiting for the cluster.

Not due is an answer, not a failure: it exits zero and says why. A cut by hand
with 'relctl rc' does not consult this.

Reads HEAD from the local clone and runs from GitHub, so it needs both.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDue(cmd.Context(), cmd.OutOrStdout(), opts, schedule)
		},
	}

	cmd.Flags().StringVar(&schedule, "schedule", "", "on or off; empty means on")

	return cmd
}

func runDue(ctx context.Context, out io.Writer, opts *Options, schedule string) error {
	if err := opts.validateOutput(); err != nil {
		return err
	}

	enabled, err := scheduleEnabled(schedule)
	if err != nil {
		return err
	}

	answer := dueResult{}

	if enabled {
		repo := opts.repo(ctx)

		head, err := repo.Head()
		if err != nil {
			return err
		}

		tags, err := repo.TagsAt(head)
		if err != nil {
			return err
		}

		client, err := opts.client(ctx)
		if err != nil {
			return err
		}

		var running []gh.Run

		for _, workflow := range []string{gh.WorkflowRelease, gh.WorkflowUpgrade} {
			for _, status := range inFlightStatuses {
				runs, err := client.Runs(ctx, gh.ListRuns{Workflow: workflow, Status: status, Limit: 10})
				if err != nil {
					return err
				}

				running = append(running, runs...)
			}
		}

		answer.Due, answer.Reason = decideDue(head, tags, running)
	} else {
		answer.Reason = "scheduled candidates are switched off (RC_SCHEDULE=off)"
	}

	switch opts.Output {
	case OutputJSON:
		return writeJSON(out, answer)
	case OutputGitHub:
		_, err := fmt.Fprintf(out, "due=%t\nreason=%s\n", answer.Due, oneLine(answer.Reason))

		return err
	default:
		verdict := "not due"
		if answer.Due {
			verdict = "due"
		}

		_, err := fmt.Fprintf(out, "A candidate is %s: %s\n", verdict, answer.Reason)

		return err
	}
}

// scheduleEnabled reads RC_SCHEDULE.
//
// Strict about what it accepts. The variable exists to switch the schedule off,
// so a typo that silently read as "on" would defeat it, and one that silently
// read as "off" would stop candidates with nobody noticing. Either way the run
// should fail where someone will see it.
func scheduleEnabled(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "on", "true":
		return true, nil
	case "off", "false":
		return false, nil
	default:
		return false, fmt.Errorf("RC_SCHEDULE must be on or off, got %q", value)
	}
}

// decideDue is the scheduler's decision, separated from the fetching so it can
// be tested directly.
func decideDue(head string, headTags []string, running []gh.Run) (bool, string) {
	for _, tag := range headTags {
		_, _, candidate := version.ParseCandidate(tag)

		if candidate || version.IsFinal(tag) {
			return false, fmt.Sprintf("HEAD %s is already %s; nothing has changed since", short(head), tag)
		}
	}

	if len(running) > 0 {
		run := running[0]

		return false, fmt.Sprintf("%s is still %s for %s (%s); a new candidate would queue behind it",
			run.Workflow, run.State(), orNone(run.HeadBranch), run.URL)
	}

	return true, fmt.Sprintf("HEAD %s has no candidate yet", short(head))
}

// oneLine keeps a value from breaking out of a key=value line in
// $GITHUB_OUTPUT, where a newline would start a new key.
func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// recordedAgo renders when a candidate's result was recorded.
func recordedAgo(info candidateInfo, now time.Time) string {
	if info.At == nil {
		return ""
	}

	return elapsed(*info.At, now) + " ago"
}
