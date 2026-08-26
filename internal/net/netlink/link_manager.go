// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/klog/v2"
)

// Package-level netlink link function vars for testability, following the
// same convention as the rule functions in route_table_setup.go.
//
// Every link lookup, add and delete in link_manager.go goes through these. A
// test that cannot intercept the lookup cannot exercise how its error is
// classified, and that classification is what isLinkGoneError exists for.
// Other files in this package still call netlink directly where they only
// ever treat the error as fatal or best-effort.
var (
	netlinkLinkByName = netlink.LinkByName
	netlinkLinkAdd    = netlink.LinkAdd
	netlinkLinkDel    = netlink.LinkDel
)

// LinkManager manages a network link (interface) and its addresses
type LinkManager struct {
	ifaceName string
}

// NewLinkManager creates a new LinkManager for the specified interface
func NewLinkManager(ifaceName string) *LinkManager {
	return &LinkManager{
		ifaceName: ifaceName,
	}
}

// EnsureIPIPInterfaceWithRemote creates a point-to-point IPIP tunnel interface
// with 20 bytes of overhead (just an outer IP header, no UDP wrapper).
func (lm *LinkManager) EnsureIPIPInterfaceWithRemote(local, remote net.IP) error {
	_, err := netlinkLinkByName(lm.ifaceName)
	if err == nil {
		return nil
	}

	if lookupErr := lm.lookupError(err); lookupErr != nil {
		return lookupErr
	}

	klog.Infof("Creating IPIP interface %s (local %s, remote %s)", lm.ifaceName, local, remote)

	ipipLink := &netlink.Iptun{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
		Local:  local,
		Remote: remote,
	}
	if err := netlinkLinkAdd(ipipLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create IPIP interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("ipip").Inc()

	return nil
}

// EnsureIPIPExternalInterface creates an external/flow-based IPIP interface
// for use with the eBPF tunnel dataplane. The BPF program sets the tunnel
// destination per-packet via bpf_skb_set_tunnel_key.
func (lm *LinkManager) EnsureIPIPExternalInterface() error {
	existing, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		if lookupErr := lm.lookupError(err); lookupErr != nil {
			return lookupErr
		}
	} else {
		if tun, ok := existing.(*netlink.Iptun); ok && tun.FlowBased {
			return nil
		}
		// Exists but not flow-based -- delete and recreate
		klog.Infof("Recreating IPIP interface %s with external mode", lm.ifaceName)

		if delErr := netlinkLinkDel(existing); delErr != nil {
			return fmt.Errorf("failed to delete IPIP interface %s for recreation: %w", lm.ifaceName, delErr)
		}
	}

	klog.Infof("Creating IPIP interface %s (external/FlowBased)", lm.ifaceName)

	ipipLink := &netlink.Iptun{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
		FlowBased: true,
	}
	if err := netlinkLinkAdd(ipipLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create IPIP external interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("ipip").Inc()

	return nil
}

// EnsureGeneveInterfaceWithRemote creates a point-to-point GENEVE interface
// with a fixed remote tunnel endpoint. Each peer gets its own interface.
func (lm *LinkManager) EnsureGeneveInterfaceWithRemote(vni uint32, dstPort int, remote net.IP) error {
	_, err := netlinkLinkByName(lm.ifaceName)
	if err == nil {
		return nil
	}

	if lookupErr := lm.lookupError(err); lookupErr != nil {
		return lookupErr
	}

	klog.Infof("Creating GENEVE interface %s (VNI %d, port %d, remote %s)", lm.ifaceName, vni, dstPort, remote)
	geneveLink := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
		ID:     vni,
		Dport:  uint16(dstPort),
		Remote: remote,
	}

	if err := netlinkLinkAdd(geneveLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create GENEVE interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("geneve").Inc()

	return nil
}

// EnsureGeneveInterface creates a GENEVE interface if it does not already
// exist. The interface is created in point-to-multipoint mode (no fixed
// remote) with learning disabled so that FDB entries are managed explicitly.
//
// If mac is non-nil it is applied as the hardware address at creation
// time via LinkAttrs.HardwareAddr. This avoids a race where LinkAdd
// returns before the kernel finishes async MAC assignment and a
// subsequent LinkSetHardwareAddr is silently dropped, leaving the
// device with a kernel-randomized MAC.
func (lm *LinkManager) EnsureGeneveInterface(vni uint32, dstPort int, mac net.HardwareAddr) error {
	existing, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		if lookupErr := lm.lookupError(err); lookupErr != nil {
			return lookupErr
		}
	} else {
		// If the existing interface is not in external/FlowBased mode (or has
		// a fixed VNI), delete and recreate it.
		if gn, ok := existing.(*netlink.Geneve); ok && (!gn.FlowBased || gn.ID != 0) {
			klog.Infof("Recreating GENEVE interface %s with external mode (was FlowBased=%v, VNI=%d)", lm.ifaceName, gn.FlowBased, gn.ID)

			if delErr := netlinkLinkDel(existing); delErr != nil {
				return fmt.Errorf("failed to delete GENEVE interface %s for recreation: %w", lm.ifaceName, delErr)
			}
		} else {
			return nil
		}
	}

	klog.Infof("Creating GENEVE interface %s (port %d, external/FlowBased)", lm.ifaceName, dstPort)
	geneveLink := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name:         lm.ifaceName,
			HardwareAddr: mac,
		},
		Dport:     uint16(dstPort),
		FlowBased: true,
	}

	if err := netlinkLinkAdd(geneveLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create GENEVE interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("geneve").Inc()

	return nil
}

