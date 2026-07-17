// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all tunable settings for the playtime client.
//
// The defaults describe a VXLAN overlay whose underlay rides the unbounded-net
// WireGuard mesh: the local box bootstraps its own dedicated "playtime" site
// (assigned to a gateway pool), joins the mesh as a temporary node in that site,
// peers with the pool's external gateways, and reaches a self-configuring demo
// pod running on some other site. The client MUST live in a different site than
// the demo pod: same-site nodes reach each other over their shared local
// underlay (which the roaming client is not on), so only inter-site traffic is
// relayed through the gateways the client actually peers with. Bootstrapping a
// dedicated site guarantees that separation regardless of where the pod lands.
type Config struct {
	// Kubernetes access.
	KubeContext string
	Namespace   string

	// TTL bounds the lifetime of every cluster resource a run creates. The
	// Node anchor is stamped with an expires-at annotation (now + TTL) and the
	// demo pod is given an activeDeadlineSeconds backstop, so orphaned runs are
	// always reaped even if the client, the pod, or the network disappears. It
	// is a fixed, non-renewable budget: a session that must outlive it has to
	// pass a larger --ttl up front.
	TTL time.Duration

	// Temporary Node object used purely as a mesh identity.
	NodeName       string
	NodeSite       string
	NodeInternalIP string
	NodePodCIDR    string

	// Dedicated site bootstrap. playtime creates a Site named NodeSite (if it
	// does not already exist) plus a SiteGatewayPoolAssignment binding it to
	// GatewayPools, so the temporary node has a home site that is distinct from
	// wherever the demo pod runs. SiteNodeCIDR must contain NodeInternalIP and
	// SitePodCIDR must contain NodePodCIDR. GatewayPools is configurable so the
	// site can be attached to whichever gateway pool serves this cluster.
	SiteNodeCIDR string
	SitePodCIDR  string
	GatewayPools []string

	// Local WireGuard underlay interfaces. One interface is created per
	// gateway so that BOTH gw-main gateways learn our roaming endpoint. This
	// is required because the remote node's eBPF dataplane load-balances
	// return traffic across gateways per flow tuple (asymmetric routing), so a
	// reply may come back through a gateway we never handshook with.
	WGInterfaceBase  string
	WGListenPortBase int
	GatewayEndpoints []string
	GatewayPubKeys   []string
	RouteCIDRs       []string
	Keepalive        int
	StateDir         string

	// Local VXLAN overlay (userspace, in-process). VXLANInterface names the
	// device the demo pod creates on its side.
	VXLANInterface  string
	VNI             int
	VXLANPort       int
	OverlayLocalIP  string
	OverlayRemoteIP string
	OverlayPrefix   int
	OverlayMTU      int

	// NetbootProxyPort, when non-zero, makes the demo pod run an HTTP reverse
	// proxy on the pod bridge IP (OverlayRemoteIP) at this port that forwards
	// to http://OverlayLocalIP:NetbootProxyPort (the client-side netboot HTTP
	// server, reached over the overlay). The guest bootloader/installer then
	// fetch large netboot artifacts over the fast pod<->guest LAN hop while the
	// pod re-originates to the client over the high-latency overlay using the
	// pod's real Linux kernel TCP (window scaling), avoiding the bootloader's
	// tiny TCP window becoming the bottleneck across the overlay RTT. Zero
	// disables the proxy.
	NetbootProxyPort int

	// ProxySourceIP, when set, is the source IP the TFTP and forward proxies
	// bind their egress sockets to when dialing host-loopback services (e.g.
	// metalman). It should be the guest's overlay lease IP so those services
	// observe that IP as the request source rather than 127.0.0.1.
	ProxySourceIP string

	// Demo pod (server side of the overlay).
	PodName  string
	PodNode  string
	PodImage string

	// Arch is the CPU architecture of the host the demo pod runs on. It is
	// normalized (x86/x86_64 -> amd64, arm/aarch64 -> arm64) and drives a
	// kubernetes.io/arch nodeSelector so the scheduler picks a matching node.
	// It is ignored when PodNode pins the pod to an explicit node.
	Arch string

	// KVMNodeLabel is the "key=value" node label a node must carry to be
	// considered KVM-capable. In VM mode it is added to the demo pod's
	// nodeSelector so the pod only lands on nodes that advertise /dev/kvm
	// support (there is no cluster-wide KVM signal to discover otherwise, so
	// operators label KVM-capable nodes themselves). A bare "key" implies the
	// value "true".
	KVMNodeLabel string

	// In-pod virtual machine. The server pod always bridges the VXLAN overlay
	// to a tap device and provisions a diskless cloud-hypervisor guest whose
	// single virtio NIC lives on the overlay. The guest starts powered off and
	// is driven by the in-pod Redfish server (see below). When it is powered
	// on it leases its overlay address via DHCP and PXE network-boots (via the
	// UEFI firmware's network stack), both served from the client side (the
	// DHCP relay plus a userspace TFTP proxy that forwards to the client-side
	// TFTP server). BridgeInterface carries the pod overlay address (gateway)
	// with VXLANInterface and TapInterface enslaved.
	VMMemoryMiB     int
	VMCPUs          int
	VMMAC           string
	BridgeInterface string
	TapInterface    string

	// VMDiskSizeGiB is the size of the guest's backing disk in GiB. When zero
	// the guest is diskless (network-boot only, no persistent OS). When
	// positive a sparse raw disk of this size is provisioned in the pod and
	// attached as a virtio block device, so a netboot installer can write an OS
	// image and the guest can boot the installed OS from disk. VMDiskPath is
	// the path to the disk image inside the pod; when empty it defaults to
	// defaultVMDiskPath.
	VMDiskSizeGiB int
	VMDiskPath    string

	// In-pod Redfish server. The pod always serves a minimal Redfish service
	// over HTTPS (self-signed cert) that controls the guest's power state via
	// cloud-hypervisor's REST API. RedfishPort is the port it listens on
	// (bound to the pod overlay IP). The client exposes it locally by
	// forwarding 127.0.0.1:RedfishLocalPort through the overlay to the pod, so
	// a locally running metalman can drive power on/off. RedfishUsername and
	// RedfishPassword are the credentials (Basic auth and Redfish sessions);
	// RedfishDeviceID is the ComputerSystem id exposed under
	// /redfish/v1/Systems.
	RedfishPort      int
	RedfishLocalPort int
	RedfishUsername  string
	RedfishPassword  string
	RedfishDeviceID  string

	// TFTPServer is the upstream TFTP server the overlay TFTP proxy forwards to
	// ("host" or "host:port", default port 69). PXE clients on the overlay send
	// their read requests to the client's overlay IP; the proxy relays them to
	// this server (typically the same client-side dnsmasq that answers DHCP).
	// When empty it defaults to the DHCPServer host.
	TFTPServer string

	// TCP port forwards that expose the client's loopback to the overlay.
	// Each entry is "OVERLAYPORT:LOOPBACKPORT" (or a bare "PORT" meaning the
	// same port on both sides). Remote processes that connect to the client's
	// overlay IP on OVERLAYPORT are proxied to 127.0.0.1:LOOPBACKPORT on the
	// client, all in userspace (no root, no TAP, no iptables).
	Forwards []string

	// DHCP relay. When DHCPRelayPort is non-zero the relay is enabled: DHCP
	// BOOTREQUEST frames from the overlay (pod side) are forwarded to the
	// upstream DHCPServer with giaddr stamped, and the server's replies are
	// relayed back to the pod, so processes on the pod can use standard DHCP
	// clients. Binding the standard relay port 67 is privileged; a high port
	// is not. DHCPServer is the upstream server ("host" or "host:port",
	// default port 67). DHCPGiaddr overrides the auto-detected host source IP
	// used as giaddr (it must be routable back to the host so replies return).
	DHCPServer    string
	DHCPGiaddr    string
	DHCPRelayPort int
}

