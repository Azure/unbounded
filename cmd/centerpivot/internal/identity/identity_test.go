// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceID(t *testing.T) {
	t.Parallel()

	// A test node's /etc/machine-id, and the instance ID its trident-acl-agent
	// sent Nebraska.
	got, err := InstanceID("34a267d412544d6a9e97b5ba6762f6e7\n")
	require.NoError(t, err)
	assert.Equal(t, "ebb9a2bb-9365-d34e-133f-8da0664e7366", got)

	for _, bad := range []string{"", "34a267d4", "34a267d4-1254-4d6a-9e97-b5ba6762f6e7", "not a machine id"} {
		_, err := InstanceID(bad)
		assert.Error(t, err, bad)
	}
}

func TestOSReleaseValue(t *testing.T) {
	t.Parallel()

	content := `# ACL's os-release
NAME="Microsoft Azure Container Linux"
VERSION_ID=3.0.20261104
IMAGE_VERSION='202606.29.0'
EMPTY=""

BUILD_ID = 1244962
`
	for key, want := range map[string]string{
		"NAME":          "Microsoft Azure Container Linux",
		"VERSION_ID":    "3.0.20261104",
		"IMAGE_VERSION": "202606.29.0",
		"BUILD_ID":      "1244962",
	} {
		got, ok := OSReleaseValue(content, key)
		assert.True(t, ok, key)
		assert.Equal(t, want, got, key)
	}

	for _, key := range []string{"EMPTY", "MISSING"} {
		_, ok := OSReleaseValue(content, key)
		assert.False(t, ok, key)
	}
}

func TestAgentInfo(t *testing.T) {
	t.Parallel()

	info, err := NewAgentInfo("34a267d412544d6a9e97b5ba6762f6e7", "202606.29.0")
	require.NoError(t, err)

	value, err := info.Marshal()
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"schemaVersion": "1.0",
		"machineId": "ebb9a2bb-9365-d34e-133f-8da0664e7366",
		"currentVersion": "202606.29.0"
	}`, value)

	parsed, err := ParseAgentInfo(value)
	require.NoError(t, err)
	assert.Equal(t, info, parsed)
}

func TestAgentInfoLeavesOutAVersionNebraskaCantCompare(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"", "3.0", "v202606.29.0", "not a version"} {
		info, err := NewAgentInfo("34a267d412544d6a9e97b5ba6762f6e7", version)
		require.NoError(t, err)
		assert.Empty(t, info.CurrentVersion, version)
	}

	parsed, err := ParseAgentInfo(`{"schemaVersion": "1.0", "machineId": "ebb9a2bb-9365-d34e-133f-8da0664e7366", "currentVersion": "3.0"}`)
	require.NoError(t, err)
	assert.Empty(t, parsed.CurrentVersion)

	assert.True(t, IsVersion("202606.29.0-hotfix.1"))
}

func TestParseAgentInfoRefusesWhatItCantUse(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"not JSON":               "agent-info",
		"another schema version": `{"schemaVersion": "2.0", "machineId": "ebb9a2bb-9365-d34e-133f-8da0664e7366"}`,
		"no machine ID":          `{"schemaVersion": "1.0"}`,
	} {
		_, err := ParseAgentInfo(value)
		assert.Error(t, err, name)
	}
}
