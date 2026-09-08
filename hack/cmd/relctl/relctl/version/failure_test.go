// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"errors"
	"strings"
	"testing"
)

// Tests for what happens when git itself fails.
//
// The direction of a failure matters more than its handling. In the resolver a
// swallowed error fails toward REFUSING, because the ancestry check catches it.
// In Classify it would fail toward SHIPPING, because an empty tag set reads as
// "nothing outranks this" and Latest is what releases/latest/download resolves
// to. hack/cmd/semver, which Classify absorbs, refused that case deliberately;
// the port briefly did not.

// brokenRepo fails whichever query it is told to.
type brokenRepo struct {
	*fakeRepo

	failReachable  bool
	failAll        bool
	failTagExists  bool
	failIsAncestor bool
}

var errGitBroke = errors.New("git exploded")

func (b *brokenRepo) ReachableTags(pattern string) ([]string, error) {
	if b.failReachable {
		return nil, errGitBroke
	}

	return b.fakeRepo.ReachableTags(pattern)
}

func (b *brokenRepo) AllTags(pattern string) ([]string, error) {
	if b.failAll {
		return nil, errGitBroke
	}

	return b.fakeRepo.AllTags(pattern)
}

func (b *brokenRepo) TagExists(tag string) (bool, error) {
	if b.failTagExists {
		return false, errGitBroke
	}

	return b.fakeRepo.TagExists(tag)
}

func (b *brokenRepo) IsAncestor(commit string) (bool, error) {
	if b.failIsAncestor {
		return false, errGitBroke
	}

	return b.fakeRepo.IsAncestor(commit)
}

// TestClassifyRefusesWhenTheTagQueryFails is the guard on the direction of
// failure. Marking a release Latest because git broke would repoint the install
// command in README.md and every guide.
func TestClassifyRefusesWhenTheTagQueryFails(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		repo *brokenRepo
	}{
		{
			name: "reachable query fails",
			repo: &brokenRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"}), failReachable: true},
		},
		{
			name: "series query fails",
			repo: &brokenRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"}), failAll: true},
		},
		{
			// The reachability check is what decides provenance, and a
			// swallowed failure there reads as "cut from a release branch".
			name: "ancestry query fails",
			repo: &brokenRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"}), failIsAncestor: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := Classify(tc.repo, "v0.4.0")
			if err == nil {
				t.Fatalf("Classify: want an error, got Latest=%v", result.Latest)
			}

			if result != nil && result.Latest {
				t.Error("Latest = true on a failed query; must never ship on a broken command")
			}
		})
	}
}

// TestClassifyRefusesAnEmptyTagSet covers the case the absorbed tool kept apart
// from "no finals exist". The tag being classified is itself a tag here, so an
// empty result cannot be an honest answer.
func TestClassifyRefusesAnEmptyTagSet(t *testing.T) {
	t.Parallel()

	// A repo whose queries succeed but return nothing, which is what a subtly
	// broken invocation looks like from the caller's side.
	empty := &emptyQueryRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"})}

	_, err := Classify(empty, "v0.4.0")
	if err == nil {
		t.Fatal("Classify: want an error for an empty tag set")
	}

	if !strings.Contains(err.Error(), "did not run properly") {
		t.Errorf("error = %q, want it to name the broken query", err)
	}
}

// TestClassifyStillAnswersWhenNothingHasShipped keeps the two states apart in
// the other direction: tags exist, none is final, and that IS an answer.
func TestClassifyStillAnswersWhenNothingHasShipped(t *testing.T) {
	t.Parallel()

	result, err := Classify(newFakeRepo([]string{"v0.1.0-rc.1"}), "v0.1.0-rc.1")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	// A prerelease can never be Latest, so this exercises the path without
	// depending on the prerelease short-circuit above it.
	if result.Latest {
		t.Error("Latest = true for a prerelease")
	}
}

// emptyQueryRepo succeeds at every query and returns nothing.
type emptyQueryRepo struct {
	*fakeRepo
}

func (e *emptyQueryRepo) ReachableTags(string) ([]string, error) { return nil, nil }
func (e *emptyQueryRepo) AllTags(string) ([]string, error)       { return nil, nil }

// TestResolvePropagatesTagQueryFailure is the resolver half. It already failed
// toward refusing, but only because Head() happened to be called first; now the
// query itself reports.
func TestResolvePropagatesTagQueryFailure(t *testing.T) {
	t.Parallel()

	repo := &brokenRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"}), failReachable: true}

	_, err := Resolve(repo, Request{Mode: ModeRelease, Bump: BumpMinor})
	if err == nil {
		t.Fatal("Resolve: want an error when tag discovery fails")
	}

	if !errors.Is(err, errGitBroke) {
		t.Errorf("error = %v, want it to wrap the git failure", err)
	}
}

// TestResolvePropagatesTagExistsFailure covers the guard that stops a duplicate
// tag being minted. Reporting a git failure as "the tag does not exist" would
// let validate fall straight through it.
func TestResolvePropagatesTagExistsFailure(t *testing.T) {
	t.Parallel()

	repo := &brokenRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"}), failTagExists: true}

	_, err := Resolve(repo, Request{Mode: ModeRelease, Bump: BumpMinor})
	if err == nil {
		t.Fatal("Resolve: want an error when the tag-exists check fails")
	}
}

// TestClassifyNeverReportsBrokenGitAsProvenance is the direction guard on
// FromMain, and it is not the same shape as the Latest guard above.
//
// FromMain=false is not a harmless default. release-upgrade skips deploy, Orca
// and smoke for a release whose from_main is not 'true', and then publishes it
// anyway: the workflow's own notice says it will publish without a soak. So a
// swallowed git failure here does not withhold a claim, it ships a release that
// was never deployed anywhere. The answer must be an error, not a provenance
// fact nobody established.
func TestClassifyNeverReportsBrokenGitAsProvenance(t *testing.T) {
	t.Parallel()

	repo := &brokenRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"}), failIsAncestor: true}

	result, err := Classify(repo, "v0.4.0")
	if err == nil {
		t.Fatalf("Classify: want an error, got FromMain=%v", result.FromMain)
	}

	if !errors.Is(err, errGitBroke) {
		t.Errorf("error = %v, want it to wrap the git failure", err)
	}

	if result != nil {
		t.Errorf("result = %+v, want nil so no field can be read as an answer", result)
	}
}

// TestResolveReportsABrokenAncestryCheckHonestly is the resolver half.
//
// This one already refused, because "not an ancestor" and "could not tell" both
// stop the tag being minted. What was wrong was the diagnosis: it accused the
// commit of not being on the branch when git had actually failed, which sends
// whoever hits it looking at the wrong thing.
func TestResolveReportsABrokenAncestryCheckHonestly(t *testing.T) {
	t.Parallel()

	repo := &brokenRepo{fakeRepo: newFakeRepo([]string{"v0.4.0"}), failIsAncestor: true}

	_, err := Resolve(repo, Request{Mode: ModeRelease, Bump: BumpMinor})
	if err == nil {
		t.Fatal("Resolve: want an error when the ancestry check fails")
	}

	if !errors.Is(err, errGitBroke) {
		t.Errorf("error = %v, want it to wrap the git failure", err)
	}

	if strings.Contains(err.Error(), "not an ancestor") {
		t.Errorf("error blames the commit for a git failure: %v", err)
	}
}
