// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

func benchmarkNode(name string, annotations map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
}

func benchmarkNodeWithIP(name string, annotations map[string]string, ip string) *corev1.Node {
	node := benchmarkNode(name, annotations)
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}

	return node
}

func decodeWithBenchmarks(t *testing.T, dir string, benchmarks benchmarkState) *storageconfig.Config {
	t.Helper()

	data, err := RenderConfigWithBenchmarks(dir, ringState{}, benchmarks)
	require.NoError(t, err)

	var cfg storageconfig.Config

	require.NoError(t, proto.Unmarshal(data, &cfg))

	return &cfg
}

func TestComputeBenchmarksSourceNode(t *testing.T) {
	source := benchmarkNode("source-a", map[string]string{
		benchmarkScenarioAnnotation:        rdmaLoadgenScenario,
		benchmarkTargetNodeAnnotation:      "target-b",
		benchmarkRdmaAddrAnnotation:        "hex:01020304",
		benchmarkWorkersAnnotation:         "4",
		benchmarkSeedAnnotation:            "17",
		benchmarkObjectCountAnnotation:     "1000",
		benchmarkReadBytesAnnotation:       "4096",
		benchmarkVerifyAnnotation:          "true",
		benchmarkStripeSizeBytesAnnotation: "8192",
		benchmarkObjectSizeBytesAnnotation: "16384",
	})
	target := benchmarkNode("target-b", map[string]string{benchmarkRdmaAddrAnnotation: "hex:0a0b"})

	state := computeBenchmarks([]*corev1.Node{target, source}, "source-a", 0)

	require.Len(t, state.rdmaLoadgens, 1)
	bench := state.rdmaLoadgens[0]
	assert.Equal(t, "rdma_source-a_to_target-b", bench.name)
	assert.Equal(t, "source-a", bench.sourceNode)
	assert.Equal(t, "target-b", bench.targetNode)
	assert.True(t, bench.runLoadgen)
	assert.Equal(t, nodeID("source-a"), bench.localNodeID)
	assert.Equal(t, nodeID("target-b"), bench.peerNodeID)
	assert.Equal(t, "hex:0a0b", bench.peerAddr)
	assert.False(t, bench.peerTCP)
	assert.False(t, bench.cacheMiss)
	require.NotNil(t, bench.workers)
	assert.Equal(t, uint32(4), *bench.workers)
	require.NotNil(t, bench.seed)
	assert.Equal(t, uint64(17), *bench.seed)
	require.NotNil(t, bench.objectCount)
	assert.Equal(t, uint64(1000), *bench.objectCount)
	require.NotNil(t, bench.readBytes)
	assert.Equal(t, uint64(4096), *bench.readBytes)
	assert.True(t, bench.verify)
	require.NotNil(t, bench.stripeSizeBytes)
	assert.Equal(t, uint64(8192), *bench.stripeSizeBytes)
	require.NotNil(t, bench.objectSizeBytes)
	assert.Equal(t, uint64(16384), *bench.objectSizeBytes)
}

