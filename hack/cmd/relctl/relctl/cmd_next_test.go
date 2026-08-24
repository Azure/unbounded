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

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
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

	// Exactly the two keys the workflow reads, one per line, nothing else.
	// Extra output here lands in $GITHUB_OUTPUT and can corrupt it.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 {
		t.Fatalf("github output has %d lines, want 2:\n%s", len(lines), got)
	}

	if lines[0] != "tag=v0.5.0" {
		t.Errorf("line 1 = %q, want tag=v0.5.0", lines[0])
	}

	if !strings.HasPrefix(lines[1], "base=") {
		t.Errorf("line 2 = %q, want a base= line", lines[1])
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