// EnsureVXLANInterface creates an external/flow-based VXLAN interface if it
// does not already exist. The interface is created with FlowBased=true (no
// fixed remote, no fixed VNI), Learning disabled, and the specified UDP
// destination port. Per-peer routing is done via lightweight tunnel encap
// directives on routes rather than per-peer interfaces.
// EnsureVXLANInterface creates a flow-based VXLAN interface if it does not
// exist. The srcPortLow/srcPortHigh parameters control the UDP source port
// range used for outbound VXLAN packets. A narrow range reduces the number
// of distinct flows seen by cloud platform networking, helping avoid VM flow
// table limits on platforms like Azure or GCP.
//
// If mac is non-nil it is applied as the hardware address at creation
// time via LinkAttrs.HardwareAddr. This avoids a race where LinkAdd
// returns before the kernel finishes async MAC assignment and a
// subsequent LinkSetHardwareAddr is silently dropped, leaving the
// device with a kernel-randomized MAC.
func (lm *LinkManager) EnsureVXLANInterface(dstPort, srcPortLow, srcPortHigh int, mac net.HardwareAddr) error {
	_, err := netlinkLinkByName(lm.ifaceName)
	if err == nil {
		return nil
	}

	if lookupErr := lm.lookupError(err); lookupErr != nil {
		return lookupErr
	}

	klog.Infof("Creating VXLAN interface %s (port %d, srcPorts %d-%d, external/FlowBased)", lm.ifaceName, dstPort, srcPortLow, srcPortHigh)
	vxlanLink := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:         lm.ifaceName,
			HardwareAddr: mac,
		},
		FlowBased: true,
		Learning:  false,
		Port:      dstPort,
		VxlanId:   0,
		PortLow:   srcPortLow,
		PortHigh:  srcPortHigh,
	}

	if err := netlinkLinkAdd(vxlanLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create VXLAN interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("vxlan").Inc()

	return nil
}

// EnsureWireGuardInterface creates the WireGuard interface if it doesn't exist
func (lm *LinkManager) EnsureWireGuardInterface() error {
	// Check if interface exists
	_, err := netlinkLinkByName(lm.ifaceName)
	if err == nil {
		// Interface already exists
		return nil
	}

	if lookupErr := lm.lookupError(err); lookupErr != nil {
		return lookupErr
	}

	// Create WireGuard interface
	klog.Infof("Creating WireGuard interface %s", lm.ifaceName)
	wgLink := &netlink.Wireguard{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
	}

	if err := netlinkLinkAdd(wgLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create WireGuard interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("wireguard").Inc()

	return nil
}

// SetLinkUp brings the interface up
func (lm *LinkManager) SetLinkUp() error {
	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up link %s: %w", lm.ifaceName, err)
	}

	return nil
}

// SetLinkNoARP disables ARP on the interface. This is needed for external/
// flow-based tunnel interfaces where the BPF program handles encapsulation --
// the kernel should send packets directly without neighbor resolution.
func (lm *LinkManager) SetLinkNoARP() error {
	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
	}

	if err := netlink.LinkSetARPOff(link); err != nil {
		return fmt.Errorf("failed to set NOARP on %s: %w", lm.ifaceName, err)
	}

	return nil
}

// SetLinkAddress sets the hardware (MAC) address on the interface.
func (lm *LinkManager) SetLinkAddress(addr net.HardwareAddr) error {
	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
	}

	if err := netlink.LinkSetHardwareAddr(link, addr); err != nil {
		return fmt.Errorf("failed to set MAC on %s: %w", lm.ifaceName, err)
	}

	return nil
}