func TestComputeBenchmarksCacheMissSourceNode(t *testing.T) {
	source := benchmarkNode("source-a", map[string]string{
		benchmarkScenarioAnnotation:        rdmaCacheMissScenario,
		benchmarkTargetNodeAnnotation:      "target-b",
		benchmarkRdmaAddrAnnotation:        "hex:01020304",
		benchmarkReadBytesAnnotation:       "4096",
		benchmarkObjectSizeBytesAnnotation: "16384",
		benchmarkDiskPathAnnotation:        "/var/lib/unbounded/bench-cache.bin",
		benchmarkDiskSizeBytesAnnotation:   "104857600",
		benchmarkWarmupOpsAnnotation:       "25",
	})
	target := benchmarkNode("target-b", map[string]string{benchmarkRdmaAddrAnnotation: "hex:0a0b"})

	state := computeBenchmarks([]*corev1.Node{target, source}, "source-a", 0)

	require.Len(t, state.rdmaLoadgens, 1)
	bench := state.rdmaLoadgens[0]
	assert.Equal(t, "rdma_source-a_to_target-b", bench.name)
	assert.True(t, bench.runLoadgen)
	assert.True(t, bench.cacheMiss)
	assert.Equal(t, nodeID("source-a"), bench.localNodeID)
	assert.Equal(t, nodeID("target-b"), bench.peerNodeID)
	assert.Equal(t, "hex:0a0b", bench.peerAddr)
	assert.False(t, bench.peerTCP)
	require.NotNil(t, bench.readBytes)
	assert.Equal(t, uint64(4096), *bench.readBytes)
	require.NotNil(t, bench.objectSizeBytes)
	assert.Equal(t, uint64(16384), *bench.objectSizeBytes)
	require.NotNil(t, bench.warmupOps)
	assert.Equal(t, uint64(25), *bench.warmupOps)
	assert.Equal(t, "/var/lib/unbounded/bench-cache.bin", bench.diskPath)
	assert.Equal(t, uint64(104857600), bench.diskSizeBytes)
}

