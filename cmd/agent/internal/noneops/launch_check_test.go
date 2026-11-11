// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

const testAgentBinary = "/opt/unbounded/agent/bin/unbounded-agent"

func allocationAgentConfig() *config.AgentConfig {
	return &config.AgentConfig{
		MachineName: "test-machine",
		Kubelet: config.AgentKubeletConfig{
			Configuration: map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + "6d3f0d6e"},
		},
	}
}

func unitFiles(t *testing.T, dir string) []string {
	t.Helper()

	var files []string

	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		files = append(files, rel)

		return err
	}))

	slices.Sort(files)

	return files
}

func launchCheckFiles(dir string) (unit, dropIn string) {
	return filepath.Join(dir, none.UnitKubeletLaunchCheck), filepath.Join(none.DropInDir(dir, none.UnitKubelet), "10-launch-check.conf")
}

// The units are decided from the node's own config, so every unit write on an
// allocation host carries the check and no other host gets it.
func TestNodeKubeletLaunchCheck(t *testing.T) {
	t.Setenv(goalstates.EnvDaemonBinary, testAgentBinary)

	t.Run("allocation host", func(t *testing.T) {
		f := newFixture(t, true, nil)
		f.node.p.Config = allocationAgentConfig()

		_, err := f.node.Prepare(t.Context())
		require.NoError(t, err)

		unit, dropIn := launchCheckFiles(f.sys)

		data, err := os.ReadFile(unit)
		require.NoError(t, err)
		require.Contains(t, string(data), "\nExecStart="+testAgentBinary+" allocation-launch-check\n")
		require.Equal(t, string(none.RenderKubeletLaunchCheck(testAgentBinary+" "+LaunchCheckCommand)), string(data))

		data, err = os.ReadFile(dropIn)
		require.NoError(t, err)
		require.Equal(t, string(none.RenderKubeletLaunchCheckDropIn()), string(data))
	})

	for name, cfg := range map[string]*config.AgentConfig{
		"non-allocation config": {MachineName: "test-machine", Kubelet: config.AgentKubeletConfig{Configuration: map[string]any{"providerID": "azure:///subscriptions/x/vm"}}},
		"nil config":            nil,
	} {
		t.Run(name, func(t *testing.T) {
			baseline := newFixture(t, true, nil)
			_, err := baseline.node.Prepare(t.Context())
			require.NoError(t, err)

			f := newFixture(t, true, nil)
			f.node.p.Config = cfg

			_, err = f.node.Prepare(t.Context())
			require.NoError(t, err)

			unit, dropIn := launchCheckFiles(f.sys)
			require.NoFileExists(t, unit)
			require.NoFileExists(t, dropIn)
			require.Equal(t, unitFiles(t, baseline.sys), unitFiles(t, f.sys), "the same files as a node without the check")
		})
	}

	t.Run("rollback keeps the check", func(t *testing.T) {
		f := newFixture(t, false, nil)
		f.node.p.Config = allocationAgentConfig()
		f.node.p.Previous = allocationAgentConfig()
		f.node.p.PreviousGoalState = f.gs
		installPrevious(t, f, "1.33.0-aaaaaaaa")

		f.hook.kubeletErr = errors.New("kubelet crashlooping")

		err := f.node.Upgrade(t.Context())
		require.ErrorContains(t, err, "rolled back to 1.33.0-aaaaaaaa")

		// The previous config's units, which the rollback wrote, carry the check.
		require.NotNil(t, f.node.backend.RollbackUnits)
		require.Equal(t, testAgentBinary+" "+LaunchCheckCommand, f.node.backend.RollbackUnits.LaunchCheckCommand)

		unit, dropIn := launchCheckFiles(f.sys)
		require.FileExists(t, unit)
		require.FileExists(t, dropIn)
	})
}
