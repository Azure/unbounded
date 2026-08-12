// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"fmt"
	"log/slog"
	"sort"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// BuildClusterState folds the Kubernetes objects that carry racer's desired
// state into the form the derivation understands.
//
// There is no custom resource anywhere in this: a StorageClass whose
// provisioner is racer's driver *is* a universe, a PersistentVolume bound to
// one *is* a volume, and a Node's annotations are its identity and its status.
// The whole cluster-scoped schema is annotations on objects Kubernetes already
// has, which is what "no new CRDs" costs and buys.
//
// Objects that are not ready yet are skipped rather than rejected. A Node the
// operator has not assigned an id to, a StorageClass without a universe id, a
// PersistentVolume whose extents have not been allocated: each is a normal
// intermediate state during provisioning, and a node that refused to render
// anything until they resolved would stall the entire zone behind the slowest
// object. Anything skipped is logged once per reconcile so a genuinely stuck
// object is still visible.
func BuildClusterState(
	nodes []*corev1.Node,
	classes []*storagev1.StorageClass,
	volumes []*corev1.PersistentVolume,
	memberships []*corev1.ConfigMap,
	log *slog.Logger,
) racerctrl.ClusterState {
	state := racerctrl.ClusterState{
		Nodes:     buildNodeStates(nodes, log),
		Universes: buildUniverseStates(classes, volumes, memberships, log),
	}

	return state
}

func buildNodeStates(nodes []*corev1.Node, log *slog.Logger) []racerctrl.NodeState {
	states := make([]racerctrl.NodeState, 0, len(nodes))

	for _, node := range nodes {
		state, err := racerctrl.ParseNodeState(node.Name, node.Annotations)
		if err != nil {
			log.Warn("ignoring node with unreadable racer annotations",
				"node", node.Name, "error", err)

			continue
		}

		// A node with no id has not been admitted by the operator yet. It is
		// not a member of any catalog and publishes nothing, so it contributes
		// nothing to any other node's config.
		if state.ID == 0 {
			continue
		}

		states = append(states, state)
	}

	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })

	return states
}

func buildUniverseStates(
	classes []*storagev1.StorageClass,
	volumes []*corev1.PersistentVolume,
	memberships []*corev1.ConfigMap,
	log *slog.Logger,
) []racerctrl.UniverseState {
	byClass := make(map[string]*racerctrl.UniverseState, len(classes))
	states := make([]racerctrl.UniverseState, 0, len(classes))

	for _, class := range classes {
		if class.Provisioner != racerctrl.DriverName {
			continue
		}

		state, err := racerctrl.ParseUniverseState(class.Name, class.Annotations)
		if err != nil {
			log.Warn("ignoring storage class with unreadable racer annotations",
				"storageClass", class.Name, "error", err)

			continue
		}

		// A class the operator has not allocated a universe id for yet is a
		// universe that does not exist.
		if state.ID == 0 {
			continue
		}

		states = append(states, state)
	}

	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })

	byID := make(map[uint32]*racerctrl.UniverseState, len(states))

	for i := range states {
		byClass[states[i].Class] = &states[i]
		byID[states[i].ID] = &states[i]
	}

	for _, membership := range memberships {
		universeID, zone, ok := racerctrl.ParseMembershipLabels(membership.Labels)
		if !ok {
			continue
		}

		universe, ok := byID[universeID]
		if !ok {
			// A membership for a universe this node cannot see. Either the
			// class was deleted or the informer caught the ConfigMap first.
			continue
		}

		members, err := racerctrl.ParseMembership(membership.Data[racerctrl.MembershipDataKey])
		if err != nil {
			log.Warn("ignoring unreadable membership",
				"configMap", membership.Name, "error", err)

			continue
		}

		universe.Members[zone] = members
	}

	for _, volume := range volumes {
		class, ok := racerVolumeClass(volume)
		if !ok {
			continue
		}

		universe, ok := byClass[class]
		if !ok {
			// Either the class is not a racer class or it has no universe id
			// yet. Either way there is nowhere to put this volume.
			continue
		}

		state, err := racerctrl.ParseVolumeState(volume.Name, volume.Annotations)
		if err != nil {
			log.Warn("ignoring volume with unreadable racer annotations",
				"volume", volume.Name, "error", err)

			continue
		}

		// A volume whose extents have not been allocated yet contributes no
		// address space, so it cannot appear in anyone's config.
		if len(state.Composition) == 0 {
			continue
		}

		universe.Volumes = append(universe.Volumes, state)
	}

	for i := range states {
		sort.Slice(states[i].Volumes, func(a, b int) bool {
			return states[i].Volumes[a].Name < states[i].Volumes[b].Name
		})
	}

	return states
}

// racerVolumeClass reports the StorageClass of a PersistentVolume provisioned
// by racer, and whether it is one at all.
func racerVolumeClass(volume *corev1.PersistentVolume) (string, bool) {
	if volume.Spec.CSI == nil || volume.Spec.CSI.Driver != racerctrl.DriverName {
		return "", false
	}

	if volume.Spec.StorageClassName == "" {
		return "", false
	}

	return volume.Spec.StorageClassName, true
}

