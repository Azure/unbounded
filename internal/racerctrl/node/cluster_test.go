// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"io"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/racerctrl"
)

func TestTransportTo(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	onFabric := racerctrl.NodeState{ID: 7, FabricID: "rail-a", RDMAAddr: "192.168.9.7"}

	tests := []struct {
		name   string
		self   racerctrl.NodeState
		peer   racerctrl.NodeState
		export racerctrl.FabricExport
		trtype string
		addr   string
	}{
		{
			name:   "same fabric, both listening",
			self:   onFabric,
			peer:   racerctrl.NodeState{ID: 8, FabricID: "rail-a", RDMAAddr: "192.168.9.8"},
			export: racerctrl.FabricExport{Addr: "10.0.0.8:4420", RDMAAddr: "192.168.9.8:4421"},
			trtype: "rdma",
			addr:   "192.168.9.8:4421",
		},
		{
			name:   "different fabric",
			self:   onFabric,
			peer:   racerctrl.NodeState{ID: 8, FabricID: "rail-b", RDMAAddr: "192.168.9.8"},
			export: racerctrl.FabricExport{Addr: "10.0.0.8:4420", RDMAAddr: "192.168.9.8:4421"},
			trtype: "tcp",
			addr:   "10.0.0.8:4420",
		},
		{
			name: "neither declares a fabric",
			self: racerctrl.NodeState{ID: 7, RDMAAddr: "192.168.9.7"},
			peer: racerctrl.NodeState{ID: 8, RDMAAddr: "192.168.9.8"},
			// An absent fabric id is not a fabric two nodes share: RDMA
			// reachability is exactly what the annotation asserts, so nothing
			// is assumed in its absence.
			export: racerctrl.FabricExport{Addr: "10.0.0.8:4420", RDMAAddr: "192.168.9.8:4421"},
			trtype: "tcp",
			addr:   "10.0.0.8:4420",
		},
		{
			name: "peer has not advertised its port yet",
			self: onFabric,
			peer: racerctrl.NodeState{ID: 8, FabricID: "rail-a", RDMAAddr: "192.168.9.8"},
			// The peer declared an address but its port has not come up, so it
			// advertises none and is dialled over TCP until it does.
			export: racerctrl.FabricExport{Addr: "10.0.0.8:4420"},
			trtype: "tcp",
			addr:   "10.0.0.8:4420",
		},
		{
			name:   "we have no RDMA of our own",
			self:   racerctrl.NodeState{ID: 7, FabricID: "rail-a"},
			peer:   racerctrl.NodeState{ID: 8, FabricID: "rail-a", RDMAAddr: "192.168.9.8"},
			export: racerctrl.FabricExport{Addr: "10.0.0.8:4420", RDMAAddr: "192.168.9.8:4421"},
			trtype: "tcp",
			addr:   "10.0.0.8:4420",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trtype, addr := transportTo(fabric, test.self, test.peer, test.export)
			if trtype != test.trtype || addr != test.addr {
				t.Fatalf("selected %s %s, want %s %s", trtype, addr, test.trtype, test.addr)
			}
		})
	}
}

