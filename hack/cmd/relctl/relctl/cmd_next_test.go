// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Tests for the command layer.
//
// The github output format is the one that matters most here: release-prepare
// appends it straight to $GITHUB_OUTPUT, so its shape is a contract with a
// workflow rather than a presentation choice. It replaces what next-version.sh
// printed, and any drift breaks steps two workflows away.

// tagRepo builds a repository with one final tag.
//
// The environment is neutralized for the same reason the version package's
// fixtures are: a maintainer with commit.gpgsign, tag.gpgSign, a hooks path or
// a commit template would otherwise fail these for reasons that have nothing to
// do with relctl. Verified against a config setting all four.
func tagRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()

		cmd := exec.Command("git", args...) //nolint:gosec // fixed binary, test args
		cmd.Dir = dir

		cmd.Env = gitEnv()

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("tag", "v0.4.0")

	return dir
}

func TestNextGitHubOutputMatchesTheWorkflowContract(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"next", "--branch", "main", "--repo-path", dir, "-o", "github"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("next: %v\n%s", err, out.String())
	}

	got := out.String()

	// Exactly the four keys the workflow reads, one per line, nothing else.
	// Anything extra lands in $GITHUB_OUTPUT and can corrupt it.
	//
	// Four rather than two because this replaces BOTH scripts: release-prepare
	// reads bump= and series= from bump-for-branch.sh today, and relctl has to
	// compute the policy internally to resolve at all.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 4 {
		t.Fatalf("github output has %d lines, want 4:\n%s", len(lines), got)
	}

	want := []string{"tag=v0.5.0", "base=", "bump=minor", "series="}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want it to start with %q", i+1, lines[i], prefix)
		}
	}
}

// TestNextDefaultsToTheCheckedOutBranch is the trap the default used to set.
// --branch is policy, tag discovery is scoped by reachability from local HEAD,
// and the two are independent: defaulting to main meant a release-0.3 checkout
// got main's minor-bump policy applied to that branch's history, confidently.
func TestNextDefaultsToTheCheckedOutBranch(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)
	gitIn(t, dir, "checkout", "-q", "-b", "release-0.4")

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"next", "--repo-path", dir, "-o", "github"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("next: %v\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "tag=v0.4.1") {
		t.Errorf("want the release branch's patch, got:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "bump=patch") {
		t.Errorf("want bump=patch, got:\n%s", out.String())
	}
}

// TestNextWarnsWhenBranchDisagreesWithTheCheckout keeps an explicit --branch
// working, since resolving a hypothetical is legitimate, while making the
// mistake visible.
func TestNextWarnsWhenBranchDisagreesWithTheCheckout(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)
	gitIn(t, dir, "checkout", "-q", "-b", "release-0.4")

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"next", "--branch", "main", "--repo-path", dir, "-o", "github"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("next: %v\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "versions resolve against the CHECKOUT") {
		t.Errorf("no warning about the mismatch:\n%s", out.String())
	}
}

func TestNextAppliesTheBranchPolicy(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)

	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{
			name: "main cuts a minor",
			args: []string{"next", "--branch", "main"},
			want: "tag=v0.5.0",
		},
		{
			name: "main cuts a major when asked",
			args: []string{"next", "--branch", "main", "--major"},
			want: "tag=v1.0.0",
		},
		{
			name: "a release branch cuts a patch",
			args: []string{"next", "--branch", "release-0.4"},
			want: "tag=v0.4.1",
		},
		{
			// The rule that makes release branches possible at all: main and a
			// release branch must never compete for a number.
			name:    "a release branch may not cut a major",
			args:    []string{"next", "--branch", "release-0.4", "--major"},
			wantErr: "only valid on main",
		},
		{
			name:    "a feature branch may not release",
			args:    []string{"next", "--branch", "feat/x"},
			wantErr: "expected main or release-X.Y",
		},
		{
			// Outside its series, so refused even though the branch is valid.
			name:    "a release branch with no tags in its series",
			args:    []string{"next", "--branch", "release-9.9"},
			wantErr: "outside series 9.9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			cmd := Root()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append(append([]string{}, tc.args...), "--repo-path", dir, "-o", "github"))

			err := cmd.ExecuteContext(t.Context())

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, got:\n%s", tc.wantErr, out.String())
				}

				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("next: %v\n%s", err, out.String())
			}

			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tc.want)
			}
		})
	}
}

func TestUnknownOutputFormatIsRejected(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"next", "--repo-path", dir, "-o", "yaml"})

	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("want an error for an unknown output format")
	}

	if !strings.Contains(err.Error(), "unknown output format") {
		t.Errorf("error = %q", err)
	}
}

// TestNextNeedsNoCredential is the property that keeps version resolution
// usable in a workflow that was never granted a token, and locally before
// anyone has run gh auth login.
func TestNextNeedsNoCredential(t *testing.T) {
	dir := tagRepo(t)

	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"next", "--branch", "main", "--repo-path", dir, "-o", "github"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("next without a credential: %v\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "tag=v0.5.0") {
		t.Errorf("output = %q", out.String())
	}
}

// gitIn runs a git command inside an existing fixture.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...) //nolint:gosec // fixed binary, test args
	cmd.Dir = dir

	cmd.Env = gitEnv()

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitEnv neutralizes the ambient git configuration and supplies an identity, so
// a fixture needs no `git config` of its own and a maintainer with signing,
// hooks or a commit template configured does not fail these for reasons that
// have nothing to do with relctl.
//
// The same guard the version package uses. It lives in both because they are
// separate packages, not because the reason differs.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TEMPLATE_DIR=",
	)
}
