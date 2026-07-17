// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/spf13/cobra"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// serverUplink is the pod-side egress device the VXLAN underlay rides and out
// of which forwarded overlay traffic is masqueraded.
var serverUplink = "eth0"

// serverSelfNodeName is the name of this run's Node anchor. The in-pod reaper
// deletes it (cascading to the whole run) once the TTL expires, and uses it to
// recognize when it has reaped itself so it can shut the pod down cleanly.
var serverSelfNodeName string

// serverReapInterval controls how often the in-pod reaper scans for expired
// playtime runs.
var serverReapInterval = time.Minute

// newServerCommand builds the hidden `playtime server` subcommand that runs
// inside the demo pod. It configures the pod's side of the VXLAN overlay
// entirely through netlink and go-iptables (no shell, no iproute2/bridge/sysctl
// invocations) and then blocks until interrupted.
//
// The pod runs Privileged, so the process has the capabilities required to
// create links, add neighbours, assign addresses, toggle forwarding, and edit
// nat rules.
func newServerCommand(cfg *Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "server",
		Short:  "Configure the pod-side VXLAN overlay endpoint (runs inside the demo pod)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(cmd.Context(), *cfg)
		},
	}
	cmd.Flags().StringVar(&serverUplink, "uplink", serverUplink,
		"pod egress device carrying the VXLAN underlay and masqueraded overlay traffic")
	cmd.Flags().StringVar(&serverSelfNodeName, "self-node-name", serverSelfNodeName,
		"name of this run's Node anchor; the in-pod reaper deletes it (and cascades) once the TTL expires")
	cmd.Flags().DurationVar(&serverReapInterval, "reap-interval", serverReapInterval,
		"how often the in-pod reaper scans for expired playtime runs")

	return cmd
}

// runServer applies the overlay configuration and then waits for a termination
// signal, keeping the pod alive so the overlay endpoint persists.
func runServer(ctx context.Context, cfg Config) error {
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		return fmt.Errorf("POD_IP environment variable is not set")
	}

	if net.ParseIP(podIP) == nil {
		return fmt.Errorf("POD_IP %q is not a valid IP address", podIP)
	}

	clientUnderlay, err := cfg.clientUnderlayIP()
	if err != nil {
		return err
	}

	if err := configureServer(cfg, podIP, clientUnderlay); err != nil {
		return err
	}

	fmt.Printf("playtime-server ready: overlay %s/%d on %s (vni %d) local underlay %s; flood to %s; internet NAT via %s\n",
		cfg.OverlayRemoteIP, cfg.OverlayPrefix, cfg.VXLANInterface, cfg.VNI, podIP, clientUnderlay, serverUplink)

	// Block until interrupted so the pod (RestartPolicy Never) stays running
	// and the overlay endpoint remains configured.
	sig := make(chan os.Signal, 1)

	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The in-pod reaper cleans up previous stale runs on startup and then, on a
	// ticker, deletes any playtime run in this namespace whose TTL has expired.
	// When it deletes this run's own Node anchor it cancels runCtx so the pod
	// shuts down cleanly (stopping the VM and unwinding the overlay) ahead of
	// the kubelet's activeDeadlineSeconds backstop.
	go runReaper(runCtx, cfg, serverSelfNodeName, serverReapInterval, cancel)

	// Always provision a guest VM. The cloud-hypervisor VMM is launched powered
	// off; its power state is driven by the in-pod Redfish server (below), which
	// metalman reaches through the client's local port forward.
	vm, err := startVM(runCtx, cfg)
	if err != nil {
		return err
	}

	fmt.Printf("guest VM provisioned (powered off): single NIC (mac %s) bridged to %s via %s on the overlay (cloud-hypervisor); serial log at /tmp/playtime-vm/serial.log\n",
		cfg.VMMAC, cfg.BridgeInterface, cfg.TapInterface)

	defer func() {
		// Best-effort wait so cloud-hypervisor is reaped after ctx cancellation.
		if werr := vm.Wait(); werr != nil {
			fmt.Printf("cloud-hypervisor exited: %v\n", werr)
		}
	}()

	// Serve the Redfish API over HTTPS on the pod overlay address so metalman
	// (via the client's local forward) can control the guest's power.
	go func() {
		if rerr := startRedfishServer(runCtx, cfg, vm); rerr != nil {
			fmt.Printf("redfish server error: %v\n", rerr)
		}
	}()

	// When enabled, run a pod-local netboot HTTP reverse proxy so the guest
	// bootloader fetches the netboot payload over the fast pod<->guest LAN while
	// the pod re-originates to the client over the overlay with kernel TCP.
	go func() {
		if perr := startNetbootProxy(runCtx, cfg); perr != nil {
			fmt.Printf("netboot proxy error: %v\n", perr)
		}
	}()

	select {
	case <-runCtx.Done():
	case <-sig:
	}

	cancel()

	return nil
}

