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

	state := computeBenchmarks([]*corev1.Node{target, source}, "source-a")

	require.Len(t, state.rdmaLoadgens, 1)
	bench := state.rdmaLoadgens[0]
	assert.Equal(t, "rdma_source-a_to_target-b", bench.name)
	assert.Equal(t, "source-a", bench.sourceNode)
	assert.Equal(t, "target-b", bench.targetNode)
	assert.True(t, bench.runLoadgen)
	assert.Equal(t, nodeID("source-a"), bench.localNodeID)
	assert.Equal(t, nodeID("target-b"), bench.peerNodeID)
	assert.Equal(t, "hex:0a0b", bench.peerAddr)
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

	state := computeBenchmarks([]*corev1.Node{source, target}, "target-b")

	require.Len(t, state.rdmaLoadgens, 1)
	bench := state.rdmaLoadgens[0]
	assert.False(t, bench.runLoadgen)
	assert.Equal(t, nodeID("target-b"), bench.localNodeID)
	assert.Equal(t, nodeID("source-a"), bench.peerNodeID)
	assert.Equal(t, "hex:0102", bench.peerAddr)
	require.NotNil(t, bench.workers)
	assert.Equal(t, uint32(2), *bench.workers)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := []*corev1.Node{benchmarkNode("source-a", tt.source)}
			if tt.includePeer {
				nodes = append(nodes, benchmarkNode("target-b", tt.target))
			}

			state := computeBenchmarks(nodes, "source-a")
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
	assert.Empty(t, cfg.GetFrontends())
	assert.Equal(t, uint64(20), cfg.GetNeighborhoods()[0].GetLocalNodeId())
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
