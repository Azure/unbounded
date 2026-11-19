// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestImageWorkflowPassesExactGitCommit(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../.github/workflows/images.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var workflow struct {
		Jobs map[string]struct {
			With map[string]string `yaml:"with"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	buildArgs := workflow.Jobs["build"].With["build-args"]
	for _, expected := range []string{
		"VERSION=${{ needs.resolve.outputs.tag }}",
		"GIT_COMMIT=${{ github.sha }}",
	} {
		if !strings.Contains(buildArgs, expected) {
			t.Errorf("image build arguments do not include %q:\n%s", expected, buildArgs)
		}
	}
}
