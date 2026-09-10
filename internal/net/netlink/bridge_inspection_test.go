// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func TestInspectBridgePodCIDRsSafeSnapshots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cidrs     []string
		snapshot  bridgeInspectionSnapshot
		targets   map[int][]bridgeInspectionLink
		targetErr map[int]error
	}{
		{
			name:     "absent bridge",
			cidrs:    []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{absent: true},
		},
		{
			name:     "empty bridge",
			cidrs:    []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{bridgeIndex: 10},
		},
		{
			name:  "dual stack addresses and link local addresses",
			cidrs: []string{"fd00:244:1::99/64", "10.244.1.99/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-managed", linkType: "veth", hostIndex: 100,
						peerIndex: 2, peerNetNsID: 0, masterIndex: 10,
					},
				},
			},
			targets: map[int][]bridgeInspectionLink{
				0: {
					{
						name: "eth0", linkType: "veth", index: 2, parentIndex: 100,
						addresses: bridgeInspectionAddresses(
							"10.244.1.5",
							"fd00:244:1::5",
							"169.254.8.9",
							"fe80::1",
						),
					},
				},
			},
		},
		{
			name:  "interface without addresses",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-empty", linkType: "veth", hostIndex: 100,
						peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
					},
				},
			},
			targets: map[int][]bridgeInspectionLink{
				7: {{name: "eth0", linkType: "veth", index: 2, parentIndex: 100}},
			},
		},
		{
			name:  "unrelated multus interface is ignored",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-managed", linkType: "veth", hostIndex: 100,
						peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
					},
				},
			},
			targets: map[int][]bridgeInspectionLink{
				7: {
					{
						name: "eth0", linkType: "veth", index: 2, parentIndex: 100,
						addresses: bridgeInspectionAddresses("10.244.1.5"),
					},
					{
						name: "net1", linkType: "veth", index: 3, parentIndex: 200,
						addresses: bridgeInspectionAddresses("192.168.50.10"),
					},
				},
			},
		},
		{
			name:  "wrong peer indices are ignored",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-managed", linkType: "veth", hostIndex: 100,
						peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
					},
				},
			},
			targets: map[int][]bridgeInspectionLink{
				7: {
					{
						name: "wrong-parent", linkType: "veth", index: 2, parentIndex: 999,
						addresses: bridgeInspectionAddresses("192.168.50.10"),
					},
					{
						name: "wrong-index", linkType: "veth", index: 9, parentIndex: 100,
						addresses: bridgeInspectionAddresses("192.168.50.11"),
					},
					{
						name: "peer", linkType: "veth", index: 2, parentIndex: 100,
						addresses: bridgeInspectionAddresses("10.244.1.8"),
					},
				},
			},
		},
		{
			name:  "host index collision in one unrelated namespace is ignored",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-managed", linkType: "veth", hostIndex: 100,
						peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
					},
				},
			},
			targets: map[int][]bridgeInspectionLink{
				7: {
					{
						name: "peer", linkType: "veth", index: 2, parentIndex: 100,
						addresses: bridgeInspectionAddresses("10.244.1.8"),
					},
				},
				8: {
					{
						name: "collision", linkType: "veth", index: 77, parentIndex: 100,
						addresses: bridgeInspectionAddresses("192.168.50.10"),
					},
				},
			},
		},
		{
			name:  "unrelated disappearing namespace is ignored",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-managed", linkType: "veth", hostIndex: 100,
						peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
					},
				},
			},
			targets: bridgeInspectionPeerWithAddresses(7, "10.244.1.8"),
			targetErr: map[int]error{
				8: unix.ENOENT,
			},
		},
		{
			name:  "ambiguous peer across namespaces",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-a", linkType: "veth", hostIndex: 100,
						peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
					},
					{
						name: "veth-b", linkType: "veth", hostIndex: 200,
						peerIndex: 2, peerNetNsID: 8, masterIndex: 10,
					},
				},
			},
			targets: map[int][]bridgeInspectionLink{
				7: {{name: "peer-a", linkType: "veth", index: 2, parentIndex: 100}},
				8: {{name: "peer-b", linkType: "veth", index: 2, parentIndex: 200}},
			},
		},
		{
			name:  "same host peer is inspected deliberately",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-host", linkType: "veth", hostIndex: 100,
						peerIndex: 101, peerNetNsID: -1, masterIndex: 10,
					},
				},
			},
			targets: map[int][]bridgeInspectionLink{
				-1: {
					{
						name: "veth-peer", linkType: "veth", index: 101, parentIndex: 100,
						addresses: bridgeInspectionAddresses("10.244.1.8"),
					},
				},
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			deps := bridgeInspectionTestDependencies(
				[]bridgeInspectionSnapshot{test.snapshot, test.snapshot},
				test.targets,
				test.targetErr,
			)

			if err := inspectBridgePodCIDRs(
				context.Background(),
				"cbr0",
				test.cidrs,
				deps,
			); err != nil {
				t.Fatalf("inspectBridgePodCIDRs() error = %v", err)
			}
		})
	}
}

