// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type docsStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type docsJob struct {
	Needs       any               `yaml:"needs"`
	If          string            `yaml:"if"`
	Uses        string            `yaml:"uses"`
	With        map[string]string `yaml:"with"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []docsStep        `yaml:"steps"`
}

type docsWorkflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Permissions map[string]string    `yaml:"permissions"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]docsJob `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, name string) docsWorkflow {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("../../.github/workflows", name))
	if err != nil {
		t.Fatal(err)
	}

	var wf docsWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}

	return wf
}

func TestDocsWorkflowTriggers(t *testing.T) {
	t.Parallel()

	wf := loadWorkflow(t, "docs.yaml")

	if _, ok := wf.On["push"]; ok {
		t.Fatal("docs must not deploy on push")
	}

	var release struct {
		Types []string `yaml:"types"`
	}
	if n, ok := wf.On["release"]; !ok {
		t.Fatal("docs must trigger on release")
	} else if err := n.Decode(&release); err != nil {
		t.Fatal(err)
	}

	if strings.Join(release.Types, ",") != "published" {
		t.Fatalf("release types = %v, want [published]", release.Types)
	}

	if _, ok := wf.On["workflow_call"]; ok {
		t.Fatal("docs must not be a reusable workflow; release-upgrade dispatches it")
	}

	for _, event := range []string{"workflow_dispatch"} {
		var trig struct {
			Inputs map[string]struct {
				Required bool   `yaml:"required"`
				Type     string `yaml:"type"`
			} `yaml:"inputs"`
		}

		n, ok := wf.On[event]
		if !ok {
			t.Fatalf("docs must trigger on %s", event)
		}

		if err := n.Decode(&trig); err != nil {
			t.Fatal(err)
		}

		tag := trig.Inputs["tag"]
		if !tag.Required || tag.Type != "string" {
			t.Fatalf("%s tag input = %+v, want required string", event, tag)
		}
	}

	if wf.Concurrency.Group != "pages" || wf.Concurrency.CancelInProgress {
		t.Fatalf("concurrency = %+v, want pages without cancel", wf.Concurrency)
	}
}

func TestDocsWorkflowPermissionsAndOrdering(t *testing.T) {
	t.Parallel()

	wf := loadWorkflow(t, "docs.yaml")

	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Fatalf("workflow permissions = %v, want contents: read only", wf.Permissions)
	}

	for _, name := range []string{"resolve", "build"} {
		p := wf.Jobs[name].Permissions
		if len(p) != 1 || p["contents"] != "read" {
			t.Fatalf("%s permissions = %v, want contents: read only", name, p)
		}
	}

	deploy := wf.Jobs["deploy"]
	if len(deploy.Permissions) != 2 || deploy.Permissions["pages"] != "write" ||
		deploy.Permissions["id-token"] != "write" {
		t.Fatalf("deploy permissions = %v", deploy.Permissions)
	}

	if deploy.Needs != "build" {
		t.Fatalf("deploy needs = %v, want build", deploy.Needs)
	}

	build := wf.Jobs["build"]
	if build.Needs != "resolve" || build.If != "needs.resolve.outputs.tag != ''" {
		t.Fatalf("build must wait for a validated tag: needs=%v if=%q", build.Needs, build.If)
	}

	checkout := build.Steps[0]
	if !strings.HasPrefix(checkout.Uses, "actions/checkout@") ||
		checkout.With["ref"] != "refs/tags/${{ needs.resolve.outputs.tag }}" {
		t.Fatalf("build first step must check out the validated tag: %+v", checkout)
	}

	if !strings.Contains(build.Steps[1].Run, `"$ACTUAL_SHA" != "$EXPECTED_SHA"`) ||
		build.Steps[1].Env["EXPECTED_SHA"] != "${{ needs.resolve.outputs.sha }}" {
		t.Fatalf("build must verify the validated commit: %+v", build.Steps[1])
	}

	for _, step := range wf.Jobs["resolve"].Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			t.Fatal("resolve must not check anything out before validation")
		}

		if strings.Contains(step.Run, "${{") {
			t.Fatalf("resolve script interpolates expressions; use env vars:\n%s", step.Run)
		}
	}
}

func TestReleaseUpgradeDispatchesDocs(t *testing.T) {
	t.Parallel()

	wf := loadWorkflow(t, "release-upgrade.yaml")

	docs, ok := wf.Jobs["docs"]
	if !ok {
		t.Fatal("release-upgrade must dispatch docs")
	}

	// A reusable call would hold deploy-unbounded-stable while the
	// github-pages environment waits for approval.
	if docs.Uses != "" {
		t.Fatalf("docs must be dispatched, not called: uses %q", docs.Uses)
	}

	needs, _ := docs.Needs.([]any)

	got := []string{}
	for _, n := range needs {
		got = append(got, n.(string))
	}

	if strings.Join(got, ",") != "resolve,publish,publish-forced" {
		t.Fatalf("docs needs = %v", got)
	}

	for _, want := range []string{
		"!cancelled()",
		"needs.resolve.result == 'success'",
		"needs.resolve.outputs.latest == 'true'",
		"(needs.publish.result == 'success' || needs.publish-forced.result == 'success')",
	} {
		if !strings.Contains(docs.If, want) {
			t.Fatalf("docs condition missing %q:\n%s", want, docs.If)
		}
	}

	if len(docs.Permissions) != 1 || docs.Permissions["actions"] != "write" {
		t.Fatalf("docs permissions = %v, want only actions: write", docs.Permissions)
	}

	if len(docs.Steps) != 1 {
		t.Fatalf("docs steps = %d, want 1", len(docs.Steps))
	}

	step := docs.Steps[0]
	if step.Uses != "" {
		t.Fatalf("dispatch step uses %q", step.Uses)
	}

	if step.Env["TAG"] != "${{ needs.resolve.outputs.tag }}" ||
		step.Env["GH_TOKEN"] != "${{ github.token }}" ||
		step.Env["GH_REPO"] != "${{ github.repository }}" {
		t.Fatalf("dispatch env = %v", step.Env)
	}

	if strings.Contains(step.Run, "${{") {
		t.Fatalf("dispatch script interpolates expressions; use env vars:\n%s", step.Run)
	}

	runDispatch(t, step.Run)
}

// runDispatch runs the dispatch script against a fake gh and checks that it
// targets main's docs.yaml with the resolved tag, passed as one argument.
func runDispatch(t *testing.T, script string) {
	t.Helper()

	if err := os.MkdirAll("../../tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := os.MkdirTemp("../../tmp", "docs-dispatch-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	gh := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$GH_LOG\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}

	log := filepath.Join(dir, "gh.log")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	cmd.Env = []string{
		"PATH=" + dir + ":" + os.Getenv("PATH"),
		"GH_LOG=" + log,
		"GH_TOKEN=token",
		"GH_REPO=Azure/unbounded",
		"TAG=v1.4.0 extra",
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dispatch failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	want := "workflow\nrun\ndocs.yaml\n--ref\nmain\n-f\ntag=v1.4.0 extra\n"
	if string(data) != want {
		t.Fatalf("gh args = %q, want %q", data, want)
	}
}

const (
	latestTag = "v1.4.0"
	tagSHA    = "0123456789abcdef0123456789abcdef01234567"
)

// fakeGH mimics `gh api ... --jq .<field>` for releases/latest and tag commits.
// releases/latest never returns drafts or prereleases, so those tags simply
// differ from latestTag.
const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "$GH_LOG"
case "$2" in
  */releases/latest) [ -n "${FAKE_LATEST:-}" ] || exit 1; echo "$FAKE_LATEST" ;;
  */commits/refs/tags/*) [ -n "${FAKE_SHA:-}" ] || exit 1; echo "$FAKE_SHA" ;;
  *) exit 1 ;;
esac
`