// DefaultConfig returns the validated defaults established for the
// unbounded-stable cluster.
func DefaultConfig() Config {
	return Config{
		KubeContext: "unbounded-stable",
		Namespace:   "jordan-testing",

		TTL: 1 * time.Hour,

		NodeName:       "jordan-playtime",
		NodeSite:       "playtime",
		NodeInternalIP: "10.242.0.1",
		NodePodCIDR:    "100.123.0.0/24",

		SiteNodeCIDR: "10.242.0.0/16",
		SitePodCIDR:  "100.123.0.0/16",
		GatewayPools: []string{"gw-main"},

		WGInterfaceBase:  "pt-wg",
		WGListenPortBase: 51900,
		GatewayEndpoints: []string{"20.104.49.219:51820", "20.151.222.173:51820"},
		GatewayPubKeys: []string{
			"jmiGvW/EIsSYMDhq+veuuiJgdsg2lGWP3TA8wuilJkg=",
			"b9Srd7xYMkfX0VdG4eT5QXFZKvJ2504J6rc76CHVzCY=",
		},
		RouteCIDRs: []string{"100.125.0.0/16", "100.124.0.0/16", "10.224.0.0/12"},
		Keepalive:  15,
		StateDir:   "/tmp/playtime",

		VXLANInterface:  "pt-vx0",
		VNI:             42,
		VXLANPort:       4789,
		OverlayLocalIP:  "172.31.99.2",
		OverlayRemoteIP: "172.31.99.1",
		OverlayPrefix:   24,
		OverlayMTU:      1230,

		NetbootProxyPort: 0,

		PodName:  "playtime-server",
		PodNode:  "",
		PodImage: DefaultPodImage,

		Arch:         "amd64",
		KVMNodeLabel: DefaultKVMNodeLabel,

		VMMemoryMiB:     512,
		VMCPUs:          1,
		VMMAC:           "52:54:00:12:34:56",
		BridgeInterface: "pt-br0",
		TapInterface:    "pt-tap0",
		VMDiskSizeGiB:   20,
		VMDiskPath:      defaultVMDiskPath,

		RedfishPort:      8443,
		RedfishLocalPort: 8443,
		RedfishUsername:  "admin",
		RedfishPassword:  "password",
		RedfishDeviceID:  "1",
	}
}

