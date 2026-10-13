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

	content, err := os.ReadFile("../../images/project-signal-runner/Containerfile")
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
		`"nvidia.com/gpu":"{{` + "`{{inputs.parameters.gpu-count}}`" + `}}"`,
		`nvidia.com/gpu: "{{` + "`{{workflow.parameters.expected-gpu-count}}`" + `}}"`,
	} {
		assert.Contains(t, manifest, setting)
	}
}

func TestDappleFullNodeProfileUsesPinnedRunnerAndSingleAttempt(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("../signal-dapple/00-b200-full-node.yaml.tmpl")
	require.NoError(t, err)

	manifest := string(content)
	assert.Contains(t, manifest, "name: dapple-b200-full-node")
	assert.Contains(t, manifest, "purpose: FullCertification")
	assert.Contains(t, manifest, `expected-gpu-count: "8"`)
	assert.Contains(t, manifest, "maxAttempts: 1")
	assert.Contains(t, manifest, "project-signal-runner@sha256:")
	assert.NotContains(t, manifest, "REPLACE_WITH")
}
