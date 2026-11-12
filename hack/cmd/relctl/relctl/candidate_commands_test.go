// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
)

// Tests for the candidate model: every final is a promoted candidate, a
// candidate's soak result is a commit status, and drafts that stop being useful
// are cleaned up.

// candidateRepo builds main with v0.4.0, then v0.5.0-rc.1 and v0.5.0-rc.2 on
// commits of their own, with HEAD at rc.2.
func candidateRepo(t *testing.T) string {
	t.Helper()

	dir := tagRepo(t)

	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "work for rc.1")
	gitIn(t, dir, "tag", "v0.5.0-rc.1")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "work for rc.2")
	gitIn(t, dir, "tag", "v0.5.0-rc.2")

	return dir
}

// revParse returns the commit a ref names in a fixture.
func revParse(t *testing.T, dir, ref string) string {
	t.Helper()

	cmd := exec.Command("git", "rev-list", "-n1", ref) //nolint:gosec // fixed binary, test args
	cmd.Dir = dir
	cmd.Env = gitEnv()

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-list %s: %v", ref, err)
	}

	return strings.TrimSpace(string(out))
}

// runSplit runs relctl with stdout and stderr kept apart, for the outputs
// whose stdout is a contract with a workflow.
func runSplit(t *testing.T, stub *stubGitHub, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	cmd := newRoot(&Options{
		Repo:    gh.DefaultRepo,
		Output:  OutputText,
		BaseURL: stub.start(t),
		Token:   func(context.Context) (string, error) { return "test-token", nil },
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)

	err := cmd.ExecuteContext(t.Context())

	return stdout.String(), stderr.String(), err
}

// The promote tests.

// TestPromoteSendsTheNewestPassingCandidate is the scheduled-candidate case:
// the highest failed, the one below it passed. Promote ships the one that
// passed, and sends it explicitly, so the workflow finalizes what was
// previewed rather than whatever is newest when it runs.
func TestPromoteSendsTheNewestPassingCandidate(t *testing.T) {
	dir := candidateRepo(t)
	stub := &stubGitHub{statuses: map[string]stubStatus{
		revParse(t, dir, "v0.5.0-rc.1"): {State: "success"},
		revParse(t, dir, "v0.5.0-rc.2"): {State: "failure"},
	}}

	out, err := runDispatch(t, stub, "", "promote", "--repo-path", dir, "--yes")
	if err != nil {
		t.Fatalf("promote: %v\n%s", err, out)
	}

	if stub.dispatched != 1 {
		t.Fatalf("dispatched %d times, want 1", stub.dispatched)
	}

	if stub.lastInputs["candidate"] != "v0.5.0-rc.1" || stub.lastInputs["mode"] != "promote" {
		t.Errorf("inputs = %v, want mode=promote candidate=v0.5.0-rc.1", stub.lastInputs)
	}

	if _, ok := stub.lastInputs["force"]; ok {
		t.Errorf("an ordinary promote sent force: %v", stub.lastInputs)
	}

	if !strings.Contains(out, "This will promote v0.5.0-rc.1 to v0.5.0 on main") {
		t.Errorf("no preview of what ships:\n%s", out)
	}
}

// TestPromoteRefusesWithoutAPassingCandidate is the gate. Nothing has passed,
// so nothing is dispatched, and the refusal shows what each candidate did.
func TestPromoteRefusesWithoutAPassingCandidate(t *testing.T) {
	dir := candidateRepo(t)
	stub := &stubGitHub{statuses: map[string]stubStatus{
		revParse(t, dir, "v0.5.0-rc.1"): {State: "failure"},
	}}

	out, err := runDispatch(t, stub, "", "promote", "--repo-path", dir, "--yes")
	if err == nil {
		t.Fatalf("promote: want a refusal\n%s", out)
	}

	if !strings.Contains(err.Error(), "no candidate of v0.5.0 has passed its soak") {
		t.Errorf("error = %q", err)
	}

	for _, want := range []string{"v0.5.0-rc.2", "no result", "v0.5.0-rc.1", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal does not show %q:\n%s", want, out)
		}
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched %d times without a passing candidate", stub.dispatched)
	}
}

