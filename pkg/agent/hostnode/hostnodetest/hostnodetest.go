// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hostnodetest holds the fixtures the tests of the none and container
// compositions share. Only tests import it.
package hostnodetest

import (
	"io"
	"log/slog"
	"net/netip"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// Quiet returns a logger that discards everything.
func Quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// GoalState is a complete machine goal state for slot with a bootstrap-token
// kubelet. dns enables LocalDNS in both halves.
func GoalState(slot string, dns bool) *goalstates.MachineGoalState {
	return &goalstates.MachineGoalState{
		RootFS: &goalstates.RootFS{
			HostArch: "amd64", KubernetesVersion: "v1.34.1", ContainerdVersion: "2.1.8", RunCVersion: "1.5.0", CNIPluginVersion: "1.5.1",
			LocalDNS: goalstates.LocalDNS{Enabled: dns, CoreDNSVersion: "1.12.3"},
		},
		NodeStart: &goalstates.NodeStart{
			MachineName: slot, NodeName: "worker-1",
			Containerd: goalstates.Containerd{SandboxImage: "pause", CNIBinDir: "/opt/cni/bin", CNIConfDir: "/etc/cni/net.d", MetricsAddress: "0.0.0.0:10257"},
			Gantry:     goalstates.Gantry{Disabled: true},
			Kubelet: goalstates.Kubelet{
				APIServer: "https://api", CACertData: []byte("ca"), ClusterDNS: "10.0.0.10",
				KubeletAuthInfo: config.KubeletAuthInfo{BootstrapToken: "tok"},
			},
			LocalDNS: goalstates.LocalDNS{
				Enabled: dns, NodeListenerIP: netip.MustParseAddr("169.254.20.10"), ClusterListenerIP: netip.MustParseAddr("169.254.20.11"),
				Corefile: []byte(".:53 {}\n"), CPULimitInMilliCores: 500, MemoryLimitInMB: 64,
			},
		},
	}
}

// MinimalGoalState is the smaller goal state the bootstrap stage plans are
// tested with: kube1, no LocalDNS, a bootstrap-token kubelet.
func MinimalGoalState() *goalstates.MachineGoalState {
	gs := &goalstates.MachineGoalState{
		RootFS: &goalstates.RootFS{HostArch: "amd64", KubernetesVersion: "v1.34.1"},
		NodeStart: &goalstates.NodeStart{
			MachineName: "kube1", NodeName: "w1",
			Containerd: goalstates.Containerd{SandboxImage: "pause", CNIBinDir: "/opt/cni/bin", CNIConfDir: "/etc/cni/net.d", MetricsAddress: "0.0.0.0:1"},
			Gantry:     goalstates.Gantry{Disabled: true},
			Kubelet:    goalstates.Kubelet{APIServer: "https://api", CACertData: []byte("ca"), ClusterDNS: "10.0.0.10"},
		},
	}
	gs.NodeStart.Kubelet.BootstrapToken = "tok"

	return gs
}