// DeleteLink removes the interface
func (lm *LinkManager) DeleteLink() error {
	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		// Absent is the goal state, so lookupError reports nothing for it. Any
		// other lookup failure has to surface: reporting success here would
		// tell the caller the interface was removed when it is still there.
		return lm.lookupError(err)
	}

	klog.Infof("Removing interface %s", lm.ifaceName)

	if err := netlinkLinkDel(link); err != nil {
		return fmt.Errorf("failed to delete link %s: %w", lm.ifaceName, err)
	}

	return nil
}

// EnsureBridge creates the bridge interface if it does not exist and brings
// it up. Used to ensure cbr0 exists on gateway nodes where the CNI plugin
// may not have created it.
func (lm *LinkManager) EnsureBridge() error {
	_, err := netlinkLinkByName(lm.ifaceName)
	if err == nil {
		return nil
	}

	if lookupErr := lm.lookupError(err); lookupErr != nil {
		return lookupErr
	}

	klog.Infof("Creating bridge %s", lm.ifaceName)

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
	}
	if err := netlinkLinkAdd(bridge); err != nil {
		return fmt.Errorf("failed to create bridge %s: %w", lm.ifaceName, err)
	}

	if err := netlink.LinkSetUp(bridge); err != nil {
		return fmt.Errorf("failed to bring up bridge %s: %w", lm.ifaceName, err)
	}

	return nil
}

// EnsureDummyInterface creates a dummy interface if it does not exist and
// brings it up. Dummy interfaces are ideal for eBPF tunnel dataplanes --
// the kernel routes packets to the device and TC egress BPF handles
// encapsulation and redirect. ARP/NDP requests on dummy interfaces go
// nowhere (no physical medium), so no neighbor resolution issues.
func (lm *LinkManager) EnsureDummyInterface() error {
	existing, err := netlinkLinkByName(lm.ifaceName)
	if err == nil {
		// Ensure NOARP is set (may need to be set on existing interfaces
		// from older code versions that didn't set it).
		if existing.Attrs().Flags&net.FlagUp == 0 {
			_ = netlink.LinkSetUp(existing) //nolint:errcheck
		}

		_ = netlink.LinkSetARPOff(existing) //nolint:errcheck

		return nil
	}

	if lookupErr := lm.lookupError(err); lookupErr != nil {
		return lookupErr
	}

	klog.Infof("Creating dummy interface %s (NOARP)", lm.ifaceName)

	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
	}
	if err := netlinkLinkAdd(dummy); err != nil {
		return fmt.Errorf("failed to create dummy interface %s: %w", lm.ifaceName, err)
	}

	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get dummy interface %s after creation: %w", lm.ifaceName, err)
	}

	if err := netlink.LinkSetARPOff(link); err != nil {
		return fmt.Errorf("failed to set NOARP on %s: %w", lm.ifaceName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", lm.ifaceName, err)
	}

	return nil
}

// SyncAddresses performs a differential update of addresses on the interface.
// It adds addresses that are in desired but not currently configured,
// and removes addresses that are configured but not in desired.
// When removeAll is true, link-local addresses are also removed during cleanup;
// when false, link-local addresses are preserved (normal operational mode).
// Returns the number of addresses added and removed.
func (lm *LinkManager) SyncAddresses(desiredAddrs []string, removeAll bool) (added, removed int, err error) {
	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
	}

	// Get current addresses
	currentAddrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to list addresses: %w", err)
	}

	// Build set of current addresses (by IP string, not including prefix length for matching)
	currentSet := make(map[string]netlink.Addr)

	for _, addr := range currentAddrs {
		if addr.IP != nil {
			currentSet[addr.IP.String()] = addr
		}
	}

	// Build set of desired addresses
	desiredSet := make(map[string]*netlink.Addr)

	for _, addrStr := range desiredAddrs {
		addr, err := parseAddress(addrStr)
		if err != nil {
			klog.Warningf("Invalid address %s: %v", addrStr, err)
			continue
		}

		desiredSet[addr.IP.String()] = addr
	}

	// Add addresses that are desired but not current
	for ipStr, addr := range desiredSet {
		if _, exists := currentSet[ipStr]; !exists {
			if err := netlink.AddrAdd(link, addr); err != nil {
				// Ignore "file exists" errors
				if !strings.Contains(err.Error(), "file exists") {
					klog.Errorf("Failed to add address %s: %v", addr.IPNet.String(), err)
					continue
				}
			}

			added++

			klog.Infof("Added address %s to %s", addr.IPNet.String(), lm.ifaceName)
		}
	}

	// Remove addresses that are current but not desired
	// Only remove addresses that look like WireGuard IPs (skip link-local, etc.)
	for ipStr, addr := range currentSet {
		if _, exists := desiredSet[ipStr]; !exists {
			// Skip link-local addresses unless removeAll is set
			if !removeAll && (addr.IP.IsLinkLocalUnicast() || addr.IP.IsLinkLocalMulticast()) {
				continue
			}

			if err := netlink.AddrDel(link, &addr); err != nil {
				klog.Errorf("Failed to remove address %s: %v", addr.IPNet.String(), err)
				continue
			}

			removed++

			klog.Infof("Removed address %s from %s", addr.IPNet.String(), lm.ifaceName)
		}
	}

	return added, removed, nil
}

