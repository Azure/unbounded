// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Tests for the candidate model in the release workflows.
//
// The logic that decides anything lives in relctl, where it is tested properly.
// What these pin is the wiring that relctl cannot see: which job runs when,
// with which permissions, and in what order the steps that decide and the
// steps that act come. Getting that wrong is silent until a release is cut.

type candidateStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type candidateJob struct {
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Environment any               `yaml:"environment"`
	Env         map[string]string `yaml:"env"`
	Steps       []candidateStep   `yaml:"steps"`
}

type candidateWorkflow struct {
	On struct {
		Schedule         []map[string]string `yaml:"schedule"`
		WorkflowDispatch struct {
			Inputs map[string]struct {
				Type    string   `yaml:"type"`
				Default any      `yaml:"default"`
				Options []string `yaml:"options"`
			} `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency struct {
		Group string `yaml:"group"`
	} `yaml:"concurrency"`
	Jobs map[string]candidateJob `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, name string) candidateWorkflow {
	t.Helper()

	data, err := os.ReadFile("../../.github/workflows/" + name)
	if err != nil {
		t.Fatal(err)
	}

	var wf candidateWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}

	return wf
}

// stepIndex finds a step by a fragment of its run script, or -1.
func stepIndex(job candidateJob, fragment string) int {
	return slices.IndexFunc(job.Steps, func(s candidateStep) bool { return strings.Contains(s.Run, fragment) })
}

func requireAll(t *testing.T, what, text string, fragments ...string) {
	t.Helper()

	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Errorf("%s is missing %q:\n%s", what, fragment, text)
		}
	}
}

// The release-prepare tests.

// TestReleasePrepareRetiresReleaseMode pins the rule the model rests on: the
// workflow can cut a candidate or promote one, and nothing else.
func TestReleasePrepareRetiresReleaseMode(t *testing.T) {
	t.Parallel()

	mode := loadWorkflow(t, "release-prepare.yaml").On.WorkflowDispatch.Inputs["mode"]

	if !slices.Equal(mode.Options, []string{"prerelease", "promote"}) {
		t.Errorf("mode options = %v, want [prerelease promote]", mode.Options)
	}

	if mode.Default != "prerelease" {
		t.Errorf("mode default = %v, want prerelease", mode.Default)
	}
}

// TestReleasePrepareScheduleIsEasternTime keeps the cadence where it was agreed:
// around 8am, 4pm and midnight US Eastern, off the top of the hour, and in a
// named zone so daylight saving does not move it.
func TestReleasePrepareScheduleIsEasternTime(t *testing.T) {
	t.Parallel()

	schedule := loadWorkflow(t, "release-prepare.yaml").On.Schedule

	if len(schedule) != 1 {
		t.Fatalf("schedule has %d entries, want 1: %v", len(schedule), schedule)
	}

	if schedule[0]["cron"] != "15 0,8,16 * * *" {
		t.Errorf("cron = %q, want 15 0,8,16 * * *", schedule[0]["cron"])
	}

	if schedule[0]["timezone"] != "America/New_York" {
		t.Errorf("timezone = %q, want America/New_York", schedule[0]["timezone"])
	}
}

// TestReleasePrepareStaysReadOnly keeps the tag push on the deploy key. A
// contents:write token here would be a second, ungated way to push tags.
func TestReleasePrepareStaysReadOnly(t *testing.T) {
	t.Parallel()

	wf := loadWorkflow(t, "release-prepare.yaml")

	for scope, level := range wf.Permissions {
		if level != "read" {
			t.Errorf("permission %s: %s, want read", scope, level)
		}
	}

	for _, scope := range []string{"contents", "actions", "statuses"} {
		if wf.Permissions[scope] != "read" {
			t.Errorf("permission %s = %q, want read", scope, wf.Permissions[scope])
		}
	}
}

