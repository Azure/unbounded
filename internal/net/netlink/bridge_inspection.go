// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	vnetlink "github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
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
	peerNetNsID int
	peerError   string
	masterIndex int
}

type bridgeInspectionSnapshot struct {
	absent      bool
	bridgeIndex int
	ports       []bridgeInspectionPort
}

type bridgeInspectionLink struct {
	name        string
	linkType    string
	index       int
	parentIndex int
	addresses   []netip.Addr
}

type bridgeInspectionDependencies struct {
	hostSnapshot   func(context.Context, string) (bridgeInspectionSnapshot, error)
	targetSnapshot func(context.Context, int, map[int]int) ([]bridgeInspectionLink, error)
}

type bridgeInspectionRouteExecutor interface {
	Execute(*nl.NetlinkRequest, uint16) ([][]byte, error)
	Close()
}

type bridgeInspectionRouteSocket struct {
	socket *nl.NetlinkSocket
	handle *nl.SocketHandle
}

type bridgeInspectionRouteSocketOperations struct {
	subscribe         func(int, ...uint) (*nl.NetlinkSocket, error)
	setStrictCheck    func(int, int, int, int) error
	setSendTimeout    func(*nl.NetlinkSocket, *unix.Timeval) error
	setReceiveTimeout func(*nl.NetlinkSocket, *unix.Timeval) error
}

// InspectBridgePodCIDRs verifies that non-link-local addresses on pod-side
// veth peers attached to bridgeName belong to at least one assigned PodCIDR.
func InspectBridgePodCIDRs(ctx context.Context, bridgeName string, cidrs []string) error {
	return inspectBridgePodCIDRs(ctx, bridgeName, cidrs, realBridgeInspectionDependencies())
}

func inspectBridgePodCIDRs(
	ctx context.Context,
	bridgeName string,
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

	for attempt := 1; attempt <= bridgeInspectionAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("inspect bridge %s: %w", bridgeName, err)
		}

		before, err := deps.hostSnapshot(ctx, bridgeName)
		if err != nil {
			return fmt.Errorf("inspect managed bridge %s: %w", bridgeName, err)
		}

		inspectionErr := inspectBridgeSnapshot(ctx, bridgeName, prefixes, before, deps)

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
	prefixes []netip.Prefix,
	snapshot bridgeInspectionSnapshot,
	deps bridgeInspectionDependencies,
) error {
	if snapshot.absent || len(snapshot.ports) == 0 {
		return nil
	}

	targets := make(map[int]map[int]int)

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

		hostPeers := targets[port.peerNetNsID]
		if hostPeers == nil {
			hostPeers = make(map[int]int)
			targets[port.peerNetNsID] = hostPeers
		}

		hostPeers[port.hostIndex] = port.peerIndex
	}

	var findings []error

	for _, netNsID := range sortedBridgeInspectionTargetIDs(targets) {
		if err := ctx.Err(); err != nil {
			return err
		}

		links, err := deps.targetSnapshot(ctx, netNsID, targets[netNsID])
		if err != nil {
			findings = append(findings, fmt.Errorf(
				"inspect network namespace ID %s: %w",
				bridgeInspectionTargetName(netNsID),
				err,
			))

			continue
		}

		matches := make(map[int]int, len(links))

		for _, link := range links {
			expectedPeer, ok := targets[netNsID][link.parentIndex]
			if !ok || link.linkType != "veth" || link.index != expectedPeer {
				continue
			}

			matches[link.parentIndex]++

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
						"managed bridge %s pod interface %s in network namespace ID %s has address %s outside assigned PodCIDRs %v",
						bridgeName,
						link.name,
						bridgeInspectionTargetName(netNsID),
						address,
						prefixes,
					))
				}
			}
		}

		for hostIndex, peerIndex := range targets[netNsID] {
			switch matches[hostIndex] {
			case 0:
				findings = append(findings, fmt.Errorf(
					"pod-side peer for managed bridge %s host index %d (peer index %d) was not found in network namespace ID %s",
					bridgeName,
					hostIndex,
					peerIndex,
					bridgeInspectionTargetName(netNsID),
				))
			case 1:
			default:
				findings = append(findings, fmt.Errorf(
					"pod-side peer for managed bridge %s host index %d (peer index %d) is ambiguous in network namespace ID %s",
					bridgeName,
					hostIndex,
					peerIndex,
					bridgeInspectionTargetName(netNsID),
				))
			}
		}
	}

	return errors.Join(findings...)
}