func TestPromoteANamedCandidateMustHavePassed(t *testing.T) {
	dir := candidateRepo(t)
	stub := &stubGitHub{statuses: map[string]stubStatus{
		revParse(t, dir, "v0.5.0-rc.1"): {State: "failure"},
		revParse(t, dir, "v0.5.0-rc.2"): {State: "success"},
	}}

	_, err := runDispatch(t, stub, "", "promote", "--repo-path", dir, "--candidate", "v0.5.0-rc.1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "v0.5.0-rc.1 has not passed its soak") {
		t.Fatalf("promote --candidate rc.1 = %v, want a refusal naming it", err)
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched a candidate that did not pass")
	}

	if _, err := runDispatch(t, stub, "", "promote", "--repo-path", dir, "--candidate", "v0.5.0-rc.2", "--yes"); err != nil {
		t.Fatalf("promote --candidate rc.2: %v", err)
	}

	if stub.lastInputs["candidate"] != "v0.5.0-rc.2" {
		t.Errorf("candidate input = %v, want v0.5.0-rc.2", stub.lastInputs["candidate"])
	}
}

func TestPromoteForceNeedsAReason(t *testing.T) {
	dir := candidateRepo(t)

	for _, args := range [][]string{
		{"promote", "--repo-path", dir, "--force"},
		{"promote", "--repo-path", dir, "--reason", "cluster down"},
	} {
		stub := &stubGitHub{}

		if _, err := runDispatch(t, stub, "promote v0.5.0 unsoaked\n", args...); err == nil {
			t.Errorf("%v: want a refusal", args)
		}

		if stub.dispatched != 0 {
			t.Errorf("%v dispatched", args)
		}
	}
}

// TestPromoteForceIsBreakGlass holds a forced promote to the same model as the
// other break-glass paths: --yes cannot reach it, and only the exact phrase
// does.
func TestPromoteForceIsBreakGlass(t *testing.T) {
	dir := candidateRepo(t)

	// No statuses at all: a forced promote must not need one.
	stub := &stubGitHub{}

	_, err := runDispatch(t, stub, "y\n",
		"promote", "--repo-path", dir, "--force", "--reason", "cluster down", "--yes")
	if err == nil || !strings.Contains(err.Error(), "--yes does not apply") {
		t.Fatalf("promote --force --yes = %v, want --yes refused", err)
	}

	if stub.dispatched != 0 {
		t.Fatalf("dispatched on --yes")
	}

	out, err := runDispatch(t, stub, "promote v0.5.0 unsoaked\n",
		"promote", "--repo-path", dir, "--force", "--reason", "cluster down")
	if err != nil {
		t.Fatalf("promote --force with the phrase: %v\n%s", err, out)
	}

	if stub.dispatched != 1 {
		t.Fatalf("dispatched %d times, want 1", stub.dispatched)
	}

	if stub.lastInputs["force"] != true || stub.lastInputs["reason"] != "cluster down" {
		t.Errorf("inputs = %v, want force=true and the reason", stub.lastInputs)
	}
}

func TestPublishRefusesACandidate(t *testing.T) {
	stub := &stubGitHub{}

	_, err := runDispatch(t, stub, "publish v0.5.0-rc.1 unsoaked\n",
		"publish", "v0.5.0-rc.1", "--reason", "cluster down")
	if err == nil || !strings.Contains(err.Error(), "relctl promote") {
		t.Fatalf("publish rc = %v, want a refusal pointing at promote", err)
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched a publish of a candidate")
	}
}

// The next tests.

func TestNextRefusesTheRetiredMode(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"next", "--branch", "main", "--mode", "release", "--repo-path", tagRepo(t)})

	err := cmd.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("next --mode release = %v, want it refused as retired", err)
	}

	if out.Len() != 0 {
		t.Errorf("refused but wrote:\n%s", out.String())
	}
}

// TestNextPromotesANamedCandidate is the contract release-prepare relies on:
// the tag is the final, and the base is the NAMED candidate's commit, not the
// highest rc's and not HEAD.
func TestNextPromotesANamedCandidate(t *testing.T) {
	t.Parallel()

	dir := candidateRepo(t)

	var out bytes.Buffer

	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"next", "--branch", "main", "--mode", "promote", "--candidate", "v0.5.0-rc.1",
		"--repo-path", dir, "-o", "github",
	})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("next: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("github output has %d lines, want 4:\n%s", len(lines), out.String())
	}

	if lines[0] != "tag=v0.5.0" {
		t.Errorf("line 1 = %q, want tag=v0.5.0", lines[0])
	}

	if want := "base=" + revParse(t, dir, "v0.5.0-rc.1"); lines[1] != want {
		t.Errorf("line 2 = %q, want %q", lines[1], want)
	}
}

