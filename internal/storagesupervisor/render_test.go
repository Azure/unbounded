// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"bytes"
	"log/slog"
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

	return decodeWithState(t, dir, renderState{})
}

func decodeWithState(t *testing.T, dir string, state renderState) *storageconfig.Config {
	t.Helper()

	data, err := RenderConfig(dir, state)
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	return &cfg
}

func tcpPeer(name, addr string) *storageconfig.PeerSpec {
	return &storageconfig.PeerSpec{
		Name: name,
		Config: &storageconfig.PeerSpec_Tcp{
			Tcp: &storageconfig.TcpPeerConfig{Addr: addr},
		},
	}
}

func rdmaPeer(name, addr string) *storageconfig.PeerSpec {
	return &storageconfig.PeerSpec{
		Name: name,
		Config: &storageconfig.PeerSpec_Rdma{
			Rdma: &storageconfig.RdmaPeerConfig{Addr: addr},
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
    auto_rdma:
      hcas_per_numa_node: 2
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
  metrics:
    addr: "0.0.0.0:9100"
`)

	cfg := decode(t, dir)

	assert.Equal(t, uint64(7), cfg.Version)
	assert.True(t, cfg.GetStartup().GetMemory().GetNoHugepages())
	assert.NotNil(t, cfg.GetStartup().GetMemory().MemoryTotalBytes)
	assert.Equal(t, uint64(134217728), cfg.GetStartup().GetMemory().GetMemoryTotalBytes())
	assert.NotNil(t, cfg.GetStartup().GetFabric().GetAutoRdma().HcasPerNumaNode)
	assert.Equal(t, uint64(2), cfg.GetStartup().GetFabric().GetAutoRdma().GetHcasPerNumaNode())
	assert.NotNil(t, cfg.GetStartup().GetFabric().ProgressThreads)
	assert.Equal(t, uint32(3), cfg.GetStartup().GetFabric().GetProgressThreads())
	assert.NotNil(t, cfg.GetStartup().GetFabric().ProgressPollUs)
	assert.Equal(t, uint32(25), cfg.GetStartup().GetFabric().GetProgressPollUs())
	assert.NotNil(t, cfg.GetStartup().GetFabric().RpcWorkerThreads)
	assert.Equal(t, uint32(8), cfg.GetStartup().GetFabric().GetRpcWorkerThreads())
	assert.NotNil(t, cfg.GetStartup().GetFabric().MaxInflight)
	assert.Equal(t, uint32(2048), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.True(t, cfg.GetStartup().GetTopology().GetUseSmtSiblings())
	assert.True(t, cfg.GetStartup().GetTopology().GetIgnoreIsolated())
	assert.True(t, cfg.GetStartup().GetTopology().GetIncludeNodeCpu0())
	assert.True(t, cfg.GetStartup().GetTopology().GetAllowInactivePort())
	assert.True(t, cfg.GetStartup().GetTopology().GetDisableRdma())
	assert.NotNil(t, cfg.GetStartup().GetTopology().ServingCores)
	assert.Equal(t, uint64(12), cfg.GetStartup().GetTopology().GetServingCores())
	assert.NotNil(t, cfg.GetStartup().GetTopology().NicWorkers)
	assert.Equal(t, uint64(6), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Equal(t, "0.0.0.0:9100", cfg.GetStartup().GetMetrics().GetAddr())
}

func TestRenderConfigDefaultsConfigMap(t *testing.T) {
	// The committed ConfigMap defaults render to a config the daemon accepts.
	// Explicit values are preserved for the daemon to consume; absent optional
	// fields are left for the daemon's apply_defaults.
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
  metrics:
    addr: ""
`)

	cfg := decode(t, dir)

	assert.Equal(t, uint64(0), cfg.Version)
	assert.False(t, cfg.GetStartup().GetMemory().GetNoHugepages())
	assert.Equal(t, "0.0.0.0:0", cfg.GetStartup().GetFabric().GetTcp().GetAddr())
	assert.NotNil(t, cfg.GetStartup().GetMemory().MemoryTotalBytes)
	assert.Equal(t, uint64(134217728), cfg.GetStartup().GetMemory().GetMemoryTotalBytes())
	assert.NotNil(t, cfg.GetStartup().GetFabric().MaxInflight)
	assert.Equal(t, uint32(1024), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.NotNil(t, cfg.GetStartup().GetTopology().ServingCores)
	assert.Equal(t, uint64(0), cfg.GetStartup().GetTopology().GetServingCores())
	assert.NotNil(t, cfg.GetStartup().GetTopology().NicWorkers)
	assert.Equal(t, uint64(4), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Empty(t, cfg.GetStartup().GetMetrics().GetAddr())
}

func TestRenderConfigEmptyLeavesUnset(t *testing.T) {
	// An empty config.yaml yields a message with no optional defaults set; the
	// daemon promotes absent/null values to documented defaults.
	dir := writeSource(t, "")

	cfg := decode(t, dir)

	assert.Equal(t, uint64(0), cfg.Version)
	assert.Equal(t, uint32(0), cfg.GetStartup().GetFabric().GetMaxInflight())
	assert.Equal(t, uint64(0), cfg.GetStartup().GetTopology().GetNicWorkers())
	assert.Nil(t, cfg.GetStartup().GetFabric())
	assert.Nil(t, cfg.GetStartup().GetTopology())
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

			_, err := RenderConfig(dir, renderState{})
			require.Error(t, err)
		})
	}
}

