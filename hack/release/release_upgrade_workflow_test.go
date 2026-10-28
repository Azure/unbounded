// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublishRequiresResolvedRelease(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../.github/workflows/release-upgrade.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var workflow struct {
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	condition := workflow.Jobs["publish"].If
	if !strings.Contains(condition, "needs.resolve.result == 'success'") {
		t.Fatalf("publish condition can run without a resolved release:\n%s", condition)
	}
}

type workflowStep struct {
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type workflowJob struct {
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

func readWorkflow(t *testing.T, name string) (on map[string]any, jobs map[string]workflowJob) {
	t.Helper()

	data, err := os.ReadFile("../../.github/workflows/" + name)
	if err != nil {
		t.Fatal(err)
	}

	var wf struct {
		On   map[string]any         `yaml:"on"`
		Jobs map[string]workflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}

	return wf.On, wf.Jobs
}

func TestDocsDeploysLatestReleaseOnDispatchOnly(t *testing.T) {
	t.Parallel()

	on, jobs := readWorkflow(t, "docs.yaml")
	if inputs, ok := on["workflow_dispatch"]; len(on) != 1 || !ok || inputs != nil {
		t.Fatalf("docs must trigger only on input-less workflow_dispatch: %v", on)
	}

	build := jobs["build"]
	if build.If != "github.ref == 'refs/heads/main'" {
		t.Fatalf("build must be guarded to main: %q", build.If)
	}

	if len(build.Steps) == 0 || build.Steps[0].ID != "release" ||
		!strings.Contains(build.Steps[0].Run, `releases/latest`) || build.Steps[0].Env["GH_TOKEN"] == "" {
		t.Fatalf("first build step must resolve the latest release: %+v", build.Steps)
	}

	checkouts := 0

	for name, job := range jobs {
		for _, s := range job.Steps {
			if s.Uses != "" && !strings.HasPrefix(s.Uses, "actions/") {
				t.Errorf("job %s uses non-official action %q", name, s.Uses)
			}

			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				checkouts++

				if s.With["ref"] != "refs/tags/${{ steps.release.outputs.tag }}" {
					t.Errorf("checkout must use the latest release tag, got %q", s.With["ref"])
				}
			}
		}
	}

	if checkouts != 1 {
		t.Fatalf("expected exactly one checkout, got %d", checkouts)
	}
}

func TestReleaseUpgradeDispatchesLatestDocs(t *testing.T) {
	t.Parallel()

	_, jobs := readWorkflow(t, "release-upgrade.yaml")
	docs := jobs["docs"]

	if fmt.Sprint(docs.Needs) != "[resolve publish publish-forced]" {
		t.Fatalf("docs needs = %v", docs.Needs)
	}

	for _, want := range []string{
		"needs.resolve.result == 'success'",
		"needs.resolve.outputs.latest == 'true'",
		"needs.publish.result == 'success' || needs.publish-forced.result == 'success'",
	} {
		if !strings.Contains(docs.If, want) {
			t.Errorf("docs condition missing %q:\n%s", want, docs.If)
		}
	}

	if len(docs.Permissions) != 1 || docs.Permissions["actions"] != "write" {
		t.Errorf("docs permissions = %v, want only actions: write", docs.Permissions)
	}

	if len(docs.Steps) != 1 || docs.Steps[0].Uses != "" ||
		strings.TrimSpace(docs.Steps[0].Run) != "gh workflow run docs.yaml --ref main" {
		t.Errorf("docs must only dispatch docs.yaml on main: %+v", docs.Steps)
	}
}