func TestDocsResolveScript(t *testing.T) {
	t.Parallel()

	var script string

	for _, step := range loadWorkflow(t, "docs.yaml").Jobs["resolve"].Steps {
		if step.ID == "validate" {
			script = step.Run
		}
	}

	if script == "" {
		t.Fatal("validate step not found")
	}

	cases := []struct {
		name    string
		event   string
		tag     string
		ref     string
		latest  string
		sha     string
		wantErr bool
		wantTag string
	}{
		{name: "published latest", event: "release", tag: latestTag, wantTag: latestTag},
		{name: "release prerelease skipped", event: "release", tag: "v1.5.0-rc.1"},
		{name: "release old stable skipped", event: "release", tag: "v1.3.9"},
		{name: "dispatch latest", event: "workflow_dispatch", tag: latestTag, ref: "refs/heads/main", wantTag: latestTag},
		{name: "call from workflow_run", event: "workflow_run", tag: latestTag, ref: "refs/heads/main", wantTag: latestTag},
		{name: "dispatch off main", event: "workflow_dispatch", tag: latestTag, ref: "refs/heads/feature", wantErr: true},
		{name: "dispatch old stable", event: "workflow_dispatch", tag: "v1.3.9", ref: "refs/heads/main", wantErr: true},
		{name: "dispatch draft or unknown", event: "workflow_dispatch", tag: "v9.9.9", ref: "refs/heads/main", wantErr: true},
		{name: "dispatch prerelease", event: "workflow_dispatch", tag: "v1.4.0-rc.1", ref: "refs/heads/main", wantErr: true},
		{name: "dispatch empty", event: "workflow_dispatch", tag: "", ref: "refs/heads/main", wantErr: true},
		{name: "injection", event: "workflow_dispatch", tag: "v1.4.0\";touch pwned;\"", ref: "refs/heads/main", wantErr: true},
		{name: "newline injection", event: "workflow_dispatch", tag: "v1.4.0\nsha=evil", ref: "refs/heads/main", wantErr: true},
		{name: "no latest release", event: "workflow_dispatch", tag: latestTag, ref: "refs/heads/main", latest: "-", wantErr: true},
		{name: "bad sha", event: "workflow_dispatch", tag: latestTag, ref: "refs/heads/main", sha: "nope", wantErr: true},
		{name: "multiline sha", event: "workflow_dispatch", tag: latestTag, ref: "refs/heads/main", sha: tagSHA + "\ntag=evil", wantErr: true},
		{name: "release latest lookup fails", event: "release", tag: latestTag, latest: "-", wantErr: true},
		{name: "release commit lookup fails", event: "release", tag: latestTag, sha: "-", wantErr: true},
		{name: "release bad sha", event: "release", tag: latestTag, sha: "nope", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := os.MkdirAll("../../tmp", 0o755); err != nil {
				t.Fatal(err)
			}

			dir, err := os.MkdirTemp("../../tmp", "docs-resolve-")
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = os.RemoveAll(dir) })

			dir, err = filepath.Abs(dir)
			if err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeGH), 0o755); err != nil {
				t.Fatal(err)
			}

			latest := tc.latest
			switch latest {
			case "":
				latest = latestTag
			case "-":
				latest = ""
			}

			sha := tc.sha
			switch sha {
			case "":
				sha = tagSHA
			case "-":
				sha = ""
			}

			output := filepath.Join(dir, "output")

			inputTag, eventTag := tc.tag, ""
			if tc.event == "release" {
				inputTag, eventTag = "", tc.tag
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, "bash", "-c", script)
			cmd.Dir = dir
			cmd.Env = []string{
				"PATH=" + dir + ":" + os.Getenv("PATH"),
				"GITHUB_OUTPUT=" + output,
				"GITHUB_REPOSITORY=Azure/unbounded",
				"GH_LOG=" + filepath.Join(dir, "gh.log"),
				"EVENT_NAME=" + tc.event,
				"EVENT_TAG=" + eventTag,
				"INPUT_TAG=" + inputTag,
				"RUN_REF=" + tc.ref,
				"FAKE_LATEST=" + latest,
				"FAKE_SHA=" + sha,
			}

			out, err := cmd.CombinedOutput()
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tc.wantErr, out)
			}

			if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
				t.Fatal("tag was executed as shell")
			}

			got, _ := os.ReadFile(output)

			want := ""
			if tc.wantTag != "" {
				want = "tag=" + tc.wantTag + "\nsha=" + tagSHA + "\n"
			}

			if string(got) != want {
				t.Fatalf("outputs = %q, want %q\n%s", got, want, out)
			}
		})
	}
}
