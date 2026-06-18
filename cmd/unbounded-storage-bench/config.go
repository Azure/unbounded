// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

type scenarioKind string

const (
	scenarioBlockDisk  scenarioKind = "block-disk"
	scenarioFabricRPC  scenarioKind = "fabric-rpc"
	scenarioIntegrated scenarioKind = "integrated"
)

type scenarioConfig struct {
	Scenario       scenarioKind
	Node           nodeSpec
	Nodes          []nodeSpec
	Workers        uint
	ObjectSize     uint64
	ObjectCount    uint64
	ReadBytes      uint64
	StripeSize     uint64
	DiskSize       uint64
	MemoryBytes    uint64
	ServingCores   uint64
	FabricProvider string
}

type renderedNodeConfig struct {
	node   nodeSpec
	config string
}

type configTemplateData struct {
	Scenario       scenarioKind
	Node           nodeSpec
	BackendName    string
	Neighborhood   bool
	Cache          bool
	FrontendSource string
	CacheSource    string
	Peers          []nodeSpec
	Workers        uint
	ObjectSize     uint64
	ObjectCount    uint64
	ReadBytes      uint64
	StripeSize     uint64
	DiskSize       uint64
	MemoryBytes    uint64
	ServingCores   uint64
	DisableRDMA    bool
	PeerTable      string
}

var storageConfigTemplate = template.Must(template.New("storage-config").Parse(strings.TrimSpace(`
[[backends]]
name = "{{ .BackendName }}"

[backends.config.fake]
stripe_size_bytes = {{ .StripeSize }}
object_size_bytes = {{ .ObjectSize }}
{{ if .Neighborhood }}

[[neighborhoods]]
name = "p2p"
source = "{{ .BackendName }}"
local_node_id = {{ .Node.ID }}
{{ .PeerTable }}{{ end }}
{{ if .Cache }}

[[caches]]
name = "cache"
source = "{{ .CacheSource }}"

[[caches.disks]]
page_size_bytes = 4096
skip_recovery_scan = true
{{ if .Node.BlockDevice }}
[caches.disks.config.block]
path = "{{ .Node.BlockDevice }}"
{{ else }}
[caches.disks.config.file]
path = "{{ .Node.DiskPath }}"
size = {{ .DiskSize }}
{{ end }}{{ end }}

[[frontends]]
name = "loadgen"
source = "{{ .FrontendSource }}"

[frontends.config.loadgen]
workers = {{ .Workers }}
object_count = {{ .ObjectCount }}
read_bytes = {{ .ReadBytes }}
verify = true

[startup.memory]
no_hugepages = true
memory_total_bytes = {{ .MemoryBytes }}

[startup.fabric]
addr = "{{ .Node.ListenAddr }}"

[startup.metrics]
addr = "{{ .Node.MetricsAddr }}"

[startup.topology]
disable_rdma = {{ .DisableRDMA }}
serving_cores = {{ .ServingCores }}
nic_workers = 1
`) + "\n"))

func renderConfig(cfg scenarioConfig) (string, error) {
	data := configTemplateData{
		Scenario:     cfg.Scenario,
		Node:         cfg.Node,
		BackendName:  "origin",
		Workers:      cfg.Workers,
		ObjectSize:   cfg.ObjectSize,
		ObjectCount:  cfg.ObjectCount,
		ReadBytes:    cfg.ReadBytes,
		StripeSize:   cfg.StripeSize,
		DiskSize:     cfg.DiskSize,
		MemoryBytes:  cfg.MemoryBytes,
		ServingCores: cfg.ServingCores,
		DisableRDMA:  cfg.FabricProvider == "tcp",
		PeerTable:    renderPeers(cfg.Node, cfg.Nodes, cfg.FabricProvider),
	}

	switch cfg.Scenario {
	case scenarioBlockDisk:
		data.Cache = true
		data.CacheSource = data.BackendName
		data.FrontendSource = "cache"
	case scenarioFabricRPC:
		data.Neighborhood = true
		data.FrontendSource = "p2p"
	case scenarioIntegrated:
		data.Neighborhood = true
		data.Cache = true
		data.CacheSource = "p2p"
		data.FrontendSource = "cache"
	default:
		return "", fmt.Errorf("unknown scenario %q", cfg.Scenario)
	}

	if data.Workers == 0 {
		data.Workers = 1
	}

	if data.ObjectCount == 0 {
		data.ObjectCount = 1
	}

	if data.StripeSize == 0 {
		data.StripeSize = 4 * 1024 * 1024
	}

	if data.MemoryBytes == 0 {
		data.MemoryBytes = 128 * 1024 * 1024
	}

	if data.ServingCores == 0 {
		data.ServingCores = 1
	}

	var out bytes.Buffer
	if err := storageConfigTemplate.Execute(&out, data); err != nil {
		return "", err
	}

	return out.String(), nil
}

func renderPeers(local nodeSpec, nodes []nodeSpec, provider string) string {
	peers := make([]nodeSpec, 0, len(nodes)-1)
	for _, node := range nodes {
		if node.ID != local.ID {
			peers = append(peers, node)
		}
	}

	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })

	var out strings.Builder
	for _, peer := range peers {
		fmt.Fprintf(&out, "\n[[neighborhoods.peers]]\nid = %d\n", peer.ID)
		if provider == "rdma" {
			fmt.Fprintf(&out, "\n[neighborhoods.peers.config.rdma]\naddr = %q\n", peer.FabricAddr)
		} else {
			fmt.Fprintf(&out, "\n[neighborhoods.peers.config.tcp]\naddr = %q\n", peer.FabricAddr)
		}
	}

	if len(peers) > 0 {
		successor, predecessor := ringNeighbors(local, peers)
		fmt.Fprintf(&out, "\n[neighborhoods.routing_plan]\nfingers = [%s]\nsuccessor = %d\npredecessor = %d\n", peerIDs(peers), successor, predecessor)
	}

	return out.String()
}

func ringNeighbors(local nodeSpec, peers []nodeSpec) (uint64, uint64) {
	localRing := nodeToRing(local.ID)
	successor := peers[0].ID
	predecessor := peers[0].ID
	successorDistance := ringDistance(localRing, nodeToRing(successor))
	predecessorDistance := ringDistance(nodeToRing(predecessor), localRing)

	for _, peer := range peers[1:] {
		peerRing := nodeToRing(peer.ID)
		forward := ringDistance(localRing, peerRing)
		backward := ringDistance(peerRing, localRing)

		if forward < successorDistance || forward == successorDistance && peer.ID < successor {
			successor = peer.ID
			successorDistance = forward
		}

		if backward < predecessorDistance || backward == predecessorDistance && peer.ID < predecessor {
			predecessor = peer.ID
			predecessorDistance = backward
		}
	}

	return successor, predecessor
}

func nodeToRing(id uint64) uint64 {
	x := id
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb

	return x ^ (x >> 31)
}

func ringDistance(from, to uint64) uint64 {
	return to - from
}

func peerIDs(peers []nodeSpec) string {
	ids := make([]string, 0, len(peers))
	for _, peer := range peers {
		ids = append(ids, fmt.Sprint(peer.ID))
	}

	return strings.Join(ids, ", ")
}
