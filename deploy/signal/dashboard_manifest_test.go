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

func TestDashboardRBACIsReadOnlyAndNodeListIsNotGranted(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("02-rbac.yaml.tmpl")
	require.NoError(t, err)

	_, dashboardRBAC, found := strings.Cut(string(content), "kind: Role\nmetadata:\n  name: project-signal-dashboard")
	require.True(t, found)
	assert.NotContains(t, dashboardRBAC, `verbs: ["create"`)
	assert.NotContains(t, dashboardRBAC, `verbs: ["update"`)
	assert.NotContains(t, dashboardRBAC, `verbs: ["patch"`)
	assert.NotContains(t, dashboardRBAC, `verbs: ["delete"`)
	assert.Contains(t, dashboardRBAC, `resources: ["nodes"]`+"\n"+`    verbs: ["get"]`)
	assert.NotContains(t, dashboardRBAC, `resources: ["nodes"]`+"\n"+`    verbs: ["get", "list"`)
}

func TestDashboardServiceIsInternalAndImageFailsClosed(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("07-dashboard.yaml.tmpl")
	require.NoError(t, err)

	manifest := string(content)

	assert.Contains(t, manifest, "type: ClusterIP")
	assert.NotContains(t, manifest, "kind: Ingress")
	assert.NotContains(t, manifest, "type: LoadBalancer")
	assert.Contains(t, manifest, "example.invalid/project-signal-dashboard:replace-me")
	assert.Contains(t, manifest, "serviceAccountName: project-signal-dashboard")
}
