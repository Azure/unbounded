// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockedInputs(t *testing.T) {
	t.Parallel()

	command := exec.Command("python3", "build.py", "check")

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build.py check failed: %v\n%s", err, output)
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode build.py output: %v\n%s", err, output)
	}

	if result.Status != "locked-inputs-verified" {
		t.Fatalf("status = %q, want locked-inputs-verified", result.Status)
	}
}

func TestRunnerDoesNotOwnCertificationScripts(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Containerfile", "build.py", "validate-image.py", "runner-contract.json"} {
		content, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		text := string(content)
		for _, forbidden := range []string{"script-contract.json", "project-signal-certification/scripts"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s contains private package dependency %q", name, forbidden)
			}
		}
	}
}