// The candidate command tests.

// TestCandidatePickGitHubOutput is release-prepare's contract: two keys, the
// candidate and its commit, nothing else on stdout.
func TestCandidatePickGitHubOutput(t *testing.T) {
	t.Parallel()

	dir := candidateRepo(t)
	stub := &stubGitHub{statuses: map[string]stubStatus{
		revParse(t, dir, "v0.5.0-rc.1"): {State: "success"},
		revParse(t, dir, "v0.5.0-rc.2"): {State: "failure"},
	}}

	stdout, stderr, err := runSplit(t, stub, "candidate", "pick", "--repo-path", dir, "-o", "github")
	if err != nil {
		t.Fatalf("candidate pick: %v\n%s", err, stderr)
	}

	want := "candidate=v0.5.0-rc.1\ncommit=" + revParse(t, dir, "v0.5.0-rc.1") + "\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestCandidatePickRefusesWithAnEmptyStdout keeps a refusal from leaving a
// half-written answer in $GITHUB_OUTPUT.
func TestCandidatePickRefusesWithAnEmptyStdout(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"github", "json"} {
		stdout, _, err := runSplit(t, &stubGitHub{}, "candidate", "pick", "--repo-path", candidateRepo(t), "-o", format)
		if err == nil {
			t.Errorf("-o %s: want a refusal when nothing passed", format)
		}

		if stdout != "" {
			t.Errorf("-o %s wrote to stdout despite refusing:\n%s", format, stdout)
		}
	}
}

func TestCandidateRecordWritesTheStatus(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{commits: map[string]string{"v0.5.0-rc.1": "abc123"}}

	out, err := runCommand(t, stub, "",
		"candidate", "record", "v0.5.0-rc.1", "--state", "success", "--target-url", "https://example.invalid/run/7")
	if err != nil {
		t.Fatalf("candidate record: %v\n%s", err, out)
	}

	if len(stub.recorded) != 1 {
		t.Fatalf("recorded %d statuses, want 1", len(stub.recorded))
	}

	got := stub.recorded[0]
	for key, want := range map[string]string{
		"sha":         "abc123",
		"state":       "success",
		"context":     gh.CandidateContext,
		"target_url":  "https://example.invalid/run/7",
		"description": "v0.5.0-rc.1 passed its soak",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestCandidateRecordRefuses(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		// A final's soak decides publication, not a candidate status.
		{"candidate", "record", "v0.5.0", "--state", "success"},
		{"candidate", "record", "v0.5.0-rc.1", "--state", "pending"},
		{"candidate", "record", "v0.5.0-rc.1"},
	} {
		stub := &stubGitHub{commits: map[string]string{"v0.5.0": "abc", "v0.5.0-rc.1": "abc"}}

		if _, err := runCommand(t, stub, "", args...); err == nil {
			t.Errorf("%v: want a refusal", args)
		}

		if len(stub.recorded) != 0 {
			t.Errorf("%v recorded a status", args)
		}
	}
}

func TestScheduleEnabled(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]bool{"": true, "on": true, "ON": true, "true": true, "off": false, " Off ": false, "false": false} {
		got, err := scheduleEnabled(value)
		if err != nil || got != want {
			t.Errorf("scheduleEnabled(%q) = %v, %v; want %v", value, got, err, want)
		}
	}

	// A typo must fail loudly rather than silently read as either answer.
	for _, value := range []string{"of", "disabled", "1"} {
		if _, err := scheduleEnabled(value); err == nil {
			t.Errorf("scheduleEnabled(%q) = nil error, want a refusal", value)
		}
	}
}

func TestDecideDue(t *testing.T) {
	t.Parallel()

	soak := gh.Run{Workflow: gh.WorkflowUpgrade, Status: "pending", HeadBranch: "main", URL: "https://example.invalid/run/3"}

	cases := []struct {
		name       string
		tags       []string
		running    []gh.Run
		want       bool
		wantReason string
	}{
		{name: "untagged and idle", want: true, wantReason: "no candidate yet"},
		{name: "already a candidate", tags: []string{"v0.5.0-rc.2"}, wantReason: "already v0.5.0-rc.2"},
		{name: "already a final", tags: []string{"v0.5.0"}, wantReason: "already v0.5.0"},
		{
			// Another kind of tag is not a release of this tree.
			name: "an unrelated tag", tags: []string{"images/host-ubuntu2404/v1"},
			want: true, wantReason: "no candidate yet",
		},
		{name: "a soak still waiting", running: []gh.Run{soak}, wantReason: "release-upgrade.yaml is still pending"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, reason := decideDue("0123456789abcdef", tc.tags, tc.running)
			if got != tc.want || !strings.Contains(reason, tc.wantReason) {
				t.Errorf("decideDue = (%v, %q), want (%v, containing %q)", got, reason, tc.want, tc.wantReason)
			}
		})
	}
}