func sortedBridgeInspectionTargetIDs(targets map[int]map[int]int) []int {
	ids := make([]int, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}

	sort.Ints(ids)

	return ids
}

func bridgeInspectionTargetName(netNsID int) string {
	if netNsID < 0 {
		return "host"
	}

	return fmt.Sprintf("%d", netNsID)
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
		hostSnapshot:   realBridgeInspectionHostSnapshot,
		targetSnapshot: realBridgeInspectionTargetSnapshot,
	}
}

func realBridgeInspectionHostSnapshot(ctx context.Context, bridgeName string) (bridgeInspectionSnapshot, error) {
	executor, err := newBridgeInspectionRouteSocket(ctx)
	if err != nil {
		return bridgeInspectionSnapshot{}, err
	}
	defer executor.Close()

	return bridgeInspectionHostSnapshotWithExecutor(ctx, bridgeName, executor)
}

func bridgeInspectionHostSnapshotWithExecutor(
	ctx context.Context,
	bridgeName string,
	executor bridgeInspectionRouteExecutor,
) (bridgeInspectionSnapshot, error) {
	request := nl.NewNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_DUMP)
	request.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))

	messages, err := executor.Execute(request, unix.RTM_NEWLINK)
	if err != nil {
		return bridgeInspectionSnapshot{}, fmt.Errorf("query host links: %w", bridgeInspectionContextError(ctx, err))
	}

	links := make([]vnetlink.Link, 0, len(messages))
	for _, message := range messages {
		if len(message) < unix.SizeofIfInfomsg {
			return bridgeInspectionSnapshot{}, fmt.Errorf("parse host link response: message is shorter than a link header")
		}

		link, err := vnetlink.LinkDeserialize(nil, message)
		if err != nil {
			return bridgeInspectionSnapshot{}, fmt.Errorf("parse host link response: %w", err)
		}

		links = append(links, link)
	}

	var bridge vnetlink.Link

	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return bridgeInspectionSnapshot{}, err
		}

		if link.Attrs().Name == bridgeName {
			bridge = link

			break
		}
	}

	if bridge == nil {
		return bridgeInspectionSnapshot{absent: true}, nil
	}

	if _, ok := bridge.(*vnetlink.Bridge); !ok {
		return bridgeInspectionSnapshot{}, fmt.Errorf(
			"link %s has type %s, expected bridge",
			bridgeName,
			bridge.Type(),
		)
	}

	snapshot := bridgeInspectionSnapshot{bridgeIndex: bridge.Attrs().Index}
	for _, link := range links {
		if link.Attrs().MasterIndex != snapshot.bridgeIndex {
			continue
		}

		snapshot.ports = append(snapshot.ports, bridgeInspectionPort{
			name:        link.Attrs().Name,
			linkType:    link.Type(),
			hostIndex:   link.Attrs().Index,
			peerIndex:   link.Attrs().ParentIndex,
			peerNetNsID: link.Attrs().NetNsID,
			masterIndex: link.Attrs().MasterIndex,
		})
	}

	sort.Slice(snapshot.ports, func(i, j int) bool {
		if snapshot.ports[i].hostIndex != snapshot.ports[j].hostIndex {
			return snapshot.ports[i].hostIndex < snapshot.ports[j].hostIndex
		}

		return snapshot.ports[i].name < snapshot.ports[j].name
	})

	return snapshot, nil
}

func realBridgeInspectionTargetSnapshot(
	ctx context.Context,
	netNsID int,
	hostPeers map[int]int,
) ([]bridgeInspectionLink, error) {
	executor, err := newBridgeInspectionRouteSocket(ctx)
	if err != nil {
		return nil, err
	}
	defer executor.Close()

	return bridgeInspectionTargetSnapshotWithExecutor(ctx, netNsID, hostPeers, executor)
}

