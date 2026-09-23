// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer"
)

// Immutable topology identities and persisted volume state.

const (
	defaultSlots     uint32 = racer.SlotCount
	generationFormat int    = 3
)

func identity(domain, value string) string {
	return racer.Identity(domain, value)
}

func identityBytes(domain, value string) [32]byte {
	return sha256.Sum256([]byte("racer/" + domain + "/v1\x00" + value))
}

type member struct {
	ID     string `json:"id"`
	IP     string `json:"ip,omitempty"`
	Fabric string `json:"fabric,omitempty"`
	PodUID string `json:"podUID,omitempty"`
	// Optional for compatibility with generations persisted before Pod GET checks.
	PodNamespace string `json:"podNamespace,omitempty"`
	PodName      string `json:"podName,omitempty"`
}

type volumeSpec struct {
	ID                 string `json:"id"`
	Name               string `json:"name,omitempty"`
	ResourceGeneration int64  `json:"resourceGeneration,omitempty"`
	CacheSocket        string `json:"cacheSocket,omitempty"`
	OriginSocket       string `json:"originSocket,omitempty"`
	Port               int32  `json:"port"`
	Slots              uint32 `json:"slots"`
	Cache              uint64 `json:"cache"`
	Algorithm          uint32 `json:"algorithm"`
	Attempts           uint32 `json:"attempts"`
}

type originSpec struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

func (o originSpec) address(podIP string) string {
	if net.ParseIP(podIP).To4() != nil {
		return o.IPv4
	}

	return o.IPv6
}

// generation is immutable after commit. Historical recipients and slot counts
// survive removals, so a reconnecting process receives a monotonic replacement.
type generation struct {
	Format      int               `json:"format"`
	Universe    string            `json:"universe"`
	Revision    uint64            `json:"revision"`
	Nodes       map[string]member `json:"nodes"`
	Volume      *volumeSpec       `json:"volume,omitempty"`
	Owners      []string          `json:"owners,omitempty"`
	SlotHistory map[string]uint32 `json:"slotHistory"`
	Ports       map[string]int32  `json:"ports"`
	Withdrawn   map[string]bool   `json:"withdrawn,omitempty"`
	// Keep the primary volume fields readable by existing persisted generations.
	Additional []volumeState `json:"additional,omitempty"`
}

type volumeState struct {
	Volume *volumeSpec `json:"volume"`
	Owners []string    `json:"owners,omitempty"`
}

func (g *generation) volumes() []volumeState {
	var result []volumeState
	if g.Volume != nil {
		result = append(result, volumeState{g.Volume, g.Owners})
	}

	return append(result, g.Additional...)
}

// Balanced slot placement and physical failure diversity.

func degree(p uint32) uint32 {
	d := uint32(1)
	for uint64(d)*uint64(d)*uint64(d) < uint64(p) {
		d++
	}

	return d
}

