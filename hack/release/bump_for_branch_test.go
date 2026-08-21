// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for bump-for-branch.sh.
//
// The script encodes the versioning rule: main cuts minors and majors, a
// release branch cuts patches, and nothing else may release at all. Every case
// below is a way of getting that wrong, because getting it wrong produces a tag
// that collides with another branch's number space rather than an error anyone
// would notice at the time.

// runBumpForBranch executes the script with the given branch and environment.
// It deliberately does not use the fake-kubectl harness the other scripts need:
// this one talks to nothing.
func runBumpForBranch(t *testing.T, branch string, env map[string]string) (string, int) {
	t.Helper()

	script, err := filepath.Abs("bump-for-branch.sh")
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	args := []string{script}
	if branch != "" {
		args = append(args, branch)
	}

	command := exec.Command("bash", args...) //nolint:gosec // fixed script path

	command.Env = append(os.Environ(), envSlice(env)...)

	output, err := command.CombinedOutput()

	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec only returns this
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run script: %v", err)
	}

	return string(output), code
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}

	return out
}

// field extracts one key=value line from the script's stdout.
func field(t *testing.T, output, key string) string {
	t.Helper()

	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return rest
		}
	}

	return ""
}

func TestBumpForBranchResolves(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	cases := []struct {
		name       string
		branch     string
		major      string
		wantBump   string
		wantSeries string
	}{
		{name: "main cuts a minor", branch: "main", wantBump: "minor"},
		{name: "main cuts a major when asked", branch: "main", major: "true", wantBump: "major"},
		{name: "main with major false is still a minor", branch: "main", major: "false", wantBump: "minor"},
		{name: "release branch cuts a patch", branch: "release-0.2", wantBump: "patch", wantSeries: "0.2"},
		{name: "multi-digit series", branch: "release-12.34", wantBump: "patch", wantSeries: "12.34"},
		{name: "zero series", branch: "release-0.0", wantBump: "patch", wantSeries: "0.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := map[string]string{}
			if tc.major != "" {
				env["MAJOR"] = tc.major
			}

			output, code := runBumpForBranch(t, tc.branch, env)

			requireCode(t, code, 0, output)

			if got := field(t, output, "bump"); got != tc.wantBump {
				t.Errorf("bump = %q, want %q\n--- output ---\n%s", got, tc.wantBump, output)
			}

			if got := field(t, output, "series"); got != tc.wantSeries {
				t.Errorf("series = %q, want %q\n--- output ---\n%s", got, tc.wantSeries, output)
			}
		})
	}
}

// TestBumpForBranchRefuses covers every way of asking for a release that would
// escape its branch's number space.
func TestBumpForBranchRefuses(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	cases := []struct {
		name   string
		branch string
		major  string
		want   string
	}{
		{
			// The rule's whole point. A release branch cutting a minor would
			// mint a number main owns.
			name:   "major on a release branch",
			branch: "release-0.2",
			major:  "true",
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
			// release-1 has no minor, so it names no series.
			name:   "release branch without a minor",
			branch: "release-1",
			want:   "expected main or release-X.Y",
		},
		{
			// v01.2.3 is not a version, so release-01.2 names a series that no
			// tag could ever match.
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
			name:   "a non-boolean MAJOR",
			branch: "main",
			major:  "yes",
			want:   "MAJOR must be true or false",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := map[string]string{}
			if tc.major != "" {
				env["MAJOR"] = tc.major
			}

			output, code := runBumpForBranch(t, tc.branch, env)

			requireCode(t, code, 1, output)
			requireContains(t, output, tc.want)
			requireNotContains(t, output, "bump=")
		})
	}
}
