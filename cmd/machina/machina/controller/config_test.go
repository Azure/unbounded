// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigExperimentalContainerIsolation(t *testing.T) {
	t.Parallel()

	load := func(t *testing.T, body string) Config {
		t.Helper()

		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

		cfg, err := LoadConfig(path)
		require.NoError(t, err)

		return cfg
	}

	require.False(t, DefaultConfig().ExperimentalContainerIsolation, "off by default")
	require.False(t, load(t, "metricsAddr: \":8080\"\n").ExperimentalContainerIsolation)
	require.True(t, load(t, "experimentalContainerIsolation: true\n").ExperimentalContainerIsolation)
}