func TestInspectBridgePodCIDRsBlocksUnsafeOrInconclusiveSnapshots(t *testing.T) {
	t.Parallel()

	baseSnapshot := bridgeInspectionSnapshot{
		bridgeIndex: 10,
		ports: []bridgeInspectionPort{
			{
				name: "veth-managed", linkType: "veth", hostIndex: 100,
				peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
			},
		},
	}

	tests := []struct {
		name      string
		snapshot  bridgeInspectionSnapshot
		targets   map[int][]bridgeInspectionLink
		targetErr map[int]error
		hostError error
		want      string
	}{
		{
			name:     "address outside assigned prefix",
			snapshot: baseSnapshot,
			targets:  bridgeInspectionPeerWithAddresses(7, "10.244.2.5"),
			want:     "10.244.2.5 outside assigned PodCIDRs",
		},
		{
			name: "unsupported live bridge port",
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{name: "tap0", linkType: "tun", hostIndex: 100, peerNetNsID: 7, masterIndex: 10},
				},
			},
			want: "unsupported live port tap0",
		},
		{
			name: "unresolved live peer",
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{
						name: "veth-managed", linkType: "veth", hostIndex: 100,
						peerNetNsID: 7, masterIndex: 10, peerError: "device busy",
					},
				},
			},
			want: "resolve pod-side peer",
		},
		{
			name:     "live peer not found",
			snapshot: baseSnapshot,
			targets:  map[int][]bridgeInspectionLink{7: {}},
			want:     "was not found in network namespace ID 7",
		},
		{
			name:      "target namespace query failure",
			snapshot:  baseSnapshot,
			targetErr: map[int]error{7: errors.New("operation not supported")},
			want:      "operation not supported",
		},
		{
			name:      "raw address inspection ENOENT still blocks",
			snapshot:  baseSnapshot,
			targetErr: map[int]error{7: unix.ENOENT},
			want:      "no such file or directory",
		},
		{
			name:     "wrapped address inspection ENOENT still blocks",
			snapshot: baseSnapshot,
			targetErr: map[int]error{
				7: fmt.Errorf("query IPv4 addresses: %w", unix.ENOENT),
			},
			want: "query IPv4 addresses",
		},
		{
			name:     "ambiguous reciprocal peers block",
			snapshot: baseSnapshot,
			targets: map[int][]bridgeInspectionLink{
				7: {
					{name: "peer-a", linkType: "veth", index: 2, parentIndex: 100},
					{name: "peer-b", linkType: "veth", index: 2, parentIndex: 100},
				},
			},
			want: "is ambiguous",
		},
		{
			name:      "namespace inspection failure",
			snapshot:  baseSnapshot,
			targetErr: map[int]error{7: errors.New("permission denied")},
			want:      "permission denied",
		},
		{
			name:      "managed peer namespace disappeared",
			snapshot:  baseSnapshot,
			targetErr: map[int]error{7: unix.ENOENT},
			want:      "no such file or directory",
		},
		{
			name:      "failed bridge lookup",
			hostError: errors.New("netlink socket failed"),
			want:      "netlink socket failed",
		},
		{
			name:      "wrong bridge type",
			hostError: errors.New("link cbr0 has type dummy, expected bridge"),
			want:      "expected bridge",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var hostCalls int

			deps := bridgeInspectionTestDependencies(
				[]bridgeInspectionSnapshot{test.snapshot, test.snapshot},
				test.targets,
				test.targetErr,
			)
			if test.hostError != nil {
				deps.hostSnapshot = func(
					context.Context,
					string,
				) (bridgeInspectionSnapshot, error) {
					hostCalls++

					return bridgeInspectionSnapshot{}, test.hostError
				}
			}

			err := inspectBridgePodCIDRs(
				context.Background(),
				"cbr0",
				[]string{"10.244.1.0/24"},
				deps,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("inspectBridgePodCIDRs() error = %v, want containing %q", err, test.want)
			}

			if test.hostError != nil && hostCalls != 1 {
				t.Fatalf("host snapshot calls = %d, want 1", hostCalls)
			}
		})
	}
}