// runReaper runs the in-pod garbage collector. It builds a client from the
// pod's ServiceAccount and, immediately and then every interval, deletes every
// expired playtime run in the pod's namespace scope (cascading to each run's
// Pod, ServiceAccount, ClusterRole, and ClusterRoleBinding). Deleting the run
// whose Node anchor is selfNodeName triggers cancel so this pod shuts down.
func runReaper(ctx context.Context, cfg Config, selfNodeName string, interval time.Duration, cancel context.CancelFunc) {
	c, err := newInClusterClient()
	if err != nil {
		fmt.Printf("in-pod reaper disabled: %v\n", err)
		return
	}

	reap := func() {
		reaped, err := reapExpired(ctx, c, cfg.Namespace, time.Now())
		if err != nil {
			fmt.Printf("in-pod reaper: %v\n", err)
			return
		}

		for _, name := range reaped {
			fmt.Printf("in-pod reaper: reaped expired playtime run %q\n", name)

			if name == selfNodeName {
				fmt.Printf("in-pod reaper: own run expired, shutting down\n")
				cancel()
			}
		}
	}

	reap()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

// configureServer performs every mutating step required to stand up the pod's
// VXLAN endpoint. Learning is left enabled so the pod learns the client's
// underlay address from the first received packet; an all-zeros FDB entry
// floods broadcast and unknown-unicast frames (for example a DHCP DISCOVER) to
// the client's underlay address so they reach the client's userspace endpoint.
// IP forwarding plus a MASQUERADE rule out the uplink lets overlay-sourced
// traffic reach the internet via the pod (server-side NAT), keeping egress NAT
// off the client.
func configureServer(cfg Config, podIP, clientUnderlay string) error {
	uplink, err := netlink.LinkByName(serverUplink)
	if err != nil {
		return fmt.Errorf("look up uplink %q: %w", serverUplink, err)
	}

	// Recreate the VXLAN device so repeated runs converge to a known state,
	// mirroring the old `ip link del ... || true` then `ip link add`.
	if existing, err := netlink.LinkByName(cfg.VXLANInterface); err == nil {
		if err := netlink.LinkDel(existing); err != nil {
			return fmt.Errorf("delete existing link %q: %w", cfg.VXLANInterface, err)
		}
	}

	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: cfg.VXLANInterface,
			MTU:  cfg.OverlayMTU,
		},
		VxlanId:      cfg.VNI,
		Port:         cfg.VXLANPort,
		SrcAddr:      net.ParseIP(podIP),
		VtepDevIndex: uplink.Attrs().Index,
		Learning:     true,
	}
	if err := netlink.LinkAdd(vxlan); err != nil {
		return fmt.Errorf("add vxlan link %q: %w", cfg.VXLANInterface, err)
	}

	// Flood broadcast/unknown-unicast frames to the client's underlay address.
	floodIP := net.ParseIP(clientUnderlay)
	if floodIP == nil {
		return fmt.Errorf("client underlay %q is not a valid IP address", clientUnderlay)
	}

	if err := netlink.NeighAppend(&netlink.Neigh{
		LinkIndex:    vxlan.Attrs().Index,
		Family:       unix.AF_BRIDGE,
		State:        netlink.NUD_PERMANENT,
		Flags:        netlink.NTF_SELF,
		IP:           floodIP,
		HardwareAddr: net.HardwareAddr{0, 0, 0, 0, 0, 0},
	}); err != nil {
		return fmt.Errorf("append flood fdb entry to %s: %w", clientUnderlay, err)
	}

	// Assign the overlay address and bring the device up at the overlay MTU.
	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", cfg.OverlayRemoteIP, cfg.OverlayPrefix))
	if err != nil {
		return fmt.Errorf("parse overlay address %s/%d: %w", cfg.OverlayRemoteIP, cfg.OverlayPrefix, err)
	}

	if err := netlink.AddrAdd(vxlan, addr); err != nil {
		return fmt.Errorf("add overlay address %s to %q: %w", addr, cfg.VXLANInterface, err)
	}

	if err := netlink.LinkSetMTU(vxlan, cfg.OverlayMTU); err != nil {
		return fmt.Errorf("set mtu %d on %q: %w", cfg.OverlayMTU, cfg.VXLANInterface, err)
	}

	if err := netlink.LinkSetUp(vxlan); err != nil {
		return fmt.Errorf("set link %q up: %w", cfg.VXLANInterface, err)
	}

	// Enable IPv4 forwarding (replaces `sysctl -w net.ipv4.ip_forward=1`).
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}

	// Masquerade forwarded overlay traffic out the uplink (idempotent).
	ipt, err := iptables.New()
	if err != nil {
		return fmt.Errorf("init iptables: %w", err)
	}

	if err := ipt.AppendUnique("nat", "POSTROUTING", "-o", serverUplink, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("append masquerade rule out %q: %w", serverUplink, err)
	}

	return nil
}