// The catalog and the epoch that names it are published in one ConfigMap, so a
// node reads them together and never pairs a new catalog with the old epoch.
func TestBuildClusterStateDatesAMembership(t *testing.T) {
	class := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "fast"},
		Provisioner: racerctrl.DriverName,
	}
	class.Annotations = map[string]string{
		racerctrl.UniverseIDAnnotation:  "1",
		racerctrl.CatalogSizeAnnotation: "3",
		racerctrl.EpochAnnotation:       "4",
	}

	membership := func(zone uint32, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:   racerctrl.MembershipConfigMapName(1, zone),
				Labels: racerctrl.MembershipLabels(1, zone),
			},
			Data: data,
		}
	}

	state := BuildClusterState(nil,
		[]*storagev1.StorageClass{class},
		nil,
		[]*corev1.ConfigMap{
			membership(1, map[string]string{
				racerctrl.MembershipDataKey:  "1?cohort=0,2?cohort=1,3?cohort=2",
				racerctrl.MembershipEpochKey: "7",
			}),
			// Written before the epoch travelled with the membership.
			membership(2, map[string]string{
				racerctrl.MembershipDataKey: "4?cohort=0,5?cohort=1,6?cohort=2",
			}),
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if len(state.Universes) != 1 {
		t.Fatalf("expected one universe, got %d", len(state.Universes))
	}

	universe := &state.Universes[0]

	if got := universe.EpochFor(1); got != 7 {
		t.Fatalf("zone 1 runs at epoch %d, want the 7 its catalog was published with", got)
	}

	if got := universe.EpochFor(2); got != 4 {
		t.Fatalf("an undated membership runs at epoch %d, want the class's 4", got)
	}
}

// A node the catalog has stopped naming is not finished with the universe: it
// still holds registers, and it drops one only once it has asked the new
// members whether they hold that version. Both halves of that conversation need
// the fabric, so a draining node keeps its export and stays in every survivor's
// allowed hosts.
func TestPlanFabricKeepsADrainingNodeConnected(t *testing.T) {
	self := racerctrl.NodeState{
		Name: "n1", ID: 1, Zone: 1,
		Fabric: []racerctrl.FabricExport{{UniverseID: 1, DeviceID: 7}},
	}

	universe := racerctrl.UniverseState{
		ID: 1,
		Members: map[uint32]racerctrl.Membership{1: {
			{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
		}},
		Draining: map[uint32]racerctrl.Membership{1: {{NodeID: 1, Cohort: 0}}},
	}

	plan := PlanFabric(&Fabric{}, racerctrl.ClusterState{
		Nodes:     []racerctrl.NodeState{self},
		Universes: []racerctrl.UniverseState{universe},
	}, self)

	if len(plan.Exports) != 1 {
		t.Fatalf("a draining node published %d exports, want the one it is draining through", len(plan.Exports))
	}

	if got := plan.Exports[0].AllowedNodes; len(got) != 4 {
		t.Fatalf("allowed hosts %v, want the three new members and itself", got)
	}
}

// The survivors have to admit the node handing groups to them, or the query it
// makes before dropping a register never gets an answer.
func TestPlanFabricAdmitsTheNodeDrainingIntoUs(t *testing.T) {
	self := racerctrl.NodeState{
		Name: "n4", ID: 4, Zone: 1,
		Fabric: []racerctrl.FabricExport{{UniverseID: 1, DeviceID: 7}},
	}

	universe := racerctrl.UniverseState{
		ID: 1,
		Members: map[uint32]racerctrl.Membership{1: {
			{NodeID: 4, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2},
		}},
		Draining: map[uint32]racerctrl.Membership{1: {{NodeID: 1, Cohort: 0}}},
	}

	plan := PlanFabric(&Fabric{}, racerctrl.ClusterState{
		Nodes:     []racerctrl.NodeState{self},
		Universes: []racerctrl.UniverseState{universe},
	}, self)

	found := false

	for _, id := range plan.Exports[0].AllowedNodes {
		if id == 1 {
			found = true
		}
	}

	if !found {
		t.Fatalf("allowed hosts %v omit the draining node, which can then never hand its groups over", plan.Exports[0].AllowedNodes)
	}
}

// The draining set travels with the membership, because it is the same
// decision: who this zone's nodes link to and admit.
func TestBuildClusterStateCarriesTheDrainingSet(t *testing.T) {
	class := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "fast"},
		Provisioner: racerctrl.DriverName,
	}
	class.Annotations = map[string]string{
		racerctrl.UniverseIDAnnotation:  "1",
		racerctrl.CatalogSizeAnnotation: "3",
		racerctrl.EpochAnnotation:       "4",
	}

	state := BuildClusterState(nil,
		[]*storagev1.StorageClass{class},
		nil,
		[]*corev1.ConfigMap{{
			ObjectMeta: metav1.ObjectMeta{
				Name:   racerctrl.MembershipConfigMapName(1, 1),
				Labels: racerctrl.MembershipLabels(1, 1),
			},
			Data: map[string]string{
				racerctrl.MembershipDataKey:     "4?cohort=0,2?cohort=1,3?cohort=2",
				racerctrl.MembershipDrainingKey: "1?cohort=0",
				racerctrl.MembershipEpochKey:    "7",
			},
		}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !state.Universes[0].Draining[1].Contains(1) {
		t.Fatalf("draining set %v lost the node it names", state.Universes[0].Draining[1])
	}
}