func TestCandidateDueSkipsATaggedHead(t *testing.T) {
	t.Parallel()

	stdout, stderr, err := runSplit(t, &stubGitHub{}, "candidate", "due", "--repo-path", candidateRepo(t), "-o", "github")
	if err != nil {
		t.Fatalf("candidate due: %v\n%s", err, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || lines[0] != "due=false" || !strings.Contains(lines[1], "v0.5.0-rc.2") {
		t.Errorf("stdout = %q, want due=false naming the tag", stdout)
	}
}

func TestCandidateDueWhenMainHasMoved(t *testing.T) {
	t.Parallel()

	dir := candidateRepo(t)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "merged since")

	stdout, stderr, err := runSplit(t, &stubGitHub{}, "candidate", "due", "--repo-path", dir, "-o", "github")
	if err != nil {
		t.Fatalf("candidate due: %v\n%s", err, stderr)
	}

	if !strings.HasPrefix(stdout, "due=true\n") {
		t.Errorf("stdout = %q, want due=true", stdout)
	}
}

// TestCandidateDueWaitsForARunningSoak is why the check exists. A soak waiting
// on the cluster is pending, and GitHub keeps one pending run per concurrency
// group, so a new candidate's soak would cancel it.
func TestCandidateDueWaitsForARunningSoak(t *testing.T) {
	t.Parallel()

	dir := candidateRepo(t)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "merged since")

	stub := &stubGitHub{runs: map[string][]stubRun{
		"release-upgrade.yaml": {{ID: 3, Status: "pending", HeadBranch: "main", CreatedAt: ago(time.Minute)}},
	}}

	stdout, stderr, err := runSplit(t, stub, "candidate", "due", "--repo-path", dir, "-o", "github")
	if err != nil {
		t.Fatalf("candidate due: %v\n%s", err, stderr)
	}

	if !strings.HasPrefix(stdout, "due=false\n") || !strings.Contains(stdout, "release-upgrade.yaml is still pending") {
		t.Errorf("stdout = %q, want due=false naming the waiting soak", stdout)
	}
}

// TestCandidateDueOffTouchesNothing keeps the off switch cheap and safe: no
// clone is read and no request is made.
func TestCandidateDueOffTouchesNothing(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{}

	stdout, stderr, err := runSplit(t, stub, "candidate", "due", "--schedule", "off",
		"--repo-path", t.TempDir(), "-o", "github")
	if err != nil {
		t.Fatalf("candidate due --schedule off: %v\n%s", err, stderr)
	}

	if !strings.HasPrefix(stdout, "due=false\n") {
		t.Errorf("stdout = %q, want due=false", stdout)
	}

	if got := stub.requested(); len(got) != 0 {
		t.Errorf("made requests with the schedule off: %v", got)
	}
}

// The drafts tests.

func draft(tag string, id int64) gh.Release {
	return gh.Release{ID: id, Tag: tag, Draft: true}
}

func tags(releases []gh.Release) []string {
	out := make([]string, 0, len(releases))
	for _, release := range releases {
		out = append(out, release.Tag)
	}

	slices.Sort(out)

	return out
}

// passedOnly answers passed() from a set, and counts the questions asked.
func passedOnly(passing ...string) (func(gh.Release) (bool, error), *int) {
	asked := 0

	return func(release gh.Release) (bool, error) {
		asked++

		return slices.Contains(passing, release.Tag), nil
	}, &asked
}

func TestPlanPruneForAFinal(t *testing.T) {
	t.Parallel()

	drafts := []gh.Release{
		draft("v0.5.0-rc.1", 1),
		draft("v0.5.0-rc.2", 2),
		draft("v0.6.0-rc.1", 3),
		draft("v0.4.0-rc.9", 4),
		// Not a candidate: a final's own draft is never this command's to remove.
		draft("v0.5.0", 5),
		// Legacy suffixes are not candidates either.
		draft("v0.5.0-beta.1", 6),
	}

	passed, asked := passedOnly()

	remove, _, err := planPrune("v0.5.0", drafts, passed)
	if err != nil {
		t.Fatalf("planPrune: %v", err)
	}

	assertTags(t, remove, []string{"v0.5.0-rc.1", "v0.5.0-rc.2"})

	if *asked != 0 {
		t.Errorf("asked %d soak results for a final, which needs none", *asked)
	}
}

