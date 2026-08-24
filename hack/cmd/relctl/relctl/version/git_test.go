// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"testing"
)

// Tests for GitRepo, and for the fake standing in for it.
//
// The permanent suite runs against fakeRepo, which buys speed and makes the
// train model assertable. It also creates a hazard: if the fake and git ever
// disagree, that suite becomes a test of the fake. Nothing else would catch it
// once the shell oracle is deleted, because the differential is the only other
// thing that touches real git and it goes at the same time.
//
// TestFakeRepoMatchesGit is what closes that. It resolves the same fixtures
// through both and requires the same answer.

// TestGitRepoScopesTagsByReachability is the property everything else rests on.
// A v9.0.0 cut on someone's feature branch must not become the latest final and
// make the next release from main v9.0.1.
func TestGitRepoScopesTagsByReachability(t *testing.T) {
	requireGit(t)
	t.Parallel()

	dir := fixture(t, []string{"v0.2.4", "v0.9.9@off"})
	repo := NewGitRepo(t.Context(), dir)

	reachable, err := repo.ReachableTags("v[0-9]*")
	if err != nil {
		t.Fatalf("ReachableTags: %v", err)
	}

	assertSlice(t, "ReachableTags", reachable, []string{"v0.2.4"})

	// Existence is a different question from reachability, and the resolver
	// depends on them differing: a name taken off-branch still blocks a train.
	exists, err := repo.TagExists("v0.9.9")
	if err != nil {
		t.Fatalf("TagExists: %v", err)
	}

	if !exists {
		t.Error("TagExists(v0.9.9) = false; an off-branch tag still exists")
	}
}

func TestGitRepoResolvesCommits(t *testing.T) {
	requireGit(t)
	t.Parallel()

	dir := fixture(t, []string{"v0.2.4", "v0.2.5-rc.1@new"})
	repo := NewGitRepo(t.Context(), dir)

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	candidate, err := repo.CommitOf("v0.2.5-rc.1")
	if err != nil {
		t.Fatalf("CommitOf: %v", err)
	}

	if candidate != head {
		t.Errorf("CommitOf(v0.2.5-rc.1) = %s, want HEAD %s", candidate, head)
	}

	older, err := repo.CommitOf("v0.2.4")
	if err != nil {
		t.Fatalf("CommitOf: %v", err)
	}

	ancestor, err := repo.IsAncestor(older)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}

	if !ancestor {
		t.Error("IsAncestor(v0.2.4) = false, want true")
	}

	count, err := repo.CountCommits(older)
	if err != nil {
		t.Fatalf("CountCommits: %v", err)
	}

	if count != 1 {
		t.Errorf("CountCommits(v0.2.4..HEAD) = %d, want 1", count)
	}

	subject, err := repo.Subject(older)
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	if subject == "" {
		t.Error("Subject = empty")
	}
}

// TestGitRepoRejectsAnOffBranchCommitAsAncestor covers the guard that stops a
// candidate orphaned by a force-push from being tagged.
func TestGitRepoRejectsAnOffBranchCommitAsAncestor(t *testing.T) {
	requireGit(t)
	t.Parallel()

	dir := fixture(t, []string{"v0.2.4", "v0.9.9@off"})
	repo := NewGitRepo(t.Context(), dir)

	offBranch, err := repo.CommitOf("v0.9.9")
	if err != nil {
		t.Fatalf("CommitOf: %v", err)
	}

	ancestor, err := repo.IsAncestor(offBranch)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}

	if ancestor {
		t.Error("IsAncestor(off-branch commit) = true, want false")
	}
}

// baseLabel describes which commit a resolution tagged, without comparing the
// opaque hashes that necessarily differ between git and the fake.
func baseLabel(t *testing.T, repo Repo, tags []string, base string) string {
	t.Helper()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	if base == head {
		return "HEAD"
	}

	for _, spec := range tags {
		name := spec
		for _, suffix := range []string{"@new", "@off"} {
			if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
				name = name[:len(name)-len(suffix)]
			}
		}

		commit, err := repo.CommitOf(name)
		if err != nil {
			continue
		}

		if commit == base {
			return name
		}
	}

	return "unknown:" + base
}