// FindSelf locates this node's own state in a cluster snapshot.
func FindSelf(state racerctrl.ClusterState, name string) (racerctrl.NodeState, error) {
	for _, node := range state.Nodes {
		if node.Name == name {
			return node, nil
		}
	}

	return racerctrl.NodeState{}, fmt.Errorf(
		"node %q has no racer identity yet: waiting for the operator to assign %s, %s and %s",
		name, racerctrl.NodeIDAnnotation, racerctrl.NodeCohortAnnotation, racerctrl.NodeZoneAnnotation)
}

// PlanFabric works out what this node must publish and attach.
//
// A node publishes a namespace for every universe it joins and attaches one
// from every other node it needs to reach in that universe: its catalog peers,
// and the gateways of every other zone the universe spans. The allowed-host
// list on each published namespace is that same set plus the node itself, which
// makes the fabric's reachability exactly the universe's membership.
func PlanFabric(
	fabric *Fabric,
	state racerctrl.ClusterState,
	self racerctrl.NodeState,
) FabricPlan {
	var plan FabricPlan

	byID := make(map[uint32]racerctrl.NodeState, len(state.Nodes))
	for _, node := range state.Nodes {
		byID[node.ID] = node
	}

	fabricIDs := map[uint32]uint32{}
	for _, export := range self.Fabric {
		fabricIDs[export.UniverseID] = export.DeviceID
	}

	for _, universe := range state.Universes {
		if !universeJoinsNode(universe, self) {
			continue
		}

		deviceID, ok := fabricIDs[universe.ID]
		if !ok {
			// The minor has not been assigned yet; the reconcile that assigns
			// it will run this again.
			continue
		}

		reach := universeReach(universe, self)

		plan.Exports = append(plan.Exports, FabricExportRequest{
			UniverseID:   universe.ID,
			DeviceID:     deviceID,
			AllowedNodes: reach,
		})

		for _, peerID := range reach {
			peer, ok := byID[peerID]
			if !ok || peerID == self.ID {
				continue
			}

			for _, export := range peer.Fabric {
				if export.UniverseID != universe.ID || export.NQN == "" || export.Addr == "" {
					continue
				}

				trtype, addr := transportTo(fabric, self, peer, export)

				plan.Imports = append(plan.Imports, FabricImportRequest{
					UniverseID: universe.ID,
					PeerNodeID: peerID,
					NQN:        export.NQN,
					Addr:       addr,
					Trtype:     trtype,
				})
			}
		}
	}

	return plan
}

// transportTo picks how to dial one peer, and the address to dial.
//
// RDMA when both ends declare the same fabric and the peer has advertised a
// listening RDMA port, TCP otherwise. Fabric identity is the first test: RDMA
// reachability is a property of the physical network a node is cabled into,
// which nothing in Kubernetes knows and no probe from here could establish
// cheaply, so it is declared. The peer's advertisement is the second: it
// appears only once that node's nvmet RDMA port is actually up, which makes
// bring-up the same two-round handshake the NQN and TCP address already use,
// and keeps a half-configured node reachable rather than isolated.
func transportTo(
	fabric *Fabric,
	self, peer racerctrl.NodeState,
	export racerctrl.FabricExport,
) (string, string) {
	sameFabric := self.FabricID != "" && self.FabricID == peer.FabricID
	if !sameFabric || self.RDMAAddr == "" || export.RDMAAddr == "" {
		return fabricTrtypeTCP, export.Addr
	}

	addr := fabric.RDMAAddress(export.RDMAAddr)
	if addr == "" {
		return fabricTrtypeTCP, export.Addr
	}

	return fabricTrtypeRDMA, addr
}

// universeJoinsNode reports whether this node has any business in a universe:
// it is either a member of the universe's catalog in its own zone, or it
// exports one of the universe's volumes to a local pod.
func universeJoinsNode(universe racerctrl.UniverseState, self racerctrl.NodeState) bool {
	if universe.Members[self.Zone].Contains(self.ID) {
		return true
	}

	for _, binding := range self.Devices {
		for _, volume := range universe.Volumes {
			if volume.Name == binding.Volume {
				return true
			}
		}
	}

	return false
}

// universeReach is the set of node ids this node must be able to talk to in a
// universe: every other member of its own zone's catalog, plus every gateway of
// every other zone the universe spans.
func universeReach(universe racerctrl.UniverseState, self racerctrl.NodeState) []uint32 {
	seen := map[uint32]struct{}{}

	for _, member := range universe.Members[self.Zone] {
		seen[member.NodeID] = struct{}{}
	}

	for zone, gateways := range universe.Gateways {
		if zone == self.Zone {
			continue
		}

		for _, gateway := range gateways {
			seen[gateway] = struct{}{}
		}
	}

	delete(seen, 0)

	reach := make([]uint32, 0, len(seen))
	for id := range seen {
		reach = append(reach, id)
	}

	sort.Slice(reach, func(i, j int) bool { return reach[i] < reach[j] })

	return reach
}
