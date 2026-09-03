// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/go-github/v75/github"
)

// Workflow file names relctl reads and dispatches.
const (
	WorkflowPrepare = "release-prepare.yaml"
	WorkflowRelease = "release.yaml"
	WorkflowUpgrade = "release-upgrade.yaml"
	WorkflowBranch  = "create-release-branch.yaml"
	WorkflowNightly = "nightly.yaml"
	WorkflowCI      = "ci.yaml"
)

// Run is one workflow run, reduced to what relctl reports on.
type Run struct {
	ID         int64
	Workflow   string
	Status     string
	Conclusion string
	Event      string
	HeadBranch string
	HeadSHA    string
	// CreatedAt is when the run was CREATED, which for a push is when the push
	// happened. It does not move when a run is re-run, and that is what makes
	// it the right clock for Attribute. See RunStartedAt.
	CreatedAt time.Time
	// RunStartedAt is when the LATEST ATTEMPT began, so it moves on a re-run
	// and CreatedAt does not. Carried for display, never for correlation.
	RunStartedAt time.Time
	// UpdatedAt is when the run last changed, which for a finished run is when
	// it finished. Used as the closing edge of a run's window.
	UpdatedAt time.Time
	// Actor is who GitHub credits the run to.
	//
	// NOT who caused it, for a tag push. release-prepare pushes tags over SSH
	// with a deploy key, and GitHub attributes a deploy-key push to whoever
	// registered the key, so every release.yaml run names the same person
	// regardless of who cut the release. Attribute exists because of this.
	Actor string
	// TriggeringActor is who started the latest attempt, so it names whoever
	// pressed re-run rather than whoever caused the run to exist.
	TriggeringActor string
	URL             string
}

// Done reports whether the run has finished, whatever the outcome.
func (r Run) Done() bool { return r.Status == "completed" }

// Succeeded reports whether the run finished cleanly.
func (r Run) Succeeded() bool { return r.Conclusion == "success" }

// Failed reports whether the run finished and GitHub said it did not succeed.
//
// Deliberately not !Succeeded(). A completed run with no conclusion at all
// renders as "completed" (see State) and is an absence of evidence, not
// evidence of failure. Anywhere a failure EXCLUDES something, treating that
// absence as a failure is the unsafe direction: it discards a run that may
// have been fine and falls through to whatever the code does when it finds
// nothing.
func (r Run) Failed() bool { return r.Done() && r.Conclusion != "" && !r.Succeeded() }

// State renders the run's outcome, or its status while it is still going.
func (r Run) State() string {
	if !r.Done() {
		if r.Status == "" {
			return "unknown"
		}

		return r.Status
	}

	if r.Conclusion == "" {
		return "completed"
	}

	return r.Conclusion
}

// ListRuns describes which runs to fetch.
type ListRuns struct {
	// Workflow is the workflow file name.
	Workflow string
	// Branch filters by head branch. For a tag push this is the TAG, which is
	// what makes a build run identifiable by the tag it built.
	Branch string
	// Event filters by triggering event.
	Event string
	// HeadSHA filters server-side by commit.
	HeadSHA string
	// Status filters server-side, e.g. "in_progress" or "queued".
	//
	// Server-side rather than a Go filter over one page: a workflow with ten
	// completed runs newer than a still-running one would otherwise drop it
	// entirely, which is exactly what concurrent tag pushes produce.
	Status string
	// Limit is the page size, capped by GitHub at 100.
	//
	// Named for what it is. It is not a total: nothing here paginates, because
	// every caller wants a recent window rather than the whole history.
	Limit int
}

