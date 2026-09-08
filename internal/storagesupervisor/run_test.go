// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package storagesupervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

// decodeFile reads a rendered config file and unmarshals it.
func decodeFile(t *testing.T, path string) *storageconfig.Config {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	return &cfg
}

func TestReconcileRendersFile(t *testing.T) {
	src := writeSource(t, "version: 5\nstartup:\n  fabric:\n    max_inflight: 2048\n")
	dest := filepath.Join(t.TempDir(), "config.binpb")

	require.NoError(t, reconcile(Config{SourceDir: src, ConfigPath: dest}, nil))

	cfg := decodeFile(t, dest)
	assert.Equal(t, uint64(5), cfg.Version)
	assert.NotNil(t, cfg.GetStartup().GetFabric().MaxInflight)
	assert.Equal(t, uint32(2048), cfg.GetStartup().GetFabric().GetMaxInflight())
}

func TestReconcileBadValueErrors(t *testing.T) {
	src := writeSource(t, "version: \"nope\"")
	dest := filepath.Join(t.TempDir(), "config.binpb")

	require.Error(t, reconcile(Config{SourceDir: src, ConfigPath: dest}, nil))
}

func TestRunRendersInitialAndReRenders(t *testing.T) {
	src := writeSource(t, "version: 1")
	dest := filepath.Join(t.TempDir(), "config.binpb")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- Run(ctx, Config{SourceDir: src, ConfigPath: dest}) }()

	// The initial render happens before the watch loop blocks; poll for it.
	require.Eventually(t, func() bool {
		_, err := os.Stat(dest)

		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, uint64(1), decodeFile(t, dest).Version)

	// Mutate the source document; the watcher should re-render with the new
	// value.
	require.NoError(t, os.WriteFile(filepath.Join(src, sourceConfigFile), []byte("version: 2"), 0o644))

	require.Eventually(t, func() bool {
		return decodeFile(t, dest).Version == 2
	}, 3*time.Second, 20*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRunInitialRenderFails(t *testing.T) {
	// A bad source value surfaces as an error from the initial render rather
	// than starting the watch loop.
	src := writeSource(t, "version: \"bad\"")
	dest := filepath.Join(t.TempDir(), "config.binpb")

	err := Run(context.Background(), Config{SourceDir: src, ConfigPath: dest})
	require.Error(t, err)
}

func TestRunMissingSourceDirErrors(t *testing.T) {
	// RenderConfig tolerates an absent config.yaml, but the watcher cannot
	// watch a nonexistent source directory, so Run fails after the (empty)
	// initial render.
	dest := filepath.Join(t.TempDir(), "config.binpb")

	err := Run(context.Background(), Config{
		SourceDir:  filepath.Join(t.TempDir(), "does-not-exist"),
		ConfigPath: dest,
	})
	require.Error(t, err)
}