func TestRenderConfigRejectsDanglingRenderedBindings(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "frontend source",
			yaml: `
frontends:
  - name: f
    source: ghost
    loadgen: {}
`,
			wantErr: `frontend "f" references unknown source "ghost"`,
		},
		{
			name: "cache source",
			yaml: `
caches:
  - name: c
    source: ghost
`,
			wantErr: `cache "c" references unknown backend source "ghost"`,
		},
		{
			name: "duplicate backend cache component name",
			yaml: `
backends:
  - name: same
    fake: {}
caches:
  - name: same
    source: same
`,
			wantErr: `duplicate component name "same" while adding cache`,
		},
		{
			name: "duplicate cache frontend component name",
			yaml: `
backends:
  - name: origin
    fake: {}
caches:
  - name: same
    source: origin
frontends:
  - name: same
    source: same
    loadgen: {}
`,
			wantErr: `duplicate component name "same" while adding frontend`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeSource(t, tt.yaml)

			_, err := RenderConfig(dir, renderState{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRenderConfigActiveRingOverlay(t *testing.T) {
	// An active ring injects self + peers and overrides the
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
`)

	ring := ringState{
		active:         true,
		selfName:       "node-a",
		selfListenAddr: "10.0.0.5:9000",
		peers: []*storageconfig.PeerSpec{
			tcpPeer("node-a", "10.0.0.5:9000"),
			tcpPeer("node-b", "10.0.0.6:9000"),
			tcpPeer("node-c", "10.0.0.7:9000"),
		},
	}

	data, err := RenderConfig(dir, renderState{ring: ring})
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	assert.Equal(t, uint64(3), cfg.Version)
	assert.NotNil(t, cfg.GetStartup().GetFabric().MaxInflight)
	assert.Equal(t, uint32(2048), cfg.GetStartup().GetFabric().GetMaxInflight())
	// addr is overridden with the node's own routable address.
	assert.Equal(t, "10.0.0.5:9000", cfg.GetStartup().GetFabric().GetTcp().GetAddr())

	assert.Equal(t, "node-a", cfg.GetSelf())

	require.Len(t, cfg.GetPeers(), 3)
	assert.Equal(t, "node-a", cfg.GetPeers()[0].GetName())
	assert.Equal(t, "10.0.0.5:9000", cfg.GetPeers()[0].GetTcp().GetAddr())
	assert.Equal(t, "node-b", cfg.GetPeers()[1].GetName())
	assert.Equal(t, "10.0.0.6:9000", cfg.GetPeers()[1].GetTcp().GetAddr())
	assert.Equal(t, "node-c", cfg.GetPeers()[2].GetName())
	assert.Equal(t, "10.0.0.7:9000", cfg.GetPeers()[2].GetTcp().GetAddr())
}

func TestRenderConfigActiveRingMergesPeers(t *testing.T) {
	// Peers declared in the YAML are merged with discovered peers, and
	// YAML-declared routing knobs are preserved while self is stamped in.
	dir := writeSource(t, `
fingers_per_node: 5
startup:
  fabric:
    tcp:
      addr: "0.0.0.0:9000"
backends:
  - name: origin
    fake: {}
peers:
  - name: node-z
    tcp:
      addr: "10.0.0.100:9000"
`)

	ring := ringState{
		active:         true,
		selfName:       "node-a",
		selfListenAddr: "10.0.0.5:9000",
		peers: []*storageconfig.PeerSpec{
			tcpPeer("node-c", "10.0.0.7:9000"),
			tcpPeer("node-a", "10.0.0.5:9000"),
			tcpPeer("node-b", "10.0.0.6:9000"),
		},
	}

	data, err := RenderConfig(dir, renderState{ring: ring})
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	// YAML routing knobs preserved; self injected.
	assert.Equal(t, "node-a", cfg.GetSelf())
	assert.NotNil(t, cfg.FingersPerNode)
	assert.Equal(t, uint32(5), cfg.GetFingersPerNode())

	// Discovered peers merge with declared node-z and are sorted by name.
	require.Len(t, cfg.GetPeers(), 4)
	assert.Equal(t, "node-a", cfg.GetPeers()[0].GetName())
	assert.Equal(t, "node-b", cfg.GetPeers()[1].GetName())
	assert.Equal(t, "node-c", cfg.GetPeers()[2].GetName())
	assert.Equal(t, "node-z", cfg.GetPeers()[3].GetName())
	assert.Equal(t, "10.0.0.100:9000", cfg.GetPeers()[3].GetTcp().GetAddr())
}

func TestRenderConfigActiveRDMARingPreservesStartupFabric(t *testing.T) {
	dir := writeSource(t, `
startup:
  fabric:
    auto_rdma:
      hcas_per_numa_node: 2
    max_inflight: 2048
backends:
  - name: origin
    fake: {}
`)

	ring := ringState{
		active:   true,
		selfName: "node-a",
		peers: []*storageconfig.PeerSpec{
			rdmaPeer("node-a", "hex:self"),
			rdmaPeer("node-b", "hex:peer"),
		},
	}

	cfg := decodeWithState(t, dir, renderState{ring: ring})

	assert.NotNil(t, cfg.GetStartup().GetFabric().GetAutoRdma())
	assert.Equal(t, uint64(2), cfg.GetStartup().GetFabric().GetAutoRdma().GetHcasPerNumaNode())
	assert.Nil(t, cfg.GetStartup().GetFabric().GetTcp())
	assert.Equal(t, "node-a", cfg.GetSelf())
	require.Len(t, cfg.GetPeers(), 2)
	assert.Equal(t, "hex:self", cfg.GetPeers()[0].GetRdma().GetAddr())
	assert.Equal(t, "hex:peer", cfg.GetPeers()[1].GetRdma().GetAddr())
}

func TestRenderConfigActiveRingInjectsMeshWithoutDeclaredPeers(t *testing.T) {
	dir := writeSource(t, `
startup:
  fabric:
    tcp:
      addr: "0.0.0.0:9000"
backends:
  - name: origin
    fake: {}
`)

	var logs bytes.Buffer

	previousLogger := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	ring := ringState{
		active:         true,
		selfName:       "node-a",
		selfListenAddr: "10.0.0.5:9000",
		peers: []*storageconfig.PeerSpec{
			tcpPeer("node-a", "10.0.0.5:9000"),
			tcpPeer("node-b", "10.0.0.6:9000"),
		},
	}

	data, err := RenderConfig(dir, renderState{ring: ring})
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	assert.Equal(t, "node-a", cfg.GetSelf())
	require.Len(t, cfg.GetPeers(), 2)
	assert.Equal(t, "node-a", cfg.GetPeers()[0].GetName())
	assert.Equal(t, "node-b", cfg.GetPeers()[1].GetName())
	assert.Equal(t, "10.0.0.5:9000", cfg.GetStartup().GetFabric().GetTcp().GetAddr())
	assert.NotContains(t, logs.String(), "discovered storage peers were not injected")
}

func TestRenderConfigMergeDropsNameCollisions(t *testing.T) {
	// A declared peer whose name collides with a discovered peer is dropped in
	// favor of the discovered one. Self remains in the roster because the daemon
	// uses it to derive the local fabric/ring identity.
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
peers:
  - name: node-b
    tcp:
      addr: "10.9.9.9:9000"
  - name: node-z
    tcp:
      addr: "10.9.9.42:9000"
`)

	ring := ringState{
		active:   true,
		selfName: "node-a",
		peers: []*storageconfig.PeerSpec{
			tcpPeer("node-a", "10.0.0.5:9000"),
			tcpPeer("node-b", "10.0.0.6:9000"),
		},
	}

	data, err := RenderConfig(dir, renderState{ring: ring})
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	// Discovered node-b wins the collision and declared node-z is preserved.
	peers := cfg.GetPeers()
	require.Len(t, peers, 3)
	assert.Equal(t, "node-a", peers[0].GetName())
	assert.Equal(t, "node-b", peers[1].GetName())
	assert.Equal(t, "10.0.0.6:9000", peers[1].GetTcp().GetAddr())
	assert.Equal(t, "node-z", peers[2].GetName())
}

func TestRenderConfigInactiveRingPassesThrough(t *testing.T) {
	// An inactive ring leaves the YAML-declared mesh fields untouched and
	// the ConfigMap's fabric addr unchanged.
	dir := writeSource(t, `
self: manual
startup:
  fabric:
    tcp:
      addr: "0.0.0.0:0"
backends:
  - name: origin
    fake: {}
peers:
  - name: manual
    tcp:
      addr: "10.0.0.100:9000"
`)

	cfg := decode(t, dir)

	assert.Equal(t, "manual", cfg.GetSelf())
	require.Len(t, cfg.GetPeers(), 1)
	assert.Equal(t, "manual", cfg.GetPeers()[0].GetName())
	assert.Equal(t, "0.0.0.0:0", cfg.GetStartup().GetFabric().GetTcp().GetAddr())
}

func TestRenderConfigInjectsAnnotatedBlockDisks(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageDisksAnnotation: "/dev/nvme1n1;queue_depth=256;page_size_bytes=4096;numa=0;skip_recovery_scan=true;force_format=true;bypass_admission=true",
		},
	})

	require.Len(t, cfg.GetDisks(), 1)

	disk := cfg.GetDisks()[0]
	assert.Equal(t, "/dev/nvme1n1", disk.GetBlock().GetPath())
	assert.NotNil(t, disk.QueueDepth)
	assert.Equal(t, uint32(256), disk.GetQueueDepth())
	assert.NotNil(t, disk.PageSizeBytes)
	assert.Equal(t, uint64(4096), disk.GetPageSizeBytes())
	assert.NotNil(t, disk.GetBlock().Numa)
	assert.Equal(t, uint32(0), disk.GetBlock().GetNuma())
	assert.True(t, disk.GetSkipRecoveryScan())
	assert.True(t, disk.GetForceFormat())
	assert.True(t, disk.GetBypassAdmission())
}

