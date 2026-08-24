// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"strings"
	"testing"
)

// Tests for Classify, ported from hack/release/classify_release_test.go.
//
// Two questions that are not the same question. from_main is provenance: does
// this release belong on the cluster that soaks main. latest is ordering: does
// anything outrank it. An earlier design answered both with one version
// comparison and got the first one wrong.
//
// The fixtures build real branch topology, so a "release branch" here is a
// commit that is genuinely not an ancestor of HEAD, which is the only thing
// from_main actually reads.

// classifyFixture builds a repository with main and optional side branches.
type classifyFixture struct {
	// mainTags are tagged on main, in order, each on its own commit.
	mainTags []string
	// branchTags are tagged on a branch cut from branchFrom, in order.
	branchTags []string
	// branchFrom is the tag the side branch is cut from.
	branchFrom string
	// extraMainTags are tagged on main AFTER the branch was cut.
	extraMainTags []string
}

func (f classifyFixture) build(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")

	main := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

	for _, tag := range f.mainTags {
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "work for "+tag)
		git(t, dir, "tag", tag)
	}

	if f.branchFrom != "" {
		git(t, dir, "checkout", "-q", "-b", "side", f.branchFrom)

		for _, tag := range f.branchTags {
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "fix for "+tag)
			git(t, dir, "tag", tag)
		}

		git(t, dir, "checkout", "-q", main)
	}

	for _, tag := range f.extraMainTags {
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "work for "+tag)
		git(t, dir, "tag", tag)
	}

	return dir
}

func TestClassify(t *testing.T) {
	requireGit(t)
	t.Parallel()

	cases := []struct {
		name         string
		fixture      classifyFixture
		tag          string
		wantFromMain bool
		wantLatest   bool
	}{
		{
			// The ordinary release. Soaks, and is Latest.
			name:         "release cut from main",
			fixture:      classifyFixture{mainTags: []string{"v0.4.0"}},
			tag:          "v0.4.0",
			wantFromMain: true,
			wantLatest:   true,
		},
		{
			// The case that proves the two questions differ. A patch cut from a
			// release branch while main has not moved is the newest release, so
			// it IS Latest, but it must not soak: it did not come from main.
			name: "patch on a release branch, main not moved",
			fixture: classifyFixture{
				mainTags:   []string{"v0.4.0"},
				branchFrom: "v0.4.0",
				branchTags: []string{"v0.4.1"},
			},
			tag:          "v0.4.1",
			wantFromMain: false,
			wantLatest:   true,
		},
		{
			name: "patch on a release branch after main moved on",
			fixture: classifyFixture{
				mainTags:      []string{"v0.4.0"},
				branchFrom:    "v0.4.0",
				branchTags:    []string{"v0.4.1"},
				extraMainTags: []string{"v0.5.0"},
			},
			tag:          "v0.4.1",
			wantFromMain: false,
			wantLatest:   false,
		},
		{
			// Backfill. v0.4.2 exists on the branch and is invisible from main,
			// so scoping to main alone would mark the older v0.4.1 Latest and
			// flip the marker backwards. This is why the series half of the
			// query is deliberately NOT reachability-scoped.
			name: "republishing a superseded patch on the same branch",
			fixture: classifyFixture{
				mainTags:   []string{"v0.4.0"},
				branchFrom: "v0.4.0",
				branchTags: []string{"v0.4.1", "v0.4.2"},
			},
			tag:          "v0.4.1",
			wantFromMain: false,
			wantLatest:   false,
		},
		{
			name:         "candidate from main",
			fixture:      classifyFixture{mainTags: []string{"v0.4.0", "v0.5.0-rc.1"}},
			tag:          "v0.5.0-rc.1",
			wantFromMain: true,
			wantLatest:   false,
		},
		{
			// A stray final on an unmerged branch must not suppress Latest
			// forever, which is why the trunk half IS reachability-scoped.
			name: "stray final on an unrelated branch is ignored",
			fixture: classifyFixture{
				mainTags:   []string{"v0.4.0"},
				branchFrom: "v0.4.0",
				branchTags: []string{"v9.0.0"},
			},
			tag:          "v0.4.0",
			wantFromMain: true,
			wantLatest:   true,
		},
		{
			// Both halves at once: the stray outranks nothing, and the branch's
			// own newer patch does.
			name: "series tags outrank, stray tags do not",
			fixture: classifyFixture{
				mainTags:   []string{"v0.4.0"},
				branchFrom: "v0.4.0",
				branchTags: []string{"v0.4.1", "v9.9.9"},
			},
			tag:          "v0.4.0",
			wantFromMain: true,
			wantLatest:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := tc.fixture.build(t)

			got, err := Classify(NewGitRepo(t.Context(), dir), tc.tag)
			if err != nil {
				t.Fatalf("Classify(%s): %v", tc.tag, err)
			}

			if got.FromMain != tc.wantFromMain {
				t.Errorf("FromMain = %v, want %v\n%s", got.FromMain, tc.wantFromMain,
					strings.Join(got.Report, "\n"))
			}

			if got.Latest != tc.wantLatest {
				t.Errorf("Latest = %v, want %v\n%s", got.Latest, tc.wantLatest,
					strings.Join(got.Report, "\n"))
			}
		})
	}
}

func TestClassifyRefuses(t *testing.T) {
	requireGit(t)
	t.Parallel()

	cases := []struct {
		name string
		tag  string
		want string
	}{
		{name: "no tag", tag: "", want: "no tag given"},
		{name: "not a version", tag: "nonsense", want: "not a release tag"},
		{name: "no v prefix", tag: "0.4.0", want: "not a release tag"},
		{name: "leading zeros", tag: "v01.2.3", want: "not a release tag"},
		{name: "unknown tag", tag: "v9.9.9", want: "does not exist here"},
		{name: "two-part version", tag: "v0.4", want: "not a release tag"},
		{name: "ten digit component", tag: "v1234567890.0.0", want: "not a release tag"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := classifyFixture{mainTags: []string{"v0.4.0"}}.build(t)

			_, err := Classify(NewGitRepo(t.Context(), dir), tc.tag)
			if err == nil {
				t.Fatalf("Classify(%q): want an error", tc.tag)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestClassifyAcceptsALegacySuffix pins the deliberate looseness of the tag
// pattern. alpha and beta predate rc being the only prerelease suffix, and a
// historical tag must still classify rather than becoming unreadable.
func TestClassifyAcceptsALegacySuffix(t *testing.T) {
	requireGit(t)
	t.Parallel()

	dir := classifyFixture{mainTags: []string{"v0.4.0", "v0.5.0-alpha.3"}}.build(t)

	got, err := Classify(NewGitRepo(t.Context(), dir), "v0.5.0-alpha.3")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if !got.FromMain {
		t.Error("FromMain = false, want true")
	}

	if got.Latest {
		t.Error("Latest = true; a prerelease can never be Latest")
	}
}