func bridgeInspectionTargetSnapshotWithExecutor(
	ctx context.Context,
	netNsID int,
	hostPeers map[int]int,
	executor bridgeInspectionRouteExecutor,
) ([]bridgeInspectionLink, error) {
	links, err := bridgeInspectionTargetLinksWithExecutor(ctx, netNsID, hostPeers, executor)
	if err != nil {
		return nil, err
	}

	linksByHostIndex := make(map[int]bridgeInspectionLink, len(links))
	for _, link := range links {
		linksByHostIndex[link.parentIndex] = link
	}

	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		request := nl.NewNetlinkRequest(unix.RTM_GETADDR, unix.NLM_F_DUMP)
		request.AddData(nl.NewIfAddrmsg(family))

		if netNsID >= 0 {
			request.AddData(nl.NewRtAttr(unix.IFA_TARGET_NETNSID, nl.Uint32Attr(uint32(netNsID))))
		}

		messages, err := executor.Execute(request, unix.RTM_NEWADDR)
		if err != nil {
			return nil, fmt.Errorf(
				"query %s addresses: %w",
				bridgeInspectionFamilyName(family),
				bridgeInspectionContextError(ctx, err),
			)
		}

		for _, message := range messages {
			index, address, err := parseBridgeInspectionAddress(message, family)
			if err != nil {
				return nil, fmt.Errorf("parse %s address response: %w", bridgeInspectionFamilyName(family), err)
			}

			for hostIndex, link := range linksByHostIndex {
				if link.index != index {
					continue
				}

				link.addresses = append(link.addresses, address)
				linksByHostIndex[hostIndex] = link

				break
			}
		}
	}

	result := make([]bridgeInspectionLink, 0, len(linksByHostIndex))
	for _, link := range linksByHostIndex {
		result = append(result, link)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].parentIndex < result[j].parentIndex
	})

	return result, nil
}

func bridgeInspectionTargetLinksWithExecutor(
	ctx context.Context,
	netNsID int,
	hostPeers map[int]int,
	executor bridgeInspectionRouteExecutor,
) ([]bridgeInspectionLink, error) {
	request := nl.NewNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_DUMP)
	request.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))

	if netNsID >= 0 {
		request.AddData(nl.NewRtAttr(unix.IFLA_TARGET_NETNSID, nl.Uint32Attr(uint32(netNsID))))
	}

	messages, err := executor.Execute(request, unix.RTM_NEWLINK)
	if err != nil {
		return nil, fmt.Errorf("query links: %w", bridgeInspectionContextError(ctx, err))
	}

	linksByHostIndex := make(map[int]bridgeInspectionLink)

	for _, message := range messages {
		if len(message) < unix.SizeofIfInfomsg {
			return nil, fmt.Errorf("parse link response: message is shorter than a link header")
		}

		link, err := vnetlink.LinkDeserialize(nil, message)
		if err != nil {
			return nil, fmt.Errorf("parse link response: %w", err)
		}

		expectedPeer, ok := hostPeers[link.Attrs().ParentIndex]
		if !ok || link.Attrs().Index != expectedPeer || link.Type() != "veth" {
			continue
		}

		if _, exists := linksByHostIndex[link.Attrs().ParentIndex]; exists {
			return nil, fmt.Errorf(
				"multiple reciprocal veth links returned for host index %d and peer index %d",
				link.Attrs().ParentIndex,
				expectedPeer,
			)
		}

		linksByHostIndex[link.Attrs().ParentIndex] = bridgeInspectionLink{
			name:        link.Attrs().Name,
			linkType:    link.Type(),
			index:       link.Attrs().Index,
			parentIndex: link.Attrs().ParentIndex,
		}
	}

	result := make([]bridgeInspectionLink, 0, len(linksByHostIndex))
	for _, link := range linksByHostIndex {
		result = append(result, link)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].parentIndex < result[j].parentIndex
	})

	return result, nil
}