// place retains balanced prior ownership, then repairs physical failure diversity.
// This also migrates persisted block layouts on ordinary reconciliation, not load:
// the changed Owners vector is committed with a new revision/epoch before serving.
func place(p uint32, names, previous []string) ([]string, error) {
	if p == 0 || p > 262144 || len(names) == 0 || len(names) > int(p) || len(names) > 100000 {
		return nil, fmt.Errorf("%d slots require 1..min(slots,100000) participants; have %d", p, len(names))
	}

	names = append([]string(nil), names...)
	sort.Strings(names)

	quota := make(map[string]int, len(names))
	for i, name := range names {
		if name == "" || (i > 0 && name == names[i-1]) {
			return nil, fmt.Errorf("empty or duplicate participant %q", name)
		}

		quota[name] = int(p) / len(names)
		if i < int(p)%len(names) {
			quota[name]++
		}
	}

	if len(names) == 2 {
		return placePair(int(p), names, previous), nil
	}

	owners := make([]string, p)

	counts := make(map[string]int, len(names))
	for slot, name := range previous {
		if slot >= len(owners) {
			break
		}

		if quota[name] > 0 {
			owners[slot] = name
			counts[name]++
		}
	}

	var deficits []string

	for _, name := range names {
		if counts[name] < quota[name] {
			deficits = append(deficits, name)
		}
	}

	i := 0
	// Prefer replacements which do not create an equal adjacency. A second
	// pass fills any remaining quota holes before the bounded diversity repair.
	for pass := 0; pass < 2; pass++ {
		for slot, old := range owners {
			if old != "" && counts[old] <= quota[old] {
				continue
			}

			for counts[deficits[i]] == quota[deficits[i]] {
				deficits[i] = deficits[len(deficits)-1]
				deficits = deficits[:len(deficits)-1]
				i %= len(deficits)
			}

			name := deficits[i]
			if pass == 0 && !placementSafe(owners, slot, name) {
				found := false

				for offset := 1; offset < min(len(deficits), 8); offset++ {
					j := (i + offset) % len(deficits)
					if counts[deficits[j]] < quota[deficits[j]] && placementSafe(owners, slot, deficits[j]) {
						i = j
						name = deficits[i]
						found = true

						break
					}
				}

				if !found {
					continue
				}
			}

			counts[old]--
			owners[slot] = name
			counts[name]++
			i = (i + 1) % len(deficits)
		}
	}

	if len(names) > 2 {
		if err := diversify(owners); err != nil {
			// Bounded repair can get stuck in a local minimum. A deterministic
			// proper balanced interleaving is preferable to rejecting membership.
			for slot := range owners {
				owners[slot] = names[slot%len(names)]
			}

			if owners[0] == owners[len(owners)-1] {
				owners[len(owners)-1], owners[len(owners)-2] = owners[len(owners)-2], owners[len(owners)-1]
			}
		}
	}

	return owners, nil
}

func placementSafe(owners []string, slot int, name string) bool {
	p := len(owners)
	return owners[(slot+p-1)%p] != name && owners[(slot+1)%p] != name
}

// With two owners the even ring has exactly two proper interleavings. Choose
// the one retaining most ownership (lexical phase breaks ties). An odd ring
// necessarily has one repeated adjacency; evaluate every position of that seam
// in O(P), retaining the best phase rather than rebuilding at an arbitrary seam.
func placePair(p int, names, previous []string) []string {
	match := func(slot int, name string) int {
		if slot < len(previous) && previous[slot] == name {
			return 1
		}

		return 0
	}

	score := 0
	for i := 0; i < p; i++ {
		score += match(i, names[i%2])
	}

	best, seam := score, 0

	if p%2 == 0 {
		other := 0
		for i := 0; i < p; i++ {
			other += match(i, names[1-i%2])
		}

		if other > best {
			seam = 1
		}
	} else {
		for k, step := 0, 1; step < p; step++ {
			j := (k + 1) % p
			score += match(k, names[1]) + match(j, names[0]) - match(k, names[0]) - match(j, names[1])

			k = (k + 2) % p
			if score > best {
				best, seam = score, k
			}
		}
	}

	owners := make([]string, p)
	for i := range owners {
		owners[i] = names[(i-seam+p)%p%2]
	}

	return owners
}

// For >=3 balanced owners, repair equal adjacencies by quota-preserving swaps.
// Select the first offending slot and scan donors in stable cyclic slot order.
// A swap must make both changed slots proper, so it cannot create new conflicts.
// Unchanged valid layouts are fixed points. This is retention-first local repair,
// not a claim of a global minimum recoloring for arbitrary membership histories.
func diversify(owners []string) error {
	p := len(owners)
	donor := 0

	budget := 16 * p
	for slot := 0; slot < p; slot++ {
		if owners[slot] != owners[(slot+p-1)%p] {
			continue
		}

		found := false

		for tried := 0; tried < 2*p && budget > 0; tried++ {
			budget--

			x := slot
			if tried >= p {
				x = (slot + p - 1) % p
			}

			y := donor
			donor = (donor + 1) % p

			if owners[y] == owners[x] {
				continue
			}

			owners[x], owners[y] = owners[y], owners[x]
			if placementSafe(owners, x, owners[x]) && placementSafe(owners, y, owners[y]) {
				found = true
				break
			}

			owners[x], owners[y] = owners[y], owners[x]
		}

		if !found {
			return fmt.Errorf("physical interleaving repair exhausted donors")
		}
	}

	return nil
}