func TestInspectBridgePodCIDRsAssignmentValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		bridgeName string
		cidrs      []string
		want       string
	}{
		{name: "no assignments", bridgeName: "cbr0", want: "PodCIDRs are empty"},
		{name: "empty assignment", bridgeName: "cbr0", cidrs: []string{""}, want: "PodCIDR is empty"},
		{
			name:       "invalid assignment",
			bridgeName: "cbr0",
			cidrs:      []string{"not-a-prefix"},
			want:       "parse assigned PodCIDR",
		},
		{
			name:  "empty bridge name",
			cidrs: []string{"10.244.1.0/24"},
			want:  "bridge name is empty",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := inspectBridgePodCIDRs(
				context.Background(),
				test.bridgeName,
				test.cidrs,
				bridgeInspectionDependencies{},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("inspectBridgePodCIDRs() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestInspectBridgePodCIDRsRetriesConfirmedTeardown(t *testing.T) {
	t.Parallel()

	live := bridgeInspectionSnapshot{
		bridgeIndex: 10,
		ports: []bridgeInspectionPort{
			{
				name: "veth-managed", linkType: "veth", hostIndex: 100,
				peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
			},
		},
	}
	empty := bridgeInspectionSnapshot{bridgeIndex: 10}
	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{live, empty, empty, empty},
		map[int][]bridgeInspectionLink{7: {}},
		nil,
	)

	if err := inspectBridgePodCIDRs(
		context.Background(),
		"cbr0",
		[]string{"10.244.1.0/24"},
		deps,
	); err != nil {
		t.Fatalf("inspectBridgePodCIDRs() error = %v", err)
	}
}

func TestInspectBridgePodCIDRsBlocksContinuouslyChangingTopology(t *testing.T) {
	t.Parallel()

	snapshot := func(netNsID int) bridgeInspectionSnapshot {
		return bridgeInspectionSnapshot{
			bridgeIndex: 10,
			ports: []bridgeInspectionPort{
				{
					name: "veth-managed", linkType: "veth", hostIndex: 100,
					peerIndex: 2, peerNetNsID: netNsID, masterIndex: 10,
				},
			},
		}
	}
	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{
			snapshot(7), snapshot(8),
			snapshot(7), snapshot(8),
			snapshot(7), snapshot(8),
		},
		map[int][]bridgeInspectionLink{
			7: bridgeInspectionPeerWithAddresses(7, "10.244.1.8")[7],
		},
		nil,
	)

	err := inspectBridgePodCIDRs(
		context.Background(),
		"cbr0",
		[]string{"10.244.1.0/24"},
		deps,
	)
	if err == nil || !strings.Contains(err.Error(), "topology changed during 3 inspection attempts") {
		t.Fatalf("inspectBridgePodCIDRs() error = %v, want topology change error", err)
	}
}

func TestInspectBridgePodCIDRsHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := inspectBridgePodCIDRs(
		ctx,
		"cbr0",
		[]string{"10.244.1.0/24"},
		bridgeInspectionDependencies{},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("inspectBridgePodCIDRs() error = %v, want context cancellation", err)
	}
}

func TestInspectBridgePodCIDRsHonorsCancellationDuringNamespaceScan(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	snapshot := bridgeInspectionSnapshot{
		bridgeIndex: 10,
		ports: []bridgeInspectionPort{
			{
				name: "veth-managed", linkType: "veth", hostIndex: 100,
				peerIndex: 2, peerNetNsID: 7, masterIndex: 10,
			},
		},
	}
	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{snapshot, snapshot},
		bridgeInspectionPeerWithAddresses(7, "10.244.1.8"),
		nil,
	)
	originalTargetSnapshot := deps.targetSnapshot
	deps.targetSnapshot = func(
		ctx context.Context,
		netNsID int,
		hostPeers map[int]int,
	) ([]bridgeInspectionLink, error) {
		links, err := originalTargetSnapshot(ctx, netNsID, hostPeers)

		cancel()

		return links, err
	}

	err := inspectBridgePodCIDRs(
		ctx,
		"cbr0",
		[]string{"10.244.1.0/24"},
		deps,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("inspectBridgePodCIDRs() error = %v, want context cancellation", err)
	}
}

