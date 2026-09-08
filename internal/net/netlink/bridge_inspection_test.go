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
	"syscall"
	"testing"
	"time"

	vnetlink "github.com/vishvananda/netlink"
)

func TestInspectBridgePodCIDRsSafeSnapshots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		cidrs           []string
		snapshot        bridgeInspectionSnapshot
		namespaces      map[string][]bridgeInspectionLink
		namespaceErrors map[string]error
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
					{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
				},
			},
			namespaces: map[string][]bridgeInspectionLink{
				"/proc/101/ns/net": {
					{
						name:        "eth0",
						linkType:    "veth",
						index:       2,
						parentIndex: 100,
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
					{name: "veth-empty", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
				},
			},
			namespaces: map[string][]bridgeInspectionLink{
				"/proc/101/ns/net": {
					{name: "eth0", linkType: "veth", index: 2, parentIndex: 100},
				},
			},
		},
		{
			name:  "unrelated multus interface is ignored",
			cidrs: []string{"10.244.1.0/24"},
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
				},
			},
			namespaces: map[string][]bridgeInspectionLink{
				"/proc/101/ns/net": {
					{
						name:        "eth0",
						linkType:    "veth",
						index:       2,
						parentIndex: 100,
						addresses:   bridgeInspectionAddresses("10.244.1.5"),
					},
					{
						name:        "net1",
						linkType:    "veth",
						index:       3,
						parentIndex: 200,
						addresses:   bridgeInspectionAddresses("192.168.50.10"),
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
					{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
				},
			},
			namespaces: map[string][]bridgeInspectionLink{
				"/proc/101/ns/net": {
					{
						name:        "wrong-parent",
						linkType:    "veth",
						index:       2,
						parentIndex: 999,
						addresses:   bridgeInspectionAddresses("192.168.50.10"),
					},
					{
						name:        "wrong-index",
						linkType:    "veth",
						index:       9,
						parentIndex: 100,
						addresses:   bridgeInspectionAddresses("192.168.50.11"),
					},
					{
						name:        "eth0",
						linkType:    "veth",
						index:       2,
						parentIndex: 100,
						addresses:   bridgeInspectionAddresses("10.244.1.8"),
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
					{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
				},
			},
			namespaces: map[string][]bridgeInspectionLink{
				"/proc/101/ns/net": {
					{
						name:        "collision",
						linkType:    "veth",
						index:       77,
						parentIndex: 100,
						addresses:   bridgeInspectionAddresses("192.168.50.10"),
					},
				},
				"/proc/202/ns/net": {
					{
						name:        "eth0",
						linkType:    "veth",
						index:       2,
						parentIndex: 100,
						addresses:   bridgeInspectionAddresses("10.244.1.8"),
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
					{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
				},
			},
			namespaces: map[string][]bridgeInspectionLink{
				"/proc/101/ns/net": {
					{
						name:        "eth0",
						linkType:    "veth",
						index:       2,
						parentIndex: 100,
						addresses:   bridgeInspectionAddresses("10.244.1.8"),
					},
				},
			},
			namespaceErrors: map[string]error{
				"/proc/202/ns/net": &bridgeInspectionNamespaceGoneError{err: syscall.ENOENT},
				"/proc/303/ns/net": &bridgeInspectionNamespaceGoneError{err: syscall.ESRCH},
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			deps := bridgeInspectionTestDependencies(
				[]bridgeInspectionSnapshot{test.snapshot, test.snapshot},
				test.namespaces,
				test.namespaceErrors,
			)

			if err := inspectBridgePodCIDRs(
				context.Background(),
				"cbr0",
				"/proc",
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
			{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
		},
	}
	validPeer := map[string][]bridgeInspectionLink{
		"/proc/101/ns/net": {
			{
				name:        "eth0",
				linkType:    "veth",
				index:       2,
				parentIndex: 100,
				addresses:   bridgeInspectionAddresses("10.244.1.5"),
			},
		},
	}

	tests := []struct {
		name             string
		snapshot         bridgeInspectionSnapshot
		namespaces       map[string][]bridgeInspectionLink
		namespaceErrors  map[string]error
		hostError        error
		namespaceListErr error
		want             string
	}{
		{
			name:       "address outside assigned prefix",
			snapshot:   baseSnapshot,
			namespaces: bridgeInspectionPeerWithAddresses("10.244.2.5"),
			want:       "10.244.2.5 outside assigned PodCIDRs",
		},
		{
			name: "unsupported live bridge port",
			snapshot: bridgeInspectionSnapshot{
				bridgeIndex: 10,
				ports: []bridgeInspectionPort{
					{name: "tap0", linkType: "tun", hostIndex: 100, masterIndex: 10},
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
						name:        "veth-managed",
						linkType:    "veth",
						hostIndex:   100,
						masterIndex: 10,
						peerError:   "device busy",
					},
				},
			},
			want: "resolve pod-side peer",
		},
		{
			name:       "live peer not found",
			snapshot:   baseSnapshot,
			namespaces: map[string][]bridgeInspectionLink{},
			want:       "was not found through /proc",
		},
		{
			name:     "managed peer namespace disappeared",
			snapshot: baseSnapshot,
			namespaceErrors: map[string]error{
				"/proc/101/ns/net": &bridgeInspectionNamespaceGoneError{err: syscall.ENOENT},
			},
			want: "was not found through /proc",
		},
		{
			name:       "raw address inspection ENOENT still blocks",
			snapshot:   baseSnapshot,
			namespaces: validPeer,
			namespaceErrors: map[string]error{
				"/proc/202/ns/net": syscall.ENOENT,
			},
			want: "no such file or directory",
		},
		{
			name:       "wrapped address inspection ENOENT still blocks",
			snapshot:   baseSnapshot,
			namespaces: validPeer,
			namespaceErrors: map[string]error{
				"/proc/202/ns/net": fmt.Errorf("list addresses on interface eth9: %w", syscall.ENOENT),
			},
			want: "list addresses on interface eth9",
		},
		{
			name:     "ambiguous peer across namespaces",
			snapshot: baseSnapshot,
			namespaces: map[string][]bridgeInspectionLink{
				"/proc/101/ns/net": validPeer["/proc/101/ns/net"],
				"/proc/202/ns/net": validPeer["/proc/101/ns/net"],
			},
			want: "is ambiguous across",
		},
		{
			name:            "namespace inspection failure",
			snapshot:        baseSnapshot,
			namespaces:      validPeer,
			namespaceErrors: map[string]error{"/proc/101/ns/net": errors.New("list addresses on interface eth0: permission denied")},
			want:            "list addresses on interface eth0",
		},
		{
			name:             "namespace discovery failure",
			snapshot:         baseSnapshot,
			namespaceListErr: errors.New("read failed"),
			want:             "discover network namespaces",
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
				test.namespaces,
				test.namespaceErrors,
			)

			deps.namespacePaths = func(
				_ context.Context,
				_ string,
			) ([]bridgeInspectionNamespace, error) {
				if test.namespaceListErr != nil {
					return nil, test.namespaceListErr
				}

				return bridgeInspectionTestNamespaces(test.namespaces, test.namespaceErrors), nil
			}
			if test.hostError != nil {
				deps.hostSnapshot = func(
					_ context.Context,
					_ string,
				) (bridgeInspectionSnapshot, error) {
					hostCalls++

					return bridgeInspectionSnapshot{}, test.hostError
				}
			}

			err := inspectBridgePodCIDRs(
				context.Background(),
				"cbr0",
				"/proc",
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
		procRoot   string
		cidrs      []string
		want       string
	}{
		{name: "no assignments", bridgeName: "cbr0", procRoot: "/proc", want: "PodCIDRs are empty"},
		{name: "empty assignment", bridgeName: "cbr0", procRoot: "/proc", cidrs: []string{""}, want: "PodCIDR is empty"},
		{
			name:       "invalid assignment",
			bridgeName: "cbr0",
			procRoot:   "/proc",
			cidrs:      []string{"not-a-prefix"},
			want:       "parse assigned PodCIDR",
		},
		{
			name:     "empty bridge name",
			procRoot: "/proc",
			cidrs:    []string{"10.244.1.0/24"},
			want:     "bridge name is empty",
		},
		{
			name:       "empty proc root",
			bridgeName: "cbr0",
			cidrs:      []string{"10.244.1.0/24"},
			want:       "process root is empty",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := inspectBridgePodCIDRs(
				context.Background(),
				test.bridgeName,
				test.procRoot,
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
			{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
		},
	}
	empty := bridgeInspectionSnapshot{bridgeIndex: 10}
	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{live, empty, empty, empty},
		map[string][]bridgeInspectionLink{},
		nil,
	)

	if err := inspectBridgePodCIDRs(
		context.Background(),
		"cbr0",
		"/proc",
		[]string{"10.244.1.0/24"},
		deps,
	); err != nil {
		t.Fatalf("inspectBridgePodCIDRs() error = %v", err)
	}
}

func TestInspectBridgePodCIDRsBlocksContinuouslyChangingTopology(t *testing.T) {
	t.Parallel()

	empty := bridgeInspectionSnapshot{bridgeIndex: 10}
	live := bridgeInspectionSnapshot{
		bridgeIndex: 10,
		ports: []bridgeInspectionPort{
			{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
		},
	}
	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{empty, live, empty, live, empty, live},
		map[string][]bridgeInspectionLink{},
		nil,
	)

	err := inspectBridgePodCIDRs(
		context.Background(),
		"cbr0",
		"/proc",
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
		"/proc",
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
			{name: "veth-managed", linkType: "veth", hostIndex: 100, peerIndex: 2, masterIndex: 10},
		},
	}
	deps := bridgeInspectionTestDependencies(
		[]bridgeInspectionSnapshot{snapshot, snapshot},
		bridgeInspectionPeerWithAddresses("10.244.1.8"),
		nil,
	)
	originalNamespaceSnapshot := deps.namespaceSnapshot
	deps.namespaceSnapshot = func(
		ctx context.Context,
		namespace bridgeInspectionNamespace,
		hostPeers map[int]int,
	) ([]bridgeInspectionLink, error) {
		links, err := originalNamespaceSnapshot(ctx, namespace, hostPeers)

		cancel()

		return links, err
	}

	err := inspectBridgePodCIDRs(
		ctx,
		"cbr0",
		"/proc",
		[]string{"10.244.1.0/24"},
		deps,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("inspectBridgePodCIDRs() error = %v, want context cancellation", err)
	}
}

func TestRealBridgeInspectionHostSnapshotFiltersOtherBridges(t *testing.T) {
	t.Parallel()

	bridge := &vnetlink.Bridge{
		LinkAttrs: vnetlink.LinkAttrs{Name: "cbr0", Index: 10},
	}
	managedPort := &vnetlink.Veth{
		LinkAttrs: vnetlink.LinkAttrs{Name: "veth-managed", Index: 100, MasterIndex: 10},
	}
	otherBridgePort := &vnetlink.Veth{
		LinkAttrs: vnetlink.LinkAttrs{Name: "veth-other", Index: 200, MasterIndex: 20},
	}
	otherBridgeUnsupportedPort := &vnetlink.Dummy{
		LinkAttrs: vnetlink.LinkAttrs{Name: "dummy-other", Index: 201, MasterIndex: 20},
	}
	peerCalls := 0
	operations := bridgeInspectionHostOperations{
		linkByName: func(name string) (vnetlink.Link, error) {
			if name != "cbr0" {
				t.Fatalf("linkByName() name = %q, want cbr0", name)
			}

			return bridge, nil
		},
		linkList: func() ([]vnetlink.Link, error) {
			return []vnetlink.Link{otherBridgePort, managedPort, otherBridgeUnsupportedPort}, nil
		},
		vethPeerIndex: func(veth *vnetlink.Veth) (int, error) {
			peerCalls++

			if veth.Attrs().Index != managedPort.Attrs().Index {
				t.Fatalf("vethPeerIndex() called for other bridge port %s", veth.Attrs().Name)
			}

			return 2, nil
		},
	}

	snapshot, err := realBridgeInspectionHostSnapshotWithOperations(context.Background(), "cbr0", operations)
	if err != nil {
		t.Fatalf("realBridgeInspectionHostSnapshotWithOperations() error = %v", err)
	}

	if len(snapshot.ports) != 1 || snapshot.ports[0].name != "veth-managed" {
		t.Fatalf("snapshot ports = %#v, want only veth-managed", snapshot.ports)
	}

	if peerCalls != 1 {
		t.Fatalf("vethPeerIndex() calls = %d, want 1", peerCalls)
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

	namespaces, err := realBridgeInspectionNamespacePaths(context.Background(), procRoot)
	if err != nil {
		t.Fatalf("realBridgeInspectionNamespacePaths() error = %v", err)
	}

	if len(namespaces) != 1 {
		t.Fatalf("realBridgeInspectionNamespacePaths() returned %d namespaces, want 1", len(namespaces))
	}

	if namespaces[0].path != podPath {
		t.Fatalf("realBridgeInspectionNamespacePaths() path = %q, want %q", namespaces[0].path, podPath)
	}
}

func bridgeInspectionTestDependencies(
	snapshots []bridgeInspectionSnapshot,
	namespaceLinks map[string][]bridgeInspectionLink,
	namespaceErrors map[string]error,
) bridgeInspectionDependencies {
	nextSnapshot := 0

	return bridgeInspectionDependencies{
		hostSnapshot: func(
			_ context.Context,
			_ string,
		) (bridgeInspectionSnapshot, error) {
			if nextSnapshot >= len(snapshots) {
				return snapshots[len(snapshots)-1], nil
			}

			snapshot := snapshots[nextSnapshot]
			nextSnapshot++

			return snapshot, nil
		},
		namespacePaths: func(
			_ context.Context,
			_ string,
		) ([]bridgeInspectionNamespace, error) {
			return bridgeInspectionTestNamespaces(namespaceLinks, namespaceErrors), nil
		},
		namespaceSnapshot: func(
			_ context.Context,
			namespace bridgeInspectionNamespace,
			_ map[int]int,
		) ([]bridgeInspectionLink, error) {
			if err := namespaceErrors[namespace.path]; err != nil {
				return nil, err
			}

			return namespaceLinks[namespace.path], nil
		},
	}
}

func bridgeInspectionTestNamespaces(
	namespaceLinks map[string][]bridgeInspectionLink,
	namespaceErrors map[string]error,
) []bridgeInspectionNamespace {
	paths := make(map[string]struct{}, len(namespaceLinks)+len(namespaceErrors))
	for path := range namespaceLinks {
		paths[path] = struct{}{}
	}

	for path := range namespaceErrors {
		paths[path] = struct{}{}
	}

	namespaces := make([]bridgeInspectionNamespace, 0, len(paths))
	inode := uint64(1)

	for path := range paths {
		namespaces = append(namespaces, bridgeInspectionNamespace{
			path: path,
			id:   networkNamespaceID{device: 1, inode: inode},
		})
		inode++
	}

	return namespaces
}

func bridgeInspectionPeerWithAddresses(addresses ...string) map[string][]bridgeInspectionLink {
	return map[string][]bridgeInspectionLink{
		"/proc/101/ns/net": {
			{
				name:        "eth0",
				linkType:    "veth",
				index:       2,
				parentIndex: 100,
				addresses:   bridgeInspectionAddresses(addresses...),
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
