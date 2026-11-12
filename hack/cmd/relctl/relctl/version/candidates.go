// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// State is a repository's release state, without resolving a version.
//
// The dashboards need the train view and nothing else. They used to get it by
// resolving a release they never meant to cut, which stopped working once
// mode=release was retired, and which failed for the wrong reason whenever the
// hypothetical version could not be computed.
type State struct {
	// LatestFinal is the highest final reachable from HEAD, or v0.0.0.
	LatestFinal string
	// Live are cores still in flight.
	Live []string
	// Stale are cores whose candidates were abandoned.
	Stale []string
}

// Discover reports the release state of the repository.
func Discover(repo Repo) (*State, error) {
	r := &resolver{repo: repo}

	if err := r.discover(); err != nil {
		return nil, err
	}

	return &State{LatestFinal: r.latestFinal, Live: r.live, Stale: r.stale}, nil
}

// Candidate is one rc tag.
type Candidate struct {
	// Tag is the rc tag, e.g. v0.3.0-rc.2.
	Tag string
	// Core is the version it is a candidate for, e.g. v0.3.0.
	Core string
	// Number is the rc number, 2 for v0.3.0-rc.2.
	Number int
	// Commit is the commit the tag names.
	Commit string
}

// ParseCandidate splits a canonical rc tag into the version it is a candidate
// for and its number.
//
// Canonical means the shape the resolver mints and discovery counts: rc only,
// no leading zeros, at most nine digits. A legacy alpha or a malformed suffix
// is not a candidate, here or anywhere else.
func ParseCandidate(tag string) (core string, number int, ok bool) {
	if !semverRCTag.MatchString(tag) {
		return "", 0, false
	}

	i := strings.LastIndex(tag, "-rc.")

	// Cannot fail for a string the pattern admitted. Checked rather than
	// discarded for the same reason as parseCore.
	n, err := strconv.Atoi(tag[i+len("-rc."):])
	if err != nil {
		return "", 0, false
	}

	return tag[:i], n, true
}

// IsFinal reports whether tag is a canonical final vX.Y.Z.
func IsFinal(tag string) bool {
	return semverTag.MatchString(tag)
}

// Candidates lists a version's candidates reachable from HEAD, newest first.
func Candidates(repo Repo, core string) ([]Candidate, error) {
	if !IsFinal(core) {
		return nil, fmt.Errorf("not a final version: %s", core)
	}

	r := &resolver{repo: repo}

	tags, err := r.rcTags(core)
	if err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(tags))

	for _, tag := range tags {
		_, number, ok := ParseCandidate(tag)
		if !ok {
			continue
		}

		commit, err := repo.CommitOf(tag)
		if err != nil {
			return nil, fmt.Errorf("could not resolve the commit of %s: %w", tag, err)
		}

		out = append(out, Candidate{Tag: tag, Core: core, Number: number, Commit: commit})
	}

	// Numeric, for the reason maxRC gives: rc.10 is newer than rc.9.
	slices.SortFunc(out, func(a, b Candidate) int { return cmp.Compare(b.Number, a.Number) })

	return out, nil
}

// CandidateAt finds the candidate a final was promoted from: the newest rc of
// the same version that names the same commit. ok is false when there is none,
// which is what a final cut before candidates were mandatory looks like.
func CandidateAt(repo Repo, final string) (Candidate, bool, error) {
	if !IsFinal(final) {
		return Candidate{}, false, fmt.Errorf("not a final version: %s", final)
	}

	commit, err := repo.CommitOf(final)
	if err != nil {
		return Candidate{}, false, fmt.Errorf("could not resolve the commit of %s: %w", final, err)
	}

	candidates, err := Candidates(repo, final)
	if err != nil {
		return Candidate{}, false, err
	}

	for _, candidate := range candidates {
		if candidate.Commit == commit {
			return candidate, true, nil
		}
	}

	return Candidate{}, false, nil
}