// ArchLabelKey is the standard Kubernetes node label carrying a node's CPU
// architecture (amd64/arm64). playtime selects the demo pod's host by it.
const ArchLabelKey = "kubernetes.io/arch"

// DefaultKVMNodeLabel is the default node label a node must carry to be treated
// as KVM-capable in VM mode. There is no standard cluster-wide KVM signal, so
// operators label KVM-capable nodes with this and playtime selects on it.
const DefaultKVMNodeLabel = "playtime.unbounded-cloud.io/kvm=true"

// defaultVMDiskPath is the in-pod path of the guest's backing disk image when
// VMDiskPath is not overridden. It lives under the VM state dir alongside the
// cloud-hypervisor API socket and serial log.
const defaultVMDiskPath = "/tmp/playtime-vm/disk.img"

// diskPath returns the in-pod path of the guest's backing disk image, falling
// back to defaultVMDiskPath when VMDiskPath is unset.
func (c Config) diskPath() string {
	if strings.TrimSpace(c.VMDiskPath) != "" {
		return c.VMDiskPath
	}

	return defaultVMDiskPath
}

// normalizeArch maps common architecture spellings to the canonical
// kubernetes.io/arch value. It accepts x86/x86_64/x64/amd64 for amd64 and
// arm/arm64/aarch64 for arm64.
func normalizeArch(arch string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86", "x86_64", "x64", "intel":
		return "amd64", nil
	case "arm64", "arm", "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported --arch %q (want amd64/x86_64 or arm64/aarch64)", arch)
	}
}

// validateArch reports whether the configured architecture is supported.
func (c Config) validateArch() error {
	_, err := normalizeArch(c.Arch)
	return err
}

// kvmNodeLabel parses KVMNodeLabel into its key and value. A bare "key" (no
// "=") implies the value "true".
func (c Config) kvmNodeLabel() (string, string, error) {
	key, value, found := strings.Cut(strings.TrimSpace(c.KVMNodeLabel), "=")

	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	if key == "" {
		return "", "", fmt.Errorf("invalid --kvm-node-label %q: missing label key", c.KVMNodeLabel)
	}

	if !found {
		value = "true"
	}

	return key, value, nil
}

