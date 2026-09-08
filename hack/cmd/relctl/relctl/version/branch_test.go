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
