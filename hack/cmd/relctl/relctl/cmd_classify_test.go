// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"bytes"
	"strings"
	"testing"
)

// Tests for the classify command.
//
// The output contract is the interesting part: release-upgrade appends it to
// $GITHUB_OUTPUT and turns the keys into job conditions, so its shape decides
// whether a cluster gets touched and whether an install command is repointed.

// releaseBranchRepo builds main with v0.4.0 and a release-0.4 branch carrying
// v0.4.1, which is the shape where the two questions give different answers.
func releaseBranchRepo(t *testing.T) string {
	t.Helper()

	dir := tagRepo(t) // main, v0.4.0

	gitIn(t, dir, "checkout", "-q", "-b", "release-0.4")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "cherry-pick")
	gitIn(t, dir, "tag", "v0.4.1")
	gitIn(t, dir, "checkout", "-q", "main")

	return dir
}

func TestClassifyGitHubOutputMatchesTheWorkflowContract(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{}) // reasoning goes here, not into $GITHUB_OUTPUT
	cmd.SetArgs([]string{"classify", "v0.4.0", "--repo-path", dir, "-o", "github"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("classify: %v\n%s", err, out.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("github output has %d lines, want 2:\n%s", len(lines), out.String())
	}

	if lines[0] != "from_main=true" {
		t.Errorf("line 1 = %q, want from_main=true", lines[0])
	}

	if lines[1] != "latest=true" {
		t.Errorf("line 2 = %q, want latest=true", lines[1])
	}
}

// TestClassifyKeepsReasoningOutOfStdout is why the reasoning goes to stderr.
// Anything on stdout lands in $GITHUB_OUTPUT, where a stray line can corrupt
// the step's outputs.
func TestClassifyKeepsReasoningOutOfStdout(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)

	var stdout, stderr bytes.Buffer

	cmd := Root()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"classify", "v0.4.0", "--repo-path", dir, "-o", "github"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("classify: %v", err)
	}

	if !strings.Contains(stderr.String(), "reachable from HEAD") {
		t.Errorf("reasoning not on stderr:\n%s", stderr.String())
	}

	if strings.Contains(stdout.String(), "reachable from HEAD") {
		t.Errorf("reasoning leaked into stdout:\n%s", stdout.String())
	}
}

// TestClassifySeparatesTheTwoQuestions is the case the whole design exists for:
// a patch cut from a release branch while main has not moved is the newest
// release, so it IS Latest, but it must not soak.
func TestClassifySeparatesTheTwoQuestions(t *testing.T) {
	t.Parallel()

	dir := releaseBranchRepo(t)

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"classify", "v0.4.1", "--repo-path", dir, "-o", "github"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("classify: %v\n%s", err, out.String())
	}

	got := out.String()

	if !strings.Contains(got, "from_main=false") {
		t.Errorf("a release-branch patch must not soak:\n%s", got)
	}

	if !strings.Contains(got, "latest=true") {
		t.Errorf("the newest release must be Latest even off main:\n%s", got)
	}
}

func TestClassifyRefusesAnUnknownTag(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"classify", "v9.9.9", "--repo-path", dir})

	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("want an error for a tag that does not exist here")
	}

	if !strings.Contains(err.Error(), "does not exist here") {
		t.Errorf("error = %q", err)
	}
}

func TestClassifyTextOutput(t *testing.T) {
	t.Parallel()

	dir := tagRepo(t)

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"classify", "v0.4.0", "--repo-path", dir})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("classify: %v\n%s", err, out.String())
	}

	for _, want := range []string{"Soaks:", "Latest:", "yes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}