func TestComputeBenchmarksTargetNode(t *testing.T) {
	source := benchmarkNode("source-a", map[string]string{
		benchmarkLegacyScenarioAnnotation:  rdmaScenarioAlias,
		benchmarkTargetNodeAnnotation:      "target-b",
		benchmarkRdmaAddrAnnotation:        "hex:0102",
		benchmarkWorkersAnnotation:         "2",
		benchmarkObjectCountAnnotation:     "99",
		benchmarkStripeSizeBytesAnnotation: "4096",
	})
	target := benchmarkNode("target-b", map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"})

	state := computeBenchmarks([]*corev1.Node{source, target}, "target-b", 0)

	require.Len(t, state.rdmaLoadgens, 1)
	bench := state.rdmaLoadgens[0]
	assert.False(t, bench.runLoadgen)
	assert.Equal(t, nodeID("target-b"), bench.localNodeID)
	assert.Equal(t, nodeID("source-a"), bench.peerNodeID)
	assert.Equal(t, "hex:0102", bench.peerAddr)
	assert.False(t, bench.peerTCP)
	require.NotNil(t, bench.workers)
	assert.Equal(t, uint32(2), *bench.workers)
}

func TestComputeBenchmarksTCPSourceNode(t *testing.T) {
	source := benchmarkNodeWithIP("source-a", map[string]string{
		benchmarkScenarioAnnotation:        tcpCacheMissScenario,
		benchmarkTargetNodeAnnotation:      "target-b",
		benchmarkReadBytesAnnotation:       "4096",
		benchmarkObjectSizeBytesAnnotation: "16384",
		benchmarkDiskPathAnnotation:        "/var/lib/unbounded/bench-cache.bin",
		benchmarkDiskSizeBytesAnnotation:   "104857600",
	}, "10.0.0.1")
	target := benchmarkNodeWithIP("target-b", nil, "10.0.0.2")

	state := computeBenchmarks([]*corev1.Node{target, source}, "source-a", 19001)

	require.Len(t, state.rdmaLoadgens, 1)
	bench := state.rdmaLoadgens[0]
	assert.True(t, bench.runLoadgen)
	assert.True(t, bench.cacheMiss)
	assert.Equal(t, nodeID("source-a"), bench.localNodeID)
	assert.Equal(t, nodeID("target-b"), bench.peerNodeID)
	assert.Equal(t, "10.0.0.2:19001", bench.peerAddr)
	assert.True(t, bench.peerTCP)
}

func TestComputeBenchmarksTCPMissingTargetInternalIPSkipped(t *testing.T) {
	source := benchmarkNodeWithIP("source-a", map[string]string{
		benchmarkScenarioAnnotation:   tcpLoadgenScenario,
		benchmarkTargetNodeAnnotation: "target-b",
	}, "10.0.0.1")
	target := benchmarkNode("target-b", map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"})

	state := computeBenchmarks([]*corev1.Node{target, source}, "source-a", 19001)

	assert.Empty(t, state.rdmaLoadgens)
}

func TestComputeBenchmarksTCPPortAnnotationOverridesDefault(t *testing.T) {
	source := benchmarkNodeWithIP("source-a", map[string]string{
		benchmarkScenarioAnnotation:   tcpLoadgenScenario,
		benchmarkTargetNodeAnnotation: "target-b",
		benchmarkTCPPortAnnotation:    "19002",
	}, "10.0.0.1")
	target := benchmarkNodeWithIP("target-b", map[string]string{benchmarkTCPPortAnnotation: "19003"}, "10.0.0.2")

	state := computeBenchmarks([]*corev1.Node{target, source}, "source-a", 19001)

	require.Len(t, state.rdmaLoadgens, 1)
	assert.Equal(t, "10.0.0.2:19003", state.rdmaLoadgens[0].peerAddr)
}

func TestComputeBenchmarksSkipsInvalidAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		source      map[string]string
		target      map[string]string
		includePeer bool
	}{
		{
			name: "missing target",
			source: map[string]string{
				benchmarkScenarioAnnotation: rdmaLoadgenScenario,
				benchmarkRdmaAddrAnnotation: "hex:0102",
			},
			target:      map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"},
			includePeer: true,
		},
		{
			name: "target missing",
			source: map[string]string{
				benchmarkScenarioAnnotation:   rdmaLoadgenScenario,
				benchmarkTargetNodeAnnotation: "target-b",
				benchmarkRdmaAddrAnnotation:   "hex:0102",
			},
		},
		{
			name: "bad rdma addr",
			source: map[string]string{
				benchmarkScenarioAnnotation:   rdmaLoadgenScenario,
				benchmarkTargetNodeAnnotation: "target-b",
				benchmarkRdmaAddrAnnotation:   "hex:010",
			},
			target:      map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"},
			includePeer: true,
		},
		{
			name: "tcp target missing internal ip",
			source: map[string]string{
				benchmarkScenarioAnnotation:   tcpLoadgenScenario,
				benchmarkTargetNodeAnnotation: "target-b",
			},
			target:      map[string]string{},
			includePeer: true,
		},
		{
			name: "bad worker count",
			source: map[string]string{
				benchmarkScenarioAnnotation:   rdmaLoadgenScenario,
				benchmarkTargetNodeAnnotation: "target-b",
				benchmarkRdmaAddrAnnotation:   "hex:0102",
				benchmarkWorkersAnnotation:    "-1",
			},
			target:      map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"},
			includePeer: true,
		},
		{
			name: "cache miss missing disk path",
			source: map[string]string{
				benchmarkScenarioAnnotation:      rdmaCacheMissScenario,
				benchmarkTargetNodeAnnotation:    "target-b",
				benchmarkRdmaAddrAnnotation:      "hex:0102",
				benchmarkDiskSizeBytesAnnotation: "1048576",
			},
			target:      map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"},
			includePeer: true,
		},
		{
			name: "cache miss missing disk size",
			source: map[string]string{
				benchmarkScenarioAnnotation:   rdmaCacheMissScenario,
				benchmarkTargetNodeAnnotation: "target-b",
				benchmarkRdmaAddrAnnotation:   "hex:0102",
				benchmarkDiskPathAnnotation:   "/var/lib/unbounded/bench-cache.bin",
			},
			target:      map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"},
			includePeer: true,
		},
		{
			name: "cache miss bad disk size",
			source: map[string]string{
				benchmarkScenarioAnnotation:      rdmaCacheMissScenario,
				benchmarkTargetNodeAnnotation:    "target-b",
				benchmarkRdmaAddrAnnotation:      "hex:0102",
				benchmarkDiskPathAnnotation:      "/var/lib/unbounded/bench-cache.bin",
				benchmarkDiskSizeBytesAnnotation: "not-a-number",
			},
			target:      map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"},
			includePeer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := []*corev1.Node{benchmarkNode("source-a", tt.source)}
			if tt.includePeer {
				nodes = append(nodes, benchmarkNode("target-b", tt.target))
			}

			state := computeBenchmarks(nodes, "source-a", 19001)
			assert.Empty(t, state.rdmaLoadgens)
		})
	}
}