func TestRenderConfigInjectsMultipleAnnotatedBlockDisks(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageDisksAnnotation: "/dev/nvme1n1;queue_depth=128,/dev/nvme2n1;numa=1",
		},
	})

	require.Len(t, cfg.GetDisks(), 2)
	assert.Equal(t, "/dev/nvme1n1", cfg.GetDisks()[0].GetBlock().GetPath())
	assert.Equal(t, uint32(128), cfg.GetDisks()[0].GetQueueDepth())
	assert.Equal(t, "/dev/nvme2n1", cfg.GetDisks()[1].GetBlock().GetPath())
	assert.Equal(t, uint32(1), cfg.GetDisks()[1].GetBlock().GetNuma())
}

func TestRenderConfigIgnoresInvalidAnnotatedOptions(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	var logs bytes.Buffer

	previousLogger := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageDisksAnnotation: "/dev/nvme1n1;unknown=x;queue_depth=0;queue_depth=512;queue_depth=1024;page_size_bytes=bad;skip_recovery_scan=maybe;force_format=maybe;bypass_admission=maybe;numa=bad;empty=;=value;missing",
		},
	})

	require.Len(t, cfg.GetDisks(), 1)
	disk := cfg.GetDisks()[0]
	assert.Equal(t, "/dev/nvme1n1", disk.GetBlock().GetPath())
	assert.NotNil(t, disk.QueueDepth)
	assert.Equal(t, uint32(512), disk.GetQueueDepth())
	assert.Nil(t, disk.PageSizeBytes)
	assert.Nil(t, disk.GetBlock().Numa)
	assert.False(t, disk.GetSkipRecoveryScan())
	assert.False(t, disk.GetForceFormat())
	assert.False(t, disk.GetBypassAdmission())
	assert.Contains(t, logs.String(), "ignoring unknown storage disk option")
	assert.Contains(t, logs.String(), "ignoring duplicate storage disk option")
}

