// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

const (
	benchmarkScenarioAnnotation        = "unbounded-cloud.io/storage-benchmark.scenario"
	benchmarkLegacyScenarioAnnotation  = "unbounded-cloud.io/storage-benchmark"
	benchmarkTargetNodeAnnotation      = "unbounded-cloud.io/storage-benchmark.target-node"
	benchmarkWorkersAnnotation         = "unbounded-cloud.io/storage-benchmark.workers"
	benchmarkSeedAnnotation            = "unbounded-cloud.io/storage-benchmark.seed"
	benchmarkObjectCountAnnotation     = "unbounded-cloud.io/storage-benchmark.object-count"
	benchmarkReadBytesAnnotation       = "unbounded-cloud.io/storage-benchmark.read-bytes"
	benchmarkVerifyAnnotation          = "unbounded-cloud.io/storage-benchmark.verify"
	benchmarkStripeSizeBytesAnnotation = "unbounded-cloud.io/storage-benchmark.stripe-size-bytes"
	benchmarkObjectSizeBytesAnnotation = "unbounded-cloud.io/storage-benchmark.object-size-bytes"
	benchmarkDiskPathAnnotation        = "unbounded-cloud.io/storage-benchmark.disk-path"
	benchmarkDiskSizeBytesAnnotation   = "unbounded-cloud.io/storage-benchmark.disk-size-bytes"
	benchmarkWarmupOpsAnnotation       = "unbounded-cloud.io/storage-benchmark.warmup-operations"
	benchmarkRdmaAddrAnnotation        = "unbounded-cloud.io/storage-rdma.addr"

	rdmaLoadgenScenario   = "rdma-loadgen"
	rdmaCacheMissScenario = "rdma-cache-miss"
	rdmaScenarioAlias     = "rdma"

	benchmarkComponentPrefix = "__unbounded_benchmark_"
)

// benchmarkState is the supervisor-only overlay computed from watched Nodes.
// It is rendered into the daemon's existing config schema; no daemon API is
// added for benchmark control.
type benchmarkState struct {
	rdmaLoadgens []rdmaLoadgenBenchmark
}

type rdmaLoadgenBenchmark struct {
	name string

	sourceNode string
	targetNode string
	runLoadgen bool

	localNodeID uint64
	peerNodeID  uint64
	peerAddr    string
	cacheMiss   bool

	workers     *uint32
	seed        *uint64
	objectCount *uint64
	readBytes   *uint64
	verify      bool
	warmupOps   *uint64

	stripeSizeBytes *uint64
	objectSizeBytes *uint64
	diskPath        string
	diskSizeBytes   uint64
}

// computeBenchmarks turns Node annotations into benchmark overlays relevant to
// selfName. A source node carries the scenario and target annotations; both the
// source and target nodes carry their RDMA fabric address annotation.
func computeBenchmarks(nodes []*corev1.Node, selfName string) benchmarkState {
	if selfName == "" {
		return benchmarkState{}
	}

	byName := make(map[string]*corev1.Node, len(nodes))
	names := make([]string, 0, len(nodes))

	for _, node := range nodes {
		if node == nil || node.Name == "" {
			continue
		}

		if _, exists := byName[node.Name]; !exists {
			names = append(names, node.Name)
		}

		byName[node.Name] = node
	}

	sort.Strings(names)

	state := benchmarkState{}

	for _, sourceName := range names {
		source := byName[sourceName]

		scenario := benchmarkScenario(source)
		if scenario == "" {
			continue
		}

		if !isRDMALoadgenScenario(scenario) && !isRDMACacheMissScenario(scenario) {
			slog.Warn("skipping storage benchmark with unsupported scenario",
				"node", sourceName, "scenario", scenario)

			continue
		}

		bench, ok := parseRDMALoadgenBenchmark(source, byName, selfName, isRDMACacheMissScenario(scenario))
		if !ok {
			continue
		}

		state.rdmaLoadgens = append(state.rdmaLoadgens, bench)
	}

	sort.Slice(state.rdmaLoadgens, func(i, j int) bool {
		return state.rdmaLoadgens[i].name < state.rdmaLoadgens[j].name
	})

	return state
}