// TestReleasePrepareGatesEveryCut is the ordering that makes the schedule
// safe: the due check runs on the schedule only, and nothing that can push a
// tag runs once it has said no.
func TestReleasePrepareGatesEveryCut(t *testing.T) {
	t.Parallel()

	job := loadWorkflow(t, "release-prepare.yaml").Jobs["tag"]

	due := stepIndex(job, "candidate due")
	if due < 0 {
		t.Fatal("no step asks whether a candidate is due")
	}

	if job.Steps[due].If != "github.event_name == 'schedule'" {
		t.Errorf("due check runs when %q, want the schedule only", job.Steps[due].If)
	}

	if job.Steps[due].Env["RC_SCHEDULE"] != "${{ vars.RC_SCHEDULE }}" {
		t.Errorf("due check does not read the RC_SCHEDULE variable: %v", job.Steps[due].Env)
	}

	for i, step := range job.Steps[due+1:] {
		if !strings.Contains(step.If, "steps.due.outputs.due != 'false'") {
			t.Errorf("step %d (%s) after the due check is not gated on it: if=%q", due+1+i, step.Name, step.If)
		}
	}

	pick := stepIndex(job, "candidate pick")
	compute := stepIndex(job, "next --branch")
	push := stepIndex(job, "git push origin")

	if due >= pick || pick >= compute || compute >= push {
		t.Errorf("steps out of order: due=%d pick=%d compute=%d push=%d", due, pick, compute, push)
	}

	requireAll(t, "the pick step's condition", job.Steps[pick].If, "env.MODE == 'promote'", "env.FORCE != 'true'")
	requireAll(t, "the compute step's candidate", job.Steps[compute].Env["CANDIDATE"], "steps.pick.outputs.candidate")
}

// TestReleasePrepareDefaultsForTheSchedule covers the inputs a scheduled run
// does not have. Dispatch defaults do not apply to the schedule, so every one
// the job reads must carry its own.
func TestReleasePrepareDefaultsForTheSchedule(t *testing.T) {
	t.Parallel()

	env := loadWorkflow(t, "release-prepare.yaml").Jobs["tag"].Env

	for key, want := range map[string]string{
		"MODE":   "inputs.mode || 'prerelease'",
		"BRANCH": "inputs.branch || 'main'",
		"MAJOR":  "inputs.major == true || inputs.major == 'true'",
		"FORCE":  "inputs.force == true || inputs.force == 'true'",
	} {
		if !strings.Contains(env[key], want) {
			t.Errorf("env %s = %q, want it to contain %q", key, env[key], want)
		}
	}
}

// TestReleasePrepareForcedPromoteNeedsApproval keeps break glass behind the
// reviewers' environment, and keeps a refused approval from falling through
// to an ordinary run.
func TestReleasePrepareForcedPromoteNeedsApproval(t *testing.T) {
	t.Parallel()

	wf := loadWorkflow(t, "release-prepare.yaml")
	gate := wf.Jobs["approve-force"]

	if fmt.Sprint(gate.Environment) != "unbounded-reviewers" {
		t.Errorf("approve-force environment = %v, want unbounded-reviewers", gate.Environment)
	}

	requireAll(t, "approve-force's condition", gate.If, "inputs.force == true", "inputs.force == 'true'")

	tag := wf.Jobs["tag"]

	if fmt.Sprint(tag.Needs) != "approve-force" {
		t.Errorf("tag needs = %v, want approve-force", tag.Needs)
	}

	requireAll(t, "tag's condition", tag.If,
		"!cancelled()",
		"needs.approve-force.result == 'success'",
		"needs.approve-force.result == 'skipped'",
		"inputs.force != true",
		"inputs.force != 'true'",
	)
}

// The release-upgrade tests.

// TestReleaseUpgradeSoaksOnThePlan replaces from_main with the soak plan in
// every gate, so a promoted final can skip its soak the way a release-branch
// cut does.
func TestReleaseUpgradeSoaksOnThePlan(t *testing.T) {
	t.Parallel()

	wf := loadWorkflow(t, "release-upgrade.yaml")

	resolve := wf.Jobs["resolve"]
	if resolve.Permissions["statuses"] != "read" {
		t.Errorf("resolve permissions = %v, want statuses: read", resolve.Permissions)
	}

	if stepIndex(resolve, "relctl soak-plan") < 0 {
		t.Error("resolve does not plan the soak")
	}

	for _, name := range []string{"deploy", "deploy-orca", "smoke-discover", "smoke-tests"} {
		job := wf.Jobs[name]

		if !strings.Contains(job.If, "needs.resolve.outputs.soak == 'true'") {
			t.Errorf("%s runs when %q, want it gated on the soak plan", name, job.If)
		}

		if strings.Contains(job.If, "from_main") {
			t.Errorf("%s still gates on from_main: %q", name, job.If)
		}
	}

	requireAll(t, "publish's condition", wf.Jobs["publish"].If, "needs.resolve.outputs.soak == 'false'")
}

