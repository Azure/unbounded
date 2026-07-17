// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/version"
)

// sliceMembershipTimeout bounds how long `join` waits for the controller to
// publish the temporary node into a SiteNodeSlice.
const sliceMembershipTimeout = 60 * time.Second

func main() {
	cfg := DefaultConfig()

	pingCount := 4
	downAll := false
	keepUp := false

	root := &cobra.Command{
		Use:     "playtime",
		Short:   "Ride a VXLAN overlay over the unbounded-net WireGuard mesh",
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate("playtime {{.Version}}\n")

	bindGlobalFlags(root, &cfg)

	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Bring up the userspace overlay to the demo pod and ping it (no root required)",
		Long: "Registers the temporary node, brings up an in-process userspace WireGuard " +
			"and VXLAN overlay dataplane, ensures the demo pod, and pings it over the mesh. " +
			"The dataplane is torn down automatically when this process exits.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUp(cmd.Context(), cfg, pingCount, keepUp)
		},
	}
	upCmd.Flags().IntVarP(&pingCount, "count", "c", pingCount, "number of echo requests to send")
	upCmd.Flags().BoolVar(&keepUp, "keep-up", false, "hold the dataplane open until interrupted")

	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Delete the cluster resources created by playtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDown(cmd.Context(), cfg, downAll)
		},
	}
	downCmd.Flags().BoolVar(&downAll, "all", false, "delete every playtime run in the namespace scope (not just the most recent)")

	cleanupCmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete the shared resources playtime otherwise never cleans up",
		Long: "Deletes every playtime run in the namespace scope and then the shared, " +
			"unowned resources that up creates if missing and reuses across runs: the " +
			"reaper RBAC (ClusterRoleBinding, ClusterRole, ServiceAccount), the " +
			"bootstrapped Site and SiteGatewayPoolAssignment, and the shared namespace. " +
			"It is idempotent and safe to run when the scope is already partly gone.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCleanup(cmd.Context(), cfg)
		},
	}

	root.AddCommand(upCmd, downCmd, cleanupCmd, newServerCommand(&cfg), version.Command())

	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// bindGlobalFlags wires every Config field to a persistent flag so the defaults
