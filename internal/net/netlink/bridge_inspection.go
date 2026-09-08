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
	"sort"
	"strings"
	"syscall"
	"time"

	vnetlink "github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	bridgeInspectionAttempts      = 3
	bridgeInspectionSocketTimeout = time.Second
)

type bridgeInspectionPort struct {
	name        string
	linkType    string
	hostIndex   int
	peerIndex   int
	peerError   string
	masterIndex int
}

type bridgeInspectionSnapshot struct {
	absent      bool
	bridgeIndex int
	ports       []bridgeInspectionPort
}

type bridgeInspectionNamespace struct {
	path string
	id   networkNamespaceID
}

type bridgeInspectionNamespaceGoneError struct {
	err error
}

func (e *bridgeInspectionNamespaceGoneError) Error() string {
	return e.err.Error()
}

func (e *bridgeInspectionNamespaceGoneError) Unwrap() error {
	return e.err
}

type bridgeInspectionLink struct {
	name        string
	linkType    string
	index       int
	parentIndex int
	addresses   []netip.Addr
}

type bridgeInspectionDependencies struct {
	hostSnapshot      func(context.Context, string) (bridgeInspectionSnapshot, error)
	namespacePaths    func(context.Context, string) ([]bridgeInspectionNamespace, error)
	namespaceSnapshot func(context.Context, bridgeInspectionNamespace, map[int]int) ([]bridgeInspectionLink, error)
}

type bridgeInspectionHostOperations struct {
	linkByName    func(string) (vnetlink.Link, error)
	linkList      func() ([]vnetlink.Link, error)
	vethPeerIndex func(*vnetlink.Veth) (int, error)
}

// InspectBridgePodCIDRs verifies that non-link-local addresses on pod-side
// veth peers attached to bridgeName belong to at least one assigned PodCIDR.
func InspectBridgePodCIDRs(ctx context.Context, bridgeName, procRoot string, cidrs []string) error {
	return inspectBridgePodCIDRs(ctx, bridgeName, procRoot, cidrs, realBridgeInspectionDependencies())
}

func inspectBridgePodCIDRs(
	ctx context.Context,
	bridgeName string,
	procRoot string,
	cidrs []string,
	deps bridgeInspectionDependencies,
) error {
	prefixes, err := parseBridgeInspectionPrefixes(cidrs)
	if err != nil {
		return err
	}

	if strings.TrimSpace(bridgeName) == "" {
		return fmt.Errorf("managed bridge name is empty")
	}

	if strings.TrimSpace(procRoot) == "" {
		return fmt.Errorf("process root is empty")
	}

	for attempt := 1; attempt <= bridgeInspectionAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("inspect bridge %s: %w", bridgeName, err)
		}

		before, err := deps.hostSnapshot(ctx, bridgeName)
		if err != nil {
			return fmt.Errorf("inspect managed bridge %s: %w", bridgeName, err)
		}

		inspectionErr := inspectBridgeSnapshot(ctx, bridgeName, procRoot, prefixes, before, deps)

		if err := ctx.Err(); err != nil {
			return fmt.Errorf("inspect bridge %s: %w", bridgeName, err)
		}

		after, err := deps.hostSnapshot(ctx, bridgeName)
		if err != nil {
			return fmt.Errorf("recheck managed bridge %s: %w", bridgeName, err)
		}

		if !bridgeInspectionSnapshotsEqual(before, after) {
			continue
		}

		return inspectionErr
	}

	return fmt.Errorf("managed bridge %s topology changed during %d inspection attempts", bridgeName, bridgeInspectionAttempts)
}