// podNodeSelector returns the label selector used to schedule the demo pod. It
// pins the pod to the requested CPU architecture and to a node advertising KVM
// support via KVMNodeLabel (the pod always provisions a cloud-hypervisor
// guest). It returns nil when the pod is pinned to an explicit node
// (--pod-node), which bypasses the scheduler.
func (c Config) podNodeSelector() (map[string]string, error) {
	if c.PodNode != "" {
		return nil, nil
	}

	arch, err := normalizeArch(c.Arch)
	if err != nil {
		return nil, err
	}

	selector := map[string]string{ArchLabelKey: arch}

	key, value, err := c.kvmNodeLabel()
	if err != nil {
		return nil, err
	}

	selector[key] = value

	return selector, nil
}

// TempNodeLabelKey marks Node objects created by playtime so they are easy to
// find and clean up. Its value is the run's namespace, which also scopes the
// reaper so one developer's pod only reaps its own namespace's stale runs.
const TempNodeLabelKey = "playtime.unbounded-cloud.io/temp"

// ReaperServiceAccountName is the fixed name of the shared ServiceAccount the
// in-pod reaper runs as. It lives in the run namespace and is shared by every
// run in that namespace; it is created if missing and never reaped.
const ReaperServiceAccountName = "playtime-reaper"

// ExpiresAtAnnotation records the RFC3339 instant after which a run is
// considered orphaned and may be reaped. It is stamped on the Node anchor and
// is the single source of truth for the reaper.
const ExpiresAtAnnotation = "playtime.unbounded-cloud.io/expires-at"

// TTLAnnotation records the human-readable TTL the run was created with (for
// debugging; the reaper only consults ExpiresAtAnnotation).
const TTLAnnotation = "playtime.unbounded-cloud.io/ttl"

// WireGuardPubKeyAnnotation is the annotation unbounded-net reads to learn a
// node's WireGuard public key.
const WireGuardPubKeyAnnotation = "net.unbounded-cloud.io/wg-pubkey"

// SiteLabelKey is the unbounded-net site membership label.
const SiteLabelKey = "net.unbounded-cloud.io/site"

// AKSManagedLabelKey set to "false" stops the AKS cloud-node-lifecycle
// controller from deleting our fake Node.
const AKSManagedLabelKey = "kubernetes.azure.com/managed"

// reaperClusterName is the name of the shared cluster-scoped ClusterRole and
// ClusterRoleBinding for the in-pod reaper. It is scoped by namespace so
// separate namespaces get independent (but internally shared) RBAC and never
// collide on the cluster-scoped names.
func (c Config) reaperClusterName() string {
	return ReaperServiceAccountName + "-" + c.Namespace
}

// gateway describes one local WireGuard underlay interface peered with a single
// gw-main gateway.
type gateway struct {
	iface    string
	port     int
	endpoint string
	pubKey   string
}

// gateways expands the parallel endpoint/pubkey lists into per-interface
// gateway descriptors. Interface names and listen ports are derived from the
// configured bases (e.g. pt-wg0/51900, pt-wg1/51901). The number of gateways is
// the shorter of the two lists to avoid index panics on mismatched flags.
func (c Config) gateways() []gateway {
	n := len(c.GatewayEndpoints)
	if len(c.GatewayPubKeys) < n {
		n = len(c.GatewayPubKeys)
	}

	gws := make([]gateway, 0, n)
	for i := 0; i < n; i++ {
		gws = append(gws, gateway{
			iface:    fmt.Sprintf("%s%d", c.WGInterfaceBase, i),
			port:     c.WGListenPortBase + i,
			endpoint: c.GatewayEndpoints[i],
			pubKey:   c.GatewayPubKeys[i],
		})
	}

	return gws
}

// wgAddress returns the local WireGuard interface address (the .1 of the pod
// CIDR) as a host prefix, e.g. "100.100.240.1/32".
func (c Config) wgAddress() (string, error) {
	prefix, err := netip.ParsePrefix(c.NodePodCIDR)
	if err != nil {
		return "", fmt.Errorf("parse node pod cidr %q: %w", c.NodePodCIDR, err)
	}

	gw := prefix.Masked().Addr().Next()

	return netip.PrefixFrom(gw, gw.BitLen()).String(), nil
}