// TestFakeRepoMatchesGit is the conformance proof for the fake.
//
// Hashes cannot be compared, so the base is compared by what it POINTS AT:
// HEAD, or the tag whose commit it is. That is the property the resolver
// actually depends on, and the one promote gets wrong if it drifts.
func TestFakeRepoMatchesGit(t *testing.T) {
	requireGit(t)
	t.Parallel()

	// Shapes chosen for the Repo behaviours the resolver leans on: glob
	// matching, reachability scoping, existence-versus-reachability, candidates
	// behind HEAD, and malformed tags that must match no pattern.
	tagSets := [][]string{
		{},
		{"v0.2.4"},
		{"v0.2.4", "v0.2.5-rc.1"},
		{"v0.2.4", "v0.2.5-rc.1", "v0.2.5-rc.2@new"},
		{"v0.2.4", "v0.2.5-rc.9", "v0.2.5-rc.10"},
		{"v0.2.4", "v0.1.24-rc.1", "v0.3.0-rc.1"},
		{"v0.2.4", "v0.2.5-rc.1@off"},
		{"v0.2.4@off", "v0.1.0"},
		{"v0.2.4", "v0.2.5-rc.0", "v0.2.5-rc.01", "v0.2.5-alpha.1", "v0.2.5-rc"},
		{"v0.2.4", "v0.2", "v0.2.4.1", "v01.2.3"},
		{"v0.9.0", "v0.10.0"},
	}

	for _, mode := range []Mode{ModeRelease, ModePrerelease, ModePromote} {
		for i, tags := range tagSets {
			t.Run(string(mode)+"/set"+string(rune('a'+i)), func(t *testing.T) {
				t.Parallel()

				req := Request{Mode: mode, Bump: BumpPatch}

				gitRepo := NewGitRepo(t.Context(), fixture(t, tags))
				fake := newFakeRepo(tags)

				gitResult, gitErr := Resolve(gitRepo, req)
				fakeResult, fakeErr := Resolve(fake, req)

				if (gitErr == nil) != (fakeErr == nil) {
					t.Fatalf("git err = %v, fake err = %v", gitErr, fakeErr)
				}

				if gitErr != nil {
					if gitErr.Error() != fakeErr.Error() {
						t.Errorf("git refused with %q, fake with %q", gitErr, fakeErr)
					}

					return
				}

				if gitResult.Tag != fakeResult.Tag {
					t.Errorf("tag: git = %q, fake = %q", gitResult.Tag, fakeResult.Tag)
				}

				if gitResult.LatestFinal != fakeResult.LatestFinal {
					t.Errorf("LatestFinal: git = %q, fake = %q", gitResult.LatestFinal, fakeResult.LatestFinal)
				}

				assertSlice(t, "Live (git vs fake)", gitResult.Live, fakeResult.Live)
				assertSlice(t, "Stale (git vs fake)", gitResult.Stale, fakeResult.Stale)

				gitBase := baseLabel(t, gitRepo, tags, gitResult.Base)
				fakeBase := baseLabel(t, fake, tags, fakeResult.Base)

				if gitBase != fakeBase {
					t.Errorf("base: git points at %s, fake at %s", gitBase, fakeBase)
				}
			})
		}
	}
}

// TestGitRepoReportsQueryFailures is the guard on the swallow that used to be
// here. It matters that this exercises GitRepo rather than the fake: the
// resolver-level tests use a fake that returns whatever it is told, so they
// would keep passing if GitRepo went back to reporting a git failure as "no
// tags". An empty tag list is a floor of v0.0.0 that a version gets computed
// from, and for Classify it reads as "nothing outranks this".
func TestGitRepoReportsQueryFailures(t *testing.T) {
	requireGit(t)
	t.Parallel()

	// A directory that is not a repository, which is what every real failure
	// mode looks like from git's side: exit 128 with a message on stderr.
	repo := NewGitRepo(t.Context(), t.TempDir())

	if _, err := repo.ReachableTags("v*"); err == nil {
		t.Error("ReachableTags outside a repository returned no error")
	}

	if _, err := repo.AllTags("v*"); err == nil {
		t.Error("AllTags outside a repository returned no error")
	}

	if _, err := repo.TagExists("v0.4.0"); err == nil {
		t.Error("TagExists outside a repository returned no error")
	}

	if _, err := repo.Head(); err == nil {
		t.Error("Head outside a repository returned no error")
	}
}

// TestGitRepoTagExistsDistinguishesMissingFromBroken is the other half. A tag
// that is simply absent is an answer, not a failure, and conflating the two in
// either direction is a bug: reporting absence as an error would refuse every
// first release, and reporting failure as absence would let the duplicate-tag
// guard fall through.
func TestGitRepoTagExistsDistinguishesMissingFromBroken(t *testing.T) {
	requireGit(t)
	t.Parallel()

	repo := NewGitRepo(t.Context(), fixture(t, []string{"v0.4.0"}))

	exists, err := repo.TagExists("v0.4.0")
	if err != nil {
		t.Fatalf("TagExists(present): %v", err)
	}

	if !exists {
		t.Error("TagExists(v0.4.0) = false, want true")
	}

	exists, err = repo.TagExists("v9.9.9")
	if err != nil {
		t.Fatalf("TagExists(absent): %v, want no error for a merely missing tag", err)
	}

	if exists {
		t.Error("TagExists(v9.9.9) = true, want false")
	}
}