func parseRDMALoadgenBenchmark(source *corev1.Node, nodes map[string]*corev1.Node, selfName string, cacheMiss bool) (rdmaLoadgenBenchmark, bool) {
	targetName := strings.TrimSpace(source.Annotations[benchmarkTargetNodeAnnotation])
	if targetName == "" {
		slog.Warn("skipping RDMA storage benchmark without target node annotation",
			"node", source.Name, "annotation", benchmarkTargetNodeAnnotation)

		return rdmaLoadgenBenchmark{}, false
	}

	if targetName == source.Name {
		slog.Warn("skipping RDMA storage benchmark whose target is the source node", "node", source.Name)

		return rdmaLoadgenBenchmark{}, false
	}

	target := nodes[targetName]
	if target == nil {
		slog.Warn("skipping RDMA storage benchmark with missing target node",
			"node", source.Name, "target", targetName)

		return rdmaLoadgenBenchmark{}, false
	}

	if selfName != source.Name && selfName != targetName {
		return rdmaLoadgenBenchmark{}, false
	}

	sourceAddr, ok := rdmaAddr(source)
	if !ok {
		slog.Warn("skipping RDMA storage benchmark because source node has no valid RDMA address",
			"node", source.Name, "annotation", benchmarkRdmaAddrAnnotation)

		return rdmaLoadgenBenchmark{}, false
	}

	targetAddr, ok := rdmaAddr(target)
	if !ok {
		slog.Warn("skipping RDMA storage benchmark because target node has no valid RDMA address",
			"node", source.Name, "target", targetName, "annotation", benchmarkRdmaAddrAnnotation)

		return rdmaLoadgenBenchmark{}, false
	}

	sourceID := nodeID(source.Name)

	targetID := nodeID(targetName)
	if sourceID == targetID {
		slog.Warn("skipping RDMA storage benchmark: node id collision",
			"node", source.Name, "target", targetName, "id", sourceID)

		return rdmaLoadgenBenchmark{}, false
	}

	workers, ok := parseUint32Annotation(source, benchmarkWorkersAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	seed, ok := parseUint64Annotation(source, benchmarkSeedAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	objectCount, ok := parseUint64Annotation(source, benchmarkObjectCountAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	readBytes, ok := parseUint64Annotation(source, benchmarkReadBytesAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	stripeSizeBytes, ok := parseUint64Annotation(source, benchmarkStripeSizeBytesAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	objectSizeBytes, ok := parseUint64Annotation(source, benchmarkObjectSizeBytesAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	verify, ok := parseBoolAnnotation(source, benchmarkVerifyAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	warmupOps, ok := parseUint64Annotation(source, benchmarkWarmupOpsAnnotation)
	if !ok {
		return rdmaLoadgenBenchmark{}, false
	}

	diskPath := ""
	diskSizeBytes := uint64(0)
	if cacheMiss {
		diskPath = strings.TrimSpace(source.Annotations[benchmarkDiskPathAnnotation])
		if diskPath == "" {
			slog.Warn("skipping RDMA cache-miss storage benchmark without disk path annotation",
				"node", source.Name, "annotation", benchmarkDiskPathAnnotation)

			return rdmaLoadgenBenchmark{}, false
		}

		parsedDiskSizeBytes, ok := parseRequiredUint64Annotation(source, benchmarkDiskSizeBytesAnnotation)
		if !ok {
			return rdmaLoadgenBenchmark{}, false
		}

		diskSizeBytes = parsedDiskSizeBytes
	}

	bench := rdmaLoadgenBenchmark{
		name:            benchmarkName(source.Name, targetName),
		sourceNode:      source.Name,
		targetNode:      targetName,
		runLoadgen:      selfName == source.Name,
		localNodeID:     sourceID,
		peerNodeID:      targetID,
		peerAddr:        targetAddr,
		cacheMiss:       cacheMiss,
		workers:         workers,
		seed:            seed,
		objectCount:     objectCount,
		readBytes:       readBytes,
		verify:          verify,
		warmupOps:       warmupOps,
		stripeSizeBytes: stripeSizeBytes,
		objectSizeBytes: objectSizeBytes,
		diskPath:        diskPath,
		diskSizeBytes:   diskSizeBytes,
	}

	if selfName == targetName {
		bench.localNodeID = targetID
		bench.peerNodeID = sourceID
		bench.peerAddr = sourceAddr
	}

	return bench, true
}

func applyBenchmarks(cfg *storageconfig.Config, benchmarks benchmarkState) {
	for _, bench := range benchmarks.rdmaLoadgens {
		applyRDMALoadgenBenchmark(cfg, bench)
	}
}

func applyRDMALoadgenBenchmark(cfg *storageconfig.Config, bench rdmaLoadgenBenchmark) {
	backendName := bench.backendName()
	neighborhoodName := bench.neighborhoodName()
	cacheName := bench.cacheName()
	frontendName := bench.frontendName()

	reservedNames := []string{backendName, neighborhoodName}
	if bench.cacheMiss {
		reservedNames = append(reservedNames, cacheName)
	}

	for _, name := range reservedNames {
		if componentNameExists(cfg, name) {
			slog.Warn("skipping RDMA storage benchmark: reserved component name already exists",
				"benchmark", bench.name, "component", name)

			return
		}
	}

	if bench.runLoadgen && componentNameExists(cfg, frontendName) {
		slog.Warn("skipping RDMA storage benchmark: reserved component name already exists",
			"benchmark", bench.name, "component", frontendName)

		return
	}

	if !localNodeIDCompatible(cfg, bench.localNodeID) {
		slog.Warn("skipping RDMA storage benchmark: existing neighborhoods use a different local_node_id",
			"benchmark", bench.name, "local_node_id", bench.localNodeID)

		return
	}

	peer := rdmaPeer(bench.peerNodeID, bench.peerAddr)
	if !peerCompatible(cfg, peer) {
		slog.Warn("skipping RDMA storage benchmark: peer id is already declared with different peer data",
			"benchmark", bench.name, "peer_id", bench.peerNodeID)

		return
	}

	cfg.Backends = append(cfg.Backends, &storageconfig.BackendSpec{
		Name: backendName,
		Config: &storageconfig.BackendSpec_Fake{
			Fake: &storageconfig.FakeBackendConfig{
				StripeSizeBytes: bench.stripeSizeBytes,
				ObjectSizeBytes: bench.objectSizeBytes,
			},
		},
	})

	cfg.Neighborhoods = append(cfg.Neighborhoods, &storageconfig.NeighborhoodSpec{
		Name:        neighborhoodName,
		Source:      backendName,
		LocalNodeId: proto.Uint64(bench.localNodeID),
		Peers:       []*storageconfig.PeerSpec{peer},
	})

	if bench.cacheMiss {
		cfg.Caches = append(cfg.Caches, benchmarkCache(bench, cacheName, neighborhoodName))
	}

	if bench.runLoadgen {
		frontendSource := neighborhoodName
		if bench.cacheMiss {
			frontendSource = cacheName
		}

		cfg.Frontends = append(cfg.Frontends, &storageconfig.FrontendSpec{
			Name:   frontendName,
			Source: frontendSource,
			Config: &storageconfig.FrontendSpec_Loadgen{
				Loadgen: benchmarkLoadgenConfig(bench),
			},
		})
	}
}

func benchmarkLoadgenConfig(bench rdmaLoadgenBenchmark) *storageconfig.LoadgenFrontendConfig {
	cfg := &storageconfig.LoadgenFrontendConfig{
		Workers:     bench.workers,
		Seed:        bench.seed,
		ObjectCount: bench.objectCount,
		ReadBytes:   bench.readBytes,
		Verify:      bench.verify,
	}

	if bench.cacheMiss {
		cfg.FixedObjectSizeBytes = proto.Uint64(bench.effectiveObjectSizeBytes())
		cfg.RequireRemotePeer = proto.Bool(true)
		cfg.WarmupOperations = proto.Uint64(bench.effectiveWarmupOperations())
	}

	return cfg
}

func benchmarkCache(bench rdmaLoadgenBenchmark, cacheName, neighborhoodName string) *storageconfig.CacheSpec {
	cache := &storageconfig.CacheSpec{Name: cacheName, Source: neighborhoodName}
	if !bench.runLoadgen {
		cache.Disks = []*storageconfig.DiskSpec{
			{
				SkipRecoveryScan: true,
				Config: &storageconfig.DiskSpec_File{
					File: &storageconfig.FileDiskConfig{
						Path: bench.diskPath,
						Size: proto.Uint64(bench.diskSizeBytes),
					},
				},
			},
		}
	}

	return cache
}

func benchmarkScenario(node *corev1.Node) string {
	if node == nil || node.Annotations == nil {
		return ""
	}

	if scenario := strings.TrimSpace(node.Annotations[benchmarkScenarioAnnotation]); scenario != "" {
		return scenario
	}

	return strings.TrimSpace(node.Annotations[benchmarkLegacyScenarioAnnotation])
}

func isRDMALoadgenScenario(scenario string) bool {
	switch strings.ToLower(strings.TrimSpace(scenario)) {
	case rdmaLoadgenScenario, rdmaScenarioAlias:
		return true
	default:
		return false
	}
}

func isRDMACacheMissScenario(scenario string) bool {
	return strings.ToLower(strings.TrimSpace(scenario)) == rdmaCacheMissScenario
}

func rdmaAddr(node *corev1.Node) (string, bool) {
	if node == nil || node.Annotations == nil {
		return "", false
	}

	addr := strings.TrimSpace(node.Annotations[benchmarkRdmaAddrAnnotation])
	if addr == "" || !strings.HasPrefix(strings.ToLower(addr), "hex:") {
		return "", false
	}

	payload := addr[len("hex:"):]
	if payload == "" || len(payload)%2 != 0 {
		return "", false
	}

	if _, err := hex.DecodeString(payload); err != nil {
		return "", false
	}

	return "hex:" + strings.ToLower(payload), true
}

func parseUint32Annotation(node *corev1.Node, key string) (*uint32, bool) {
	raw := strings.TrimSpace(node.Annotations[key])
	if raw == "" {
		return nil, true
	}

	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		slog.Warn("skipping RDMA storage benchmark: annotation must be a uint32",
			"node", node.Name, "annotation", key, "value", raw)

		return nil, false
	}

	out := uint32(v)

	return &out, true
}

func parseUint64Annotation(node *corev1.Node, key string) (*uint64, bool) {
	raw := strings.TrimSpace(node.Annotations[key])
	if raw == "" {
		return nil, true
	}

	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		slog.Warn("skipping RDMA storage benchmark: annotation must be a uint64",
			"node", node.Name, "annotation", key, "value", raw)

		return nil, false
	}

	return &v, true
}

func parseRequiredUint64Annotation(node *corev1.Node, key string) (uint64, bool) {
	raw := strings.TrimSpace(node.Annotations[key])
	if raw == "" {
		slog.Warn("skipping RDMA storage benchmark: missing required uint64 annotation",
			"node", node.Name, "annotation", key)

		return 0, false
	}

	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		slog.Warn("skipping RDMA storage benchmark: annotation must be a uint64",
			"node", node.Name, "annotation", key, "value", raw)

		return 0, false
	}

	return v, true
}

func parseBoolAnnotation(node *corev1.Node, key string) (bool, bool) {
	raw := strings.TrimSpace(node.Annotations[key])
	if raw == "" {
		return false, true
	}

	v, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("skipping RDMA storage benchmark: annotation must be a bool",
			"node", node.Name, "annotation", key, "value", raw)

		return false, false
	}

	return v, true
}

func benchmarkName(sourceNode, targetNode string) string {
	return "rdma_" + safeComponentPart(sourceNode) + "_to_" + safeComponentPart(targetNode)
}

func safeComponentPart(s string) string {
	var b strings.Builder

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	if b.Len() == 0 {
		return "node"
	}

	return b.String()
}

func (b rdmaLoadgenBenchmark) backendName() string {
	return fmt.Sprintf("%s%s_backend", benchmarkComponentPrefix, b.name)
}

func (b rdmaLoadgenBenchmark) neighborhoodName() string {
	return fmt.Sprintf("%s%s_neighborhood", benchmarkComponentPrefix, b.name)
}

func (b rdmaLoadgenBenchmark) cacheName() string {
	return fmt.Sprintf("%s%s_cache", benchmarkComponentPrefix, b.name)
}

func (b rdmaLoadgenBenchmark) frontendName() string {
	return fmt.Sprintf("%s%s_loadgen", benchmarkComponentPrefix, b.name)
}

func (b rdmaLoadgenBenchmark) effectiveObjectSizeBytes() uint64 {
	if b.objectSizeBytes != nil {
		return *b.objectSizeBytes
	}

	return 1024 * 1024
}

func (b rdmaLoadgenBenchmark) effectiveWarmupOperations() uint64 {
	if b.warmupOps != nil {
		return *b.warmupOps
	}

	if b.objectCount != nil {
		return *b.objectCount
	}

	return 1_000_000
}

func rdmaPeer(id uint64, addr string) *storageconfig.PeerSpec {
	return &storageconfig.PeerSpec{
		Id: id,
		Config: &storageconfig.PeerSpec_Rdma{
			Rdma: &storageconfig.RdmaPeerConfig{Addr: addr},
		},
	}
}

func componentNameExists(cfg *storageconfig.Config, name string) bool {
	for _, backend := range cfg.GetBackends() {
		if backend.GetName() == name {
			return true
		}
	}

	for _, cache := range cfg.GetCaches() {
		if cache.GetName() == name {
			return true
		}
	}

	for _, neighborhood := range cfg.GetNeighborhoods() {
		if neighborhood.GetName() == name {
			return true
		}
	}

	for _, frontend := range cfg.GetFrontends() {
		if frontend.GetName() == name {
			return true
		}
	}

	return false
}

func localNodeIDCompatible(cfg *storageconfig.Config, localNodeID uint64) bool {
	for _, neighborhood := range cfg.GetNeighborhoods() {
		if neighborhood.LocalNodeId != nil && neighborhood.GetLocalNodeId() != localNodeID {
			return false
		}
	}

	return true
}

func peerCompatible(cfg *storageconfig.Config, peer *storageconfig.PeerSpec) bool {
	for _, neighborhood := range cfg.GetNeighborhoods() {
		for _, existing := range neighborhood.GetPeers() {
			if existing.GetId() == peer.GetId() && !proto.Equal(existing, peer) {
				return false
			}
		}
	}

	return true
}