func inspectBridgeSnapshot(
	ctx context.Context,
	bridgeName string,
	procRoot string,
	prefixes []netip.Prefix,
	snapshot bridgeInspectionSnapshot,
	deps bridgeInspectionDependencies,
) error {
	if snapshot.absent || len(snapshot.ports) == 0 {
		return nil
	}

	hostPeers := make(map[int]int, len(snapshot.ports))

	for _, port := range snapshot.ports {
		if err := ctx.Err(); err != nil {
			return err
		}

		if port.linkType != "veth" {
			return fmt.Errorf(
				"managed bridge %s has unsupported live port %s (index %d, type %s)",
				bridgeName,
				port.name,
				port.hostIndex,
				port.linkType,
			)
		}

		if port.peerError != "" {
			return fmt.Errorf(
				"resolve pod-side peer for managed bridge %s port %s (index %d): %s",
				bridgeName,
				port.name,
				port.hostIndex,
				port.peerError,
			)
		}

		if port.peerIndex <= 0 {
			return fmt.Errorf(
				"resolve pod-side peer for managed bridge %s port %s (index %d): invalid peer index %d",
				bridgeName,
				port.name,
				port.hostIndex,
				port.peerIndex,
			)
		}

		hostPeers[port.hostIndex] = port.peerIndex
	}

	namespaces, err := deps.namespacePaths(ctx, procRoot)
	if err != nil {
		return fmt.Errorf("discover network namespaces through %s: %w", procRoot, err)
	}

	matches := make(map[int][]string, len(hostPeers))

	var findings []error

	for _, namespace := range namespaces {
		if err := ctx.Err(); err != nil {
			return err
		}

		links, err := deps.namespaceSnapshot(ctx, namespace, hostPeers)
		if err != nil {
			var namespaceGone *bridgeInspectionNamespaceGoneError
			if errors.As(err, &namespaceGone) {
				continue
			}

			findings = append(findings, fmt.Errorf("inspect network namespace %s: %w", namespace.path, err))

			continue
		}

		for _, link := range links {
			if err := ctx.Err(); err != nil {
				return err
			}

			expectedPeer, ok := hostPeers[link.parentIndex]
			if !ok || link.linkType != "veth" || link.index != expectedPeer {
				continue
			}

			match := fmt.Sprintf("%s in %s", link.name, namespace.path)
			matches[link.parentIndex] = append(matches[link.parentIndex], match)

			for _, address := range link.addresses {
				if err := ctx.Err(); err != nil {
					return err
				}

				address = address.Unmap()
				if address.IsLinkLocalUnicast() {
					continue
				}

				if !bridgeInspectionAddressAllowed(address, prefixes) {
					findings = append(findings, fmt.Errorf(
						"managed bridge %s pod interface %s has address %s outside assigned PodCIDRs %v",
						bridgeName,
						match,
						address,
						prefixes,
					))
				}
			}
		}
	}

	for _, port := range snapshot.ports {
		if err := ctx.Err(); err != nil {
			return err
		}

		portMatches := matches[port.hostIndex]

		switch len(portMatches) {
		case 0:
			findings = append(findings, fmt.Errorf(
				"pod-side peer for managed bridge %s port %s (index %d, peer index %d) was not found through %s",
				bridgeName,
				port.name,
				port.hostIndex,
				port.peerIndex,
				procRoot,
			))
		case 1:
		default:
			sort.Strings(portMatches)
			findings = append(findings, fmt.Errorf(
				"pod-side peer for managed bridge %s port %s (index %d, peer index %d) is ambiguous across %s",
				bridgeName,
				port.name,
				port.hostIndex,
				port.peerIndex,
				strings.Join(portMatches, ", "),
			))
		}
	}

	return errors.Join(findings...)
}

func parseBridgeInspectionPrefixes(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("assigned PodCIDRs are empty")
	}

	prefixes := make([]netip.Prefix, 0, len(cidrs))
	seen := make(map[netip.Prefix]struct{}, len(cidrs))

	for _, cidr := range cidrs {
		if strings.TrimSpace(cidr) == "" {
			return nil, fmt.Errorf("assigned PodCIDR is empty")
		}

		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("parse assigned PodCIDR %q: %w", cidr, err)
		}

		prefix = prefix.Masked()
		if _, ok := seen[prefix]; ok {
			continue
		}

		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}

	sort.Slice(prefixes, func(i, j int) bool {
		return prefixes[i].String() < prefixes[j].String()
	})

	return prefixes, nil
}

func bridgeInspectionAddressAllowed(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}

	return false
}

func bridgeInspectionSnapshotsEqual(left, right bridgeInspectionSnapshot) bool {
	if left.absent != right.absent || left.bridgeIndex != right.bridgeIndex || len(left.ports) != len(right.ports) {
		return false
	}

	for i := range left.ports {
		if left.ports[i] != right.ports[i] {
			return false
		}
	}

	return true
}

func realBridgeInspectionDependencies() bridgeInspectionDependencies {
	return bridgeInspectionDependencies{
		hostSnapshot:      realBridgeInspectionHostSnapshot,
		namespacePaths:    realBridgeInspectionNamespacePaths,
		namespaceSnapshot: realBridgeInspectionNamespaceSnapshot,
	}
}

func realBridgeInspectionHostSnapshot(ctx context.Context, bridgeName string) (bridgeInspectionSnapshot, error) {
	return realBridgeInspectionHostSnapshotWithOperations(ctx, bridgeName, bridgeInspectionHostOperations{
		linkByName:    vnetlink.LinkByName,
		linkList:      vnetlink.LinkList,
		vethPeerIndex: vnetlink.VethPeerIndex,
	})
}