// TestPlanPruneForAPassingCandidate is the ordinary scheduled case. It passed,
// so it is the newest that did, and every older draft goes. A newer one may
// still be building and is left alone.
func TestPlanPruneForAPassingCandidate(t *testing.T) {
	t.Parallel()

	drafts := []gh.Release{
		draft("v0.5.0-rc.1", 1), draft("v0.5.0-rc.3", 3), draft("v0.5.0-rc.4", 4),
		draft("v0.5.0-rc.5", 5), draft("v0.5.0-rc.6", 6),
	}

	passed, asked := passedOnly("v0.5.0-rc.5", "v0.5.0-rc.3")

	remove, keep, err := planPrune("v0.5.0-rc.5", drafts, passed)
	if err != nil {
		t.Fatalf("planPrune: %v", err)
	}

	assertTags(t, remove, []string{"v0.5.0-rc.1", "v0.5.0-rc.3", "v0.5.0-rc.4"})

	if len(keep) != 0 {
		t.Errorf("kept %v; nothing older is worth keeping when this one passed", keep)
	}

	// It passed, so nothing older needed asking about.
	if *asked != 1 {
		t.Errorf("asked %d soak results, want 1", *asked)
	}
}

// TestPlanPruneForAFailingCandidateKeepsTheNewestPassing keeps what promote
// would ship, and the failed candidate itself is not in scope at all, so its
// soak can be re-run once the cause is fixed.
func TestPlanPruneForAFailingCandidateKeepsTheNewestPassing(t *testing.T) {
	t.Parallel()

	drafts := []gh.Release{
		draft("v0.5.0-rc.1", 1), draft("v0.5.0-rc.2", 2), draft("v0.5.0-rc.3", 3),
		draft("v0.5.0-rc.4", 4), draft("v0.5.0-rc.5", 5),
	}

	passed, _ := passedOnly("v0.5.0-rc.3", "v0.5.0-rc.1")

	remove, keep, err := planPrune("v0.5.0-rc.5", drafts, passed)
	if err != nil {
		t.Fatalf("planPrune: %v", err)
	}

	assertTags(t, remove, []string{"v0.5.0-rc.1", "v0.5.0-rc.2", "v0.5.0-rc.4"})

	if len(keep) != 1 || !strings.HasPrefix(keep[0], "v0.5.0-rc.3") {
		t.Errorf("keep = %v, want v0.5.0-rc.3", keep)
	}
}

func TestPlanPruneLeavesEverythingElseAlone(t *testing.T) {
	t.Parallel()

	drafts := []gh.Release{
		draft("v0.4.0-rc.1", 1),
		draft("v0.6.0-rc.1", 2),
		// Published: never in a draft listing in practice, and never removed
		// even if one turns up in it.
		{ID: 3, Tag: "v0.5.0-rc.1"},
	}

	passed, _ := passedOnly()

	remove, _, err := planPrune("v0.5.0-rc.3", drafts, passed)
	if err != nil {
		t.Fatalf("planPrune: %v", err)
	}

	if len(remove) != 0 {
		t.Errorf("removed %v, want nothing", tags(remove))
	}

	if _, _, err := planPrune("images/x/v1", drafts, passed); err == nil {
		t.Error("planPrune(not a release tag) = nil error, want a refusal")
	}
}

func TestDraftsPruneDeletesThroughTheAPI(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{releases: []stubRelease{
		{ID: 11, TagName: "v0.5.0-rc.1", Draft: true},
		{ID: 12, TagName: "v0.5.0-rc.2", Draft: true},
		{ID: 13, TagName: "v0.5.0"},
		{ID: 14, TagName: "v0.4.0-rc.1", Draft: true},
	}}

	out, err := runCommand(t, stub, "", "drafts", "prune", "v0.5.0", "--dry-run")
	if err != nil {
		t.Fatalf("drafts prune --dry-run: %v\n%s", err, out)
	}

	if len(stub.deleted) != 0 {
		t.Fatalf("a dry run deleted %v", stub.deleted)
	}

	if !strings.Contains(out, "would delete the draft of v0.5.0-rc.2") {
		t.Errorf("dry run did not say what it would do:\n%s", out)
	}

	if _, err := runCommand(t, stub, "", "drafts", "prune", "v0.5.0"); err != nil {
		t.Fatalf("drafts prune: %v", err)
	}

	slices.Sort(stub.deleted)

	if !slices.Equal(stub.deleted, []string{"11", "12"}) {
		t.Errorf("deleted %v, want [11 12]", stub.deleted)
	}
}

