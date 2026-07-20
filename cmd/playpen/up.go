// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// handshakeTimeout bounds how long `up` waits for the primary WireGuard
// handshake to complete before attempting to ping.
const handshakeTimeout = 20 * time.Second

// runUp is the single foreground command. It registers the temporary node,
// brings up the in-process userspace WireGuard + VXLAN overlay dataplane,
// ensures the demo pod, pings it over the overlay, and (optionally) holds the
// dataplane open until interrupted. The dataplane is entirely in-process, so it
// is torn down automatically when this process exits.
func runUp(ctx context.Context, cfg Config, pingCount int, keepUp bool) error {
	forwards, err := cfg.parsedForwards()
	if err != nil {
		return err
	}

	if err := cfg.validateArch(); err != nil {
		return err
	}

	if err := cfg.validateDHCP(); err != nil {
		return err
	}

	if err := cfg.validateVM(); err != nil {
		return err
	}

	if err := cfg.validateRedfish(); err != nil {
		return err
	}

	if err := cfg.validateSite(); err != nil {
		return err
	}

	// The pod always provisions a guest VM (powered off, driven by Redfish). It
	// PXE-boots and leases its overlay address from the client side, so a full
	// boot needs the DHCP relay and TFTP proxy. Warn (rather than fail) when
	// they are not configured so the Redfish endpoints can still be exercised.
	if !cfg.dhcpEnabled() {
		fmt.Printf("warning: DHCP relay not configured (--dhcp-relay-port/--dhcp-server); " +
			"the guest can be powered on via Redfish but will not PXE-boot until DHCP and TFTP are set up\n")
	}

	pubKey, err := ensureKeypair(cfg)
	if err != nil {
		return err
	}

	privHex, err := loadPrivateKeyHex(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("using WireGuard public key %s\n", pubKey)

	c, err := newClient(cfg)
	if err != nil {
		return err
	}

	// Best-effort: clean up any previous stale runs in this namespace before we
	// add a new one, so a long-dead client's objects do not accumulate.
	if reaped, err := reapExpired(ctx, c, cfg.Namespace, time.Now()); err != nil {
		fmt.Printf("warning: reaping stale runs: %v\n", err)
	} else if len(reaped) > 0 {
		fmt.Printf("reaped %d expired playpen run(s): %v\n", len(reaped), reaped)
	}

	// The temporary node registers into a dedicated site, so that site (and its
	// gateway pool assignment) must exist first. Like the namespace and shared
	// RBAC, the site is shared across runs and left in place.
	if err := ensureSite(ctx, c, cfg); err != nil {
		return err
	}

	node, err := ensureTempNode(ctx, c, cfg, pubKey, time.Now())
	if err != nil {
		return err
	}
	defer cleanupRun(c, node.Name)

	if err := writeLastRun(cfg, node.Name); err != nil {
		fmt.Printf("warning: recording run marker: %v\n", err)
	}

	fmt.Printf("temporary node %q registered in site %q (pod cidr %s); expires in %s\n",
		node.Name, cfg.NodeSite, cfg.NodePodCIDR, cfg.TTL)

	// The shared namespace must exist before the shared ServiceAccount (a
	// namespaced object) is created. Both are shared across runs and left in
	// place for future runs.
	if err := ensureNamespace(ctx, c, cfg); err != nil {
		return err
	}

	saName, err := ensureSharedRBAC(ctx, c, cfg)
	if err != nil {
		return err
	}

	if waitForSliceMembership(ctx, c, cfg, pubKey, sliceMembershipTimeout) {
		fmt.Printf("node present in a %q SiteNodeSlice; gateways will mesh us\n", cfg.NodeSite)
	} else {
		fmt.Printf("warning: node not yet visible in a %q SiteNodeSlice; continuing anyway\n", cfg.NodeSite)
	}

	podIP, podNode, err := ensureDemoPod(ctx, c, cfg, node, saName)
	if err != nil {
		return err
	}

	fmt.Printf("demo pod running on %q with pod IP %s\n", podNode, podIP)

	dp, err := newDataplane(cfg, privHex, podIP)
	if err != nil {
		return err
	}
	defer dp.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go dp.run(runCtx)

	fmt.Printf("userspace dataplane up: %d WireGuard device(s) peered with gateways %v\n", len(cfg.gateways()), cfg.GatewayEndpoints)
	fmt.Printf("overlay %s reaches pod overlay %s via VXLAN vni %d (no root, no netns)\n", cfg.OverlayLocalIP, cfg.OverlayRemoteIP, cfg.VNI)

	if dp.under.waitForHandshake(runCtx, handshakeTimeout) {
		fmt.Printf("WireGuard handshake established\n")
	} else {
		fmt.Printf("warning: no WireGuard handshake yet; pinging anyway\n")
	}

	before := dp.under.transfer()

	fmt.Printf("\npinging pod overlay %s (%d packets)...\n", cfg.OverlayRemoteIP, pingCount)

	received, pingErr := dp.over.ping(runCtx, pingCount, 3*time.Second)

	after := dp.under.transfer()
	fmt.Printf("\nWireGuard transfer:\n  before:\n%s\n  after:\n%s\n", before, after)

	switch {
	case pingErr != nil && !keepUp:
		return fmt.Errorf("ping failed: %w", pingErr)
	case received == 0 && !keepUp:
		return fmt.Errorf("ping failed: no replies received from %s", cfg.OverlayRemoteIP)
	case pingErr != nil || received == 0:
		fmt.Printf("\nwarning: overlay ping did not succeed; holding dataplane open anyway (--keep-up)\n")
	default:
		fmt.Printf("\nsuccess: %d/%d replies over the mesh\n", received, pingCount)
	}

	if len(forwards) > 0 {
		dp.over.startForwarder(runCtx, forwards)

		for _, r := range forwards {
			fmt.Printf("forwarding %s:%d -> 127.0.0.1:%d (tcp)\n", cfg.OverlayLocalIP, r.overlayPort, r.loopbackPort)
		}
	}

	if cfg.dhcpEnabled() {
		fmt.Printf("dhcp relay active: overlay BOOTREQUEST -> server %s (giaddr %s, relay port %d)\n",
			cfg.DHCPServer, dp.relay.giaddr, cfg.DHCPRelayPort)
	}

	if cfg.tftpConfigured() {
		tftpAddr, tftpErr := cfg.tftpServerAddr()
		if tftpErr != nil {
			return tftpErr
		}

		dp.over.startTFTPProxy(runCtx, tftpAddr)

		fmt.Printf("guest PXE-boot support active: DHCP relay + TFTP proxy -> %s (vni %d)\n", tftpAddr, cfg.VNI)
	}

	// When the DHCP relay is active, also steer the guest to UEFI HTTP boot
	// whenever metalman configures it via Redfish: poll the pod's Redfish for
	// boot intent and run an overlay HTTP reverse proxy to the boot server.
	if cfg.dhcpEnabled() {
		go newBootReader(dp.over, cfg, dp.boot).run(runCtx)

		if err := startHTTPBootProxy(runCtx, dp.over, dp.boot); err != nil {
			return err
		}

		fmt.Printf("guest HTTP-boot steering active: Redfish reader + HTTP proxy on overlay %s:%d\n",
			cfg.OverlayLocalIP, httpBootProxyPort)
	}

	// Expose the pod's Redfish server on a local loopback port so a locally
	// running metalman can control the guest's power.
	if err := startLocalForward(runCtx, dp.over, uint16(cfg.RedfishLocalPort), uint16(cfg.RedfishPort)); err != nil {
		return err
	}

	fmt.Printf("redfish reachable at https://127.0.0.1:%d/redfish/v1/ (-> pod %s:%d)\n",
		cfg.RedfishLocalPort, cfg.OverlayRemoteIP, cfg.RedfishPort)

	// The guest and its Redfish control plane live in the pod for the whole
	// session, so always hold the dataplane open.
	holdErr := holdUntilInterrupt(runCtx)

	return holdErr
}

// holdUntilInterrupt blocks until the context is cancelled or SIGINT/SIGTERM is
// received, keeping the in-process dataplane alive.
func holdUntilInterrupt(ctx context.Context) error {
	sig := make(chan os.Signal, 1)

	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	fmt.Printf("\nholding dataplane open; press Ctrl-C to tear down\n")

	select {
	case <-ctx.Done():
	case <-sig:
	}

	fmt.Printf("\ntearing down userspace dataplane\n")

	return nil
}
