// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v75/github"
)

// Release is a GitHub release, reduced to what relctl reports on.
type Release struct {
	Tag         string
	Draft       bool
	Prerelease  bool
	URL         string
	PublishedAt time.Time
}

// State renders the release's publication state.
func (r Release) State() string {
	switch {
	case r.Draft:
		return "draft"
	case r.Prerelease:
		return "prerelease"
	default:
		return "published"
	}
}

func toRelease(r *github.RepositoryRelease) Release {
	return Release{
		Tag:         r.GetTagName(),
		Draft:       r.GetDraft(),
		Prerelease:  r.GetPrerelease(),
		URL:         r.GetHTMLURL(),
		PublishedAt: r.GetPublishedAt().Time,
	}
}

// Release fetches one release by tag. A missing release is not an error: it
// reports nil, because "built but not yet drafted" is an ordinary state to be
// in while watching a release through.
func (c *Client) Release(ctx context.Context, tag string) (*Release, error) {
	release, resp, err := c.api.Repositories.GetReleaseByTag(ctx, c.owner, c.repo, tag)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, nil //nolint:nilnil // absence is a state, not a failure
		}

		return nil, fmt.Errorf("get release %s: %w", tag, err)
	}

	out := toRelease(release)

	return &out, nil
}

// LatestRelease fetches whatever /releases/latest resolves to.
//
// This is the release the install command in README.md and every guide points
// at, which is why relctl reports it separately from "the highest version": the
// two can disagree, and when they do that is the thing worth knowing.
func (c *Client) LatestRelease(ctx context.Context) (*Release, error) {
	release, resp, err := c.api.Repositories.GetLatestRelease(ctx, c.owner, c.repo)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, nil //nolint:nilnil // a repository with no releases
		}

		return nil, fmt.Errorf("get latest release: %w", err)
	}

	out := toRelease(release)

	return &out, nil
}

// Drafts lists unpublished releases, newest first.
//
// Worth surfacing because a draft is a release that built but never shipped,
// and the usual cause is a soak that failed. They accumulate silently
// otherwise.
func (c *Client) Drafts(ctx context.Context, limit int) ([]Release, error) {
	if limit == 0 {
		limit = 30
	}

	releases, _, err := c.api.Repositories.ListReleases(ctx, c.owner, c.repo,
		&github.ListOptions{PerPage: limit})
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}

	var drafts []Release

	for _, release := range releases {
		if release.GetDraft() {
			drafts = append(drafts, toRelease(release))
		}
	}

	return drafts, nil
}

// ReleaseBranches lists the release-X.Y branches that exist.
func (c *Client) ReleaseBranches(ctx context.Context) ([]string, error) {
	var names []string

	opts := &github.BranchListOptions{ListOptions: github.ListOptions{PerPage: 100}}

	for {
		branches, resp, err := c.api.Repositories.ListBranches(ctx, c.owner, c.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list branches: %w", err)
		}

		for _, branch := range branches {
			if strings.HasPrefix(branch.GetName(), "release-") {
				names = append(names, branch.GetName())
			}
		}

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return names, nil
}