func TestDraftsDiscard(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{releaseByTag: map[string]stubRelease{
		"v0.5.0-rc.3": {ID: 21, TagName: "v0.5.0-rc.3", Draft: true},
		"v0.5.0-rc.2": {ID: 22, TagName: "v0.5.0-rc.2"},
		"v0.5.0":      {ID: 23, TagName: "v0.5.0", Draft: true},
	}}

	if _, err := runCommand(t, stub, "", "drafts", "discard", "v0.5.0-rc.3"); err != nil {
		t.Fatalf("discard a candidate's draft: %v", err)
	}

	for _, tag := range []string{"v0.5.0-rc.2", "v0.5.0"} {
		if _, err := runCommand(t, stub, "", "drafts", "discard", tag); err == nil {
			t.Errorf("discard %s: want a refusal", tag)
		}
	}

	// A build can fail before its draft exists.
	out, err := runCommand(t, stub, "", "drafts", "discard", "v0.5.0-rc.9")
	if err != nil || !strings.Contains(out, "nothing to discard") {
		t.Errorf("discard a missing release = %v\n%s", err, out)
	}

	if !slices.Equal(stub.deleted, []string{"21"}) {
		t.Errorf("deleted %v, want only the candidate's draft [21]", stub.deleted)
	}
}

// The soak plan tests.

// statusFor answers a soak-result lookup from a map, counting lookups.
type statusFor struct {
	byCommit map[string]*gh.Status
	err      error
	asked    int
}

func (s *statusFor) lookup(_ context.Context, commit string) (*gh.Status, error) {
	s.asked++

	if s.err != nil {
		return nil, s.err
	}

	return s.byCommit[commit], nil
}

// promotedRepo is candidateRepo with v0.5.0 promoted from rc.2.
func promotedRepo(t *testing.T) string {
	t.Helper()

	dir := candidateRepo(t)
	gitIn(t, dir, "tag", "v0.5.0", "v0.5.0-rc.2")

	return dir
}

func TestPlanSoak(t *testing.T) {
	t.Parallel()

	promoted := promotedRepo(t)
	rc2 := revParse(t, promoted, "v0.5.0-rc.2")

	passed := map[string]*gh.Status{rc2: {State: gh.StatusSuccess}}
	failed := map[string]*gh.Status{rc2: {State: gh.StatusFailure}}

	cases := []struct {
		name          string
		dir           string
		tag           string
		event         string
		status        *statusFor
		wantSoak      bool
		wantCandidate string
		wantReason    string
		wantWarning   bool
		wantAsked     int
	}{
		{
			name: "a promoted final whose candidate passed", dir: promoted, tag: "v0.5.0",
			event: eventWorkflowRun, status: &statusFor{byCommit: passed},
			wantCandidate: "v0.5.0-rc.2", wantReason: "passed its soak on the same commit", wantAsked: 1,
		},
		{
			name: "a promoted final whose candidate failed", dir: promoted, tag: "v0.5.0",
			event: eventWorkflowRun, status: &statusFor{byCommit: failed},
			wantSoak: true, wantCandidate: "v0.5.0-rc.2", wantReason: "has not passed its soak (failure)", wantAsked: 1,
		},
		{
			// A forced promote looks like this.
			name: "a promoted final whose candidate has no result", dir: promoted, tag: "v0.5.0",
			event: eventWorkflowRun, status: &statusFor{},
			wantSoak: true, wantCandidate: "v0.5.0-rc.2", wantReason: "no recorded result", wantAsked: 1,
		},
		{
			// Fails toward touching the cluster, never toward skipping it.
			name: "a result that cannot be read", dir: promoted, tag: "v0.5.0",
			event: eventWorkflowRun, status: &statusFor{err: errors.New("502")},
			wantSoak: true, wantCandidate: "v0.5.0-rc.2", wantReason: "could not read", wantWarning: true, wantAsked: 1,
		},
		{
			name: "a soak dispatched by hand always runs", dir: promoted, tag: "v0.5.0",
			event: eventWorkflowDispatch, status: &statusFor{byCommit: passed},
			wantSoak: true, wantReason: "request for a soak",
		},
		{
			name: "a candidate always soaks", dir: promoted, tag: "v0.5.0-rc.2",
			event: eventWorkflowRun, status: &statusFor{byCommit: passed},
			wantSoak: true, wantReason: "a candidate always soaks",
		},
		{
			name: "a final with no candidate on its commit", dir: tagRepo(t), tag: "v0.4.0",
			event: eventWorkflowRun, status: &statusFor{},
			wantSoak: true, wantReason: "nothing has soaked this tree",
		},
		{
			name: "a release-branch release never soaks", dir: releaseBranchRepo(t), tag: "v0.4.1",
			event: eventWorkflowRun, status: &statusFor{},
			wantReason: "release branch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := (&Options{RepoPath: tc.dir}).repo(t.Context())

			plan, warning, err := planSoak(t.Context(), repo, tc.tag, tc.event, tc.status.lookup)
			if err != nil {
				t.Fatalf("planSoak: %v", err)
			}

			if plan.Soak != tc.wantSoak || plan.Candidate != tc.wantCandidate {
				t.Errorf("plan = %+v, want soak=%v candidate=%q", plan, tc.wantSoak, tc.wantCandidate)
			}

			if !strings.Contains(plan.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", plan.Reason, tc.wantReason)
			}

			if (warning != "") != tc.wantWarning {
				t.Errorf("warning = %q, want one: %v", warning, tc.wantWarning)
			}

			// Only a final from main, built by its push, needs a credential.
			if tc.status.asked != tc.wantAsked {
				t.Errorf("looked up %d soak results, want %d", tc.status.asked, tc.wantAsked)
			}
		})
	}
}

