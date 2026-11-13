// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionAlwaysReconcilesBootstrap(t *testing.T) {
	repository, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct{ name, revision, streaming, pool string }{
		{name: "new source revision", revision: "new-revision", streaming: "false"},
		{name: "streaming enabled", revision: "old-revision", streaming: "true", pool: "streaming"},
		{name: "new node pool", revision: "old-revision", streaming: "true", pool: "new-pool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()

			bin := filepath.Join(root, "bin")
			for _, directory := range []string{bin, filepath.Join(root, "tmp"), filepath.Join(root, "hack/gantry-benchmark")} {
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			write := func(path, body string) {
				t.Helper()

				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "tmp/gantry-benchmark-operator-key.pub"), "test-public-key")
			write(filepath.Join(root, "hack/gantry-benchmark/operator-vm-bootstrap.sh"), "# test bootstrap\n")
			write(filepath.Join(bin, "git"), "#!/bin/sh\nprintf '%s\\n' \"$TEST_REPO_ROOT\"\n")
			write(filepath.Join(bin, "az"), `#!/bin/sh
case "$*" in
  *"vm run-command invoke"*)
    printf '%s\n' "$@" >> "$TEST_BOOTSTRAP_LOG"
    case "$*" in
      *"--scripts @"*) printf '{}\n' ;;
      *) printf 'gantry-benchmark-bootstrap-ready\n' ;;
    esac
    ;;
  *"diskSizeGB"*) printf '512\n' ;;
  *"storageProfile.dataDisks"*) printf '/disks/gantry-benchmark-operator-build\n' ;;
  *"role assignment list"*) printf '1\n' ;;
  *) printf 'test-id\n' ;;
esac
`)
			log := filepath.Join(root, "bootstrap.log")
			command := exec.CommandContext(t.Context(), "bash", filepath.Join(repository, "hack/gantry-benchmark/operator-vm-provision.sh"))

			command.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TEST_REPO_ROOT="+root, "TEST_BOOTSTRAP_LOG="+log,
				"AZURE_SUBSCRIPTION_ID=sub", "AZURE_RESOURCE_GROUP=rg", "AZURE_AKS_CLUSTER_NAME=cluster",
				"BASELINE_ACR_NAME=baseline", "GANTRY_ACR_NAME=gantry", "AZURE_LOG_ANALYTICS_WORKSPACE_NAME=workspace",
				"AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID=baseline-pe", "AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID=gantry-pe",
				"OPERATOR_VNET_RESOURCE_GROUP=vnet-rg", "OPERATOR_VNET_NAME=vnet", "OPERATOR_VM_NAME=gantry-benchmark-operator",
				"BENCHMARK_SOURCE_REVISION="+test.revision, "BENCHMARK_ARTIFACT_STREAMING="+test.streaming,
				"BENCHMARK_NODE_POOL="+test.pool, "START_BENCHMARK=false",
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("provision: %v\n%s", err, output)
			}

			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}

			for _, want := range []string{"@" + filepath.Join(root, "hack/gantry-benchmark/operator-vm-bootstrap.sh"), test.revision, test.streaming} {
				if !strings.Contains(string(data), want+"\n") {
					t.Fatalf("bootstrap omitted %q:\n%s", want, data)
				}
			}

			if test.pool != "" && !strings.Contains(string(data), test.pool+"\n") {
				t.Fatalf("bootstrap omitted pool %q:\n%s", test.pool, data)
			}

			if strings.Contains(string(data), "gantry-benchmark-bootstrap-ready") {
				t.Fatal("provision used the stale dimension-only probe")
			}
		})
	}
}