// GetAddresses returns the current addresses on the interface
func (lm *LinkManager) GetAddresses() ([]string, error) {
	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}

	result := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IPNet != nil {
			result = append(result, addr.IPNet.String())
		}
	}

	return result, nil
}

// parseAddress parses an address string (IP or CIDR) into a netlink.Addr
func parseAddress(addrStr string) (*netlink.Addr, error) {
	// Check if it's already in CIDR format
	if strings.Contains(addrStr, "/") {
		ip, ipNet, err := net.ParseCIDR(addrStr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR: %w", err)
		}

		return &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   ip,
				Mask: ipNet.Mask,
			},
		}, nil
	}

	// Plain IP address - add appropriate prefix
	ip := net.ParseIP(addrStr)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address: %s", addrStr)
	}

	var mask net.IPMask
	if ip.To4() != nil {
		mask = net.CIDRMask(32, 32)
	} else {
		mask = net.CIDRMask(128, 128)
	}

	return &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: mask,
		},
	}, nil
}

// EnsureMTU checks the current MTU of the managed interface and sets it to
// the desired value if it differs. Returns nil when the MTU is already
// correct or was successfully updated.
func (lm *LinkManager) EnsureMTU(mtu int) error {
	link, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
	}

	current := link.Attrs().MTU
	if current == mtu {
		return nil
	}

	klog.Infof("Setting MTU on %s from %d to %d", lm.ifaceName, current, mtu)

	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		InterfaceOperationErrors.WithLabelValues("set_mtu").Inc()
		return fmt.Errorf("failed to set MTU on %s: %w", lm.ifaceName, err)
	}

	InterfaceOperations.WithLabelValues("set_mtu").Inc()

	return nil
}

// EnsureBridgePortMTUs sets the MTU on every veth interface enslaved to the
// managed bridge. Links removed concurrently with pod teardown are ignored.
func (lm *LinkManager) EnsureBridgePortMTUs(mtu int) error {
	bridge, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get bridge %s: %w", lm.ifaceName, err)
	}

	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links for bridge %s: %w", lm.ifaceName, err)
	}

	var updateErrors []error

	for _, link := range bridgeVethLinks(bridge.Attrs().Index, links) {
		if link.Attrs().MTU == mtu {
			continue
		}

		klog.Infof("Setting MTU on bridge port %s from %d to %d", link.Attrs().Name, link.Attrs().MTU, mtu)

		if err := netlink.LinkSetMTU(link, mtu); err != nil {
			if isLinkGoneError(err) {
				continue
			}

			InterfaceOperationErrors.WithLabelValues("set_mtu").Inc()

			updateErrors = append(updateErrors, fmt.Errorf("failed to set MTU on bridge port %s: %w", link.Attrs().Name, err))

			continue
		}

		InterfaceOperations.WithLabelValues("set_mtu").Inc()
	}

	return errors.Join(updateErrors...)
}