func TestPlanSoakRefusesAnUnknownEvent(t *testing.T) {
	t.Parallel()

	repo := (&Options{RepoPath: tagRepo(t)}).repo(t.Context())

	if _, _, err := planSoak(t.Context(), repo, "v0.4.0", "push", (&statusFor{}).lookup); err == nil {
		t.Error("planSoak(push) = nil error, want a refusal")
	}
}

// TestSoakPlanGitHubOutput is release-upgrade's contract: three keys, and the
// reason on one line so it cannot start a key of its own.
func TestSoakPlanGitHubOutput(t *testing.T) {
	t.Parallel()

	dir := promotedRepo(t)
	stub := &stubGitHub{statuses: map[string]stubStatus{
		revParse(t, dir, "v0.5.0-rc.2"): {State: "success"},
	}}

	stdout, stderr, err := runSplit(t, stub, "soak-plan", "v0.5.0", "--repo-path", dir, "-o", "github")
	if err != nil {
		t.Fatalf("soak-plan: %v\n%s", err, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("github output has %d lines, want 3:\n%s", len(lines), stdout)
	}

	for i, want := range []string{"soak=false", "reason=promoted from v0.5.0-rc.2", "candidate=v0.5.0-rc.2"} {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("line %d = %q, want it to start with %q", i+1, lines[i], want)
		}
	}
}

// TestSoakPlanRefusesOffTheDefaultBranch shares classify's precondition, for
// classify's reason: from a release branch, its own tags are reachable, and
// "cut from main" would come back true.
func TestSoakPlanRefusesOffTheDefaultBranch(t *testing.T) {
	t.Parallel()

	dir := releaseBranchRepo(t)
	gitIn(t, dir, "checkout", "-q", "release-0.4")

	stdout, _, err := runSplit(t, &stubGitHub{}, "soak-plan", "v0.4.1", "--repo-path", dir, "-o", "github")
	if err == nil {
		t.Fatalf("soak-plan from release-0.4: want a refusal, got:\n%s", stdout)
	}

	if stdout != "" {
		t.Errorf("refused but wrote:\n%s", stdout)
	}
}

// The watch and status tests.

func candidateBuild(tag, sha string) map[string][]stubRun {
	return map[string][]stubRun{
		"release.yaml": {{
			ID: 1, HeadBranch: tag, HeadSHA: sha, Event: "push",
			Status: "completed", Conclusion: "success", CreatedAt: ago(2 * time.Hour),
		}},
		"release-upgrade.yaml": {{
			ID: 2, HeadBranch: "main", HeadSHA: sha, Event: "workflow_run",
			Status: "completed", Conclusion: "success", CreatedAt: ago(time.Hour),
		}},
	}
}

