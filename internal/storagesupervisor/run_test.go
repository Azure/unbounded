// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	"k8s.io/client-go/kubernetes/fake"

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

func TestCurrentRenderStateUsesFabricDiscoveryPortForRdmaPeers(t *testing.T) {
	src := writeSource(t, `
startup:
  fabric_discovery:
    addr: "0.0.0.0:9101"
  fabric:
    auto_rdma: {}
`)
	cs := fake.NewSimpleClientset(
		node("self", "red", "10.0.0.1"),
		node("peer-a", "red", "10.0.0.2"),
	)
	w, err := newPeerWatcher(Config{NodeName: "self", StorageRingLabel: testRingLabel}, cs)
	require.NoError(t, err)

	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))

	state := currentRenderState(Config{SourceDir: src}, w)
	require.True(t, state.ring.active)
	require.Len(t, state.ring.peers, 2)
	assert.Equal(t, "10.0.0.2:9101", state.ring.peers[0].GetRdma().GetDiscoveryAddr())
	assert.Equal(t, "10.0.0.1:9101", state.ring.peers[1].GetRdma().GetDiscoveryAddr())
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

	runner := &recordingRunner{failAt: -1}
	err := runWithRunner(context.Background(), Config{
		SourceDir:               src,
		ConfigPath:              dest,
		StartServiceAfterRender: true,
	}, runner)
	require.Error(t, err)
	assert.Empty(t, runner.calls)
}

func TestRunStartsServiceAfterInitialRender(t *testing.T) {
	src := writeSource(t, "version: 7")
	dest := filepath.Join(t.TempDir(), "config.binpb")
	runner := &recordingRunner{failAt: 2, failErr: assert.AnError}

	err := runWithRunner(context.Background(), Config{
		SourceDir:               src,
		ConfigPath:              dest,
		Systemctl:               []string{"systemctl"},
		ServiceName:             "unbounded-storage",
		StartServiceAfterRender: true,
	}, runner)
	require.ErrorIs(t, err, assert.AnError)

	assert.Equal(t, uint64(7), decodeFile(t, dest).Version)
	assert.Equal(t, [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "unbounded-storage"},
		{"systemctl", "restart", "unbounded-storage"},
	}, runner.calls)
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