func TestRenderConfigAppliesRDMABenchmarkOnSource(t *testing.T) {
	dir := writeSource(t, `
version: 1
startup:
  fabric:
    auto_rdma: {}
  metrics:
    addr: "0.0.0.0:9100"
`)

	workers := uint32(3)
	seed := uint64(11)
	objectCount := uint64(101)
	readBytes := uint64(4096)
	stripeSize := uint64(8192)
	objectSize := uint64(16384)
	bench := rdmaLoadgenBenchmark{
		name:            "rdma_source_to_target",
		runLoadgen:      true,
		localNodeID:     10,
		peerNodeID:      20,
		peerAddr:        "hex:0102",
		workers:         &workers,
		seed:            &seed,
		objectCount:     &objectCount,
		readBytes:       &readBytes,
		verify:          true,
		stripeSizeBytes: &stripeSize,
		objectSizeBytes: &objectSize,
	}

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	assert.Equal(t, uint64(1), cfg.GetVersion())
	assert.NotNil(t, cfg.GetStartup().GetFabric().GetAutoRdma())
	assert.Equal(t, "0.0.0.0:9100", cfg.GetStartup().GetMetrics().GetAddr())
	assert.Empty(t, cfg.GetCaches())

	require.Len(t, cfg.GetBackends(), 1)
	backend := cfg.GetBackends()[0]
	assert.Equal(t, bench.backendName(), backend.GetName())
	assert.Equal(t, uint64(8192), backend.GetFake().GetStripeSizeBytes())
	assert.Equal(t, uint64(16384), backend.GetFake().GetObjectSizeBytes())

	require.Len(t, cfg.GetNeighborhoods(), 1)
	neighborhood := cfg.GetNeighborhoods()[0]
	assert.Equal(t, bench.neighborhoodName(), neighborhood.GetName())
	assert.Equal(t, bench.backendName(), neighborhood.GetSource())
	assert.Equal(t, uint64(10), neighborhood.GetLocalNodeId())
	require.Len(t, neighborhood.GetPeers(), 1)
	assert.Equal(t, uint64(20), neighborhood.GetPeers()[0].GetId())
	assert.Equal(t, "hex:0102", neighborhood.GetPeers()[0].GetRdma().GetAddr())

	require.Len(t, cfg.GetFrontends(), 1)
	frontend := cfg.GetFrontends()[0]
	assert.Equal(t, bench.frontendName(), frontend.GetName())
	assert.Equal(t, bench.neighborhoodName(), frontend.GetSource())
	loadgen := frontend.GetLoadgen()
	require.NotNil(t, loadgen)
	assert.Equal(t, uint32(3), loadgen.GetWorkers())
	assert.Equal(t, uint64(11), loadgen.GetSeed())
	assert.Equal(t, uint64(101), loadgen.GetObjectCount())
	assert.Equal(t, uint64(4096), loadgen.GetReadBytes())
	assert.True(t, loadgen.GetVerify())
	assert.False(t, loadgen.GetRequireRemotePeer())
}

func TestRenderConfigAppliesRDMABenchmarkOnTargetWithoutLoadgen(t *testing.T) {
	dir := writeSource(t, "startup:\n  fabric:\n    auto_rdma: {}\n")
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  false,
		localNodeID: 20,
		peerNodeID:  10,
		peerAddr:    "hex:0102",
	}

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetBackends(), 1)
	require.Len(t, cfg.GetNeighborhoods(), 1)
	assert.Empty(t, cfg.GetCaches())
	assert.Empty(t, cfg.GetFrontends())
	assert.Equal(t, uint64(20), cfg.GetNeighborhoods()[0].GetLocalNodeId())
}