// clientUnderlayIP returns the bare client underlay (VXLAN outer) address, i.e.
// the .1 of the node pod CIDR without a prefix, e.g. "100.100.240.1". The pod
// uses it as the flood target so broadcast/unknown-unicast frames (such as a
// DHCP DISCOVER) reach the client's userspace endpoint.
func (c Config) clientUnderlayIP() (string, error) {
	prefix, err := netip.ParsePrefix(c.NodePodCIDR)
	if err != nil {
		return "", fmt.Errorf("parse node pod cidr %q: %w", c.NodePodCIDR, err)
	}

	return prefix.Masked().Addr().Next().String(), nil
}

// privKeyPath returns the path to the WireGuard private key file.
func (c Config) privKeyPath() string {
	return c.StateDir + "/pt.priv"
}

// pubKeyPath returns the path to the WireGuard public key file.
func (c Config) pubKeyPath() string {
	return c.StateDir + "/pt.pub"
}

// lastRunPath returns the path to the file recording the Node name of the most
// recent run, so `down` can target it without a fixed (now generated) name.
func (c Config) lastRunPath() string {
	return c.StateDir + "/last-run"
}

// expiryString returns the RFC3339 instant a run created now should expire.
func (c Config) expiryString(now time.Time) string {
	return now.Add(c.TTL).UTC().Format(time.RFC3339)
}

// parseExpiry parses an ExpiresAtAnnotation value.
func parseExpiry(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}

// isExpired reports whether the RFC3339 expiry annotation value is in the past
// relative to now. A missing or malformed value is treated as not expired so an
// in-flight or foreign object is never reaped by mistake.
func isExpired(expiresAt string, now time.Time) bool {
	if strings.TrimSpace(expiresAt) == "" {
		return false
	}

	t, err := parseExpiry(expiresAt)
	if err != nil {
		return false
	}

	return now.After(t)
}

// writeLastRun records the Node name of the most recent run so `down` can target
// it without a fixed (now generated) name.
func writeLastRun(cfg Config, nodeName string) error {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	if err := os.WriteFile(cfg.lastRunPath(), []byte(nodeName+"\n"), 0o600); err != nil {
		return fmt.Errorf("write last-run marker: %w", err)
	}

	return nil
}

// readLastRun returns the Node name recorded by the most recent run, or an empty
// string if none has been recorded.
func readLastRun(cfg Config) (string, error) {
	data, err := os.ReadFile(cfg.lastRunPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}

		return "", fmt.Errorf("read last-run marker: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

// forwardRule maps a TCP port on the client's overlay IP to a port on the
// client's loopback interface.
type forwardRule struct {
	overlayPort  uint16
	loopbackPort uint16
}

// parsedForwards parses and validates the raw --forward entries into rules.
// Each entry is "OVERLAYPORT:LOOPBACKPORT" or a bare "PORT" (same on both
// sides). Duplicate overlay ports are rejected so a port maps to exactly one
// loopback target.
func (c Config) parsedForwards() ([]forwardRule, error) {
	rules := make([]forwardRule, 0, len(c.Forwards))
	seen := make(map[uint16]struct{}, len(c.Forwards))

	for _, raw := range c.Forwards {
		spec := strings.TrimSpace(raw)
		if spec == "" {
			return nil, fmt.Errorf("empty forward entry")
		}

		overlayStr, loopbackStr, hasColon := strings.Cut(spec, ":")
		if !hasColon {
			loopbackStr = overlayStr
		}

		overlayPort, err := parsePort(overlayStr)
		if err != nil {
			return nil, fmt.Errorf("forward %q: overlay port: %w", raw, err)
		}

		loopbackPort, err := parsePort(loopbackStr)
		if err != nil {
			return nil, fmt.Errorf("forward %q: loopback port: %w", raw, err)
		}

		if _, dup := seen[overlayPort]; dup {
			return nil, fmt.Errorf("forward %q: overlay port %d specified more than once", raw, overlayPort)
		}

		seen[overlayPort] = struct{}{}
		rules = append(rules, forwardRule{overlayPort: overlayPort, loopbackPort: loopbackPort})
	}

	return rules, nil
}

// parsePort parses a decimal TCP port in the valid 1-65535 range.
func parsePort(s string) (uint16, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid port %q", s)
	}

	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d out of range 1-65535", n)
	}

	return uint16(n), nil
}

