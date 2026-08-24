// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"fmt"
	"sort"
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
	CreatedAt  time.Time
	URL        string
}

// Done reports whether the run has finished, whatever the outcome.
func (r Run) Done() bool { return r.Status == "completed" }

// Succeeded reports whether the run finished cleanly.
func (r Run) Succeeded() bool { return r.Conclusion == "success" }

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
	// Limit caps how many are returned. Zero means 20.
	Limit int
}

// Runs lists workflow runs, newest first.
func (c *Client) Runs(ctx context.Context, q ListRuns) ([]Run, error) {
	limit := q.Limit
	if limit == 0 {
		limit = 20
	}

	opts := &github.ListWorkflowRunsOptions{
		Branch:      q.Branch,
		Event:       q.Event,
		HeadSHA:     q.HeadSHA,
		ListOptions: github.ListOptions{PerPage: limit},
	}

	runs, _, err := c.api.Actions.ListWorkflowRunsByFileName(ctx, c.owner, c.repo, q.Workflow, opts)
	if err != nil {
		return nil, fmt.Errorf("list runs for %s: %w", q.Workflow, err)
	}

	out := make([]Run, 0, len(runs.WorkflowRuns))

	for _, run := range runs.WorkflowRuns {
		out = append(out, Run{
			ID:         run.GetID(),
			Workflow:   q.Workflow,
			Status:     run.GetStatus(),
			Conclusion: run.GetConclusion(),
			Event:      run.GetEvent(),
			HeadBranch: run.GetHeadBranch(),
			HeadSHA:    run.GetHeadSHA(),
			CreatedAt:  run.GetCreatedAt().Time,
			URL:        run.GetHTMLURL(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })

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

	for _, sibling := range siblings {
		if sibling.CreatedAt.After(build.CreatedAt) {
			if until.IsZero() || sibling.CreatedAt.Before(until) {
				until = sibling.CreatedAt
			}
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

	sort.Slice(progress.Soaks, func(i, j int) bool {
		return progress.Soaks[i].CreatedAt.Before(progress.Soaks[j].CreatedAt)
	})

	return progress, nil
}
