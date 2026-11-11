// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"strings"
	"testing"
)

// Tests for ForBranch, ported from hack/release/bump_for_branch_test.go.
//
// It encodes the versioning rule: main cuts minors and majors, a release branch
// cuts patches, and nothing else may release at all. Every case below is a way
// of getting that wrong, because getting it wrong produces a tag that collides
// with another branch's number space rather than an error anyone would notice
// at the time.

func TestForBranchResolves(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		branch     string
		major      bool
		wantBump   Bump
		wantSeries string
	}{
		{name: "main cuts a minor", branch: "main", wantBump: BumpMinor},
		{name: "main cuts a major when asked", branch: "main", major: true, wantBump: BumpMajor},
		{name: "main with major false is still a minor", branch: "main", wantBump: BumpMinor},
		{name: "release branch cuts a patch", branch: "release-0.2", wantBump: BumpPatch, wantSeries: "0.2"},
		{name: "multi-digit series", branch: "release-12.34", wantBump: BumpPatch, wantSeries: "12.34"},
		{name: "zero series", branch: "release-0.0", wantBump: BumpPatch, wantSeries: "0.0"},
		{name: "nine digit series", branch: "release-123456789.0", wantBump: BumpPatch, wantSeries: "123456789.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ForBranch(tc.branch, tc.major)
			if err != nil {
				t.Fatalf("ForBranch(%q, %v): %v", tc.branch, tc.major, err)
			}

			if got.Bump != tc.wantBump {
				t.Errorf("bump = %q, want %q", got.Bump, tc.wantBump)
			}

			if got.Series != tc.wantSeries {
				t.Errorf("series = %q, want %q", got.Series, tc.wantSeries)
			}
		})
	}
}

func TestForBranchRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		branch string
		major  bool
		want   string
	}{
		{
			name:   "major on a release branch",
			branch: "release-0.2",
			major:  true,
			want:   "only valid on main",
		},
		{
			name:   "a feature branch",
			branch: "feat/something",
			want:   "expected main or release-X.Y",
		},
		{
			name:   "a tag-like ref",
			branch: "v0.2.4",
			want:   "expected main or release-X.Y",
		},
		{
			name:   "release branch without a minor",
			branch: "release-1",
			want:   "expected main or release-X.Y",
		},
		{
			// v01.2.3 is not a version, so this series could never match a tag.
			name:   "leading zeros in the series",
			branch: "release-01.2",
			want:   "expected main or release-X.Y",
		},
		{
			name:   "a patch-level branch name",
			branch: "release-0.2.4",
			want:   "expected main or release-X.Y",
		},
		{
			name:   "no branch at all",
			branch: "",
			want:   "no branch given",
		},
		{
			// Anchoring matters: an unanchored match would accept these.
			name:   "a prefixed release branch",
			branch: "backup/release-0.2",
			want:   "expected main or release-X.Y",
		},
		{
			name:   "a suffixed release branch",
			branch: "release-0.2-backup",
			want:   "expected main or release-X.Y",
		},
		{
			name:   "main with a suffix",
			branch: "main-2",
			want:   "expected main or release-X.Y",
		},
		{
			name:   "an over-long series component",
			branch: "release-1234567890.0",
			want:   "expected main or release-X.Y",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ForBranch(tc.branch, tc.major)
			if err == nil {
				t.Fatalf("ForBranch(%q, %v): want an error", tc.branch, tc.major)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestIsReleaseBranch pins that only release-X.Y counts. A branch that merely
// starts with "release-", such as a feature branch named release-notes-fix,
// must not be reported as a release branch.
func TestIsReleaseBranch(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"release-0.2":              true,
		"release-1.0":              true,
		"release-10.23":            true,
		"main":                     false,
		"":                         false,
		"release-":                 false,
		"release-1":                false,
		"release-01.2":             false,
		"release-0.2.4":            false,
		"release-0.2-backup":       false,
		"backup/release-0.2":       false,
		"release-candidates-first": false,
		"release-1234567890.0":     false,
	}

	for branch, want := range cases {
		if got := IsReleaseBranch(branch); got != want {
			t.Errorf("IsReleaseBranch(%q) = %v, want %v", branch, got, want)
		}
	}
}

// TestCheckModeRetiresRelease pins the rule the whole candidate model rests on:
// a final is only ever a promoted candidate. mode=release minted the final
// number before anything had shown the tree could build, so a failure needing a
// code change cost a version.
func TestCheckModeRetiresRelease(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mode    Mode
		wantErr string
	}{
		{mode: ModePrerelease},
		{mode: ModePromote},
		{mode: ModeRelease, wantErr: "mode=release is retired"},
		{mode: "nonsense", wantErr: "unexpected mode"},
	}

	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()

			err := CheckMode(tc.mode)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckMode(%s): %v", tc.mode, err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckMode(%s) = %v, want an error containing %q", tc.mode, err, tc.wantErr)
			}
		})
	}
}

// TestCheckModePointsAtTheReplacement keeps the refusal actionable. Someone
// reaching for the old command needs to be told what replaced it.
func TestCheckModePointsAtTheReplacement(t *testing.T) {
	t.Parallel()

	err := CheckMode(ModeRelease)
	if err == nil {
		t.Fatal("CheckMode(release) = nil")
	}

	for _, want := range []string{"relctl rc", "relctl promote"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}