func TestRenderConfigAppliesTCPBenchmarkPeer(t *testing.T) {
	dir := writeSource(t, "startup:\n  fabric:\n    tcp:\n      addr: \"0.0.0.0:19001\"\n")
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  true,
		localNodeID: 10,
		peerNodeID:  20,
		peerAddr:    "10.0.0.2:19001",
		peerTCP:     true,
	}

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetNeighborhoods(), 1)
	peers := cfg.GetNeighborhoods()[0].GetPeers()
	require.Len(t, peers, 1)
	assert.Equal(t, uint64(20), peers[0].GetId())
	assert.Equal(t, "10.0.0.2:19001", peers[0].GetTcp().GetAddr())
	assert.Nil(t, peers[0].GetRdma())
}

func TestRenderConfigAppliesRDMACacheMissBenchmarkOnSource(t *testing.T) {
	dir := writeSource(t, "startup:\n  fabric:\n    auto_rdma: {}\n")
	warmupOps := uint64(25)
	objectSize := uint64(16384)
	readBytes := uint64(4096)
	bench := rdmaLoadgenBenchmark{
		name:            "rdma_source_to_target",
		runLoadgen:      true,
		localNodeID:     10,
		peerNodeID:      20,
		peerAddr:        "hex:0102",
		cacheMiss:       true,
		readBytes:       &readBytes,
		warmupOps:       &warmupOps,
		objectSizeBytes: &objectSize,
		diskPath:        "/var/lib/unbounded/bench-cache.bin",
		diskSizeBytes:   104857600,
	}

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetBackends(), 1)
	require.Len(t, cfg.GetNeighborhoods(), 1)
	require.Len(t, cfg.GetCaches(), 1)
	cache := cfg.GetCaches()[0]
	assert.Equal(t, bench.cacheName(), cache.GetName())
	assert.Equal(t, bench.neighborhoodName(), cache.GetSource())
	assert.Empty(t, cache.GetDisks())

	require.Len(t, cfg.GetFrontends(), 1)
	frontend := cfg.GetFrontends()[0]
	assert.Equal(t, bench.frontendName(), frontend.GetName())
	assert.Equal(t, bench.cacheName(), frontend.GetSource())
	loadgen := frontend.GetLoadgen()
	require.NotNil(t, loadgen)
	assert.Equal(t, uint64(16384), loadgen.GetFixedObjectSizeBytes())
	assert.True(t, loadgen.GetRequireRemotePeer())
	assert.Equal(t, uint64(25), loadgen.GetWarmupOperations())
	assert.Equal(t, uint64(4096), loadgen.GetReadBytes())
}

func TestRenderConfigDefaultsRDMACacheMissWarmupToObjectCount(t *testing.T) {
	dir := writeSource(t, "startup:\n  fabric:\n    auto_rdma: {}\n")
	objectCount := uint64(42)
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  true,
		localNodeID: 10,
		peerNodeID:  20,
		peerAddr:    "hex:0102",
		cacheMiss:   true,
		objectCount: &objectCount,
	}

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetFrontends(), 1)
	assert.Equal(t, uint64(42), cfg.GetFrontends()[0].GetLoadgen().GetWarmupOperations())
}

func TestRenderConfigDefaultsRDMACacheMissObjectAndWarmup(t *testing.T) {
	dir := writeSource(t, "startup:\n  fabric:\n    auto_rdma: {}\n")
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  true,
		localNodeID: 10,
		peerNodeID:  20,
		peerAddr:    "hex:0102",
		cacheMiss:   true,
	}

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetFrontends(), 1)
	loadgen := cfg.GetFrontends()[0].GetLoadgen()
	assert.Equal(t, uint64(1024*1024), loadgen.GetFixedObjectSizeBytes())
	assert.Equal(t, uint64(1_000_000), loadgen.GetWarmupOperations())
}