// can be overridden without recompiling.
func bindGlobalFlags(root *cobra.Command, cfg *Config) {
	f := root.PersistentFlags()

	f.StringVar(&cfg.KubeContext, "context", cfg.KubeContext, "kubeconfig context to use")
	f.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "namespace for the demo pod")
	f.DurationVar(&cfg.TTL, "ttl", cfg.TTL, "lifetime budget for every resource a run creates; the run is reaped and its pod hard-stopped after this fixed, non-renewable duration")

	f.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "temporary Node object name")
	f.StringVar(&cfg.NodeSite, "node-site", cfg.NodeSite, "unbounded-net site for the temporary node")
	f.StringVar(&cfg.NodeInternalIP, "node-internal-ip", cfg.NodeInternalIP, "internal IP advertised by the temporary node")
	f.StringVar(&cfg.NodePodCIDR, "node-pod-cidr", cfg.NodePodCIDR, "pod CIDR for the temporary node")

	f.StringVar(&cfg.SiteNodeCIDR, "site-node-cidr", cfg.SiteNodeCIDR, "node CIDR for the bootstrapped dedicated site (must contain --node-internal-ip)")
	f.StringVar(&cfg.SitePodCIDR, "site-pod-cidr", cfg.SitePodCIDR, "pod CIDR for the bootstrapped dedicated site (must contain --node-pod-cidr)")
	f.StringSliceVar(&cfg.GatewayPools, "gateway-pools", cfg.GatewayPools, "gateway pools to assign the bootstrapped site to")

	f.StringVar(&cfg.WGInterfaceBase, "wg-interface-base", cfg.WGInterfaceBase, "local WireGuard interface name prefix (per-gateway index appended)")
	f.IntVar(&cfg.WGListenPortBase, "wg-listen-port-base", cfg.WGListenPortBase, "base local WireGuard listen port (per-gateway index added)")
	f.StringSliceVar(&cfg.GatewayEndpoints, "gateway-endpoints", cfg.GatewayEndpoints, "gw-main gateway host:port list (mesh port 51820)")
	f.StringSliceVar(&cfg.GatewayPubKeys, "gateway-pubkeys", cfg.GatewayPubKeys, "gateway WireGuard public key list (parallel to --gateway-endpoints)")
	f.StringSliceVar(&cfg.RouteCIDRs, "route-cidrs", cfg.RouteCIDRs, "CIDRs routed through the mesh")
	f.IntVar(&cfg.Keepalive, "keepalive", cfg.Keepalive, "WireGuard persistent keepalive seconds")
	f.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "directory holding the WireGuard keypair")

	f.StringVar(&cfg.VXLANInterface, "vxlan-interface", cfg.VXLANInterface, "VXLAN device name (pod side)")
	f.IntVar(&cfg.VNI, "vni", cfg.VNI, "VXLAN network identifier")
	f.IntVar(&cfg.VXLANPort, "vxlan-port", cfg.VXLANPort, "VXLAN UDP destination port")
	f.StringVar(&cfg.OverlayLocalIP, "overlay-local-ip", cfg.OverlayLocalIP, "local overlay IP")
	f.StringVar(&cfg.OverlayRemoteIP, "overlay-remote-ip", cfg.OverlayRemoteIP, "remote (pod) overlay IP")
	f.IntVar(&cfg.OverlayPrefix, "overlay-prefix", cfg.OverlayPrefix, "overlay subnet prefix length")
	f.IntVar(&cfg.OverlayMTU, "overlay-mtu", cfg.OverlayMTU, "overlay interface MTU")
	f.StringVar(&cfg.ProxySourceIP, "proxy-source-ip", cfg.ProxySourceIP, "source IP the TFTP and forward proxies bind to when dialing host-loopback services (e.g. the guest's overlay lease IP, so metalman sees that IP as the request source instead of 127.0.0.1)")

	f.StringVar(&cfg.PodName, "pod-name", cfg.PodName, "demo pod name")
	f.StringVar(&cfg.PodNode, "pod-node", cfg.PodNode, "pin the demo pod to a specific node by name; overrides --arch and --kvm-node-label scheduling when set")
	f.StringVar(&cfg.PodImage, "pod-image", cfg.PodImage, "container image for the demo pod")
	f.StringVar(&cfg.Arch, "arch", cfg.Arch, "CPU architecture of the host to run the demo pod on: amd64/x86_64 or arm64/aarch64 (drives a kubernetes.io/arch nodeSelector)")
	f.StringVar(&cfg.KVMNodeLabel, "kvm-node-label", cfg.KVMNodeLabel, "node label (key=value) a node must carry to be selected (the pod always provisions a KVM guest); bare key implies value \"true\"")

	f.IntVar(&cfg.VMMemoryMiB, "vm-memory", cfg.VMMemoryMiB, "guest memory in MiB")
	f.IntVar(&cfg.VMCPUs, "vm-cpus", cfg.VMCPUs, "guest vCPU count")
	f.StringVar(&cfg.VMMAC, "vm-mac", cfg.VMMAC, "guest NIC MAC address")
	f.IntVar(&cfg.VMDiskSizeGiB, "vm-disk-size", cfg.VMDiskSizeGiB,
		"guest backing disk size in GiB (0 for a diskless network-boot-only guest)")
	f.IntVar(&cfg.NetbootProxyPort, "netboot-proxy-port", cfg.NetbootProxyPort,
		"when non-zero, the demo pod runs an HTTP reverse proxy on the pod overlay IP at this port forwarding to the client-side netboot HTTP server, so the guest bootloader fetches over the fast pod<->guest LAN instead of the high-latency overlay (0 disables)")
	f.StringVar(&cfg.BridgeInterface, "bridge-interface", cfg.BridgeInterface, "pod bridge device joining the VXLAN and guest tap")
	f.StringVar(&cfg.TapInterface, "tap-interface", cfg.TapInterface, "pod tap device for the guest NIC")
	f.StringVar(&cfg.TFTPServer, "tftp-server", cfg.TFTPServer,
		"upstream TFTP server the overlay PXE proxy forwards to: host or host:port (default port 69); defaults to the --dhcp-server host")

	f.IntVar(&cfg.RedfishPort, "redfish-port", cfg.RedfishPort, "pod port the in-pod Redfish server listens on (bound to the pod overlay IP, HTTPS)")
	f.IntVar(&cfg.RedfishLocalPort, "redfish-local-port", cfg.RedfishLocalPort, "local loopback port the client forwards through the overlay to the pod Redfish server")
	f.StringVar(&cfg.RedfishUsername, "redfish-username", cfg.RedfishUsername, "Redfish username (Basic auth and session login)")
	f.StringVar(&cfg.RedfishPassword, "redfish-password", cfg.RedfishPassword, "Redfish password (Basic auth and session login)")
	f.StringVar(&cfg.RedfishDeviceID, "redfish-device-id", cfg.RedfishDeviceID, "ComputerSystem id exposed under /redfish/v1/Systems")

	f.StringSliceVar(&cfg.Forwards, "forward", cfg.Forwards,
		"expose a client localhost port to the overlay: OVERLAYPORT:LOOPBACKPORT (or bare PORT), tcp, repeatable; implies holding the dataplane open")

	f.StringVar(&cfg.DHCPServer, "dhcp-server", cfg.DHCPServer,
		"upstream DHCP server for the relay: host or host:port (default port 67); required when --dhcp-relay-port is set")
	f.StringVar(&cfg.DHCPGiaddr, "dhcp-giaddr", cfg.DHCPGiaddr,
		"relay agent IP (giaddr) stamped on relayed requests; auto-detected from the route to the server when empty")
	f.IntVar(&cfg.DHCPRelayPort, "dhcp-relay-port", cfg.DHCPRelayPort,
		"local UDP port the relay binds for server replies; 0 disables the relay, 67 needs root/CAP_NET_BIND_SERVICE; implies holding the dataplane open")
}
