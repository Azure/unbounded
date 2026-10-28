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

type workflowJob struct {
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Environment any               `yaml:"environment"`
	Steps       []struct {
		ID   string            `yaml:"id"`
		Uses string            `yaml:"uses"`
		Run  string            `yaml:"run"`
		With map[string]string `yaml:"with"`
	} `yaml:"steps"`
}

func TestReleaseUpgradeDeploysLatestDocs(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("../../.github/workflows/docs.yaml"); !os.IsNotExist(err) {
		t.Fatalf("standalone docs workflow must not exist: %v", err)
	}

	data, err := os.ReadFile("../../.github/workflows/release-upgrade.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var wf struct {
		Jobs map[string]workflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}

	docs := wf.Jobs["docs"]
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

	if fmt.Sprint(docs.Permissions) != "map[contents:read id-token:write pages:write]" {
		t.Errorf("docs permissions = %v", docs.Permissions)
	}

	if fmt.Sprint(docs.Environment) != "map[name:github-pages url:${{ steps.deployment.outputs.page_url }}]" {
		t.Errorf("docs environment = %v", docs.Environment)
	}

	var uses []string

	for _, s := range docs.Steps {
		if strings.Contains(s.Run, "gh workflow") {
			t.Errorf("docs must not dispatch another workflow: %q", s.Run)
		}

		if s.Uses == "" {
			continue
		}

		if !strings.HasPrefix(s.Uses, "actions/") {
			t.Errorf("docs uses non-official action %q", s.Uses)
		}

		name, _, _ := strings.Cut(s.Uses, "@")
		uses = append(uses, name)

		switch name {
		case "actions/checkout":
			if s.With["ref"] != "refs/tags/${{ needs.resolve.outputs.tag }}" {
				t.Errorf("checkout must use the release tag, got %q", s.With["ref"])
			}
		case "actions/upload-pages-artifact":
			if s.With["path"] != "docs/public" {
				t.Errorf("pages artifact path = %q", s.With["path"])
			}
		case "actions/deploy-pages":
			if s.ID != "deployment" {
				t.Errorf("deploy-pages id = %q", s.ID)
			}
		}
	}

	if fmt.Sprint(uses) != "[actions/checkout actions/upload-pages-artifact actions/deploy-pages]" {
		t.Errorf("docs actions = %v, want checkout, upload, deploy in order", uses)
	}
}
