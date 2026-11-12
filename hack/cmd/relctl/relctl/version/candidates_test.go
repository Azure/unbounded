// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"testing"
)

func TestParseCandidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tag      string
		wantCore string
		wantN    int
		wantOK   bool
	}{
		{tag: "v0.3.0-rc.1", wantCore: "v0.3.0", wantN: 1, wantOK: true},
		{tag: "v0.3.0-rc.18", wantCore: "v0.3.0", wantN: 18, wantOK: true},
		{tag: "v1.20.300-rc.9", wantCore: "v1.20.300", wantN: 9, wantOK: true},
		// Everything discovery declines to count must be declined here too, or
		// cleanup could act on a tag the resolver says is not a candidate.
		{tag: "v0.3.0"},
		{tag: "v0.3.0-rc.0"},
		{tag: "v0.3.0-rc.01"},
		{tag: "v0.3.0-rc"},
		{tag: "v0.3.0-alpha.1"},
		{tag: "v0.3-rc.1"},
		{tag: "0.3.0-rc.1"},
		{tag: "v0.3.0-rc.1-extra"},
		{tag: ""},
	}

	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			t.Parallel()

			core, n, ok := ParseCandidate(tc.tag)
			if ok != tc.wantOK || core != tc.wantCore || n != tc.wantN {
				t.Errorf("ParseCandidate(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.tag, core, n, ok, tc.wantCore, tc.wantN, tc.wantOK)
			}
		})
	}
}

func TestIsFinal(t *testing.T) {
	t.Parallel()

	for tag, want := range map[string]bool{
		"v0.3.0":      true,
		"v10.0.1":     true,
		"v0.3.0-rc.1": false,
		"v0.3":        false,
		"v01.3.0":     false,
		"":            false,
	} {
		if got := IsFinal(tag); got != want {
			t.Errorf("IsFinal(%q) = %v, want %v", tag, got, want)
		}
	}
}

// TestCandidatesAreNewestFirstAndNumeric is the ordering every consumer relies
// on: promote takes the newest that passed, and cleanup keeps the newest. A
// lexical order would put rc.9 above rc.10.
func TestCandidatesAreNewestFirstAndNumeric(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo([]string{
		"v0.2.4",
		"v0.2.5-rc.9@new",
		"v0.2.5-rc.10@new",
		"v0.2.5-rc.2@new",
		"v0.2.5-rc.11@off",
		"v0.2.5-alpha.1",
		"v0.3.0-rc.1",
	})

	got, err := Candidates(repo, "v0.2.5")
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	tags := make([]string, 0, len(got))
	for _, c := range got {
		tags = append(tags, c.Tag)

		if c.Core != "v0.2.5" {
			t.Errorf("%s has core %q", c.Tag, c.Core)
		}

		want, err := repo.CommitOf(c.Tag)
		if err != nil {
			t.Fatalf("CommitOf: %v", err)
		}

		if c.Commit != want {
			t.Errorf("%s commit = %s, want %s", c.Tag, c.Commit, want)
		}
	}

	// rc.11 is off-branch, alpha is not a candidate, v0.3.0-rc.1 is another
	// train. None of them belong in the list.
	assertSlice(t, "Candidates", tags, []string{"v0.2.5-rc.10", "v0.2.5-rc.9", "v0.2.5-rc.2"})
}

func TestCandidatesRefusesANonFinal(t *testing.T) {
	t.Parallel()

	if _, err := Candidates(newFakeRepo([]string{"v0.2.4"}), "v0.2.5-rc.1"); err == nil {
		t.Fatal("Candidates(rc) = nil error, want a refusal")
	}
}