func realBridgeInspectionHostSnapshotWithOperations(
	ctx context.Context,
	bridgeName string,
	operations bridgeInspectionHostOperations,
) (bridgeInspectionSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return bridgeInspectionSnapshot{}, err
	}

	bridge, err := operations.linkByName(bridgeName)
	if err != nil {
		var notFound vnetlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return bridgeInspectionSnapshot{absent: true}, nil
		}

		return bridgeInspectionSnapshot{}, fmt.Errorf("look up bridge: %w", err)
	}

	if _, ok := bridge.(*vnetlink.Bridge); !ok {
		return bridgeInspectionSnapshot{}, fmt.Errorf(
			"link %s has type %s, expected bridge",
			bridgeName,
			bridge.Type(),
		)
	}

	links, err := operations.linkList()
	if err != nil {
		return bridgeInspectionSnapshot{}, fmt.Errorf("list host links: %w", err)
	}

	snapshot := bridgeInspectionSnapshot{bridgeIndex: bridge.Attrs().Index}

	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return bridgeInspectionSnapshot{}, err
		}

		if link.Attrs().MasterIndex != snapshot.bridgeIndex {
			continue
		}

		port := bridgeInspectionPort{
			name:        link.Attrs().Name,
			linkType:    link.Type(),
			hostIndex:   link.Attrs().Index,
			masterIndex: link.Attrs().MasterIndex,
		}

		if veth, ok := link.(*vnetlink.Veth); ok {
			peerIndex, peerErr := operations.vethPeerIndex(veth)
			if peerErr != nil {
				port.peerError = peerErr.Error()
			} else {
				port.peerIndex = peerIndex
			}
		}

		snapshot.ports = append(snapshot.ports, port)
	}

	sort.Slice(snapshot.ports, func(i, j int) bool {
		if snapshot.ports[i].hostIndex != snapshot.ports[j].hostIndex {
			return snapshot.ports[i].hostIndex < snapshot.ports[j].hostIndex
		}

		return snapshot.ports[i].name < snapshot.ports[j].name
	})

	return snapshot, nil
}

func realBridgeInspectionNamespacePaths(
	ctx context.Context,
	procRoot string,
) ([]bridgeInspectionNamespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read process directory: %w", err)
	}

	selfID, err := networkNamespaceIDFromPath(filepath.Join(procRoot, "self", "ns", "net"))
	if err != nil {
		return nil, fmt.Errorf("identify current network namespace: %w", err)
	}

	seen := map[networkNamespaceID]struct{}{selfID: {}}
	namespaces := make([]bridgeInspectionNamespace, 0)

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !isNumeric(entry.Name()) {
			continue
		}

		path := filepath.Join(procRoot, entry.Name(), "ns", "net")

		id, err := networkNamespaceIDFromPath(path)
		if err != nil {
			if errors.Is(err, syscall.ENOENT) {
				continue
			}

			return nil, fmt.Errorf("identify network namespace %s: %w", path, err)
		}

		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}
		namespaces = append(namespaces, bridgeInspectionNamespace{path: path, id: id})
	}

	sort.Slice(namespaces, func(i, j int) bool {
		return namespaces[i].path < namespaces[j].path
	})

	return namespaces, nil
}

func realBridgeInspectionNamespaceSnapshot(
	ctx context.Context,
	namespace bridgeInspectionNamespace,
	hostPeers map[int]int,
) (links []bridgeInspectionLink, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	nsHandle, err := netns.GetFromPath(namespace.path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESRCH) {
			return nil, &bridgeInspectionNamespaceGoneError{err: fmt.Errorf("open namespace: %w", err)}
		}

		return nil, fmt.Errorf("open namespace: %w", err)
	}

	defer func() {
		if err := nsHandle.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close namespace: %w", err))
		}
	}()

	var stat syscall.Stat_t
	if err := syscall.Fstat(int(nsHandle), &stat); err != nil {
		return nil, fmt.Errorf("identify opened namespace: %w", err)
	}

	openedID := networkNamespaceID{device: uint64(stat.Dev), inode: stat.Ino}
	if openedID != namespace.id {
		return nil, fmt.Errorf("namespace identity changed while opening %s", namespace.path)
	}

	handle, err := vnetlink.NewHandleAt(nsHandle)
	if err != nil {
		return nil, fmt.Errorf("create netlink handle: %w", err)
	}

	defer handle.Close()

	socketTimeout, err := bridgeInspectionTimeout(ctx)
	if err != nil {
		return nil, err
	}

	if err := handle.SetSocketTimeout(socketTimeout); err != nil {
		return nil, fmt.Errorf("set netlink socket timeout: %w", err)
	}

	namespaceLinks, err := handle.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}

	for _, link := range namespaceLinks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		expectedPeer, ok := hostPeers[link.Attrs().ParentIndex]
		if !ok || link.Type() != "veth" || link.Attrs().Index != expectedPeer {
			continue
		}

		addresses, err := handle.AddrList(link, vnetlink.FAMILY_ALL)
		if err != nil {
			return nil, fmt.Errorf("list addresses on interface %s: %w", link.Attrs().Name, err)
		}

		inspected := bridgeInspectionLink{
			name:        link.Attrs().Name,
			linkType:    link.Type(),
			index:       link.Attrs().Index,
			parentIndex: link.Attrs().ParentIndex,
			addresses:   make([]netip.Addr, 0, len(addresses)),
		}

		for _, address := range addresses {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			if address.IPNet == nil {
				return nil, fmt.Errorf("interface %s returned an address without an IP", link.Attrs().Name)
			}

			ip, ok := netip.AddrFromSlice(address.IP)
			if !ok {
				return nil, fmt.Errorf("interface %s returned invalid address %q", link.Attrs().Name, address.IP)
			}

			inspected.addresses = append(inspected.addresses, ip.Unmap())
		}

		links = append(links, inspected)
	}

	return links, nil
}

func bridgeInspectionTimeout(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	timeout := bridgeInspectionSocketTimeout

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}

		timeout = min(timeout, remaining)
	}

	return max(timeout, time.Microsecond), nil
}
