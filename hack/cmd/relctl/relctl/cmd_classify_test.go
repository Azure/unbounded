// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

// runClassifyIn executes classify against a repository and returns stdout,
// stderr and the error, so a refusal can be told from a warning.
func runClassifyIn(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()

	var out, errOut bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"classify"}, append(args, "--repo-path", dir)...))

	err := cmd.ExecuteContext(t.Context())

	return out.String(), errOut.String(), err
}

// TestClassifyRefusesOffTheDefaultBranch is the precondition, enforced.
//
// From release-0.4 the tag v0.4.1 is reachable, so FromMain would come back
// true for a release that was not cut from main, and Latest would be computed
// against that branch's trunk. Both answers wrong, neither able to fail on its
// own. The doc comment on version.Classify and the command's own help both
// stated this and nothing checked it.
func TestClassifyRefusesOffTheDefaultBranch(t *testing.T) {
	t.Parallel()

	dir := releaseBranchRepo(t)
	gitIn(t, dir, "checkout", "-q", "release-0.4")

	out, _, err := runClassifyIn(t, dir, "v0.4.1")
	if err == nil {
		t.Fatalf("classify: want a refusal from release-0.4, got:\n%s", out)
	}

	// Naming both branches is what makes the message actionable; "wrong
	// branch" would leave the reader to work out which one they are on.
	for _, want := range []string{"main", "release-0.4", "--repo-path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}

	if out != "" {
		t.Errorf("refused but still wrote an answer to stdout:\n%s", out)
	}
}

// TestClassifyRefusesBeforeAnswering pins that no answer reaches stdout in any
// output format. json and github put the result there, so a caller parsing
// stdout must get nothing rather than a plausible wrong object.
func TestClassifyRefusesBeforeAnswering(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"text", "json", "github"} {
		dir := releaseBranchRepo(t)
		gitIn(t, dir, "checkout", "-q", "release-0.4")

		out, _, err := runClassifyIn(t, dir, "v0.4.1", "-o", format)
		if err == nil {
			t.Errorf("-o %s: want a refusal", format)
		}

		if out != "" {
			t.Errorf("-o %s wrote to stdout despite refusing:\n%s", format, out)
		}
	}
}

// TestClassifyWorksOnTheDefaultBranch is the other half, so the guard cannot
// pass by refusing everything.
func TestClassifyWorksOnTheDefaultBranch(t *testing.T) {
	t.Parallel()

	out, _, err := runClassifyIn(t, releaseBranchRepo(t), "v0.4.0", "-o", "github")
	if err != nil {
		t.Fatalf("classify on main: %v\n%s", err, out)
	}

	if !strings.Contains(out, "from_main=true") {
		t.Errorf("output = %q", out)
	}
}

// TestClassifyWarnsOnDetachedHead keeps "cannot tell" distinct from "is
// wrong". CurrentBranch reports empty for a detached HEAD rather than
// guessing, and refusing on that would break a legitimate checkout of a commit
// for the sake of a case that cannot actually be detected.
func TestClassifyWarnsOnDetachedHead(t *testing.T) {
	t.Parallel()

	dir := releaseBranchRepo(t)
	gitIn(t, dir, "checkout", "-q", "--detach", "main")

	out, errOut, err := runClassifyIn(t, dir, "v0.4.0", "-o", "github")
	if err != nil {
		t.Fatalf("classify detached: want a warning, got a refusal: %v", err)
	}

	if !strings.Contains(out, "from_main=true") {
		t.Errorf("detached HEAD did not answer: %q", out)
	}

	if !strings.Contains(errOut, "warning:") {
		t.Errorf("detached HEAD answered with no warning:\n%s", errOut)
	}
}
