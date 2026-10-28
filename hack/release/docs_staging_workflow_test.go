// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type docsStagingStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type docsStagingWorkflow struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
		} `yaml:"push"`
	} `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		If          string            `yaml:"if"`
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Steps       []docsStagingStep `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadDocsStagingWorkflow(t *testing.T) docsStagingWorkflow {
	t.Helper()

	data, err := os.ReadFile("../../.github/workflows/docs-staging.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var wf docsStagingWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}

	if _, ok := wf.Jobs["deploy"]; !ok {
		t.Fatal("docs-staging workflow has no deploy job")
	}

	return wf
}

func docsStagingStepNamed(t *testing.T, wf docsStagingWorkflow, name string) docsStagingStep {
	t.Helper()

	for _, s := range wf.Jobs["deploy"].Steps {
		if s.Name == name {
			return s
		}
	}

	t.Fatalf("deploy job has no step %q", name)

	return docsStagingStep{}
}

func TestDocsStagingIsMainOnlyAndOptional(t *testing.T) {
	t.Parallel()

	wf := loadDocsStagingWorkflow(t)

	if got := strings.Join(wf.On.Push.Branches, ","); got != "main" {
		t.Fatalf("push branches = %q, want only main", got)
	}

	cond := wf.Jobs["deploy"].If
	for _, want := range []string{"github.ref == 'refs/heads/main'", "vars.DOCS_STAGING_REPOSITORY != ''"} {
		if !strings.Contains(cond, want) {
			t.Fatalf("deploy condition missing %q:\n%s", want, cond)
		}
	}
}

func TestDocsStagingCannotPublishToProductionPages(t *testing.T) {
	t.Parallel()

	wf := loadDocsStagingWorkflow(t)
	job := wf.Jobs["deploy"]

	for _, perms := range []map[string]string{wf.Permissions, job.Permissions} {
		if perms["pages"] != "" || perms["id-token"] != "" {
			t.Fatalf("staging workflow must not request pages/id-token permissions: %v", perms)
		}

		if p := perms["contents"]; p != "" && p != "read" {
			t.Fatalf("contents permission = %q, want read", p)
		}
	}

	var publish *docsStagingStep

	for i, s := range job.Steps {
		if strings.Contains(s.Uses, "deploy-pages") || strings.Contains(s.Uses, "upload-pages-artifact") {
			t.Fatalf("staging workflow uses production Pages action %q", s.Uses)
		}

		if strings.HasPrefix(s.Uses, "peaceiris/actions-gh-pages@") {
			publish = &job.Steps[i]
		}
	}

	if publish == nil {
		t.Fatal("no actions-gh-pages publish step")
	}

	if publish.With["external_repository"] != "${{ vars.DOCS_STAGING_REPOSITORY }}" {
		t.Fatalf("publish must target the staging repository, got %q", publish.With["external_repository"])
	}

	if publish.With["deploy_key"] != "${{ secrets.DOCS_STAGING_DEPLOY_KEY }}" {
		t.Fatalf("publish must use the staging deploy key, got %q", publish.With["deploy_key"])
	}

	if publish.With["github_token"] != "" || publish.With["personal_token"] != "" {
		t.Fatal("publish must not use the workflow token or a personal token")
	}
}

func TestDocsStagingBuildsWithProjectBaseURLAndNoindex(t *testing.T) {
	t.Parallel()

	wf := loadDocsStagingWorkflow(t)

	if got := wf.Jobs["deploy"].Env["STAGING_BASE_URL"]; got != "${{ vars.DOCS_STAGING_BASE_URL }}" {
		t.Fatalf("STAGING_BASE_URL = %q", got)
	}

	build := docsStagingStepNamed(t, wf, "Build staging docs")
	if !strings.Contains(build.Run, `--baseURL "$STAGING_BASE_URL"`) {
		t.Fatalf("build does not use the staging base URL:\n%s", build.Run)
	}

	if build.Env["HUGO_PARAMS_NOINDEX"] != "true" {
		t.Fatalf("HUGO_PARAMS_NOINDEX = %q, want true", build.Env["HUGO_PARAMS_NOINDEX"])
	}
}

// runDocsStagingStep runs a step's bash script with only the given env, so no
// real secret or remote is ever reachable.
func runDocsStagingStep(t *testing.T, script string, env map[string]string) (string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}

	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}

	if exit, ok := err.(*exec.ExitError); ok && ctx.Err() == nil {
		return string(out), exit.ExitCode()
	}

	t.Fatalf("running step: %v\n%s", err, out)

	return "", -1
}

func TestDocsStagingValidatesSettings(t *testing.T) {
	t.Parallel()
	requireBash4(t)

	script := docsStagingStepNamed(t, loadDocsStagingWorkflow(t), "Validate staging settings").Run

	const (
		repo = "example/unbounded-staging"
		url  = "https://example.github.io/unbounded-staging/"
	)

	for _, tc := range []struct {
		name, repo, url, wantErr string
	}{
		{name: "project URL", repo: repo, url: url},
		{name: "root URL", repo: repo, url: "https://staging.example.com/"},
		{name: "no repository", repo: "", url: url, wantErr: "DOCS_STAGING_REPOSITORY"},
		{name: "same repository", repo: "Microsoft/Unbounded", url: url, wantErr: "DOCS_STAGING_REPOSITORY"},
		{name: "malformed repository", repo: "example/x/y", url: url, wantErr: "DOCS_STAGING_REPOSITORY"},
		{name: "no URL", repo: repo, url: "", wantErr: "DOCS_STAGING_BASE_URL"},
		{name: "HTTP URL", repo: repo, url: "http://example.github.io/unbounded-staging/", wantErr: "DOCS_STAGING_BASE_URL"},
		{name: "missing slash", repo: repo, url: "https://example.github.io/unbounded-staging", wantErr: "DOCS_STAGING_BASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, code := runDocsStagingStep(t, script, map[string]string{
				"GITHUB_REPOSITORY":  "microsoft/unbounded",
				"STAGING_REPOSITORY": tc.repo,
				"STAGING_BASE_URL":   tc.url,
			})
			if tc.wantErr == "" {
				if code != 0 {
					t.Fatalf("exit %d, want 0:\n%s", code, out)
				}

				return
			}

			if code == 0 || !strings.Contains(out, "::error::"+tc.wantErr) {
				t.Fatalf("exit %d, want error about %s:\n%s", code, tc.wantErr, out)
			}
		})
	}
}

func TestDocsStagingRequiresDeployKey(t *testing.T) {
	t.Parallel()
	requireBash4(t)

	script := docsStagingStepNamed(t, loadDocsStagingWorkflow(t), "Require staging deploy key").Run

	out, code := runDocsStagingStep(t, script, map[string]string{"DEPLOY_KEY": "placeholder-not-a-key"})
	if code != 0 || strings.Contains(out, "placeholder") {
		t.Fatalf("present key: exit %d, output %q", code, out)
	}

	out, code = runDocsStagingStep(t, script, nil)
	if code == 0 || !strings.Contains(out, "::error::DOCS_STAGING_DEPLOY_KEY") {
		t.Fatalf("absent key: exit %d:\n%s", code, out)
	}
}
