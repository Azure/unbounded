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
	Drafts          []draftInfo  `json:"drafts,omitempty"`
	ReleaseBranches []string     `json:"releaseBranches,omitempty"`
	InFlight        []runSummary `json:"inFlight,omitempty"`
}

// draftInfo is one release that built and never shipped.
type draftInfo struct {
	Tag string `json:"tag"`
	// Committed is the date of the commit the draft points at.
	//
	// GitHub reports this as the release's created_at, and it matches the
	// tagged commit to the second. It is NOT when the draft was made: GitHub
	// does not expose that, and published_at is null until publication, so this
	// is the only date a draft has. Displayed under a COMMITTED heading for
	// that reason, and worth leaving alone.
	//
	// A pointer so an absent date is absent from the JSON rather than
	// serializing as 0001-01-01, which reads as a real answer.
	Committed *time.Time `json:"committed,omitempty"`
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
	// Actor is the login GitHub credits the run to, carried unchanged.
	//
	// Reported alongside By rather than instead of it because it is what the
	// Actions UI shows, and a consumer reconciling the two needs both. On a tag
	// push it is the deploy key's owner and names nobody who did anything.
	Actor string `json:"actor,omitempty"`
	// By is who is actually responsible, when that could be established.
	By *gh.Attribution `json:"by,omitempty"`
}

// who renders an attribution for a column of a table.
//
// Unknown prints "?" and never the raw actor. On a tag push the actor is the
// deploy key's owner, so printing it under a heading that reads as "who did
// this" would state the one thing that is reliably false.
func (r runSummary) who() string {
	if r.By == nil || !r.By.Known() {
		return "?"
	}

	return r.By.By
}

func statusCommand(opts *Options) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show release state: trains, drafts, branches and what is in flight",
		Long: `Show release state in one place.

Release state is spread across three workflows, the tag graph and the release
list. This is all of it in one place.

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

	// Highest version first, NOT newest first. GitHub returns drafts in neither
	// order, and left alone the list interleaves rc.9 between rc.13 and rc.8.
	//
	// Version order keeps a series together, which is how drafts get dealt
	// with: a whole abandoned train goes at once. Date order would interleave
	// the series, and the COMMITTED column makes the difference visible, so
	// expect a lower version with a later date to sit below a higher one.
	//
	// Sorted here rather than at render time so the JSON is ordered too.
	slices.SortFunc(drafts, func(a, b gh.Release) int {
		if cmp := semver.Compare(b.Tag, a.Tag); cmp != 0 {
			return cmp
		}

		// semver.Compare reports 0 for two tags it cannot parse, so anything
		// unparseable would otherwise sit wherever the API left it. Order those
		// by tag so the list is the same on every run.
		return strings.Compare(a.Tag, b.Tag)
	})

	for _, draft := range drafts {
		info := draftInfo{Tag: draft.Tag}

		// Absent rather than zero. A draft whose date the API did not give us
		// is a gap in what we know, which is not the same fact as a date.
		if !draft.CreatedAt.IsZero() {
			created := draft.CreatedAt
			info.Committed = &created
		}

		result.Drafts = append(result.Drafts, info)
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

	// Fetched once for the whole table rather than per run. Correlation is a
	// pure function over this list, so one request answers "who" for every row.
	prepares, err := client.Prepares(ctx)
	if err != nil {
		return nil, err
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
				attribution := gh.Attribute(run, prepares)

				summaries = append(summaries, runSummary{
					Workflow:  workflow,
					Ref:       run.HeadBranch,
					Event:     run.Event,
					State:     run.State(),
					Succeeded: run.Succeeded(),
					URL:       run.URL,
					Actor:     run.Actor,
					By:        &attribution,
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
	// BY rather than ACTOR: GitHub's actor for a tag push is the deploy key's
	// owner, so a column headed with the API's word for it would be read as
	// naming a person who did something. "?" means we could not tell.
	flight = append(flight, []string{"  WORKFLOW", "REF", "STATE", "BY", "URL"})

	for _, run := range result.InFlight {
		flight = append(flight, []string{"  " + run.Workflow, run.Ref, run.State, run.who(), run.URL})
	}

	return table(out, flight)
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}

	return value
}

// writeDrafts lists the drafts, highest version first, one per line.
//
// A draft is a release that built and never shipped, and the usual cause is a
// soak that failed. Saying so beats leaving a bare list.
//
// The date column is headed COMMITTED because that is what it is: the date of
// the commit the draft points at, not the date the draft was made. GitHub does
// not expose the latter. The heading is the only thing stopping the column
// being read as a drafted-on date, so it stays.
//
// Older drafts collapse to one summary line rather than being dropped: the
// header count is always the true total, so a growing backlog is visible as a
// backlog even when it is not enumerated.
func writeDrafts(out io.Writer, result statusResult, all bool) error {
	if len(result.Drafts) == 0 {
		_, err := fmt.Fprintln(out, "\nDrafts: (none)")

		return err
	}

	recent, older := result.Drafts, []draftInfo(nil)

	if !all {
		recent, older = splitDraftsByAge(result.Drafts, time.Now())
	}

	if _, err := fmt.Fprintf(out,
		"\nDrafts (%d): built but not published, usually a soak that failed.\n",
		len(result.Drafts)); err != nil {
		return err
	}

	rows := make([][]string, 0, len(recent)+2)
	rows = append(rows, []string{"  TAG", "COMMITTED"})

	for _, draft := range recent {
		rows = append(rows, []string{"  " + draft.Tag, draftDate(draft)})
	}

	if len(older) > 0 {
		// Name the oldest and date it. "22 older" alone says there is a pile
		// without saying how long it has been there.
		oldest := older[len(older)-1]

		summary := fmt.Sprintf("  %d older, back to %s", len(older), oldest.Tag)
		if date := draftDate(oldest); date != "" {
			summary += " (" + date + ")"
		}

		// One cell, so tabwriter treats it as a trailing cell and it does not
		// stretch the tag column to its own width.
		rows = append(rows, []string{summary + ". --all to list them."})
	}

	return table(out, rows)
}

// draftDate renders a draft's commit date, or empty when there is none.
//
// Formatted from the API's UTC value rather than converted to local time, so
// the output does not depend on the reader's clock and matches what GitHub
// shows against the same release.
func draftDate(draft draftInfo) string {
	if draft.Committed == nil {
		return ""
	}

	return draft.Committed.UTC().Format("2006-01-02")
}

// splitDraftsByAge divides drafts into those inside the window and those not,
// preserving the newest-first order of both.
//
// A draft with no date is treated as recent. The date comes from the API and
// its absence is our gap, not evidence the draft is old, so the failure shows
// the draft rather than burying it.
func splitDraftsByAge(drafts []draftInfo, now time.Time) (recent, older []draftInfo) {
	cutoff := now.Add(-draftWindow)

	for _, draft := range drafts {
		if draft.Committed == nil || draft.Committed.After(cutoff) {
			recent = append(recent, draft)

			continue
		}

		older = append(older, draft)
	}

	return recent, older
}
