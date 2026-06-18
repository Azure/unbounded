// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

// writeKeys projects a map of dotted config keys to a temp directory one file
// per key, mirroring how Kubernetes projects a ConfigMap volume.
func writeKeys(t *testing.T, keys map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for k, v := range keys {
		require.NoError(t, os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644))
	}

	return dir
}

// decode renders the source dir and decodes the bytes back into a Config so
// assertions can be made on the realized protobuf message.
func decode(t *testing.T, dir string) *storageconfig.Config {
	t.Helper()

	data, err := RenderConfig(dir, ringState{})
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	return &cfg
}

func TestRenderConfigFullKeys(t *testing.T) {
	dir := writeKeys(t, map[string]string{
		"version":                              "7",
		"startup.memory.no_hugepages":          "true",
		"startup.memory.memory_total_bytes":    "134217728",
		"startup.fabric.listen_addr":           "10.0.0.1:7000",
		"startup.fabric.progress_threads":      "3",
		"startup.fabric.progress_poll_us":      "25",
		"startup.fabric.rpc_worker_threads":    "8",
		"startup.fabric.max_inflight":          "2048",
		"startup.topology.use_smt_siblings":    "true",
		"startup.topology.ignore_isolated":     "true",
		"startup.topology.include_node_cpu0":   "true",
		"startup.topology.allow_inactive_port": "true",
		"startup.topology.disable_rdma":        "true",
		"startup.topology.serving_cores":       "12",
		"startup.topology.nic_workers":         "6",
		"startup.metrics.bind":                 "0.0.0.0:9100",
	})

	cfg := decode(t, dir)

	assert.Equal(t, uint64(7), cfg.Version)
	assert.True(t, cfg.GetStartup().GetMemory().GetNoHugepages())
	assert.Equal(t, uint64(134217728), cfg.GetStartup().GetMemory().GetMemoryTotalBytes())
	assert.Equal(t, "10.0.0.1:7000", cfg.GetStartup().GetFabric().GetListenAddr())
	assert.Equal(t, uint32(3), cfg.GetStartup().GetFabric().GetProgressThreads())
	assert.Equal(t, uint32(25), cfg.GetStartup().GetFabric().GetProgressPollUs())
	assert.Equal(t, uint32(8), cfg.GetStartup().GetFabric().GetRpcWorkerThreads())
	assert.Equal(t, uint32(2048), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.True(t, cfg.GetStartup().GetTopology().GetUseSmtSiblings())
	assert.True(t, cfg.GetStartup().GetTopology().GetIgnoreIsolated())
	assert.True(t, cfg.GetStartup().GetTopology().GetIncludeNodeCpu0())
	assert.True(t, cfg.GetStartup().GetTopology().GetAllowInactivePort())
	assert.True(t, cfg.GetStartup().GetTopology().GetDisableRdma())
	assert.Equal(t, uint64(12), cfg.GetStartup().GetTopology().GetServingCores())
	assert.Equal(t, uint64(6), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Equal(t, "0.0.0.0:9100", cfg.GetStartup().GetMetrics().GetBind())
}

func TestRenderConfigDefaultsConfigMap(t *testing.T) {
	// The committed ConfigMap defaults render to a config the daemon accepts.
	// Unset/zero values are left for the daemon's apply_defaults; here we just
	// confirm the documented default values round-trip through the wire format.
	dir := writeKeys(t, map[string]string{
		"version":                           "0",
		"startup.memory.no_hugepages":       "false",
		"startup.memory.memory_total_bytes": "134217728",
		"startup.fabric.listen_addr":        "0.0.0.0:0",
		"startup.fabric.max_inflight":       "1024",
		"startup.topology.serving_cores":    "0",
		"startup.topology.nic_workers":      "4",
		"startup.metrics.bind":              "",
	})

	cfg := decode(t, dir)

	assert.Equal(t, uint64(0), cfg.Version)
	assert.False(t, cfg.GetStartup().GetMemory().GetNoHugepages())
	assert.Equal(t, "0.0.0.0:0", cfg.GetStartup().GetFabric().GetListenAddr())
	assert.Equal(t, uint32(1024), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.Equal(t, uint64(4), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Empty(t, cfg.GetStartup().GetMetrics().GetBind())
}

func TestRenderConfigMissingKeysLeaveZero(t *testing.T) {
	// An empty source directory yields a message with no fields set; every
	// field is the proto3 zero value, which the daemon promotes to defaults.
	dir := t.TempDir()

	cfg := decode(t, dir)

	assert.Equal(t, uint64(0), cfg.Version)
	assert.Equal(t, uint32(0), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.Equal(t, uint64(0), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Empty(t, cfg.GetStartup().GetFabric().GetListenAddr())
}

func TestRenderConfigInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{name: "bad uint64 version", key: "version", val: "lots"},
		{name: "bad bool", key: "startup.memory.no_hugepages", val: "yes-please"},
		{name: "bad uint32", key: "startup.fabric.max_inflight", val: "-1"},
		{name: "overflow uint32", key: "startup.fabric.progress_threads", val: "4294967296"},
		{name: "bad uint64 cores", key: "startup.topology.serving_cores", val: "1.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeKeys(t, map[string]string{tt.key: tt.val})

			_, err := RenderConfig(dir, ringState{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestRenderConfigTrimsWhitespace(t *testing.T) {
	// ConfigMap values commonly carry a trailing newline; it must be trimmed
	// before parsing.
	dir := writeKeys(t, map[string]string{
		"startup.fabric.max_inflight": "2048\n",
		"startup.fabric.listen_addr":  "  10.0.0.1:7000  \n",
	})

	cfg := decode(t, dir)

	assert.Equal(t, uint32(2048), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.Equal(t, "10.0.0.1:7000", cfg.GetStartup().GetFabric().GetListenAddr())
}

func TestRenderConfigActiveRingOverlay(t *testing.T) {
	// An active ring injects local_node_id + peers and overrides the
	// ConfigMap's listen_addr with this node's own routable bind, while the
	// rest of the startup settings still come from the source.
	dir := writeKeys(t, map[string]string{
		"version":                     "3",
		"startup.fabric.listen_addr":  "0.0.0.0:9000",
		"startup.fabric.max_inflight": "2048",
	})

	ring := ringState{
		active:         true,
		localNodeID:    42,
		selfListenAddr: "10.0.0.5:9000",
		peers: []*storageconfig.PeerSpec{
			{Id: 7, Address: &storageconfig.FabricAddress{Socket: "10.0.0.6:9000"}},
			{Id: 9, Address: &storageconfig.FabricAddress{Socket: "10.0.0.7:9000"}},
		},
	}

	data, err := RenderConfig(dir, ring)
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	assert.Equal(t, uint64(3), cfg.Version)
	assert.Equal(t, uint32(2048), cfg.GetStartup().GetFabric().GetMaxInflight())
	// listen_addr is overridden with the node's own routable address.
	assert.Equal(t, "10.0.0.5:9000", cfg.GetStartup().GetFabric().GetListenAddr())
	assert.Equal(t, uint64(42), cfg.GetP2P().GetLocalNodeId())

	require.Len(t, cfg.GetPeers(), 2)
	assert.Equal(t, uint64(7), cfg.GetPeers()[0].GetId())
	assert.Equal(t, "10.0.0.6:9000", cfg.GetPeers()[0].GetAddress().GetSocket())
	assert.Equal(t, uint64(9), cfg.GetPeers()[1].GetId())
	assert.Equal(t, "10.0.0.7:9000", cfg.GetPeers()[1].GetAddress().GetSocket())
}

func TestRenderConfigInactiveRingPassesThrough(t *testing.T) {
	// An inactive ring leaves the per-node sections empty and the ConfigMap's
	// listen_addr untouched.
	dir := writeKeys(t, map[string]string{
		"startup.fabric.listen_addr": "0.0.0.0:0",
	})

	cfg := decode(t, dir)

	assert.Nil(t, cfg.GetP2P())
	assert.Empty(t, cfg.GetPeers())
	assert.Equal(t, "0.0.0.0:0", cfg.GetStartup().GetFabric().GetListenAddr())
}

func TestWriteConfigAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.binpb")

	data := []byte{0x08, 0x07} // version = 7 on the wire.
	require.NoError(t, WriteConfigAtomic(path, data))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	// Overwrite leaves no stray temp files behind.
	require.NoError(t, WriteConfigAtomic(path, []byte{0x08, 0x09}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "config.binpb", entries[0].Name())
}

func TestWriteConfigAtomicMissingDir(t *testing.T) {
	// The destination directory must exist; install bootstraps it.
	err := WriteConfigAtomic(filepath.Join(t.TempDir(), "nope", "config.binpb"), []byte{0x00})
	require.Error(t, err)
}