func TestRealBridgeInspectionHostSnapshotFiltersOtherBridges(t *testing.T) {
	t.Parallel()

	executor := &bridgeInspectionTestExecutor{
		execute: func(request *nl.NetlinkRequest, responseType uint16) ([][]byte, error) {
			if request.Type != unix.RTM_GETLINK ||
				responseType != unix.RTM_NEWLINK ||
				len(request.Data) != 1 {
				t.Fatalf("unexpected host link request: %#v responseType=%d", request, responseType)
			}

			return [][]byte{
				bridgeInspectionLinkMessageWithAttrs(10, 0, 0, -1, "cbr0", "bridge"),
				bridgeInspectionLinkMessageWithAttrs(20, 0, 0, -1, "other", "bridge"),
				bridgeInspectionLinkMessageWithAttrs(200, 2, 20, 9, "veth-other", "veth"),
				bridgeInspectionLinkMessageWithAttrs(100, 2, 10, 0, "veth-managed", "veth"),
				bridgeInspectionLinkMessageWithAttrs(201, 0, 20, -1, "dummy-other", "dummy"),
			}, nil
		},
	}

	snapshot, err := bridgeInspectionHostSnapshotWithExecutor(context.Background(), "cbr0", executor)
	if err != nil {
		t.Fatalf("bridgeInspectionHostSnapshotWithExecutor() error = %v", err)
	}

	if len(snapshot.ports) != 1 || snapshot.ports[0].name != "veth-managed" {
		t.Fatalf("snapshot ports = %#v, want only veth-managed", snapshot.ports)
	}

	if snapshot.ports[0].peerNetNsID != 0 {
		t.Fatalf("snapshot peer namespace ID = %d, want valid ID 0", snapshot.ports[0].peerNetNsID)
	}

	if snapshot.ports[0].peerIndex != 2 {
		t.Fatalf("snapshot peer index = %d, want 2", snapshot.ports[0].peerIndex)
	}
}

func TestRealBridgeInspectionHostSnapshotDecodesPeerNamespaceID(t *testing.T) {
	t.Parallel()

	unassigned := ^uint32(0)
	tests := []struct {
		name       string
		rawNetNsID *uint32
		want       int
	}{
		{
			name:       "present namespace ID zero",
			rawNetNsID: bridgeInspectionUint32Pointer(0),
			want:       0,
		},
		{
			name: "absent attribute",
			want: -1,
		},
		{
			name:       "explicit unassigned namespace ID",
			rawNetNsID: &unassigned,
			want:       int(unassigned),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			executor := &bridgeInspectionTestExecutor{
				execute: func(*nl.NetlinkRequest, uint16) ([][]byte, error) {
					return [][]byte{
						bridgeInspectionLinkMessageWithAttrs(10, 0, 0, -1, "cbr0", "bridge"),
						bridgeInspectionLinkMessageWithRawNetNsID(
							100,
							2,
							10,
							test.rawNetNsID,
							"veth-managed",
							"veth",
						),
					}, nil
				},
			}

			snapshot, err := bridgeInspectionHostSnapshotWithExecutor(context.Background(), "cbr0", executor)
			if err != nil {
				t.Fatalf("bridgeInspectionHostSnapshotWithExecutor() error = %v", err)
			}

			if len(snapshot.ports) != 1 {
				t.Fatalf("snapshot ports = %#v, want one managed port", snapshot.ports)
			}

			if got := snapshot.ports[0].peerNetNsID; got != test.want {
				t.Fatalf("snapshot peer namespace ID = %d, want %d", got, test.want)
			}
		})
	}
}

func TestInspectBridgePodCIDRsAbsentPeerNamespaceIDDoesNotQueryNamespaceZero(t *testing.T) {
	t.Parallel()

	snapshot := bridgeInspectionSnapshot{
		bridgeIndex: 10,
		ports: []bridgeInspectionPort{
			{
				name: "veth-managed", linkType: "veth", hostIndex: 100,
				peerIndex: 2, peerNetNsID: -1, masterIndex: 10,
			},
		},
	}

	var targetIDs []int

	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{snapshot, snapshot},
		nil,
		nil,
	)
	deps.targetSnapshot = func(
		_ context.Context,
		netNsID int,
		_ map[int]int,
	) ([]bridgeInspectionLink, error) {
		targetIDs = append(targetIDs, netNsID)

		return []bridgeInspectionLink{
			{name: "index-collision", linkType: "veth", index: 2, parentIndex: 999},
			{name: "parent-collision", linkType: "veth", index: 77, parentIndex: 100},
		}, nil
	}

	err := inspectBridgePodCIDRs(
		context.Background(),
		"cbr0",
		[]string{"10.244.1.0/24"},
		deps,
	)
	if err == nil || !strings.Contains(err.Error(), "was not found in network namespace ID host") {
		t.Fatalf("inspectBridgePodCIDRs() error = %v, want missing confirmed host peer", err)
	}

	if fmt.Sprint(targetIDs) != "[-1]" {
		t.Fatalf("target namespace IDs = %v, want only host sentinel -1", targetIDs)
	}
}