// TestReleaseUpgradeNeverPublishesACandidate is the release-page property:
// candidates pass or fail, and stay drafts either way.
func TestReleaseUpgradeNeverPublishesACandidate(t *testing.T) {
	t.Parallel()

	wf := loadWorkflow(t, "release-upgrade.yaml")

	publish := wf.Jobs["publish"]

	step := stepIndex(publish, "--draft=false")
	if step < 0 {
		t.Fatal("publish has no publishing step")
	}

	if publish.Steps[step].If != "needs.resolve.outputs.prerelease != 'true'" {
		t.Errorf("publishing step runs when %q, want finals only", publish.Steps[step].If)
	}

	forced := wf.Jobs["publish-forced"]

	refuse := slices.IndexFunc(forced.Steps, func(s candidateStep) bool { return s.Name == "Refuse a candidate" })
	publishing := stepIndex(forced, "--draft=false")

	if refuse < 0 || publishing < 0 || refuse > publishing {
		t.Errorf("publish-forced must refuse a candidate before publishing: refuse=%d publish=%d", refuse, publishing)
	}
}

// TestReleaseUpgradeRecordsEveryCandidate keeps a failure recorded as one
// rather than left looking like "not yet", and keeps the record ahead of the
// prune that reads it.
func TestReleaseUpgradeRecordsEveryCandidate(t *testing.T) {
	t.Parallel()

	job := loadWorkflow(t, "release-upgrade.yaml").Jobs["record-candidate"]

	requireAll(t, "record-candidate's condition", job.If,
		"!cancelled()",
		"needs.resolve.result == 'success'",
		"needs.resolve.outputs.prerelease == 'true'",
		"inputs.force_publish != true",
		"inputs.force_publish != 'true'",
	)

	if strings.Contains(job.If, "needs.publish.result") {
		t.Errorf("record-candidate only runs for some publish results; it must record failures too: %q", job.If)
	}

	if job.Permissions["statuses"] != "write" || job.Permissions["contents"] != "write" {
		t.Errorf("record-candidate permissions = %v, want statuses and contents write", job.Permissions)
	}

	record := stepIndex(job, "candidate record")
	prune := stepIndex(job, "drafts prune")

	if record < 0 || prune < 0 || record > prune {
		t.Errorf("want the result recorded before drafts are pruned: record=%d prune=%d", record, prune)
	}

	requireAll(t, "the record step", job.Steps[record].Run, `PUBLISH_RESULT" == "success"`, "state=failure")
}

func TestReleaseUpgradePrunesAfterAFinalPublishes(t *testing.T) {
	t.Parallel()

	job := loadWorkflow(t, "release-upgrade.yaml").Jobs["prune-drafts"]

	requireAll(t, "prune-drafts' condition", job.If,
		"needs.resolve.outputs.prerelease != 'true'",
		"needs.publish.result == 'success' || needs.publish-forced.result == 'success'",
	)

	if stepIndex(job, "drafts prune") < 0 {
		t.Error("prune-drafts does not prune")
	}
}

// TestReleaseUpgradeDiscardsOnlyFailedCandidateBuilds keeps a failed final's
// draft, which its re-run needs.
func TestReleaseUpgradeDiscardsOnlyFailedCandidateBuilds(t *testing.T) {
	t.Parallel()

	job := loadWorkflow(t, "release-upgrade.yaml").Jobs["discard-failed-candidate"]

	requireAll(t, "discard-failed-candidate's condition", job.If,
		"github.event_name == 'workflow_run'",
		"github.event.workflow_run.event == 'push'",
		`"failure", "cancelled", "timed_out"`,
		"contains(github.event.workflow_run.head_branch, '-rc.')",
	)

	if job.Env["TAG"] != "${{ github.event.workflow_run.head_branch }}" {
		t.Errorf("TAG = %q; the tag must reach the script as data", job.Env["TAG"])
	}

	if stepIndex(job, `drafts discard "$TAG"`) < 0 {
		t.Error("discard-failed-candidate does not discard")
	}
}

// TestReleaseUpgradeKeepsFailedBuildsOutOfTheDeployGroup is the cancellation
// it prevents: GitHub keeps one pending run per concurrency group, so a run
// for a failed build waiting in the deploy group would cancel a queued soak.
func TestReleaseUpgradeKeepsFailedBuildsOutOfTheDeployGroup(t *testing.T) {
	t.Parallel()

	group := loadWorkflow(t, "release-upgrade.yaml").Concurrency.Group

	requireAll(t, "the concurrency group", group,
		"github.event.workflow_run.conclusion != 'success'",
		"github.run_id",
		"'deploy-unbounded-stable'",
	)
}
