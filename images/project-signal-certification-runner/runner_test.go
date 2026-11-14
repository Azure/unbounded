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

func TestOfflineValidationUsesExecutableScratch(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Clean("build.py"))
	if err != nil {
		t.Fatalf("read build.py: %v", err)
	}

	const executableTmpfs = `"/tmp:rw,exec,nosuid,nodev,size=64m"`
	if !strings.Contains(string(content), executableTmpfs) {
		t.Fatalf("build.py must mount executable scratch for compiled loader probes")
	}
}

func TestRuntimeImageUsesFixedNonRootIdentity(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile(filepath.Clean("Containerfile"))
	if err != nil {
		t.Fatalf("read Containerfile: %v", err)
	}

	text := string(content)
	userIndex := strings.Index(text, "\nUSER 65532:65532\n")
	rootCheckIndex := strings.Index(text, `["root_authorized_keys_check"]()`)
	runtimeCheckIndex := strings.LastIndex(text, "/opt/certification-image/validate-image.py")
	if userIndex < 0 {
		t.Fatal("Containerfile must configure the fixed certification runtime identity")
	}
	if rootCheckIndex < 0 || rootCheckIndex > userIndex {
		t.Fatal("Containerfile must check root SSH authorization before dropping privileges")
	}
	if runtimeCheckIndex < userIndex {
		t.Fatal("Containerfile must run the full validator as the fixed runtime identity")
	}
}