// Runs lists workflow runs, newest first.
func (c *Client) Runs(ctx context.Context, q ListRuns) ([]Run, error) {
	limit := q.Limit
	if limit == 0 {
		limit = 20
	}

	// GitHub silently truncates anything larger, so asking for more than a page
	// would quietly return fewer results than requested.
	if limit > 100 {
		limit = 100
	}

	opts := &github.ListWorkflowRunsOptions{
		Branch:      q.Branch,
		Event:       q.Event,
		HeadSHA:     q.HeadSHA,
		Status:      q.Status,
		ListOptions: github.ListOptions{PerPage: limit},
	}

	runs, _, err := c.api.Actions.ListWorkflowRunsByFileName(ctx, c.owner, c.repo, q.Workflow, opts)
	if err != nil {
		return nil, fmt.Errorf("list runs for %s: %w", q.Workflow, err)
	}

	out := make([]Run, 0, len(runs.WorkflowRuns))

	for _, run := range runs.WorkflowRuns {
		out = append(out, Run{
			ID:              run.GetID(),
			Workflow:        q.Workflow,
			Status:          run.GetStatus(),
			Conclusion:      run.GetConclusion(),
			Event:           run.GetEvent(),
			HeadBranch:      run.GetHeadBranch(),
			HeadSHA:         run.GetHeadSHA(),
			CreatedAt:       run.GetCreatedAt().Time,
			RunStartedAt:    run.GetRunStartedAt().Time,
			UpdatedAt:       run.GetUpdatedAt().Time,
			Actor:           run.GetActor().GetLogin(),
			TriggeringActor: run.GetTriggeringActor().GetLogin(),
			URL:             run.GetHTMLURL(),
		})
	}

	slices.SortFunc(out, func(a, b Run) int { return b.CreatedAt.Compare(a.CreatedAt) })

	return out, nil
}

// ReleaseProgress is where a release has got to, across three workflows.
type ReleaseProgress struct {
	// Build is the release.yaml run that built the tag.
	Build *Run
	// Soaks are the release-upgrade.yaml runs for that build, oldest first.
	// More than one means the soak was retried.
	Soaks []Run
}

// Progress correlates the runs belonging to one tag.
//
// The build run is exact: a tag push sets head_branch to the TAG, so
// release.yaml runs are identifiable by name.
//
// The soak is not. release-upgrade fires on workflow_run, so its head_branch is
// the default branch and the API exposes no link back to the run that triggered
// it. Matching on head_sha alone is ambiguous, because a promoted final and its
// last candidate share a commit: v0.3.0 and v0.3.0-rc.1 are both 3c9621da, as
// are v0.2.4 and v0.2.4-rc.1. Both tags therefore produce build runs with the
// same head_sha, and so do their soaks.
//
// So the sha narrows server-side and the TIME WINDOW disambiguates: a soak
// belongs to this build if it started after it and before the next build of the
// same commit. Manual workflow_dispatch retries land in the same window, which
// is what you want, since they are attempts at the same soak.
func (c *Client) Progress(ctx context.Context, tag string) (*ReleaseProgress, error) {
	builds, err := c.Runs(ctx, ListRuns{Workflow: WorkflowRelease, Branch: tag, Limit: 10})
	if err != nil {
		return nil, err
	}

	if len(builds) == 0 {
		return &ReleaseProgress{}, nil
	}

	// Newest first, so the most recent attempt at this tag.
	build := builds[0]

	siblings, err := c.Runs(ctx, ListRuns{
		Workflow: WorkflowRelease,
		HeadSHA:  build.HeadSHA,
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}

	// The window closes at the next build of the same commit, which is the
	// other tag sharing it. Open-ended when this is the newest.
	until := time.Time{}

	// created_at has second resolution, so two builds of the same commit within
	// one second would both fail an After test and leave the window open,
	// merging both builds' soaks into one report. Run IDs are monotonic, so
	// they break the tie.
	for _, sibling := range siblings {
		newer := sibling.CreatedAt.After(build.CreatedAt) ||
			(sibling.CreatedAt.Equal(build.CreatedAt) && sibling.ID > build.ID)

		if newer && (until.IsZero() || sibling.CreatedAt.Before(until)) {
			until = sibling.CreatedAt
		}
	}

	soaks, err := c.Runs(ctx, ListRuns{
		Workflow: WorkflowUpgrade,
		HeadSHA:  build.HeadSHA,
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}

	progress := &ReleaseProgress{Build: &build}

	for _, soak := range soaks {
		if soak.CreatedAt.Before(build.CreatedAt) {
			continue
		}

		if !until.IsZero() && !soak.CreatedAt.Before(until) {
			continue
		}

		progress.Soaks = append(progress.Soaks, soak)
	}

	slices.SortFunc(progress.Soaks, func(a, b Run) int { return a.CreatedAt.Compare(b.CreatedAt) })

	return progress, nil
}