func TestRenderConfigInvalidAnnotatedDiskSpecsAreSkipped(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageDisksAnnotation:    " , ;queue_depth=256, /dev/nvme1n1, /dev/nvme1n1",
			storageFileSizeAnnotation: "4294967296",
		},
	})

	require.Len(t, cfg.GetDisks(), 1)
	disk := cfg.GetDisks()[0]
	require.NotNil(t, disk.GetBlock())
	assert.Equal(t, "/dev/nvme1n1", disk.GetBlock().GetPath())
}

func TestRenderConfigAllInvalidAnnotatedDisksFallsBackToFile(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageDisksAnnotation:    " , relative-path;queue_depth=256, ;numa=1",
			storageFileSizeAnnotation: "4294967296",
		},
	})

	require.Len(t, cfg.GetDisks(), 1)
	disk := cfg.GetDisks()[0]
	require.NotNil(t, disk.GetFile())
	assert.Equal(t, defaultStorageFileDiskPath, disk.GetFile().GetPath())
	assert.Equal(t, uint64(4294967296), disk.GetFile().GetSize())
}

func TestRenderConfigBlankAnnotatedDisksFallsBackToFile(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageDisksAnnotation:    " , ",
			storageFileSizeAnnotation: "4294967296",
		},
	})

	require.Len(t, cfg.GetDisks(), 1)
	disk := cfg.GetDisks()[0]
	assert.Equal(t, defaultStorageFileDiskPath, disk.GetFile().GetPath())
	assert.Equal(t, uint64(4294967296), disk.GetFile().GetSize())
}

