// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunnerProvidesWorkflowDependencies(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("testdata/legacy/images/project-signal-runner/Containerfile")
	require.NoError(t, err)

	containerfile := string(content)
	for _, dependency := range []string{
		"openmpi-bin",
		"openssh-client",
		"openssh-server",
		"PasswordAuthentication no",
		"PermitRootLogin prohibit-password",
	} {
		assert.Contains(t, containerfile, dependency)
	}

	assert.Contains(t, containerfile, "nvidia/cuda:")
	assert.Contains(t, containerfile, "@sha256:")
}

func TestWorkflowConfiguresEphemeralMPIHostTrust(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("04-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)

	manifest := string(content)
	for _, setting := range []string{
		"StrictHostKeyChecking=accept-new",
		"UserKnownHostsFile=/tmp/project-signal-known-hosts",
		"install -d -m 0700 /run/project-signal-ssh",
		"install -m 0600 /root/.ssh/authorized_keys /run/project-signal-ssh/authorized_keys",
		"ssh-keygen -A",
		"AuthorizedKeysFile=/run/project-signal-ssh/authorized_keys",
	} {
		assert.True(t, strings.Contains(manifest, setting), "missing MPI SSH setting %q", setting)
	}

	assert.NotContains(t, manifest, "StrictHostKeyChecking=no")
	assert.NotContains(t, manifest, "StrictModes=no")
}

func TestFullNodeWorkflowGatesEightGPUAllocationOnInventoryPreflight(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("08-full-node-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)

	manifest := string(content)
	for _, setting := range []string{
		"project-signal-gpu-full-node-certification",
		`name: observed-gpu-capacity`,
		`name: observed-gpu-allocatable`,
		`when: "{{` + "`{{tasks.node-health.outputs.parameters.verdict}}`" + `}} == Pass"`,
		`when: "{{` + "`{{tasks.node-health.outputs.parameters.verdict}}`" + `}} != Pass"`,
		`depends: gpu-health.Succeeded`,
		`(mpi-backend.Succeeded || mpi-backend.Failed || mpi-backend.Errored || mpi-backend.Skipped || mpi-backend.Omitted)`,
		`"nvidia.com/gpu":"{{` + "`{{inputs.parameters.gpu-count}}`" + `}}"`,
		`nvidia.com/gpu: "{{` + "`{{inputs.parameters.gpu-count}}`" + `}}"`,
	} {
		assert.Contains(t, manifest, setting)
	}
}

func TestDappleFullNodeProfileUsesPinnedRunnerAndSingleAttempt(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("testdata/legacy/profiles/00-b200-full-node.yaml")
	require.NoError(t, err)

	manifest := string(content)
	assert.Contains(t, manifest, "name: dapple-b200-full-node")
	assert.Contains(t, manifest, "purpose: FullCertification")
	assert.Contains(t, manifest, `expected-gpu-count: "8"`)
	assert.Contains(t, manifest, "maxAttempts: 1")
	assert.Contains(t, manifest, "project-signal-runner@sha256:")
	assert.NotContains(t, manifest, "REPLACE_WITH")
}

func TestBurnCanaryUsesPinnedRunnerAndDiagnosticProjection(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile("09-burn-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)

	for _, setting := range []string{
		"project-signal-gpu-burn-canary",
		`"hostPID":true`,
		`privileged: true`,
		`"nvidia.com/gpu":"{{` + "`{{inputs.parameters.expected-gpu-count}}`" + `}}"`,
		`name: burn-duration-seconds`,
		`name: power-limit-tolerance-percent`,
		`name: power-limit-consecutive-samples`,
	} {
		assert.Contains(t, string(workflow), setting)
	}

	profiles, err := os.ReadFile("testdata/legacy/profiles/01-b200-burn-canary.yaml")
	require.NoError(t, err)

	manifest := string(profiles)
	for _, setting := range []string{
		"name: dapple-b200-burn-canary-8gpu",
		"name: dapple-b200-burn-canary-7gpu-diagnostic",
		"purpose: Diagnostic",
		`expected-gpu-count: "8"`,
		`expected-gpu-count: "7"`,
		`burn-duration-seconds: "21600"`,
		`power-limit-tolerance-percent: "5"`,
		`power-limit-consecutive-samples: "3"`,
		"deadline: 6h30m",
		"project-signal-burn-runner",
	} {
		assert.Contains(t, manifest, setting)
	}
}

func TestBurnRunnerPinsCUDAAndGPUBurn(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("testdata/legacy/images/project-signal-burn-runner/Containerfile")
	require.NoError(t, err)

	containerfile := string(content)
	assert.Contains(t, containerfile, "nvidia/cuda:12.8.1-devel-ubuntu24.04@sha256:")
	assert.Contains(t, containerfile, "nvidia/cuda:12.8.1-runtime-ubuntu24.04@sha256:")
	assert.Contains(t, containerfile, "0d19c94dcb2e9b6858b7586a4c327674cd67113b")
	assert.Contains(t, containerfile, "make -C /src/gpu-burn COMPUTE=100")
	assert.Contains(t, containerfile, "compare.fatbin /usr/local/bin/compare.fatbin")
	assert.Contains(t, containerfile, "COPY --from=builder /src/gpu-burn/LICENSE /usr/share/licenses/gpu-burn/LICENSE")
	assert.Contains(t, containerfile, "WORKDIR /usr/local/bin")

	makefile, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)
	assert.NotContains(t, string(makefile), "SIGNAL_DAPPLE_")
	assert.NotContains(t, string(makefile), "signal-dapple-manifests")
	assert.NotContains(t, string(makefile), "image-project-signal-runner-local")
	assert.NotContains(t, string(makefile), "project-signal-runner-oci-push")
	assert.Contains(t, string(makefile), "signal-example-manifests:")
	assert.Contains(t, string(makefile), "SIGNAL_RUNNER_IMAGE ?= example.invalid/gpu-runner:replace-me")
}
