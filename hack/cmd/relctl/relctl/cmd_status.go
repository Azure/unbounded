// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

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
	// draftCreated dates each draft, for the display window only. Unexported
	// so it stays out of the JSON, which is deliberately never windowed:
	// scripts get every draft whatever the terminal shows.
	draftCreated map[string]time.Time
}

// draftWindow is how far back the text view lists drafts individually.
//
// Drafts accumulate: one per build that never shipped, and they are rarely
// cleaned up. Listing every one crowds out the rest of the dashboard, so older
// ones collapse to a single summary line. The count in the header stays the
// true total, because a backlog that stops being counted is a backlog nobody
// deals with.
const draftWindow = 30 * 24 * time.Hour

// runSummary is one workflow run, reduced to what the read commands print.
//
// Ref and Event are separate because they were one field carrying both: a
// build's head branch and a soak's triggering event under a single JSON key,
// so a consumer saw "ref": "workflow_run". They are different facts.
type runSummary struct {
	Workflow string `json:"workflow"`
	// Ref is the head branch, which for a tag push is the tag itself.
	Ref string `json:"ref,omitempty"`
	// Event is what triggered the run, e.g. push or workflow_dispatch.
	Event string `json:"event,omitempty"`
	State string `json:"state"`
	// Succeeded is the run's verdict, carried rather than re-derived from
	// State. A completed run with no conclusion renders as "completed", which a
	// string comparison against "success" would read as a failure.
	Succeeded bool   `json:"succeeded"`
	URL       string `json:"url"`
}

func statusCommand(opts *Options) *cobra.Command {
	var all bool

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
			return runStatus(cmd.Context(), cmd.OutOrStdout(), opts, all)
		},
	}

	cmd.Flags().BoolVar(&all, "all", false,
		"List every draft, not just those from the last 30 days")

	return cmd
}

func runStatus(ctx context.Context, out io.Writer, opts *Options, all bool) error {
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

	// Newest first. GitHub returns drafts in neither semver nor date order, and
	// left alone the list interleaves rc.9 between rc.13 and rc.8. Sorting here
	// rather than at render time means the JSON is ordered too.
	slices.SortFunc(drafts, func(a, b gh.Release) int {
		if cmp := semver.Compare(b.Tag, a.Tag); cmp != 0 {
			return cmp
		}

		// semver.Compare reports 0 for two tags it cannot parse, so anything
		// unparseable would otherwise sit wherever the API left it. Order those
		// by tag so the list is the same on every run.
		return strings.Compare(a.Tag, b.Tag)
	})

	result.draftCreated = make(map[string]time.Time, len(drafts))

	for _, draft := range drafts {
		result.Drafts = append(result.Drafts, draft.Tag)
		result.draftCreated[draft.Tag] = draft.CreatedAt
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

	return writeStatusText(out, result, all)
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
					Workflow:  workflow,
					Ref:       run.HeadBranch,
					Event:     run.Event,
					State:     run.State(),
					Succeeded: run.Succeeded(),
					URL:       run.URL,
				})
			}
		}
	}

	return summaries, nil
}

func writeStatusText(out io.Writer, result statusResult, all bool) error {
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
	)

	if err := table(out, rows); err != nil {
		return err
	}

	if err := writeDrafts(out, result, all); err != nil {
		return err
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

// writeDrafts lists the drafts, newest first, one per line.
//
// A draft is a release that built and never shipped, and the usual cause is a
// soak that failed. Saying so beats leaving a bare list.
//
// Older drafts collapse to one summary line rather than being dropped: the
// header count is always the true total, so a growing backlog is visible as a
// backlog even when it is not enumerated.
func writeDrafts(out io.Writer, result statusResult, all bool) error {
	if len(result.Drafts) == 0 {
		_, err := fmt.Fprintln(out, "\nDrafts: (none)")

		return err
	}

	recent, older := result.Drafts, []string(nil)

	if !all {
		recent, older = splitDraftsByAge(result, time.Now())
	}

	if _, err := fmt.Fprintf(out,
		"\nDrafts (%d): built but not published, usually a soak that failed.\n",
		len(result.Drafts)); err != nil {
		return err
	}

	for _, tag := range recent {
		if _, err := fmt.Fprintf(out, "  %s\n", tag); err != nil {
			return err
		}
	}

	if len(older) == 0 {
		return nil
	}

	// Name the oldest and date it. "22 older" alone says there is a pile
	// without saying how long it has been there.
	oldest := older[len(older)-1]

	when := ""
	if created, ok := result.draftCreated[oldest]; ok && !created.IsZero() {
		when = " (" + created.Format("Jan 2006") + ")"
	}

	_, err := fmt.Fprintf(out, "  %d older, back to %s%s. --all to list them.\n",
		len(older), oldest, when)

	return err
}

// splitDraftsByAge divides drafts into those inside the window and those not,
// preserving the newest-first order of both.
//
// A draft with no date is treated as recent. The date comes from the API and
// its absence is our gap, not evidence the draft is old, so the failure shows
// the draft rather than burying it.
func splitDraftsByAge(result statusResult, now time.Time) (recent, older []string) {
	cutoff := now.Add(-draftWindow)

	for _, tag := range result.Drafts {
		created, ok := result.draftCreated[tag]
		if !ok || created.IsZero() || created.After(cutoff) {
			recent = append(recent, tag)

			continue
		}

		older = append(older, tag)
	}

	return recent, older
}