func TestRenderConfigFallbackFileDiskDefaultSize(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	cfg := decode(t, dir)

	require.Len(t, cfg.GetDisks(), 1)
	disk := cfg.GetDisks()[0]
	assert.Equal(t, defaultStorageFileDiskPath, disk.GetFile().GetPath())
	assert.Equal(t, defaultStorageFileDiskSize, disk.GetFile().GetSize())
	assert.False(t, disk.GetSkipRecoveryScan())
}

func TestRenderConfigInvalidFileSizeAnnotationFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name string
		size string
	}{
		{name: "not uint", size: "big"},
		{name: "zero", size: "0"},
		{name: "not page aligned", size: "4097"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeSource(t, `
version: 1
`)

			cfg := decodeWithState(t, dir, renderState{
				annotations: map[string]string{storageFileSizeAnnotation: tt.size},
			})

			require.Len(t, cfg.GetDisks(), 1)
			assert.Equal(t, defaultStorageFileDiskSize, cfg.GetDisks()[0].GetFile().GetSize())
		})
	}
}

func TestRenderConfigPreservesExplicitConfigMapDisks(t *testing.T) {
	dir := writeSource(t, `
disks:
  - file:
      path: /custom/cache.disk
      size: 1073741824
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{storageDisksAnnotation: "/dev/nvme1n1"},
	})

	require.Len(t, cfg.GetDisks(), 1)
	disk := cfg.GetDisks()[0]
	assert.Equal(t, "/custom/cache.disk", disk.GetFile().GetPath())
	assert.Equal(t, uint64(1073741824), disk.GetFile().GetSize())
}

func TestRenderConfigCachesUntouched(t *testing.T) {
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
caches:
  - name: cache-a
    source: origin
  - name: cache-b
    source: origin
`)

	cfg := decode(t, dir)

	require.Len(t, cfg.GetCaches(), 2)
	assert.Equal(t, "cache-a", cfg.GetCaches()[0].GetName())
	assert.Equal(t, "cache-b", cfg.GetCaches()[1].GetName())
	require.Len(t, cfg.GetDisks(), 1)
}