// EnsureBridgePodMTUs sets the MTU on the pod-side peer of every veth
// interface enslaved to the managed bridge.
func (lm *LinkManager) EnsureBridgePodMTUs(procDir string, mtu int) error {
	bridge, err := netlinkLinkByName(lm.ifaceName)
	if err != nil {
		return fmt.Errorf("failed to get bridge %s: %w", lm.ifaceName, err)
	}

	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("failed to list links for bridge %s: %w", lm.ifaceName, err)
	}

	hostPeers := make(map[int]int)

	for _, link := range bridgeVethLinks(bridge.Attrs().Index, links) {
		veth, ok := link.(*netlink.Veth)
		if !ok {
			continue
		}

		peerIndex, err := netlink.VethPeerIndex(veth)
		if err != nil {
			if isLinkGoneError(err) {
				continue
			}

			return fmt.Errorf("get peer index for bridge port %s: %w", link.Attrs().Name, err)
		}

		hostPeers[link.Attrs().Index] = peerIndex
	}

	if len(hostPeers) == 0 {
		return nil
	}

	netnsPaths, err := processNetworkNamespacePaths(procDir)
	if err != nil {
		return err
	}

	matchedHostPeers := make(map[int]bool, len(hostPeers))

	var updateErrors []error

	for _, nsPath := range netnsPaths {
		nsHandle, err := netns.GetFromPath(nsPath)
		if err != nil {
			if !errors.Is(err, syscall.ENOENT) {
				klog.V(4).Infof("Failed to open network namespace %s: %v", nsPath, err)
			}

			continue
		}

		handle, err := netlink.NewHandleAt(nsHandle)
		if err != nil {
			if closeErr := nsHandle.Close(); closeErr != nil {
				klog.V(4).Infof("Failed to close network namespace %s: %v", nsPath, closeErr)
			}

			if !errors.Is(err, syscall.ENOENT) {
				klog.V(4).Infof("Failed to create netlink handle for network namespace %s: %v", nsPath, err)
			}

			continue
		}

		nsLinks, listErr := handle.LinkList()
		if listErr != nil {
			if !isLinkGoneError(listErr) {
				klog.V(4).Infof("Failed to list links in network namespace %s: %v", nsPath, listErr)
			}
		} else {
			for _, podLink := range bridgePodVethLinks(hostPeers, nsLinks) {
				hostIndex := podLink.Attrs().ParentIndex
				matchedHostPeers[hostIndex] = true

				if podLink.Attrs().MTU == mtu {
					continue
				}

				klog.Infof("Setting MTU on pod interface %s in %s from %d to %d",
					podLink.Attrs().Name, nsPath, podLink.Attrs().MTU, mtu)

				if err := handle.LinkSetMTU(podLink, mtu); err != nil {
					if isLinkGoneError(err) {
						continue
					}

					InterfaceOperationErrors.WithLabelValues("set_mtu").Inc()

					updateErrors = append(updateErrors,
						fmt.Errorf("failed to set MTU on pod interface %s in %s: %w", podLink.Attrs().Name, nsPath, err))

					continue
				}

				InterfaceOperations.WithLabelValues("set_mtu").Inc()
			}
		}

		handle.Close()

		if err := nsHandle.Close(); err != nil {
			klog.V(4).Infof("Failed to close network namespace %s: %v", nsPath, err)
		}
	}

	for hostIndex := range hostPeers {
		if matchedHostPeers[hostIndex] {
			continue
		}

		link, err := netlink.LinkByIndex(hostIndex)
		if err != nil {
			if !isLinkGoneError(err) {
				klog.V(4).Infof("Failed to recheck bridge port index %d: %v", hostIndex, err)
			}

			continue
		}

		if link.Attrs().MasterIndex == bridge.Attrs().Index {
			updateErrors = append(updateErrors,
				fmt.Errorf("pod-side peer for bridge port %s was not found through %s", link.Attrs().Name, procDir))
		}
	}

	return errors.Join(updateErrors...)
}

type networkNamespaceID struct {
	device uint64
	inode  uint64
}

func processNetworkNamespacePaths(procDir string) ([]string, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, fmt.Errorf("read process directory %s: %w", procDir, err)
	}

	selfID, err := networkNamespaceIDFromPath(filepath.Join(procDir, "self", "ns", "net"))
	if err != nil {
		return nil, fmt.Errorf("identify current network namespace: %w", err)
	}

	seen := map[networkNamespaceID]bool{selfID: true}
	paths := make([]string, 0)

	for _, entry := range entries {
		if !isNumeric(entry.Name()) {
			continue
		}

		nsPath := filepath.Join(procDir, entry.Name(), "ns", "net")

		id, err := networkNamespaceIDFromPath(nsPath)
		if err != nil {
			if errors.Is(err, syscall.ENOENT) {
				continue
			}

			klog.V(4).Infof("Failed to identify network namespace %s: %v", nsPath, err)

			continue
		}

		if seen[id] {
			continue
		}

		seen[id] = true

		paths = append(paths, nsPath)
	}

	return paths, nil
}