// Recipient indexing, admission budgets and wire snapshots.

type topologyIndex struct {
	g          *generation
	local      map[string][]uint32
	byID       map[string]string
	additional []*topologyIndex
}

func indexGeneration(g *generation) (*topologyIndex, error) {
	t := &topologyIndex{g: g, local: map[string][]uint32{}, byID: map[string]string{}}
	for _, volume := range g.Additional {
		child := *g
		child.Volume, child.Owners, child.Additional = volume.Volume, volume.Owners, nil

		index, err := indexGeneration(&child)
		if err != nil {
			return nil, err
		}

		t.additional = append(t.additional, index)
	}

	for name, node := range g.Nodes {
		if _, exists := t.byID[node.ID]; exists {
			return nil, fmt.Errorf("duplicate node identity %s", node.ID)
		}

		t.byID[node.ID] = name
	}

	if g.Volume == nil {
		return t, nil
	}

	if len(g.Owners) == 0 {
		return t, nil
	} // Explicitly empty when no Pods are available.

	if len(g.Owners) != int(g.Volume.Slots) {
		return nil, fmt.Errorf("incomplete slot ownership")
	}

	for slot, name := range g.Owners {
		node, ok := g.Nodes[name]
		if !ok || node.IP == "" {
			return nil, fmt.Errorf("slot %d has unavailable owner %q", slot, name)
		}

		t.local[name] = append(t.local[name], uint32(slot))
	}

	return t, nil
}

// Conservative profile-1 admission before durable commit. Counts are independent
// of protobuf compression and include both scoped and global direct-peer copies.
// Exact successor validation remains the dataplane's responsibility.
func (t *topologyIndex) admit() error {
	indexes := append([]*topologyIndex{t}, t.additional...)
	if len(indexes) > 64 {
		return fmt.Errorf("profile 1 supports at most 64 volumes")
	}

	for name := range t.g.Nodes {
		var work, records, wire uint64

		for _, v := range indexes {
			l := uint64(len(v.local[name]))
			if l == 0 {
				continue
			}

			p := uint64(v.g.Volume.Slots)
			d := uint64(degree(uint32(p)))
			edges := min(p-l, l*d)
			direct := min(uint64(len(v.local)-1), 2*l*d)
			work += 64 * l
			records += l + edges + 4*direct
			wire += 5*l + 80*edges + 2048*direct + uint64(len(v.g.Volume.CacheSocket)+len(v.g.Volume.OriginSocket)+len(v.g.Volume.ID)) + 1024
		}

		if work > 64*1024*1024 || records > 2*1024*1024 || wire > 64*1024*1024-1024 {
			return fmt.Errorf("node %s exceeds profile-1 configuration budget (work=%d records=%d bytes<=%d)", name, work, records, wire)
		}
	}

	return nil
}

// Reverse edges are needed for incoming authentication. They can be computed
// in O(local_slots * degree), without retaining an O(nodes^2) adjacency matrix.
func (t *topologyIndex) connections(name string) ([]*pb.SlotPeer, map[string]bool, map[string]bool) {
	p := uint64(t.g.Volume.Slots)
	d := uint64(degree(uint32(p)))
	out, direct := map[string]bool{}, map[string]bool{}
	// Slot IDs are dense and bounded. Mark them before resolving owners so a
	// high-degree local window does not hash the same remote owner per edge.
	edges := make([]bool, p)
	incoming := make([]bool, p)

	for _, slot := range t.local[name] {
		for digit := uint64(0); digit < d; digit++ {
			next := uint32((uint64(slot)*d + digit) % p)

			edges[next] = true

			previous := (uint64(slot) + digit*p) / d

			incoming[previous] = true
		}
	}

	var neighbors []*pb.SlotPeer

	for slot, owner := range t.g.Owners {
		if owner == name {
			continue
		}

		if edges[slot] {
			neighbors = append(neighbors, &pb.SlotPeer{Slot: uint32(slot), Peer: t.g.Nodes[owner].ID})
			out[owner] = true
		}

		if edges[slot] || incoming[slot] {
			direct[owner] = true
		}
	}

	return neighbors, out, direct
}