// dhcpEnabled reports whether the DHCP relay should run.
func (c Config) dhcpEnabled() bool {
	return c.DHCPRelayPort != 0
}

// dhcpServerAddr resolves the configured upstream DHCP server address,
// defaulting to port 67 when no port is given.
func (c Config) dhcpServerAddr() (*net.UDPAddr, error) {
	spec := strings.TrimSpace(c.DHCPServer)
	if spec == "" {
		return nil, fmt.Errorf("--dhcp-server is required when --dhcp-relay-port is set")
	}

	if _, _, err := net.SplitHostPort(spec); err != nil {
		spec = net.JoinHostPort(spec, strconv.Itoa(dhcpServerPort))
	}

	addr, err := net.ResolveUDPAddr("udp4", spec)
	if err != nil {
		return nil, fmt.Errorf("resolve dhcp server %q: %w", c.DHCPServer, err)
	}

	return addr, nil
}

// dhcpGiaddr returns the configured giaddr override as a 4-byte IP, or nil when
// unset (meaning the relay auto-detects the host source IP toward the server).
func (c Config) dhcpGiaddr() (net.IP, error) {
	s := strings.TrimSpace(c.DHCPGiaddr)
	if s == "" {
		return nil, nil
	}

	ip := net.ParseIP(s).To4()
	if ip == nil {
		return nil, fmt.Errorf("invalid --dhcp-giaddr %q (must be an IPv4 address)", c.DHCPGiaddr)
	}

	return ip, nil
}

// validateDHCP checks the DHCP relay configuration without opening sockets.
func (c Config) validateDHCP() error {
	if !c.dhcpEnabled() {
		return nil
	}

	if c.DHCPRelayPort < 1 || c.DHCPRelayPort > 65535 {
		return fmt.Errorf("--dhcp-relay-port %d out of range 1-65535", c.DHCPRelayPort)
	}

	if _, err := c.dhcpServerAddr(); err != nil {
		return err
	}

	if _, err := c.dhcpGiaddr(); err != nil {
		return err
	}

	return nil
}

// tftpServerAddr resolves the upstream TFTP server the overlay TFTP proxy
// forwards PXE requests to, defaulting to port 69. When --tftp-server is empty
// it falls back to the DHCPServer host (the client-side dnsmasq typically
// serves both DHCP and TFTP).
func (c Config) tftpServerAddr() (*net.UDPAddr, error) {
	spec := strings.TrimSpace(c.TFTPServer)
	if spec == "" {
		host := strings.TrimSpace(c.DHCPServer)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		spec = host
	}

	if spec == "" {
		return nil, fmt.Errorf("--tftp-server or --dhcp-server is required for PXE boot")
	}

	if _, _, err := net.SplitHostPort(spec); err != nil {
		spec = net.JoinHostPort(spec, strconv.Itoa(tftpPort))
	}

	addr, err := net.ResolveUDPAddr("udp4", spec)
	if err != nil {
		return nil, fmt.Errorf("resolve tftp server %q: %w", spec, err)
	}

	return addr, nil
}

// tftpConfigured reports whether an upstream TFTP source is available (either
// an explicit --tftp-server or a --dhcp-server host to derive it from).
func (c Config) tftpConfigured() bool {
	return strings.TrimSpace(c.TFTPServer) != "" || strings.TrimSpace(c.DHCPServer) != ""
}

