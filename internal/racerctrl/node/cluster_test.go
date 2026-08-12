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