func (t *topologyIndex) snapshot(id string) *pb.Snapshot {
	s := t.singleSnapshot(id)
	if s == nil {
		return nil
	}

	peers := make(map[string]*pb.Peer)
	for _, peer := range s.Peers {
		peers[peer.Id] = peer
	}

	for _, child := range t.additional {
		// Revision is assigned after indexing, immediately before durable commit.
		part := child.singleSnapshot(id)
		for _, volume := range part.Volumes {
			volume.Topology.Epoch = t.g.Revision
		}

		s.Volumes = append(s.Volumes, part.Volumes...)
		for _, peer := range part.Peers {
			if peers[peer.Id] == nil {
				peers[peer.Id] = peer
			}
		}
	}

	s.Peers = nil
	for _, peer := range peers {
		s.Peers = append(s.Peers, peer)
	}

	sort.Slice(s.Peers, func(i, j int) bool { return s.Peers[i].Id < s.Peers[j].Id })

	return s
}

func (t *topologyIndex) singleSnapshot(id string) *pb.Snapshot {
	name, ok := t.byID[id]
	if !ok {
		return nil
	}

	node := t.g.Nodes[name]
	u := identityBytes("universe", t.g.Universe)

	n, err := hex.DecodeString(id)
	if err != nil {
		return nil
	}

	s := &pb.Snapshot{Universe: u[:], Node: n, Revision: t.g.Revision, Fabric: node.Fabric, Epoch: t.g.Revision}
	// IP is cleared before each membership selection. Historical recipients keep
	// only removal authority, never readiness, even if their Pod still exists.
	s.Idle = len(t.g.volumes()) == 0 && node.IP != "" && node.PodUID != ""
	if len(t.local[name]) == 0 {
		return s
	}

	v := t.g.Volume

	neighbors, outgoing, direct := t.connections(name)
	for peer := range direct {
		remote := t.g.Nodes[peer]
		s.Peers = append(s.Peers, &pb.Peer{Id: remote.ID, HttpAddress: net.JoinHostPort(remote.IP, strconv.Itoa(int(v.Port))), Fabric: remote.Fabric})
	}

	sort.Slice(s.Peers, func(i, j int) bool { return s.Peers[i].Id < s.Peers[j].Id })

	peers := make([]string, 0, len(outgoing))
	for name := range outgoing {
		peers = append(peers, t.g.Nodes[name].ID)
	}

	sort.Strings(peers)

	listen := "0.0.0.0"
	if net.ParseIP(node.IP).To4() == nil {
		listen = "::"
	}

	s.Volumes = []*pb.Volume{{
		Id: v.ID, PeerListen: net.JoinHostPort(listen, strconv.Itoa(int(v.Port))), CacheSocket: v.CacheSocket, OriginSocket: v.OriginSocket, CacheGeneration: v.Cache,
		Peers: peers, Topology: &pb.Topology{Epoch: t.g.Revision, SlotCount: v.Slots, LocalSlots: t.local[name], Neighbors: neighbors, RoutingAlgorithm: &v.Algorithm}, MaxCandidateAttempts: &v.Attempts,
	}}
	{
		scope := &pb.VolumePeerEndpoints{}
		for _, peer := range s.Peers {
			scope.Peers = append(scope.Peers, &pb.VolumePeerEndpoint{Peer: peer.Id, HttpAddress: peer.HttpAddress})
		}

		s.Volumes[0].PeerEndpoints = scope
	}

	return s
}
