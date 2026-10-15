// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
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
