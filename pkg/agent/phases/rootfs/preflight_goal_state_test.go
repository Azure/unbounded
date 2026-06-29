// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func validRootFSGoalState(t *testing.T) *goalstates.RootFS {
	t.Helper()

	dir := t.TempDir()

	return &goalstates.RootFS{
		MachineDir:          dir,
		NSpawnConfigFile:    dir + "/nspawn/kube1.nspawn",
		ServiceOverrideFile: dir + "/systemd/system/systemd-nspawn@kube1.service.d/override.conf",
		OCIImage:            "registry.example.com/unbounded/rootfs:v1",
	}
}

func TestRootFSPreflightCheckSet(t *testing.T) {
	checks := Preflight(slog.New(slog.DiscardHandler), nil, &goalstates.MachineGoalState{RootFS: validRootFSGoalState(t)})

	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name())
	}

	assert.Equal(t, []string{
		checkOCIImageReachableName,
		checkKubernetesArtifactsName,
		checkCRIArtifactsName,
		checkCNIArtifactsName,
		checkNSpawnMachineProvisioningName,
	}, names)
}