func networkNamespaceIDFromPath(path string) (networkNamespaceID, error) {
	info, err := os.Stat(path)
	if err != nil {
		return networkNamespaceID{}, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return networkNamespaceID{}, fmt.Errorf("unexpected stat type for %s", path)
	}

	return networkNamespaceID{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func isLinkGoneError(err error) bool {
	var linkNotFound netlink.LinkNotFoundError

	return errors.As(err, &linkNotFound) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.ENOENT)
}

// lookupError returns the error to propagate for a link lookup, or nil if
// there is nothing to propagate: either the lookup succeeded, or it failed
// only because the interface is absent.
//
// The distinction matters because a lookup can fail without the interface
// being missing. Code that reads "lookup failed, so create it" will, on a
// transient netlink failure, try to create an interface that already exists;
// code that reads "lookup failed, so it is already gone" will report a
// deletion it never performed.
//
// A nil error must map to nil. isLinkGoneError(nil) is false, so without the
// first condition this would wrap nil and hand back a non-nil error reading
// "%!w(<nil>)". Callers that fall through to here with err == nil, having
// deleted an interface they are about to recreate, would then abort instead.
func (lm *LinkManager) lookupError(err error) error {
	if err == nil || isLinkGoneError(err) {
		return nil
	}

	InterfaceOperationErrors.WithLabelValues("lookup").Inc()

	return fmt.Errorf("failed to look up interface %s: %w", lm.ifaceName, err)
}

func bridgeVethLinks(bridgeIndex int, links []netlink.Link) []netlink.Link {
	ports := make([]netlink.Link, 0)

	for _, link := range links {
		if link.Type() == "veth" && link.Attrs().MasterIndex == bridgeIndex {
			ports = append(ports, link)
		}
	}

	return ports
}

func bridgePodVethLinks(hostPeers map[int]int, links []netlink.Link) []netlink.Link {
	peers := make([]netlink.Link, 0)

	for _, link := range links {
		expectedPeerIndex, ok := hostPeers[link.Attrs().ParentIndex]
		if link.Type() == "veth" && ok && link.Attrs().Index == expectedPeerIndex {
			peers = append(peers, link)
		}
	}

	return peers
}

// WireGuardMTUOverhead is the number of bytes subtracted from the primary
// interface MTU to derive the WireGuard tunnel MTU. This accounts for the
// outer IP header, UDP header, and WireGuard encapsulation overhead.
const WireGuardMTUOverhead = 80

// GeneveMTUOverhead is the number of bytes subtracted from the primary
// interface MTU to derive the GENEVE tunnel MTU. This accounts for the
// outer IPv6 header (40), UDP header (8), and GENEVE base header (8),
// plus 2 bytes of padding for potential GENEVE TLV options.
const GeneveMTUOverhead = 58

// IPIPMTUOverhead is the number of bytes subtracted from the primary
// interface MTU to derive the IPIP (IP-in-IP) tunnel MTU. This accounts
// for the outer IP header (20). IPIP has the lowest overhead of any
// tunnel type since it carries no additional headers.
const IPIPMTUOverhead = 20

// VXLANMTUOverhead is the number of bytes subtracted from the primary
// interface MTU to derive the VXLAN tunnel MTU. This accounts for the
// outer IP header (20), UDP header (8), VXLAN header (8), and inner
// Ethernet header (14).
const VXLANMTUOverhead = 50

// DetectDefaultRouteMTU returns the MTU of the network interface that carries
// the IPv4 default route. Returns 0 if no default route is found or the
// interface cannot be queried.
func DetectDefaultRouteMTU() int {
	return detectDefaultRouteMTUImpl(nil)
}

// DetectDefaultRouteMTUFromCache is like DetectDefaultRouteMTU but reads from
// the provided cache instead of making direct netlink syscalls. Falls back
// to direct calls if the cache is nil.
func DetectDefaultRouteMTUFromCache(cache *NetlinkCache) int {
	return detectDefaultRouteMTUImpl(cache)
}

// DetectRouteMTU returns the MTU of the egress link selected for destination.
// Returns 0 when the route or its link cannot be resolved.
func DetectRouteMTU(destination net.IP, cache *NetlinkCache) int {
	mtu, _ := DetectRouteMTUAndInterface(destination, cache)
	return mtu
}

// DetectRouteMTUAndInterface returns the selected egress link MTU and interface
// name for destination. Unlocked RouteGet MTUs are intentionally ignored
// because Linux may report a transient destination-cache PMTU there; using it
// as an underlay MTU would subtract tunnel overhead repeatedly after a tunnel
// MTU change. Administratively locked route MTUs are still enforced.
func DetectRouteMTUAndInterface(destination net.IP, cache *NetlinkCache) (int, string) {
	if destination == nil {
		return 0, ""
	}

	routes, err := netlink.RouteGet(destination)
	if err != nil {
		klog.V(3).Infof("Failed to resolve route MTU for %s: %v", destination, err)
		return 0, ""
	}

	mtu := 0
	ifaceName := ""

	for _, route := range routes {
		if route.LinkIndex == 0 {
			if route.MTULock {
				mtu, ifaceName = selectLowerMTU(mtu, ifaceName, route.MTU, "")
			}

			continue
		}

		link, linkErr := netlink.LinkByIndex(route.LinkIndex)
		if linkErr != nil && cache != nil {
			link, linkErr = cache.LinkByIndex(route.LinkIndex)
		}

		if linkErr != nil {
			klog.V(3).Infof("Failed to get route link %d for %s: %v", route.LinkIndex, destination, linkErr)

			if route.MTULock {
				mtu, ifaceName = selectLowerMTU(mtu, ifaceName, route.MTU, "")
			}

			continue
		}

		routeIfaceName := link.Attrs().Name
		if route.MTULock {
			mtu, ifaceName = selectLowerMTU(mtu, ifaceName, route.MTU, routeIfaceName)
		}

		mtu, ifaceName = selectLowerMTU(mtu, ifaceName, link.Attrs().MTU, routeIfaceName)
	}

	return mtu, ifaceName
}

func selectLowerMTU(currentMTU int, currentIface string, candidateMTU int, candidateIface string) (int, string) {
	if candidateMTU <= 0 || (currentMTU > 0 && candidateMTU >= currentMTU) {
		return currentMTU, currentIface
	}

	return candidateMTU, candidateIface
}

func detectDefaultRouteMTUImpl(cache *NetlinkCache) int {
	var (
		routes []netlink.Route
		err    error
	)

	if cache != nil {
		routes, err = cache.RouteList(nil, netlink.FAMILY_V4)
	} else {
		routes, err = netlink.RouteList(nil, netlink.FAMILY_V4)
	}

	if err != nil {
		klog.V(3).Infof("Failed to list routes for MTU detection: %v", err)
		return 0
	}

	for _, route := range routes {
		isDefault := false
		if route.Dst == nil {
			isDefault = true
		} else if ones, bits := route.Dst.Mask.Size(); ones == 0 && bits > 0 {
			isDefault = true
		}

		if isDefault {
			var link netlink.Link
			if cache != nil {
				link, err = cache.LinkByIndex(route.LinkIndex)
			} else {
				link, err = netlink.LinkByIndex(route.LinkIndex)
			}

			if err != nil {
				klog.V(3).Infof("Failed to get link for default route: %v", err)
				continue
			}

			mtu := link.Attrs().MTU
			klog.V(4).Infof("Default route interface %s has MTU %d", link.Attrs().Name, mtu)

			return mtu
		}
	}

	klog.V(3).Info("No default route found for MTU detection")

	return 0
}

// DetectDefaultRouteInterface returns the name and link index of the network
// interface that carries the IPv4 default route. Returns an error if no
// default route is found or the interface cannot be queried.
func DetectDefaultRouteInterface() (string, int, error) {
	return detectDefaultRouteInterfaceImpl(nil)
}

// DetectDefaultRouteInterfaceFromCache is like DetectDefaultRouteInterface but
// reads from the provided cache. Falls back to direct calls if cache is nil.
func DetectDefaultRouteInterfaceFromCache(cache *NetlinkCache) (string, int, error) {
	return detectDefaultRouteInterfaceImpl(cache)
}

func detectDefaultRouteInterfaceImpl(cache *NetlinkCache) (string, int, error) {
	var (
		routes []netlink.Route
		err    error
	)

	if cache != nil {
		routes, err = cache.RouteList(nil, netlink.FAMILY_V4)
	} else {
		routes, err = netlink.RouteList(nil, netlink.FAMILY_V4)
	}

	if err != nil {
		return "", 0, fmt.Errorf("failed to list routes: %w", err)
	}

	for _, route := range routes {
		isDefault := false
		if route.Dst == nil {
			isDefault = true
		} else if ones, bits := route.Dst.Mask.Size(); ones == 0 && bits > 0 {
			isDefault = true
		}

		if isDefault {
			var link netlink.Link
			if cache != nil {
				link, err = cache.LinkByIndex(route.LinkIndex)
			} else {
				link, err = netlink.LinkByIndex(route.LinkIndex)
			}

			if err != nil {
				continue
			}

			name := link.Attrs().Name
			klog.V(4).Infof("Default route interface: %s (index %d)", name, route.LinkIndex)

			return name, route.LinkIndex, nil
		}
	}

	return "", 0, fmt.Errorf("no default route found")
}

// Exists returns true if the interface exists.
//
// A lookup that fails for any other reason also reports false, because every
// caller is a reconcile predicate that will be retried. The condition is
// counted and logged rather than returned, so that a false caused by a broken
// lookup can be told apart from a false caused by an absent interface.
func (lm *LinkManager) Exists() bool {
	_, err := netlinkLinkByName(lm.ifaceName)
	if err == nil {
		return true
	}

	if !isLinkGoneError(err) {
		InterfaceOperationErrors.WithLabelValues("lookup").Inc()
		klog.V(2).Infof("Link lookup for %s failed, reporting it as absent: %v", lm.ifaceName, err)
	}

	return false
}

// EnsureGeneveInterfaceWithCache is like EnsureGeneveInterfaceWithRemote but
// checks existence via the netlink cache instead of a netlink syscall.
// Falls back to a direct LinkByName if the cache is nil.
func (lm *LinkManager) EnsureGeneveInterfaceWithCache(cache *NetlinkCache, vni uint32, dstPort int, remote net.IP) error {
	if cache != nil {
		if cache.HasLink(lm.ifaceName) {
			return nil
		}
	} else {
		_, err := netlinkLinkByName(lm.ifaceName)
		if err == nil {
			return nil
		}

		if lookupErr := lm.lookupError(err); lookupErr != nil {
			return lookupErr
		}
	}

	klog.Infof("Creating GENEVE interface %s (VNI %d, port %d, remote %s)", lm.ifaceName, vni, dstPort, remote)
	geneveLink := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
		ID:     vni,
		Dport:  uint16(dstPort),
		Remote: remote,
	}

	if err := netlinkLinkAdd(geneveLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create GENEVE interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("geneve").Inc()

	return nil
}

// EnsureIPIPInterfaceWithCache is like EnsureIPIPInterfaceWithRemote but
// checks existence via the netlink cache instead of a netlink syscall.
// Falls back to a direct LinkByName if the cache is nil.
func (lm *LinkManager) EnsureIPIPInterfaceWithCache(cache *NetlinkCache, local, remote net.IP) error {
	if cache != nil {
		if cache.HasLink(lm.ifaceName) {
			return nil
		}
	} else {
		_, err := netlinkLinkByName(lm.ifaceName)
		if err == nil {
			return nil
		}

		if lookupErr := lm.lookupError(err); lookupErr != nil {
			return lookupErr
		}
	}

	klog.Infof("Creating IPIP interface %s (local %s, remote %s)", lm.ifaceName, local, remote)
	ipipLink := &netlink.Iptun{
		LinkAttrs: netlink.LinkAttrs{
			Name: lm.ifaceName,
		},
		Local:  local,
		Remote: remote,
	}

	if err := netlinkLinkAdd(ipipLink); err != nil {
		InterfaceOperationErrors.WithLabelValues("create").Inc()
		return fmt.Errorf("failed to create IPIP interface: %w", err)
	}

	InterfaceOperations.WithLabelValues("create").Inc()
	Interfaces.WithLabelValues("ipip").Inc()

	return nil
}

// SetLinkUpWithCache brings the interface up using a cached link lookup.
// Falls back to direct LinkByName if the cache is nil or misses.
func (lm *LinkManager) SetLinkUpWithCache(cache *NetlinkCache) error {
	var link netlink.Link
	if cache != nil {
		link, _ = cache.LinkByName(lm.ifaceName) //nolint:errcheck
	}

	if link == nil {
		var err error

		link, err = netlinkLinkByName(lm.ifaceName)
		if err != nil {
			return fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
		}
	}

	if link.Attrs().OperState == netlink.OperUp || (link.Attrs().Flags&net.FlagUp != 0) {
		return nil
	}

	return netlink.LinkSetUp(link)
}

// EnsureMTUWithCache checks and sets MTU using a cached link lookup.
// Falls back to direct LinkByName if the cache is nil or misses.
func (lm *LinkManager) EnsureMTUWithCache(cache *NetlinkCache, mtu int) error {
	var link netlink.Link
	if cache != nil {
		link, _ = cache.LinkByName(lm.ifaceName) //nolint:errcheck
	}

	if link == nil {
		var err error

		link, err = netlinkLinkByName(lm.ifaceName)
		if err != nil {
			return fmt.Errorf("failed to get link %s: %w", lm.ifaceName, err)
		}
	}

	if link.Attrs().MTU == mtu {
		return nil
	}

	klog.Infof("Setting MTU on %s from %d to %d", lm.ifaceName, link.Attrs().MTU, mtu)

	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		InterfaceOperationErrors.WithLabelValues("set_mtu").Inc()
		return fmt.Errorf("failed to set MTU on %s: %w", lm.ifaceName, err)
	}

	InterfaceOperations.WithLabelValues("set_mtu").Inc()

	return nil
}
