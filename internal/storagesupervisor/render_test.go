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

// writeSource projects a ConfigMap-style source directory holding a single
// config.yaml document, mirroring how Kubernetes projects the ConfigMap volume.
func writeSource(t *testing.T, configYAML string) string {
	t.Helper()

	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, sourceConfigFile), []byte(configYAML), 0o644))

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

func tcpPeer(id uint64, addr string) *storageconfig.PeerSpec {
	return &storageconfig.PeerSpec{
		Id: id,
		Config: &storageconfig.PeerSpec_Tcp{
			Tcp: &storageconfig.TcpPeerConfig{Addr: addr},
		},
	}
}

func TestRenderConfigFullSchema(t *testing.T) {
	dir := writeSource(t, `
version: 7
startup:
  memory:
    no_hugepages: true
    memory_total_bytes: 134217728
  fabric:
    tcp:
      addr: "10.0.0.1:7000"
    progress_threads: 3
    progress_poll_us: 25
    rpc_worker_threads: 8
    max_inflight: 2048
  topology:
    use_smt_siblings: true
    ignore_isolated: true
    include_node_cpu0: true
    allow_inactive_port: true
    disable_rdma: true
    serving_cores: 12
    nic_workers: 6
    hcas_per_numa_node: 2
  metrics:
    addr: "0.0.0.0:9100"
`)

	cfg := decode(t, dir)

	assert.Equal(t, uint64(7), cfg.Version)
	assert.True(t, cfg.GetStartup().GetMemory().GetNoHugepages())
	assert.Equal(t, uint64(134217728), cfg.GetStartup().GetMemory().GetMemoryTotalBytes())
	assert.Equal(t, "10.0.0.1:7000", cfg.GetStartup().GetFabric().GetTcp().GetAddr())
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
	assert.Equal(t, uint64(2), cfg.GetStartup().GetTopology().GetHcasPerNumaNode())
	assert.Equal(t, "0.0.0.0:9100", cfg.GetStartup().GetMetrics().GetAddr())
}

