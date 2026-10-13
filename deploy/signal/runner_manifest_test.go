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

func TestWorkflowGeneratesSSHHostKeysAtRuntime(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("04-workflowtemplate.yaml.tmpl")
	require.NoError(t, err)

	workerCommand := "mkdir -p /run/sshd && ssh-keygen -A && exec /usr/sbin/sshd -D -e"
	assert.True(t, strings.Contains(string(content), workerCommand))
}
