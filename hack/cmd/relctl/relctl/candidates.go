// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// Candidate soak results, as relctl reports them.
const (
	// candidatePassed means the soak recorded a passing status on the commit.
	candidatePassed = "passed"
	// candidateFailed means the soak recorded a failing status on the commit.
	candidateFailed = "failed"
	// candidateNoResult means nothing is recorded: still building or soaking,
	// a build that failed before any soak, or a soak that never ran.
	candidateNoResult = "no result"
)

// candidateInfo is one candidate and what its soak recorded.
type candidateInfo struct {
	Tag    string `json:"tag"`
	Commit string `json:"commit"`
	State  string `json:"state"`
	// At is when the result was recorded, absent when there is none.
	At *time.Time `json:"at,omitempty"`
	// URL links the run that recorded the result.
	URL string `json:"url,omitempty"`
}

// passed reports whether the candidate's soak passed.
func (c candidateInfo) passed() bool { return c.State == candidatePassed }

// candidateResults reads the soak results of a train's candidates, newest
// first, stopping at the first one that passed.
//
// Stopping there is not only economy. Every consumer wants the newest passing
// candidate and what, if anything, is newer than it; what an older candidate
// did is history nobody acts on, and fetching it would cost a request per rc
// on a train the scheduler adds three to a day.
//
// Statuses are per commit, so two candidates of one commit share a result. The
// lookup is cached for that reason rather than for speed.
func candidateResults(
	ctx context.Context,
	client *gh.Client,
	candidates []version.Candidate,
) ([]candidateInfo, error) {
	seen := map[string]*gh.Status{}
	out := make([]candidateInfo, 0, len(candidates))

	for _, candidate := range candidates {
		status, ok := seen[candidate.Commit]
		if !ok {
			var err error

			status, err = client.CandidateStatus(ctx, candidate.Commit)
			if err != nil {
				return nil, err
			}

			seen[candidate.Commit] = status
		}

		info := candidateInfo{Tag: candidate.Tag, Commit: candidate.Commit, State: candidateNoResult}

		if status != nil {
			info.State = candidateFailed
			if status.Passed() {
				info.State = candidatePassed
			}

			info.URL = status.TargetURL

			if !status.UpdatedAt.IsZero() {
				at := status.UpdatedAt
				info.At = &at
			}
		}

		out = append(out, info)

		if info.passed() {
			break
		}
	}

	return out, nil
}

// newestPassing returns the passing candidate candidateResults stopped at.
func newestPassing(results []candidateInfo) (candidateInfo, bool) {
	if len(results) == 0 || !results[len(results)-1].passed() {
		return candidateInfo{}, false
	}

	return results[len(results)-1], true
}

// trainToPromote decides which train a promote is for.
//
// version wins when given. Otherwise the only live train, and with several it
// refuses rather than guessing, for the reason the resolver does: guessing is
// what orphaned v0.1.24.
func trainToPromote(repo version.Repo, requested string) (string, error) {
	if requested != "" {
		norm := "v" + strings.TrimPrefix(requested, "v")
		if !version.IsFinal(norm) {
			return "", fmt.Errorf("version must be a final vX.Y.Z, got: %s", requested)
		}

		return norm, nil
	}

	state, err := version.Discover(repo)
	if err != nil {
		return "", err
	}

	switch len(state.Live) {
	case 0:
		return "", fmt.Errorf("no live train to promote; cut a candidate first with relctl rc")
	case 1:
		return state.Live[0], nil
	default:
		return "", fmt.Errorf("multiple live trains (%s); pass --version to say which one to promote",
			strings.Join(state.Live, " "))
	}
}

// pickCandidate decides which candidate a promote finalizes, and refuses
// unless that candidate passed its soak.
//
// explicit names one; it must belong to the train and must have passed.
// Otherwise the newest candidate that passed, which is routinely not the
// highest: with scheduled candidates the highest is often still building, or
// failed for a reason the one below it did not have.
func pickCandidate(
	ctx context.Context,
	client *gh.Client,
	repo version.Repo,
	requested, explicit string,
) (candidateInfo, []candidateInfo, error) {
	if explicit != "" {
		core, _, ok := version.ParseCandidate(explicit)
		if !ok {
			return candidateInfo{}, nil, fmt.Errorf("candidate must be a vX.Y.Z-rc.N tag, got: %s", explicit)
		}

		if requested != "" && "v"+strings.TrimPrefix(requested, "v") != core {
			return candidateInfo{}, nil, fmt.Errorf("%s is not a candidate for %s", explicit, requested)
		}

		requested = core
	}

	train, err := trainToPromote(repo, requested)
	if err != nil {
		return candidateInfo{}, nil, err
	}

	candidates, err := version.Candidates(repo, train)
	if err != nil {
		return candidateInfo{}, nil, err
	}

	if len(candidates) == 0 {
		return candidateInfo{}, nil, fmt.Errorf("%s has no candidates here; cut one with relctl rc", train)
	}

	if explicit != "" {
		index := slices.IndexFunc(candidates, func(c version.Candidate) bool { return c.Tag == explicit })
		if index < 0 {
			return candidateInfo{}, nil, fmt.Errorf("%s is not a candidate reachable from HEAD for %s", explicit, train)
		}

		results, err := candidateResults(ctx, client, candidates[index:index+1])
		if err != nil {
			return candidateInfo{}, nil, err
		}

		if !results[0].passed() {
			return candidateInfo{}, results, fmt.Errorf(
				"%s has not passed its soak (%s); promote a candidate that has, or use --force", explicit, results[0].State)
		}

		return results[0], results, nil
	}

	results, err := candidateResults(ctx, client, candidates)
	if err != nil {
		return candidateInfo{}, nil, err
	}

	picked, ok := newestPassing(results)
	if !ok {
		return candidateInfo{}, results, fmt.Errorf(
			"no candidate of %s has passed its soak; the newest, %s, has %s",
			train, results[0].Tag, describeState(results[0]))
	}

	return picked, results, nil
}

// describeState renders a result as a phrase, for messages.
func describeState(c candidateInfo) string {
	switch c.State {
	case candidatePassed:
		return "passed"
	case candidateFailed:
		return "failed its soak"
	default:
		return "no recorded result yet"
	}
}

// elapsed renders how long ago something happened, to the hour once it is past
// one, which is the precision a release decision needs.
func elapsed(at, now time.Time) string {
	since := now.Sub(at)
	if since < time.Hour {
		return since.Round(time.Minute).String()
	}

	return since.Round(time.Hour).String()
}
