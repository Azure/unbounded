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

	content, err := os.ReadFile("../../project-signal-certification-runner/Containerfile")
	require.NoError(t, err)

	containerfile := string(content)
	for _, dependency := range []string{
		"openmpi-bin",
		"openssh-client",
		"openssh-server",
		"dependencies.lock.json",
		"requirements.lock",
		"ubuntu-InRelease.sha256",
		"validate-image.py",
	} {
		assert.Contains(t, containerfile, dependency)
	}

	assert.Contains(t, containerfile, "nvidia/cuda:")
	assert.Contains(t, containerfile, "@sha256:")
	assert.Contains(t, containerfile, "--require-hashes")
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
	assert.Contains(t, manifest, "mpi-payload-mode")
	assert.Contains(t, manifest, "mpi-launcher")
	assert.Contains(t, manifest, "mpi-worker")
}

func TestWorkflowPublishesRecoveryReceiptGate(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("04-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)

	manifest := string(content)
	assert.Contains(t, manifest, `: > /results/recovery-receipt-configmap`)
	assert.Contains(t, manifest, `path: /results/recovery-receipt-configmap`)
	assert.Contains(t, manifest, "tasks.node-health.outputs.parameters.recovery-receipt-configmap")
}