// TestWatchReportsAPassingCandidate is the exit status for a candidate, which
// never publishes: its recorded soak result is the outcome.
func TestWatchReportsAPassingCandidate(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs:         candidateBuild("v0.5.0-rc.1", "abc"),
		statuses:     map[string]stubStatus{"abc": {State: "success"}},
		releaseByTag: map[string]stubRelease{"v0.5.0-rc.1": {TagName: "v0.5.0-rc.1", Draft: true}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0-rc.1", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	if !strings.Contains(out, "Soak result:") || !strings.Contains(out, "passed") {
		t.Errorf("watch did not report the result:\n%s", out)
	}
}

func TestWatchFailsAFailedCandidate(t *testing.T) {
	t.Parallel()

	stub := &stubGitHub{
		runs:     candidateBuild("v0.5.0-rc.1", "abc"),
		statuses: map[string]stubStatus{"abc": {State: "failure"}},
	}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0-rc.1", "--once")
	if err == nil || !strings.Contains(err.Error(), "did not pass its soak") {
		t.Fatalf("watch = %v, want a failing verdict\n%s", err, out)
	}
}

// TestWatchNamesTheScheduleForAScheduledCut keeps the cron's last editor out of
// the answer. They did nothing this time.
func TestWatchNamesTheScheduleForAScheduledCut(t *testing.T) {
	t.Parallel()

	runs := candidateBuild("v0.5.0-rc.1", "abc")
	runs["release.yaml"][0].CreatedAt = ago(122 * time.Minute)
	runs["release.yaml"][0].Actor = user(deployKeyOwner)
	runs["release-prepare.yaml"] = []stubRun{{
		ID: 9, HeadBranch: "main", Event: "schedule",
		Status: "completed", Conclusion: "success",
		CreatedAt: ago(125 * time.Minute), UpdatedAt: ago(120 * time.Minute),
		Actor: user("cron-editor"), HTMLURL: "https://example.invalid/prepare/9",
	}}

	stub := &stubGitHub{runs: runs, statuses: map[string]stubStatus{"abc": {State: "success"}}}

	out, err := runCommand(t, stub, "", "watch", "v0.5.0-rc.1", "--once")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}

	for _, want := range []string{"Cut by:", "scheduled", "https://example.invalid/prepare/9"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output missing %q:\n%s", want, out)
		}
	}

	for _, wrong := range []string{"cron-editor", deployKeyOwner} {
		if strings.Contains(out, wrong) {
			t.Errorf("watch named %s:\n%s", wrong, out)
		}
	}
}

func TestStatusShowsThePromotableCandidate(t *testing.T) {
	t.Parallel()

	dir := candidateRepo(t)
	stub := &stubGitHub{
		latest: "v0.4.0",
		statuses: map[string]stubStatus{
			revParse(t, dir, "v0.5.0-rc.1"): {State: "success", UpdatedAt: ago(3 * time.Hour)},
			revParse(t, dir, "v0.5.0-rc.2"): {State: "failure", UpdatedAt: ago(time.Hour)},
		},
	}

	out, err := runCommand(t, stub, dir, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	for _, want := range []string{"Next candidate:", "v0.5.0-rc.3", "Promotable:", "v0.5.0-rc.1", "(passed 3h0m0s ago)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusKeepsTheTrainViewWhenTheNextCandidateIsUnknown is why the two are
// resolved separately. Two live trains make the next candidate a question only
// a human can answer, and that must not blank the train view as well.
func TestStatusKeepsTheTrainViewWhenTheNextCandidateIsUnknown(t *testing.T) {
	t.Parallel()

	dir := candidateRepo(t)
	gitIn(t, dir, "tag", "v0.6.0-rc.1")

	out, err := runCommand(t, &stubGitHub{latest: "v0.4.0"}, dir, "status", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}

	var decoded statusResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}

	if decoded.LocalError != "" {
		t.Errorf("LocalError = %q; the train view was known", decoded.LocalError)
	}

	if !slices.Equal(decoded.Live, []string{"v0.5.0", "v0.6.0"}) {
		t.Errorf("Live = %v, want both trains", decoded.Live)
	}

	if decoded.NextError == "" || decoded.NextFromLocal != "" {
		t.Errorf("next = %q (error %q), want it reported as unknown", decoded.NextFromLocal, decoded.NextError)
	}
}

func assertTags(t *testing.T, got []gh.Release, want []string) {
	t.Helper()

	if names := tags(got); !slices.Equal(names, want) {
		t.Errorf("tags = %v, want %v", names, want)
	}
}