func TestRenderConfigNoCachesInjectsDisks(t *testing.T) {
	dir := writeSource(t, `
version: 1
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{storageDisksAnnotation: "/dev/nvme1n1"},
	})

	require.Len(t, cfg.GetDisks(), 1)
	assert.Equal(t, "/dev/nvme1n1", cfg.GetDisks()[0].GetBlock().GetPath())
}

func TestRenderConfigInjectsAnnotatedLoadgenFrontends(t *testing.T) {
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
caches:
  - name: cache
    source: origin
frontends:
  - name: http
    source: cache
    http:
      addr: "0.0.0.0:8080"
`)

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageLoadgenAnnotation: "lg;source=cache;workers=32;seed=1234;keyspace_objects=1000000;object_size_bytes=4194304;read_bytes=262144;zipf_exponent=1.1;verify=true;remote_only=true;fabric_only=true;local_only=true;skip_local_disk=true,lg2;source=origin",
		},
	})

	require.Len(t, cfg.GetFrontends(), 3)
	assert.Equal(t, "http", cfg.GetFrontends()[0].GetName())

	frontend := cfg.GetFrontends()[1]
	assert.Equal(t, "lg", frontend.GetName())
	assert.Equal(t, "cache", frontend.GetSource())
	loadgen := frontend.GetLoadgen()
	require.NotNil(t, loadgen)
	assert.NotNil(t, loadgen.Workers)
	assert.Equal(t, uint32(32), loadgen.GetWorkers())
	assert.NotNil(t, loadgen.Seed)
	assert.Equal(t, uint64(1234), loadgen.GetSeed())
	assert.NotNil(t, loadgen.KeyspaceObjects)
	assert.Equal(t, uint64(1000000), loadgen.GetKeyspaceObjects())
	assert.NotNil(t, loadgen.ObjectSizeBytes)
	assert.Equal(t, uint64(4194304), loadgen.GetObjectSizeBytes())
	assert.NotNil(t, loadgen.ReadBytes)
	assert.Equal(t, uint64(262144), loadgen.GetReadBytes())
	assert.NotNil(t, loadgen.ZipfExponent)
	assert.Equal(t, 1.1, loadgen.GetZipfExponent())
	assert.True(t, loadgen.GetVerify())
	assert.True(t, loadgen.GetRemoteOnly())
	assert.True(t, loadgen.GetFabricOnly())
	assert.True(t, loadgen.GetLocalOnly())
	assert.True(t, loadgen.GetSkipLocalDisk())

	assert.Equal(t, "lg2", cfg.GetFrontends()[2].GetName())
	assert.Equal(t, "origin", cfg.GetFrontends()[2].GetSource())
}

func TestRenderConfigSkipsInvalidAnnotatedLoadgenFrontends(t *testing.T) {
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
caches:
  - name: cache
    source: origin
frontends:
  - name: existing
    source: cache
    http:
      addr: "0.0.0.0:8080"
`)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	cfg := decodeWithState(t, dir, renderState{
		annotations: map[string]string{
			storageLoadgenAnnotation: "existing;source=cache,missing-source;workers=1,valid;source=cache;workers=bad;workers=2;zipf_exponent=NaN;zipf_exponent=1.3;verify=maybe;verify=true;remote_only=maybe;remote_only=true;fabric_only=maybe;fabric_only=true;local_only=maybe;local_only=true;skip_local_disk=maybe;skip_local_disk=true,valid;source=origin",
		},
	})

	require.Len(t, cfg.GetFrontends(), 2)
	assert.Equal(t, "existing", cfg.GetFrontends()[0].GetName())
	assert.Equal(t, "valid", cfg.GetFrontends()[1].GetName())
	loadgen := cfg.GetFrontends()[1].GetLoadgen()
	require.NotNil(t, loadgen)
	assert.Equal(t, uint32(2), loadgen.GetWorkers())
	assert.Equal(t, 1.3, loadgen.GetZipfExponent())
	assert.True(t, loadgen.GetVerify())
	assert.True(t, loadgen.GetRemoteOnly())
	assert.True(t, loadgen.GetFabricOnly())
	assert.True(t, loadgen.GetLocalOnly())
	assert.True(t, loadgen.GetSkipLocalDisk())
	assert.Contains(t, logs.String(), "frontend name is already declared")
	assert.Contains(t, logs.String(), "without source")
	assert.Contains(t, logs.String(), "invalid storage loadgen uint32 option")
	assert.Contains(t, logs.String(), "invalid storage loadgen float option")
	assert.Contains(t, logs.String(), "invalid storage loadgen bool option")
	assert.Contains(t, logs.String(), "duplicate annotated storage loadgen frontend")
}

func TestRenderConfigRejectsAnnotatedLoadgenWithUnknownSource(t *testing.T) {
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
`)

	_, err := RenderConfig(dir, renderState{
		annotations: map[string]string{
			storageLoadgenAnnotation: "lg;source=ghost",
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `frontend "lg" references unknown source "ghost"`)
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