func TestRenderConfigAppliesRDMACacheMissBenchmarkOnTarget(t *testing.T) {
	dir := writeSource(t, "startup:\n  fabric:\n    auto_rdma: {}\n")
	bench := rdmaLoadgenBenchmark{
		name:          "rdma_source_to_target",
		runLoadgen:    false,
		localNodeID:   20,
		peerNodeID:    10,
		peerAddr:      "hex:0102",
		cacheMiss:     true,
		diskPath:      "/var/lib/unbounded/bench-cache.bin",
		diskSizeBytes: 104857600,
	}

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetBackends(), 1)
	require.Len(t, cfg.GetNeighborhoods(), 1)
	require.Len(t, cfg.GetCaches(), 1)
	cache := cfg.GetCaches()[0]
	assert.Equal(t, bench.cacheName(), cache.GetName())
	assert.Equal(t, bench.neighborhoodName(), cache.GetSource())
	require.Len(t, cache.GetDisks(), 1)
	disk := cache.GetDisks()[0]
	assert.True(t, disk.GetSkipRecoveryScan())
	assert.Equal(t, "/var/lib/unbounded/bench-cache.bin", disk.GetFile().GetPath())
	assert.Equal(t, uint64(104857600), disk.GetFile().GetSize())
	assert.Empty(t, cfg.GetFrontends())
}

func TestRenderConfigSkipsRDMABenchmarkOnReservedNameCollision(t *testing.T) {
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  true,
		localNodeID: 10,
		peerNodeID:  20,
		peerAddr:    "hex:0102",
	}
	dir := writeSource(t, `
backends:
  - name: __unbounded_benchmark_rdma_source_to_target_backend
    fake: {}
`)

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetBackends(), 1)
	assert.Empty(t, cfg.GetNeighborhoods())
	assert.Empty(t, cfg.GetFrontends())
}

func TestRenderConfigSkipsRDMACacheMissBenchmarkOnCacheNameCollision(t *testing.T) {
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  true,
		localNodeID: 10,
		peerNodeID:  20,
		peerAddr:    "hex:0102",
		cacheMiss:   true,
	}
	dir := writeSource(t, `
caches:
  - name: __unbounded_benchmark_rdma_source_to_target_cache
    source: origin
`)

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetCaches(), 1)
	assert.Empty(t, cfg.GetBackends())
	assert.Empty(t, cfg.GetNeighborhoods())
	assert.Empty(t, cfg.GetFrontends())
}

func TestRenderConfigSkipsRDMABenchmarkOnLocalIDConflict(t *testing.T) {
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  true,
		localNodeID: 10,
		peerNodeID:  20,
		peerAddr:    "hex:0102",
	}
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
neighborhoods:
  - name: existing
    source: origin
    local_node_id: 99
`)

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetNeighborhoods(), 1)
	assert.Equal(t, "existing", cfg.GetNeighborhoods()[0].GetName())
	assert.Empty(t, cfg.GetFrontends())
}

func TestRenderConfigSkipsRDMABenchmarkOnPeerConflict(t *testing.T) {
	bench := rdmaLoadgenBenchmark{
		name:        "rdma_source_to_target",
		runLoadgen:  true,
		localNodeID: 10,
		peerNodeID:  20,
		peerAddr:    "hex:0102",
	}
	dir := writeSource(t, `
backends:
  - name: origin
    fake: {}
neighborhoods:
  - name: existing
    source: origin
    local_node_id: 10
    peers:
      - id: 20
        rdma:
          addr: "hex:0304"
`)

	cfg := decodeWithBenchmarks(t, dir, benchmarkState{rdmaLoadgens: []rdmaLoadgenBenchmark{bench}})

	require.Len(t, cfg.GetNeighborhoods(), 1)
	assert.Equal(t, "existing", cfg.GetNeighborhoods()[0].GetName())
	assert.Empty(t, cfg.GetFrontends())
}