func TestBridgeInspectionTimeoutUsesContextDeadline(t *testing.T) {
	t.Parallel()

	timeout, err := bridgeInspectionTimeout(context.Background())
	if err != nil {
		t.Fatalf("bridgeInspectionTimeout() error = %v", err)
	}

	if timeout != bridgeInspectionSocketTimeout {
		t.Fatalf("bridgeInspectionTimeout() = %v, want %v", timeout, bridgeInspectionSocketTimeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	timeout, err = bridgeInspectionTimeout(ctx)
	if err != nil {
		t.Fatalf("bridgeInspectionTimeout() with deadline error = %v", err)
	}

	if timeout <= 0 || timeout > 100*time.Millisecond {
		t.Fatalf("bridgeInspectionTimeout() with deadline = %v, want within (0, 100ms]", timeout)
	}
}

func TestRealBridgeInspectionNamespacePathsDeduplicatesAndExcludesHost(t *testing.T) {
	t.Parallel()

	procRoot := t.TempDir()

	selfPath := filepath.Join(procRoot, "self", "ns", "net")
	if err := os.MkdirAll(filepath.Dir(selfPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(selfPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	hostDuplicate := filepath.Join(procRoot, "100", "ns", "net")
	if err := os.MkdirAll(filepath.Dir(hostDuplicate), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Link(selfPath, hostDuplicate); err != nil {
		t.Fatal(err)
	}

	podPath := filepath.Join(procRoot, "200", "ns", "net")
	if err := os.MkdirAll(filepath.Dir(podPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(podPath, []byte("pod"), 0o600); err != nil {
		t.Fatal(err)
	}

	podDuplicate := filepath.Join(procRoot, "300", "ns", "net")
	if err := os.MkdirAll(filepath.Dir(podDuplicate), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Link(podPath, podDuplicate); err != nil {
		t.Fatal(err)
	}

	namespaces, err := processNetworkNamespacePaths(procRoot)
	if err != nil {
		t.Fatalf("processNetworkNamespacePaths() error = %v", err)
	}

	if len(namespaces) != 1 {
		t.Fatalf("processNetworkNamespacePaths() returned %d namespaces, want 1", len(namespaces))
	}

	if namespaces[0] != podPath {
		t.Fatalf("processNetworkNamespacePaths() path = %q, want %q", namespaces[0], podPath)
	}
}

func TestInspectBridgePodCIDRsGroupsDuplicatePeerIndicesByNamespaceID(t *testing.T) {
	t.Parallel()

	snapshot := bridgeInspectionSnapshot{
		bridgeIndex: 10,
		ports: []bridgeInspectionPort{
			{name: "veth-a", linkType: "veth", hostIndex: 100, peerIndex: 2, peerNetNsID: 0, masterIndex: 10},
			{name: "veth-b", linkType: "veth", hostIndex: 200, peerIndex: 2, peerNetNsID: 9, masterIndex: 10},
		},
	}
	calls := make(map[int]int)
	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{snapshot, snapshot},
		map[int][]bridgeInspectionLink{
			0: {{name: "eth0", linkType: "veth", index: 2, parentIndex: 100}},
			9: {{name: "eth0", linkType: "veth", index: 2, parentIndex: 200}},
		},
		nil,
	)
	originalTargetSnapshot := deps.targetSnapshot
	deps.targetSnapshot = func(
		ctx context.Context,
		netNsID int,
		hostPeers map[int]int,
	) ([]bridgeInspectionLink, error) {
		calls[netNsID]++

		return originalTargetSnapshot(ctx, netNsID, hostPeers)
	}

	if err := inspectBridgePodCIDRs(
		context.Background(),
		"cbr0",
		[]string{"10.244.1.0/24"},
		deps,
	); err != nil {
		t.Fatalf("inspectBridgePodCIDRs() error = %v", err)
	}

	if calls[0] != 1 || calls[9] != 1 {
		t.Fatalf("target snapshot calls = %v, want one query per namespace ID", calls)
	}
}

func TestBridgeInspectionTargetSnapshotUsesTargetNamespaceQueries(t *testing.T) {
	t.Parallel()

	executor := &bridgeInspectionTestExecutor{
		execute: func(request *nl.NetlinkRequest, responseType uint16) ([][]byte, error) {
			switch request.Type {
			case unix.RTM_GETLINK:
				if responseType != unix.RTM_NEWLINK {
					t.Fatalf("link response type = %d, want RTM_NEWLINK", responseType)
				}

				assertBridgeInspectionTargetAttribute(t, request, unix.IFLA_TARGET_NETNSID, 0)

				return [][]byte{
					bridgeInspectionLinkMessage(14, 15, "pr712before-p", "veth"),
					bridgeInspectionLinkMessage(3, 999, "net1", "veth"),
				}, nil
			case unix.RTM_GETADDR:
				assertBridgeInspectionTargetAttribute(t, request, unix.IFA_TARGET_NETNSID, 0)

				family := int(nl.DeserializeIfAddrmsg(request.Data[0].Serialize()).Family)
				if family == unix.AF_INET {
					return [][]byte{
						bridgeInspectionAddressMessage(14, netip.MustParseAddr("10.244.1.8")),
						bridgeInspectionAddressMessage(3, netip.MustParseAddr("192.168.1.8")),
					}, nil
				}

				return [][]byte{
					bridgeInspectionAddressMessage(14, netip.MustParseAddr("fd00:244:1::8")),
				}, nil
			default:
				t.Fatalf("unexpected request type %d", request.Type)

				return nil, nil
			}
		},
	}

	links, err := bridgeInspectionTargetSnapshotWithExecutor(
		context.Background(),
		0,
		map[int]int{15: 14},
		executor,
	)
	if err != nil {
		t.Fatalf("bridgeInspectionTargetSnapshotWithExecutor() error = %v", err)
	}

	if len(links) != 1 {
		t.Fatalf("links = %#v, want one reciprocal peer", links)
	}

	if links[0].index != 14 || links[0].parentIndex != 15 || links[0].name != "pr712before-p" {
		t.Fatalf("peer identity = %#v, want captured indices 14 and 15", links[0])
	}

	got := links[0].addresses

	want := bridgeInspectionAddresses("10.244.1.8", "fd00:244:1::8")
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

func TestBridgeInspectionTargetSnapshotDoesNotTargetHostForMissingNetNsID(t *testing.T) {
	t.Parallel()

	executor := &bridgeInspectionTestExecutor{
		execute: func(request *nl.NetlinkRequest, _ uint16) ([][]byte, error) {
			if len(request.Data) != 1 {
				t.Fatalf("host request data count = %d, want no target namespace attribute", len(request.Data))
			}

			switch request.Type {
			case unix.RTM_GETLINK:
				return [][]byte{bridgeInspectionLinkMessage(101, 100, "veth-peer", "veth")}, nil
			case unix.RTM_GETADDR:
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected request type %d", request.Type)
			}
		},
	}

	links, err := bridgeInspectionTargetSnapshotWithExecutor(
		context.Background(),
		-1,
		map[int]int{100: 101},
		executor,
	)
	if err != nil {
		t.Fatalf("bridgeInspectionTargetSnapshotWithExecutor() error = %v", err)
	}

	if len(links) != 1 || links[0].index != 101 {
		t.Fatalf("links = %#v, want same-host reciprocal peer", links)
	}
}

func TestBridgeInspectionTargetSnapshotFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		execute func(*nl.NetlinkRequest, uint16) ([][]byte, error)
		want    string
	}{
		{
			name: "link query error",
			execute: func(request *nl.NetlinkRequest, _ uint16) ([][]byte, error) {
				if request.Type == unix.RTM_GETLINK {
					return nil, unix.EOPNOTSUPP
				}

				return nil, nil
			},
			want: "query links",
		},
		{
			name: "IPv4 address query error",
			execute: func(request *nl.NetlinkRequest, _ uint16) ([][]byte, error) {
				if request.Type == unix.RTM_GETLINK {
					return [][]byte{bridgeInspectionLinkMessage(2, 100, "eth0", "veth")}, nil
				}

				return nil, unix.EPERM
			},
			want: "query IPv4 addresses",
		},
		{
			name: "IPv6 address query error",
			execute: func(request *nl.NetlinkRequest, _ uint16) ([][]byte, error) {
				if request.Type == unix.RTM_GETLINK {
					return [][]byte{bridgeInspectionLinkMessage(2, 100, "eth0", "veth")}, nil
				}

				family := int(nl.DeserializeIfAddrmsg(request.Data[0].Serialize()).Family)
				if family == unix.AF_INET6 {
					return nil, unix.EINTR
				}

				return nil, nil
			},
			want: "query IPv6 addresses",
		},
		{
			name: "malformed link response",
			execute: func(request *nl.NetlinkRequest, _ uint16) ([][]byte, error) {
				if request.Type == unix.RTM_GETLINK {
					return [][]byte{{1}}, nil
				}

				return nil, nil
			},
			want: "parse link response",
		},
		{
			name: "malformed address response",
			execute: func(request *nl.NetlinkRequest, _ uint16) ([][]byte, error) {
				if request.Type == unix.RTM_GETLINK {
					return [][]byte{bridgeInspectionLinkMessage(2, 100, "eth0", "veth")}, nil
				}

				return [][]byte{{1}}, nil
			},
			want: "parse IPv4 address response",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := bridgeInspectionTargetSnapshotWithExecutor(
				context.Background(),
				7,
				map[int]int{100: 2},
				&bridgeInspectionTestExecutor{execute: test.execute},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestNewBridgeInspectionRouteSocketEnablesStrictChecking(t *testing.T) {
	t.Parallel()

	var (
		strictLevel int
		strictName  int
		strictValue int
		sendSet     bool
		receiveSet  bool
	)

	socket, err := newBridgeInspectionRouteSocketWithOperations(
		context.Background(),
		bridgeInspectionRouteSocketOperations{
			subscribe: nl.Subscribe,
			setStrictCheck: func(_, level, name, value int) error {
				strictLevel = level
				strictName = name
				strictValue = value

				return nil
			},
			setSendTimeout: func(*nl.NetlinkSocket, *unix.Timeval) error {
				sendSet = true

				return nil
			},
			setReceiveTimeout: func(*nl.NetlinkSocket, *unix.Timeval) error {
				receiveSet = true

				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("newBridgeInspectionRouteSocketWithOperations() error = %v", err)
	}

	socket.Close()

	if strictLevel != unix.SOL_NETLINK ||
		strictName != unix.NETLINK_GET_STRICT_CHK ||
		strictValue != 1 {
		t.Fatalf(
			"strict socket option = (%d, %d, %d), want (%d, %d, 1)",
			strictLevel,
			strictName,
			strictValue,
			unix.SOL_NETLINK,
			unix.NETLINK_GET_STRICT_CHK,
		)
	}

	if !sendSet || !receiveSet {
		t.Fatalf("socket timeouts set = (%t, %t), want both true", sendSet, receiveSet)
	}
}

func TestNewBridgeInspectionRouteSocketRejectsStrictCheckFailure(t *testing.T) {
	t.Parallel()

	_, err := newBridgeInspectionRouteSocketWithOperations(
		context.Background(),
		bridgeInspectionRouteSocketOperations{
			subscribe: nl.Subscribe,
			setStrictCheck: func(int, int, int, int) error {
				return unix.ENOPROTOOPT
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "enable strict checking") {
		t.Fatalf("newBridgeInspectionRouteSocketWithOperations() error = %v, want strict checking failure", err)
	}
}

type bridgeInspectionTestExecutor struct {
	execute func(*nl.NetlinkRequest, uint16) ([][]byte, error)
}

func (e *bridgeInspectionTestExecutor) Execute(
	request *nl.NetlinkRequest,
	responseType uint16,
) ([][]byte, error) {
	return e.execute(request, responseType)
}

func (*bridgeInspectionTestExecutor) Close() {}

func assertBridgeInspectionTargetAttribute(
	t *testing.T,
	request *nl.NetlinkRequest,
	attributeType int,
	want int,
) {
	t.Helper()

	if len(request.Data) != 2 {
		t.Fatalf("request data count = %d, want header and target namespace attribute", len(request.Data))
	}

	assertBridgeInspectionTargetAttributeAt(t, request, 1, attributeType, want)
}

func assertBridgeInspectionTargetAttributeAt(
	t *testing.T,
	request *nl.NetlinkRequest,
	index int,
	attributeType int,
	want int,
) {
	t.Helper()

	attributes, err := nl.ParseRouteAttr(request.Data[index].Serialize())
	if err != nil {
		t.Fatalf("parse target namespace attribute: %v", err)
	}

	if len(attributes) != 1 || int(attributes[0].Attr.Type) != attributeType {
		t.Fatalf("target attributes = %#v, want type %d", attributes, attributeType)
	}

	if got := int(nl.NativeEndian().Uint32(attributes[0].Value)); got != want {
		t.Fatalf("target namespace ID = %d, want %d", got, want)
	}
}

func bridgeInspectionLinkMessage(index, parentIndex int, name, linkType string) []byte {
	return bridgeInspectionLinkMessageWithAttrs(index, parentIndex, 0, -1, name, linkType)
}

func bridgeInspectionLinkMessageWithAttrs(
	index int,
	parentIndex int,
	masterIndex int,
	netNsID int,
	name string,
	linkType string,
) []byte {
	var rawNetNsID *uint32

	if netNsID >= 0 {
		value := uint32(netNsID)
		rawNetNsID = &value
	}

	return bridgeInspectionLinkMessageWithRawNetNsID(
		index,
		parentIndex,
		masterIndex,
		rawNetNsID,
		name,
		linkType,
	)
}

func bridgeInspectionLinkMessageWithRawNetNsID(
	index int,
	parentIndex int,
	masterIndex int,
	rawNetNsID *uint32,
	name string,
	linkType string,
) []byte {
	header := nl.NewIfInfomsg(unix.AF_UNSPEC)
	header.Index = int32(index)

	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.ZeroTerminated(linkType))

	message := append([]byte{}, header.Serialize()...)
	message = append(message, nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(name)).Serialize()...)

	message = append(message, nl.NewRtAttr(unix.IFLA_LINK, nl.Uint32Attr(uint32(parentIndex))).Serialize()...)
	if masterIndex > 0 {
		message = append(message, nl.NewRtAttr(unix.IFLA_MASTER, nl.Uint32Attr(uint32(masterIndex))).Serialize()...)
	}

	if rawNetNsID != nil {
		message = append(message, nl.NewRtAttr(unix.IFLA_LINK_NETNSID, nl.Uint32Attr(*rawNetNsID)).Serialize()...)
	}

	message = append(message, linkInfo.Serialize()...)

	return message
}

func bridgeInspectionUint32Pointer(value uint32) *uint32 {
	return &value
}

func bridgeInspectionAddressMessage(index int, address netip.Addr) []byte {
	family := unix.AF_INET6
	rawAddress := address.AsSlice()
	prefixLength := uint8(128)

	if address.Is4() {
		family = unix.AF_INET
		addressBytes := address.As4()
		rawAddress = addressBytes[:]
		prefixLength = 32
	}

	header := nl.NewIfAddrmsg(family)
	header.Index = uint32(index)
	header.Prefixlen = prefixLength

	message := append([]byte{}, header.Serialize()...)
	message = append(message, nl.NewRtAttr(unix.IFA_LOCAL, rawAddress).Serialize()...)

	return message
}

func bridgeInspectionTestDependencies(
	snapshots []bridgeInspectionSnapshot,
	targetLinks map[int][]bridgeInspectionLink,
	targetErrors map[int]error,
) bridgeInspectionDependencies {
	nextSnapshot := 0

	return bridgeInspectionDependencies{
		hostSnapshot: func(
			context.Context,
			string,
		) (bridgeInspectionSnapshot, error) {
			if nextSnapshot >= len(snapshots) {
				return snapshots[len(snapshots)-1], nil
			}

			snapshot := snapshots[nextSnapshot]
			nextSnapshot++

			return snapshot, nil
		},
		targetSnapshot: func(
			_ context.Context,
			netNsID int,
			_ map[int]int,
		) ([]bridgeInspectionLink, error) {
			if err := targetErrors[netNsID]; err != nil {
				return nil, err
			}

			links, ok := targetLinks[netNsID]
			if !ok {
				return nil, fmt.Errorf("unexpected target network namespace ID %d", netNsID)
			}

			return links, nil
		},
	}
}

func bridgeInspectionPeerWithAddresses(
	netNsID int,
	addresses ...string,
) map[int][]bridgeInspectionLink {
	return map[int][]bridgeInspectionLink{
		netNsID: {
			{
				name: "eth0", linkType: "veth", index: 2, parentIndex: 100,
				addresses: bridgeInspectionAddresses(addresses...),
			},
		},
	}
}

func bridgeInspectionAddresses(addresses ...string) []netip.Addr {
	result := make([]netip.Addr, 0, len(addresses))

	for _, address := range addresses {
		result = append(result, netip.MustParseAddr(address))
	}

	return result
}