func TestRenderConfigDefaultsConfigMap(t *testing.T) {
	// The committed ConfigMap defaults render to a config the daemon accepts.
	// Unset/zero values are left for the daemon's apply_defaults; here we just
	// confirm the documented default values round-trip through the wire format.
	dir := writeSource(t, `
version: 0
startup:
  memory:
    no_hugepages: false
    memory_total_bytes: 134217728
  fabric:
    tcp:
      addr: "0.0.0.0:0"
    max_inflight: 1024
  topology:
    serving_cores: 0
    nic_workers: 4
    hcas_per_numa_node: 1
  metrics:
    addr: ""
`)

	cfg := decode(t, dir)

	assert.Equal(t, uint64(0), cfg.Version)
	assert.False(t, cfg.GetStartup().GetMemory().GetNoHugepages())
	assert.Equal(t, "0.0.0.0:0", cfg.GetStartup().GetFabric().GetTcp().GetAddr())
	assert.Equal(t, uint32(1024), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.Equal(t, uint64(4), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Equal(t, uint64(1), cfg.GetStartup().GetTopology().GetHcasPerNumaNode())
	assert.Empty(t, cfg.GetStartup().GetMetrics().GetAddr())
}

func TestRenderConfigEmptyLeavesZero(t *testing.T) {
	// An empty config.yaml yields a message with no fields set; every field is
	// the proto3 zero value, which the daemon promotes to defaults.
	dir := writeSource(t, "")

	cfg := decode(t, dir)

	assert.Equal(t, uint64(0), cfg.Version)
	assert.Equal(t, uint32(0), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.Equal(t, uint64(0), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Empty(t, cfg.GetStartup().GetFabric().GetTcp().GetAddr())
}

func TestRenderConfigMissingFileLeavesZero(t *testing.T) {
	// An absent config.yaml (e.g. before the ConfigMap projects) is tolerated
	// and renders an empty config rather than failing.
	cfg := decode(t, t.TempDir())

	assert.Equal(t, uint64(0), cfg.Version)
	assert.Empty(t, cfg.GetStartup().GetFabric().GetTcp().GetAddr())
}

func TestRenderConfigInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{name: "malformed yaml", yaml: "version: : :\n  - broken"},
		{name: "unknown field", yaml: "versionn: 7"},
		{name: "unknown nested field", yaml: "startup:\n  fabric:\n    listen_address: \"x\""},
		{name: "wrong type bool", yaml: "startup:\n  memory:\n    no_hugepages: \"yes-please\""},
		{name: "wrong type uint", yaml: "startup:\n  fabric:\n    max_inflight: \"lots\""},
		{name: "negative uint", yaml: "startup:\n  fabric:\n    max_inflight: -1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeSource(t, tt.yaml)

			_, err := RenderConfig(dir, ringState{})
			require.Error(t, err)
		})
	}
}

func TestRenderConfigActiveRingOverlay(t *testing.T) {
	// An active ring injects local_node_id + peers and overrides the
	// ConfigMap's fabric addr with this node's own routable bind, while the
	// rest of the startup settings still come from the source.
	dir := writeSource(t, `
version: 3
startup:
  fabric:
    tcp:
      addr: "0.0.0.0:9000"
    max_inflight: 2048
backends:
  - name: origin
    fake: {}
neighborhoods:
  - name: edge
    source: origin
`)

	ring := ringState{
		active:         true,
		localNodeID:    42,
		selfListenAddr: "10.0.0.5:9000",
		peers: []*storageconfig.PeerSpec{
			tcpPeer(7, "10.0.0.6:9000"),
			tcpPeer(9, "10.0.0.7:9000"),
		},
	}

	data, err := RenderConfig(dir, ring)
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	assert.Equal(t, uint64(3), cfg.Version)
	assert.Equal(t, uint32(2048), cfg.GetStartup().GetFabric().GetMaxInflight())
	// addr is overridden with the node's own routable address.
	assert.Equal(t, "10.0.0.5:9000", cfg.GetStartup().GetFabric().GetTcp().GetAddr())

	require.Len(t, cfg.GetNeighborhoods(), 1)
	neighborhood := cfg.GetNeighborhoods()[0]
	assert.Equal(t, uint64(42), neighborhood.GetLocalNodeId())

	require.Len(t, neighborhood.GetPeers(), 2)
	assert.Equal(t, uint64(7), neighborhood.GetPeers()[0].GetId())
	assert.Equal(t, "10.0.0.6:9000", neighborhood.GetPeers()[0].GetTcp().GetAddr())
	assert.Equal(t, uint64(9), neighborhood.GetPeers()[1].GetId())
	assert.Equal(t, "10.0.0.7:9000", neighborhood.GetPeers()[1].GetTcp().GetAddr())
}

func TestRenderConfigActiveRingMergesPeers(t *testing.T) {
	// Peers declared in the YAML are merged with discovered peers, and
	// YAML-declared neighborhood scalars (fingers_per_node, local_tags) are
	// preserved while local_node_id is stamped in.
	dir := writeSource(t, `
startup:
  fabric:
    tcp:
      addr: "0.0.0.0:9000"
backends:
  - name: origin
    fake: {}
neighborhoods:
  - name: edge
    source: origin
    fingers_per_node: 5
    local_tags: ["rack-a"]
    peers:
      - id: 100
        tcp:
          addr: "10.0.0.100:9000"
`)

	ring := ringState{
		active:         true,
		localNodeID:    42,
		selfListenAddr: "10.0.0.5:9000",
		peers: []*storageconfig.PeerSpec{
			tcpPeer(9, "10.0.0.7:9000"),
			tcpPeer(7, "10.0.0.6:9000"),
		},
	}

	data, err := RenderConfig(dir, ring)
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	require.Len(t, cfg.GetNeighborhoods(), 1)
	neighborhood := cfg.GetNeighborhoods()[0]

	// YAML neighborhood scalars preserved; local id injected.
	assert.Equal(t, uint64(42), neighborhood.GetLocalNodeId())
	assert.Equal(t, uint32(5), neighborhood.GetFingersPerNode())
	assert.Equal(t, []string{"rack-a"}, neighborhood.GetLocalTags())

	// Discovered {7,9} merged with declared {100}, sorted by id.
	require.Len(t, neighborhood.GetPeers(), 3)
	assert.Equal(t, uint64(7), neighborhood.GetPeers()[0].GetId())
	assert.Equal(t, uint64(9), neighborhood.GetPeers()[1].GetId())
	assert.Equal(t, uint64(100), neighborhood.GetPeers()[2].GetId())
	assert.Equal(t, "10.0.0.100:9000", neighborhood.GetPeers()[2].GetTcp().GetAddr())
}

func TestRenderConfigMergeDropsCollisionsAndSelf(t *testing.T) {
	// A declared peer whose id collides with a discovered peer is dropped in
	// favor of the discovered one; a declared peer whose id equals the local
	// node id is dropped entirely.
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
neighborhoods:
  - name: edge
    source: origin
    peers:
      - id: 7
        tcp:
          addr: "10.9.9.9:9000"
      - id: 42
        tcp:
          addr: "10.9.9.42:9000"
`)

	ring := ringState{
		active:      true,
		localNodeID: 42,
		peers: []*storageconfig.PeerSpec{
			tcpPeer(7, "10.0.0.6:9000"),
		},
	}

	data, err := RenderConfig(dir, ring)
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	// Only the discovered peer 7 survives; its address wins the collision and
	// the self-id peer (42) is gone.
	require.Len(t, cfg.GetNeighborhoods(), 1)
	peers := cfg.GetNeighborhoods()[0].GetPeers()
	require.Len(t, peers, 1)
	assert.Equal(t, uint64(7), peers[0].GetId())
	assert.Equal(t, "10.0.0.6:9000", peers[0].GetTcp().GetAddr())
}

func TestRenderConfigInactiveRingPassesThrough(t *testing.T) {
	// An inactive ring leaves the YAML-declared per-node sections untouched and
	// the ConfigMap's fabric addr unchanged.
	dir := writeSource(t, `
startup:
  fabric:
    tcp:
      addr: "0.0.0.0:0"
backends:
  - name: origin
    fake: {}
neighborhoods:
  - name: edge
    source: origin
    peers:
      - id: 100
        tcp:
          addr: "10.0.0.100:9000"
`)

	cfg := decode(t, dir)

	require.Len(t, cfg.GetNeighborhoods(), 1)
	assert.Zero(t, cfg.GetNeighborhoods()[0].GetLocalNodeId())
	require.Len(t, cfg.GetNeighborhoods()[0].GetPeers(), 1)
	assert.Equal(t, uint64(100), cfg.GetNeighborhoods()[0].GetPeers()[0].GetId())
	assert.Equal(t, "0.0.0.0:0", cfg.GetStartup().GetFabric().GetTcp().GetAddr())
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