func parseBridgeInspectionAddress(message []byte, expectedFamily int) (int, netip.Addr, error) {
	if len(message) < unix.SizeofIfAddrmsg {
		return 0, netip.Addr{}, fmt.Errorf("message is shorter than an address header")
	}

	header := nl.DeserializeIfAddrmsg(message)
	if int(header.Family) != expectedFamily {
		return 0, netip.Addr{}, fmt.Errorf(
			"response family is %d, expected %d",
			header.Family,
			expectedFamily,
		)
	}

	attributes, err := nl.ParseRouteAttr(message[header.Len():])
	if err != nil {
		return 0, netip.Addr{}, err
	}

	var rawAddress []byte

	for _, attribute := range attributes {
		switch attribute.Attr.Type {
		case unix.IFA_LOCAL:
			rawAddress = attribute.Value
		case unix.IFA_ADDRESS:
			if rawAddress == nil {
				rawAddress = attribute.Value
			}
		}
	}

	if rawAddress == nil {
		return 0, netip.Addr{}, fmt.Errorf("interface index %d returned an address without an IP", header.Index)
	}

	address, ok := netip.AddrFromSlice(rawAddress)
	if !ok {
		return 0, netip.Addr{}, fmt.Errorf("interface index %d returned invalid address %v", header.Index, rawAddress)
	}

	if expectedFamily == unix.AF_INET && !address.Is4() || expectedFamily == unix.AF_INET6 && !address.Is6() {
		return 0, netip.Addr{}, fmt.Errorf(
			"interface index %d returned %s address %s",
			header.Index,
			bridgeInspectionFamilyName(expectedFamily),
			address,
		)
	}

	return int(header.Index), address.Unmap(), nil
}

func bridgeInspectionFamilyName(family int) string {
	if family == unix.AF_INET {
		return "IPv4"
	}

	return "IPv6"
}

func bridgeInspectionContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

func newBridgeInspectionRouteSocket(ctx context.Context) (*bridgeInspectionRouteSocket, error) {
	return newBridgeInspectionRouteSocketWithOperations(ctx, bridgeInspectionRouteSocketOperations{
		subscribe:         nl.Subscribe,
		setStrictCheck:    unix.SetsockoptInt,
		setSendTimeout:    func(socket *nl.NetlinkSocket, timeout *unix.Timeval) error { return socket.SetSendTimeout(timeout) },
		setReceiveTimeout: func(socket *nl.NetlinkSocket, timeout *unix.Timeval) error { return socket.SetReceiveTimeout(timeout) },
	})
}

func newBridgeInspectionRouteSocketWithOperations(
	ctx context.Context,
	operations bridgeInspectionRouteSocketOperations,
) (*bridgeInspectionRouteSocket, error) {
	timeout, err := bridgeInspectionTimeout(ctx)
	if err != nil {
		return nil, err
	}

	socket, err := operations.subscribe(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open host route netlink socket: %w", err)
	}

	closeOnError := true
	defer func() {
		if closeOnError {
			socket.Close()
		}
	}()

	if err := operations.setStrictCheck(
		socket.GetFd(),
		unix.SOL_NETLINK,
		unix.NETLINK_GET_STRICT_CHK,
		1,
	); err != nil {
		return nil, fmt.Errorf("enable strict checking on host route netlink socket: %w", err)
	}

	timeval := unix.NsecToTimeval(timeout.Nanoseconds())
	if err := operations.setSendTimeout(socket, &timeval); err != nil {
		return nil, fmt.Errorf("set host route netlink send timeout: %w", err)
	}

	if err := operations.setReceiveTimeout(socket, &timeval); err != nil {
		return nil, fmt.Errorf("set host route netlink receive timeout: %w", err)
	}

	handle := &nl.SocketHandle{Socket: socket}
	closeOnError = false

	return &bridgeInspectionRouteSocket{socket: socket, handle: handle}, nil
}

func (s *bridgeInspectionRouteSocket) Execute(request *nl.NetlinkRequest, responseType uint16) ([][]byte, error) {
	request.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: s.handle}

	return request.Execute(unix.NETLINK_ROUTE, responseType)
}

func (s *bridgeInspectionRouteSocket) Close() {
	s.socket.Close()
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
