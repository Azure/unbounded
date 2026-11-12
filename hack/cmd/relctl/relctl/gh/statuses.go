// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gh

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v75/github"
)

// CandidateContext is the commit status a candidate's soak result is recorded
// under.
//
// A candidate is never published, so its release cannot say whether it passed:
// drafts are invisible to a read-only token, and a published prerelease per
// passing candidate is the release-page clutter this design avoids. A commit
// status is readable by any token that can read the repository and shows on
// the commit in GitHub.
//
// It is per COMMIT, which is what promotion consumes: promote tags a commit and
// the final is rebuilt from it. Two candidates of one commit share it, and the
// most recent soak of that tree is the one that counts.
const CandidateContext = "unbounded/candidate"

// Commit status states relctl records and reads.
const (
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// Status is a commit status, reduced to what relctl reports on.
type Status struct {
	State       string
	Description string
	TargetURL   string
	// UpdatedAt is when the status was last set, which for a candidate is when
	// its soak finished.
	UpdatedAt time.Time
}

// Passed reports whether the status records a passing candidate.
func (s *Status) Passed() bool { return s != nil && s.State == StatusSuccess }

// CandidateStatus reads the candidate status on a commit. A commit that has
// none reports nil, which is the ordinary state of every commit no candidate
// was cut from.
func (c *Client) CandidateStatus(ctx context.Context, sha string) (*Status, error) {
	// The combined status carries only the latest status per context, which is
	// exactly the "most recent soak of this tree" rule.
	combined, _, err := c.api.Repositories.GetCombinedStatus(ctx, c.owner, c.repo, sha,
		&github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("read statuses for %s: %w", short(sha), err)
	}

	for _, status := range combined.Statuses {
		if status.GetContext() != CandidateContext {
			continue
		}

		return &Status{
			State:       status.GetState(),
			Description: status.GetDescription(),
			TargetURL:   status.GetTargetURL(),
			UpdatedAt:   status.GetUpdatedAt().Time,
		}, nil
	}

	return nil, nil //nolint:nilnil // absence is an answer: no candidate soaked this commit
}

// SetCandidateStatus records a candidate's soak result on its commit.
func (c *Client) SetCandidateStatus(ctx context.Context, sha, state, description, targetURL string) error {
	switch state {
	case StatusSuccess, StatusFailure:
	default:
		return fmt.Errorf("candidate state must be %s or %s, got %q", StatusSuccess, StatusFailure, state)
	}

	// GitHub caps the description at 140 characters and rejects the whole
	// status past that, which would lose the result over its caption.
	const maxDescription = 140
	if runes := []rune(description); len(runes) > maxDescription {
		description = string(runes[:maxDescription-3]) + "..."
	}

	status := &github.RepoStatus{
		State:       github.Ptr(state),
		Context:     github.Ptr(CandidateContext),
		Description: github.Ptr(description),
	}

	if targetURL != "" {
		status.TargetURL = github.Ptr(targetURL)
	}

	if _, _, err := c.api.Repositories.CreateStatus(ctx, c.owner, c.repo, sha, status); err != nil {
		return fmt.Errorf("record %s on %s: %w", CandidateContext, short(sha), err)
	}

	return nil
}

// CommitOf resolves a ref, such as a tag, to its commit.
//
// The API rather than git, for the callers that run with no clone at all: the
// cleanup jobs check out only relctl's own source.
func (c *Client) CommitOf(ctx context.Context, ref string) (string, error) {
	sha, resp, err := c.api.Repositories.GetCommitSHA1(ctx, c.owner, c.repo, ref, "")
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("%s does not exist in %s", ref, c.Repo())
		}

		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}

	return sha, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}

	return sha
}