// TestCandidateAtFindsWhatAFinalWasPromotedFrom is what lets a promoted final
// skip its soak: the candidate at the same commit already passed one.
func TestCandidateAtFindsWhatAFinalWasPromotedFrom(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		tags   []string
		final  string
		want   string
		wantOK bool
	}{
		{
			name:   "promoted from the highest",
			tags:   []string{"v0.2.4", "v0.2.5-rc.1@new", "v0.2.5-rc.2@new", "v0.2.5"},
			final:  "v0.2.5",
			want:   "v0.2.5-rc.2",
			wantOK: true,
		},
		{
			name:   "promoted from one below the highest",
			tags:   []string{"v0.2.4", "v0.2.5-rc.1@new", "v0.2.5", "v0.2.5-rc.2@new"},
			final:  "v0.2.5",
			want:   "v0.2.5-rc.1",
			wantOK: true,
		},
		{
			// Two candidates of one commit happen when someone re-cuts by hand.
			// The newer is the one that most recently soaked it.
			name:   "two candidates of one commit",
			tags:   []string{"v0.2.4", "v0.2.5-rc.1@new", "v0.2.5-rc.2", "v0.2.5"},
			final:  "v0.2.5",
			want:   "v0.2.5-rc.2",
			wantOK: true,
		},
		{
			name:  "cut directly, before candidates were mandatory",
			tags:  []string{"v0.2.4", "v0.2.5-rc.1@new", "v0.2.5@new"},
			final: "v0.2.5",
		},
		{
			name:  "no candidates at all",
			tags:  []string{"v0.2.4", "v0.2.5@new"},
			final: "v0.2.5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok, err := CandidateAt(newFakeRepo(tc.tags), tc.final)
			if err != nil {
				t.Fatalf("CandidateAt: %v", err)
			}

			if ok != tc.wantOK || got.Tag != tc.want {
				t.Errorf("CandidateAt(%s) = (%q, %v), want (%q, %v)", tc.final, got.Tag, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestCandidateAtRefusesACandidate(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo([]string{"v0.2.4", "v0.2.5-rc.1"})

	if _, _, err := CandidateAt(repo, "v0.2.5-rc.1"); err == nil {
		t.Fatal("CandidateAt(rc) = nil error, want a refusal")
	}
}

// TestDiscoverMatchesResolve keeps the dashboards' view and the resolver's from
// drifting: they are the same discovery, and used to be literally one call.
func TestDiscoverMatchesResolve(t *testing.T) {
	t.Parallel()

	tags := []string{"v0.1.24-rc.18", "v0.2.4", "v0.2.5-rc.1", "v0.3.0-rc.1"}
	repo := newFakeRepo(tags)

	state, err := Discover(repo)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	resolved, err := Resolve(repo, Request{Mode: ModeRelease, Bump: BumpMinor})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if state.LatestFinal != resolved.LatestFinal {
		t.Errorf("LatestFinal: Discover %q, Resolve %q", state.LatestFinal, resolved.LatestFinal)
	}

	assertSlice(t, "Live", state.Live, resolved.Live)
	assertSlice(t, "Stale", state.Stale, resolved.Stale)
	assertSlice(t, "Live (want)", state.Live, []string{"v0.2.5", "v0.3.0"})
	assertSlice(t, "Stale (want)", state.Stale, []string{"v0.1.24"})
}

// TestDiscoverSucceedsWhereResolveRefuses is why Discover exists. Two live
// trains make the next candidate unresolvable, but the train view itself is a
// perfectly good answer, and the dashboard must not go blank over it.
func TestDiscoverSucceedsWhereResolveRefuses(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo([]string{"v0.2.4", "v0.2.5-rc.1", "v0.3.0-rc.1"})

	if _, err := Resolve(repo, Request{Mode: ModePrerelease, Bump: BumpMinor}); err == nil {
		t.Fatal("Resolve(prerelease) with two trains: want a refusal, or this test proves nothing")
	}

	state, err := Discover(repo)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(state.Live) != 2 {
		t.Errorf("Live = %v, want both trains", state.Live)
	}
}

// TestPromoteReportsItsCandidate is the field the workflow summary and the
// relctl preview read to say which tree is shipping.
func TestPromoteReportsItsCandidate(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo([]string{"v0.2.4", "v0.2.5-rc.1@new", "v0.2.5-rc.2@new"})

	highest, err := Resolve(repo, Request{Mode: ModePromote})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if highest.Candidate != "v0.2.5-rc.2" {
		t.Errorf("Candidate = %q, want the highest rc", highest.Candidate)
	}

	named, err := Resolve(repo, Request{Mode: ModePromote, Candidate: "v0.2.5-rc.1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if named.Candidate != "v0.2.5-rc.1" {
		t.Errorf("Candidate = %q, want the named rc", named.Candidate)
	}

	cut, err := Resolve(repo, Request{Mode: ModePrerelease, Bump: BumpMinor})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if cut.Candidate != "" {
		t.Errorf("Candidate = %q on a prerelease, want empty", cut.Candidate)
	}
}

// TestGitRepoTagsAt covers the scheduler's question, against real git: has
// HEAD already been cut?
func TestGitRepoTagsAt(t *testing.T) {
	requireGit(t)
	t.Parallel()

	dir := fixture(t, []string{"v0.2.4", "v0.2.5-rc.1@new", "v0.2.5"})
	repo := NewGitRepo(t.Context(), dir)

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	got, err := repo.TagsAt(head)
	if err != nil {
		t.Fatalf("TagsAt: %v", err)
	}

	assertSlice(t, "TagsAt(HEAD)", got, []string{"v0.2.5", "v0.2.5-rc.1"})

	git(t, dir, "commit", "-q", "--allow-empty", "-m", "untagged")

	head, err = repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	got, err = repo.TagsAt(head)
	if err != nil {
		t.Fatalf("TagsAt: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("TagsAt(untagged HEAD) = %v, want none", got)
	}

	if _, err := NewGitRepo(t.Context(), t.TempDir()).TagsAt("HEAD"); err == nil {
		t.Error("TagsAt outside a repository returned no error")
	}
}
