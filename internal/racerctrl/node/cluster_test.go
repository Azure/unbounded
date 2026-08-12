// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"testing"

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