// validateVM checks the in-pod VM configuration. The pod always provisions a
// diskless cloud-hypervisor guest, so the guest MAC and resource sizes are
// always validated. The guest PXE network-boots when powered on; a resolvable
// TFTP server is only required when one is configured (without it the guest can
// still be provisioned and power-controlled, it just cannot network-boot).
func (c Config) validateVM() error {
	if _, err := net.ParseMAC(c.VMMAC); err != nil {
		return fmt.Errorf("invalid --vm-mac %q: %w", c.VMMAC, err)
	}

	if c.tftpConfigured() {
		if _, err := c.tftpServerAddr(); err != nil {
			return err
		}
	}

	if c.VMMemoryMiB < 1 {
		return fmt.Errorf("--vm-memory %d must be positive", c.VMMemoryMiB)
	}

	if c.VMCPUs < 1 {
		return fmt.Errorf("--vm-cpus %d must be positive", c.VMCPUs)
	}

	if c.VMDiskSizeGiB < 0 {
		return fmt.Errorf("--vm-disk-size %d must not be negative", c.VMDiskSizeGiB)
	}

	if c.NetbootProxyPort != 0 && (c.NetbootProxyPort < 1 || c.NetbootProxyPort > 65535) {
		return fmt.Errorf("--netboot-proxy-port %d out of range 1-65535", c.NetbootProxyPort)
	}

	return nil
}

// validateRedfish checks the in-pod Redfish server configuration. Credentials
// must be non-empty (they gate power control), the pod and local ports must be
// valid, and a device id must be set.
func (c Config) validateRedfish() error {
	if strings.TrimSpace(c.RedfishUsername) == "" {
		return fmt.Errorf("--redfish-username must not be empty")
	}

	if strings.TrimSpace(c.RedfishPassword) == "" {
		return fmt.Errorf("--redfish-password must not be empty")
	}

	if strings.TrimSpace(c.RedfishDeviceID) == "" {
		return fmt.Errorf("--redfish-device-id must not be empty")
	}

	if c.RedfishPort < 1 || c.RedfishPort > 65535 {
		return fmt.Errorf("--redfish-port %d out of range 1-65535", c.RedfishPort)
	}

	if c.RedfishLocalPort < 1 || c.RedfishLocalPort > 65535 {
		return fmt.Errorf("--redfish-local-port %d out of range 1-65535", c.RedfishLocalPort)
	}

	return nil
}

// validateSite checks the dedicated-site bootstrap configuration. The node's
// underlay address must fall inside the site's node CIDR and the node's pod
// CIDR must fall inside the site's pod CIDR (the network controller matches a
// node into a site by these ranges), and at least one gateway pool must be
// named so the site is actually meshed.
func (c Config) validateSite() error {
	nodeNet, err := netip.ParsePrefix(c.SiteNodeCIDR)
	if err != nil {
		return fmt.Errorf("invalid --site-node-cidr %q: %w", c.SiteNodeCIDR, err)
	}

	internalIP, err := netip.ParseAddr(c.NodeInternalIP)
	if err != nil {
		return fmt.Errorf("invalid --node-internal-ip %q: %w", c.NodeInternalIP, err)
	}

	if !nodeNet.Contains(internalIP) {
		return fmt.Errorf("--node-internal-ip %q is not within --site-node-cidr %q", c.NodeInternalIP, c.SiteNodeCIDR)
	}

	sitePodNet, err := netip.ParsePrefix(c.SitePodCIDR)
	if err != nil {
		return fmt.Errorf("invalid --site-pod-cidr %q: %w", c.SitePodCIDR, err)
	}

	nodePodNet, err := netip.ParsePrefix(c.NodePodCIDR)
	if err != nil {
		return fmt.Errorf("invalid --node-pod-cidr %q: %w", c.NodePodCIDR, err)
	}

	if !sitePodNet.Contains(nodePodNet.Masked().Addr()) || nodePodNet.Bits() < sitePodNet.Bits() {
		return fmt.Errorf("--node-pod-cidr %q is not within --site-pod-cidr %q", c.NodePodCIDR, c.SitePodCIDR)
	}

	if len(c.GatewayPools) == 0 {
		return fmt.Errorf("--gateway-pools must name at least one gateway pool")
	}

	for _, pool := range c.GatewayPools {
		if strings.TrimSpace(pool) == "" {
			return fmt.Errorf("--gateway-pools must not contain an empty pool name")
		}
	}

	return nil
}
