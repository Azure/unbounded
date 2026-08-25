// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/go-github/v75/github"
)

// Dispatch starts a workflow_dispatch run.
//
// GitHub's dispatch endpoint returns 204 with no body: it does not say which
// run it created, or whether the inputs were understood. A rejected input is a
// 422 here, but a workflow that ignores an input it does not declare is a
// silent success, which is why the callers print exactly what they are sending.
func (c *Client) Dispatch(ctx context.Context, workflow, ref string, inputs map[string]any) error {
	resp, err := c.api.Actions.CreateWorkflowDispatchEventByFileName(ctx, c.owner, c.repo, workflow,
		github.CreateWorkflowDispatchEventRequest{Ref: ref, Inputs: inputs})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf(
				"dispatch %s: not found. The workflow must exist on %s AND the ref must exist: %w",
				workflow, ref, err)
		}

		return fmt.Errorf("dispatch %s on %s: %w", workflow, ref, err)
	}

	return nil
}

// FindDispatched waits for the run a dispatch created.
//
// The dispatch endpoint returns no run ID, so the run has to be found
// afterwards. Matching is on workflow, event and creation time: anything
// created after the dispatch was sent, on that workflow, by workflow_dispatch.
//
// Not exact, and deliberately not pretending to be. Two dispatches of one
// workflow within the polling window are indistinguishable, so this reports the
// newest and the caller shows its URL rather than asserting ownership.
func (c *Client) FindDispatched(
	ctx context.Context,
	workflow string,
	after time.Time,
	timeout time.Duration,
) (*Run, error) {
	deadline := time.Now().Add(timeout)

	for {
		runs, err := c.Runs(ctx, ListRuns{
			Workflow: workflow,
			Event:    "workflow_dispatch",
			Limit:    20,
		})
		if err != nil {
			return nil, err
		}

		var candidates []Run

		for _, run := range runs {
			// A second of slack: the dispatch timestamp is taken locally and
			// created_at is GitHub's, so a run genuinely caused by this
			// dispatch can carry a timestamp just before it.
			if run.CreatedAt.After(after.Add(-time.Second)) {
				candidates = append(candidates, run)
			}
		}

		if len(candidates) > 0 {
			slices.SortFunc(candidates, func(a, b Run) int {
				return b.CreatedAt.Compare(a.CreatedAt)
			})

			return &candidates[0], nil
		}

		if time.Now().After(deadline) {
			// Not an error: the dispatch succeeded, and a run that has not
			// appeared yet is a queue delay rather than a failure.
			return nil, nil //nolint:nilnil // absence is a state; the caller says so
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
