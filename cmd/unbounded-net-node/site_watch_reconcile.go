// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
	"github.com/Azure/unbounded-kube/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
)

// watchSitesAndConfigureWireGuard watches SiteNodeSlice objects and configures WireGuard peers for nodes in the same site
// Gateway nodes can operate without being assigned to a site - they will still configure themselves to accept connections
// The informers are passed in from run() and are already started and synced.
func watchSiteAndConfigureWireGuard(ctx context.Context, clientset kubernetes.Interface, dynamicClient dynamic.Interface, cfg *config, myPubKey string, nodePodCIDRs []string, manageCniPlugin bool, healthState *nodeHealthState, netlinkCache *unboundednetnetlink.NetlinkCache, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer cache.SharedIndexInformer) error {
	klog.Info("Watching Site, SiteNodeSlice, GatewayPool, GatewayPoolNode, SitePeering, SiteGatewayPoolAssignment, and GatewayPoolPeering objects for changes")

	// Read private key for WireGuard configuration
	privKeyPath := filepath.Join(cfg.WireGuardDir, "server.priv")

	privKeyData, err := os.ReadFile(privKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read private key: %w", err)
	}

	privKey := strings.TrimSpace(string(privKeyData))

	// Initialize link manager for interface operations
	linkManager := unboundednetnetlink.NewLinkManager(fmt.Sprintf("wg%d", cfg.WireGuardPort))

	// Track current state with netlink managers
	wgCollector := unboundednetnetlink.NewWireGuardCollector()
	prometheus.MustRegister(wgCollector)

	state := &wireGuardState{
		linkManager:                   linkManager,
		nodePodCIDRs:                  nodePodCIDRs, // Store node's podCIDRs for cbr0 gateway IP calculation
		manageCniPlugin:               manageCniPlugin,
		clientset:                     clientset,    // Store clientset for tainting nodes
		nodeName:                      cfg.NodeName, // Store node name for tainting
		meshPeerHealthCheckEnabled:    make(map[string]bool),
		gatewayPeerHealthCheckEnabled: make(map[string]bool),
		gatewayNames:                  make(map[string]string),
		gatewaySiteNames:              make(map[string]string),
		wgCollector:                   wgCollector,
		healthFlapMaxBackoff:          cfg.HealthFlapMaxBackoff,
		netlinkCache:                  netlinkCache,
	}

	// Set up the dedicated routing table and ip rule (not needed for eBPF
	// dataplane which uses the main table directly).
	if cfg.TunnelDataplane != "ebpf" {
		if cfg.RouteTableID != 0 && cfg.RouteTableID != 254 {
			if err := unboundednetnetlink.EnsureRtTablesEntry(cfg.RouteTableID, "unbounded-net"); err != nil {
				klog.Warningf("Failed to ensure rt_tables entry for table %d: %v", cfg.RouteTableID, err)
			}

			if err := unboundednetnetlink.EnsureIPRule(cfg.RouteTableID, 32765, 0, 0); err != nil {
				klog.Warningf("Failed to ensure ip rule for table %d: %v", cfg.RouteTableID, err)
			}
		}
	} else {
		// Clean up old dedicated routing table from previous netlink dataplane runs.
		if cfg.RouteTableID != 0 && cfg.RouteTableID != 254 {
			_ = unboundednetnetlink.FlushRouteTable(cfg.RouteTableID)           //nolint:errcheck
			_ = unboundednetnetlink.RemoveIPRule(cfg.RouteTableID, 32765, 0, 0) //nolint:errcheck
		}
	}

	// Initialize unified route manager for netlink-based route programming.
	// eBPF dataplane uses the main routing table (0) directly.
	var routeTable int
	if cfg.TunnelDataplane == "ebpf" {
		routeTable = 0
	} else {
		routeTable = cfg.RouteTableID
	}

	routeManager := unboundednetnetlink.NewUnifiedRouteManager(fmt.Sprintf("wg%d", cfg.WireGuardPort), routeTable)
	routeManager.SetNetlinkCache(netlinkCache)
	state.routeManager = routeManager
	state.routeTableID = cfg.RouteTableID

	// Set preferred source IPs for routes based on node's podCIDRs.
	// Collect both address families before calling SetPreferredSourceIPs
	// to avoid one family overwriting the other with nil.
	var preferredSrcIPv4, preferredSrcIPv6 net.IP

	for _, cidr := range nodePodCIDRs {
		gwIP := getGatewayIPFromCIDR(cidr)
		if gwIP == nil {
			continue
		}

		if gwIP.To4() != nil {
			preferredSrcIPv4 = gwIP
		} else {
			preferredSrcIPv6 = gwIP
		}
	}

	routeManager.SetPreferredSourceIPs(preferredSrcIPv4, preferredSrcIPv6)

	// Initialize healthcheck manager for peer health monitoring.
	healthCheckMgr, err := healthcheck.NewManager(cfg.NodeName, cfg.HealthCheckPort, func(peerHostname string, newState, oldState healthcheck.SessionState) {
		klog.V(2).Infof("Healthcheck state change for peer %s: %s -> %s", peerHostname, oldState, newState)
		// Toggle BPF map health for eBPF dataplane peers.
		if tm := state.tunnelMaps["ebpf"]; tm != nil {
			healthy := newState == healthcheck.StateUp
			if n := tm.SetPeerHealth(peerHostname, healthy); n > 0 {
				klog.V(2).Infof("eBPF: set %d BPF entries for peer %s healthy=%v", n, peerHostname, healthy)
			}
		}

		switch newState {
		case healthcheck.StateDown:
			if rmErr := routeManager.RemoveNexthopForPeer(peerHostname); rmErr != nil {
				klog.Errorf("Failed to remove nexthop for peer %s on health-down: %v", peerHostname, rmErr)
			}
		case healthcheck.StateUp:
			if rmErr := routeManager.RestoreNexthopForPeer(peerHostname); rmErr != nil {
				klog.Errorf("Failed to restore nexthop for peer %s on health-up: %v", peerHostname, rmErr)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("failed to create healthcheck manager: %w", err)
	}

	state.healthCheckManager = healthCheckMgr

	if err := healthCheckMgr.Start(ctx); err != nil {
		return fmt.Errorf("failed to start healthcheck manager: %w", err)
	}

	klog.Info("Healthcheck manager started")

	// Start link stats monitor to detect incrementing error/drop counters.
	// Uses a 30-second collection interval (independent of informer resync)
	// so warnings can expire promptly when counters stop incrementing.
	state.linkStatsMonitor = newLinkStatsMonitor(30 * time.Second)
	go state.linkStatsMonitor.Start(ctx)

	// Start kube-proxy health monitor (0 interval disables it).
	state.kubeProxyMonitor = newKubeProxyMonitor(cfg.KubeProxyHealthInterval)
	go state.kubeProxyMonitor.Start(ctx)

	// Fetch and cache node IPs once at startup (avoids API calls in status endpoint)
	if node, err := clientset.CoreV1().Nodes().Get(ctx, cfg.NodeName, metav1.GetOptions{}); err == nil {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				state.nodeInternalIPs = append(state.nodeInternalIPs, addr.Address)
			}

			if addr.Type == corev1.NodeExternalIP {
				state.nodeExternalIPs = append(state.nodeExternalIPs, addr.Address)
			}
		}

		klog.Infof("Cached node IPs: internal=%v, external=%v", state.nodeInternalIPs, state.nodeExternalIPs)
	} else {
		klog.Warningf("Failed to fetch node IPs (status endpoint will have empty IPs): %v", err)
	}

	// Initialize masquerade manager for iptables rules
	masqManager, err := unboundednetnetlink.NewMasqueradeManager()
	if err != nil {
		klog.Warningf("Failed to create masquerade manager (masquerade will be disabled): %v", err)
		// Don't fail - continue without masquerade
	} else {
		state.masqueradeManager = masqManager

		klog.Info("Masquerade manager initialized")
	}

	// Initialize MSS clamp manager for TCP MSS clamping on WireGuard interfaces.
	// This prevents pods (with 1500-byte veth MTU) from advertising an MSS that
	// exceeds the WireGuard tunnel MTU, which would cause silent drops of large
	// TCP responses (e.g., TLS ServerHello) at gateway forwarding hops.
	mssClampMgr, err := unboundednetnetlink.NewMSSClampManager()
	if err != nil {
		klog.Warningf("Failed to create MSS clamp manager (MSS clamping will be disabled): %v", err)
	} else {
		state.mssClampManager = mssClampMgr
		if err := mssClampMgr.EnsureRules(); err != nil {
			klog.Warningf("Failed to install MSS clamp rules: %v", err)
		}
	}

	// Initialize gateway policy manager to allow cleanup of stale policy rules
	// even when policy routing is currently disabled.
	policyManager, err := unboundednetnetlink.NewGatewayPolicyManager(cfg.WireGuardPort)
	if err != nil {
		klog.Warningf("Failed to create gateway policy manager: %v", err)
	} else {
		state.gatewayPolicyManager = policyManager
		if !cfg.EnablePolicyRouting {
			if err := state.gatewayPolicyManager.Cleanup(); err != nil {
				klog.Warningf("Failed to cleanup stale gateway policy rules while disabled: %v", err)
			} else {
				klog.Info("Cleaned up stale gateway policy rules while policy routing is disabled")
			}
		}
	}

	defer cleanupNodeNetworkingOnShutdown(cfg, state)

	// Register the status server with the health state EARLY so /status endpoints work
	// This ensures the status endpoint shows real state even during initialization
	statusSrv := &nodeStatusServer{
		state:               state,
		cfg:                 cfg,
		pubKey:              myPubKey,
		clientset:           clientset,
		siteInformer:        siteInformer,
		sliceInformer:       sliceInformer,
		gatewayPoolInformer: gatewayPoolInformer,
		sitePeeringInformer: sitePeeringInformer,
	}
	healthState.setStatusServer(statusSrv)
	klog.Info("Status server registered with health server")
	statusSrv.startRouteChangeWatcher(ctx)

	// Start websocket status transport first; periodic HTTP push remains fallback.
	wsConnected := &atomic.Bool{}
	wsMode := &atomic.Int32{}
	fallbackWSEnabled := &atomic.Bool{}
	apiPushEnabled := &atomic.Bool{}
	closeFallbackWS := &atomic.Bool{}

	var statusTransportWg sync.WaitGroup
	statusTransportWg.Add(2)

	go func() {
		defer statusTransportWg.Done()

		startStatusWebSocketPusher(ctx, cfg, healthState, wsConnected, wsMode, fallbackWSEnabled, apiPushEnabled, closeFallbackWS)
	}()
	go func() {
		defer statusTransportWg.Done()

		startStatusPusher(ctx, cfg, healthState, wsConnected, wsMode, fallbackWSEnabled, apiPushEnabled, closeFallbackWS)
	}()
	// Store the WaitGroup so the shutdown path can wait for graceful WS close
	// before tearing down tunnel interfaces.
	state.mu.Lock()
	state.statusTransportWg = &statusTransportWg
	state.mu.Unlock()

	// Start GatewayNode heartbeat updater (10s lease-style status update).
	go startGatewayNodeHeartbeat(ctx, dynamicClient, cfg.NodeName, state, gatewayNodeHeartbeatInterval)

	// Helper to trigger update - uses CRD-based site lookup
	// This finds the site by searching SiteNodeSlice and GatewayPool for this node's public key
	reconcileUpdate := func() {
		// Don't reconcile until all informer caches are synced. Event handlers
		// fire during the initial cache population, but the data from other
		// informers may not be available yet, leading to incomplete peer lists.
		for _, synced := range []cache.InformerSynced{
			sliceInformer.HasSynced,
			siteInformer.HasSynced,
			gatewayPoolInformer.HasSynced,
			gatewayNodeInformer.HasSynced,
			sitePeeringInformer.HasSynced,
			assignmentInformer.HasSynced,
			poolPeeringInformer.HasSynced,
		} {
			if !synced() {
				klog.V(4).Info("Skipping reconcile: informer caches not fully synced")
				return
			}
		}

		start := time.Now()
		// Find which site this node belongs to using CRD-based lookup
		mySiteName := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, myPubKey)

		// Store the site name for status endpoint
		state.mu.Lock()
		state.siteName = mySiteName
		state.mu.Unlock()

		// Validate host forwarding configuration at startup and each reconcile cycle.
		// Running this in reconcile ties checks to informer updates and resync events.
		refreshNodeConfigurationProblems(siteInformer, mySiteName, state, cfg.InformerResyncPeriod)

		if err := updateWireGuardFromSlices(ctx, dynamicClient, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer, cfg, mySiteName, privKey, myPubKey, manageCniPlugin, state); err != nil {
			klog.Errorf("Failed to update tunnel config: %v", err)
			nodeReconciliationTotal.WithLabelValues("error").Inc()
		} else {
			nodeReconciliationTotal.WithLabelValues("success").Inc()
		}

		nodeReconciliationDuration.Observe(time.Since(start).Seconds())
	}

	updateCh := make(chan struct{}, 1)

	var updateMu sync.Mutex

	updatePending := false
	requestUpdate := func() {
		updateMu.Lock()
		updatePending = true
		updateMu.Unlock()

		select {
		case updateCh <- struct{}{}:
		default:
		}
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-updateCh:
				for {
					updateMu.Lock()
					if !updatePending {
						updateMu.Unlock()
						break
					}

					updatePending = false
					updateMu.Unlock()

					reconcileUpdate()
				}
			}
		}
	}()

	// Periodic reconcile ensures stale GatewayNode route advertisements are aged out
	// even when only heartbeat updates are observed and filtered out below.
	go func() {
		ticker := time.NewTicker(gatewayNodeHeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				requestUpdate()
			}
		}
	}()

	// Register event handlers with cleanup on partial failure.
	// If any registration fails, already-registered handlers are removed.
	var handlerCleanups []func()

	defer func() {
		for _, fn := range handlerCleanups {
			fn()
		}
	}()

	// Set up event handlers for SiteNodeSlices
	sliceReg, err := sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { requestUpdate() },
		UpdateFunc: func(oldObj, newObj interface{}) { requestUpdate() },
		DeleteFunc: func(obj interface{}) { requestUpdate() },
	})
	if err != nil {
		return fmt.Errorf("failed to add slice event handler: %w", err)
	}

	handlerCleanups = append(handlerCleanups, func() {
		if err := sliceInformer.RemoveEventHandler(sliceReg); err != nil {
			klog.V(4).Infof("Failed to remove slice event handler: %v", err)
		}
	})

	// Set up event handlers for Sites (to detect nodeCidrs changes)
	siteReg, err := siteInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { requestUpdate() },
		UpdateFunc: func(oldObj, newObj interface{}) { requestUpdate() },
		DeleteFunc: func(obj interface{}) { requestUpdate() },
	})
	if err != nil {
		return fmt.Errorf("failed to add site event handler: %w", err)
	}

	handlerCleanups = append(handlerCleanups, func() {
		if err := siteInformer.RemoveEventHandler(siteReg); err != nil {
			klog.V(4).Infof("Failed to remove site event handler: %v", err)
		}
	})

	// Set up event handlers for GatewayPools (to detect gateway node and routedCidrs changes)
	poolReg, err := gatewayPoolInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { requestUpdate() },
		UpdateFunc: func(oldObj, newObj interface{}) { requestUpdate() },
		DeleteFunc: func(obj interface{}) { requestUpdate() },
	})
	if err != nil {
		return fmt.Errorf("failed to add gateway pool event handler: %w", err)
	}

	handlerCleanups = append(handlerCleanups, func() {
		if err := gatewayPoolInformer.RemoveEventHandler(poolReg); err != nil {
			klog.V(4).Infof("Failed to remove gateway pool event handler: %v", err)
		}
	})

	// Set up event handlers for GatewayNodes (future route advertisement source).
	gwNodeReg, err := gatewayNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { requestUpdate() },
		UpdateFunc: func(oldObj, newObj interface{}) {
			if shouldReconcileGatewayNodeUpdate(oldObj, newObj) {
				requestUpdate()
			}
		},
		DeleteFunc: func(obj interface{}) { requestUpdate() },
	})
	if err != nil {
		return fmt.Errorf("failed to add gateway node event handler: %w", err)
	}

	handlerCleanups = append(handlerCleanups, func() {
		if err := gatewayNodeInformer.RemoveEventHandler(gwNodeReg); err != nil {
			klog.V(4).Infof("Failed to remove gateway node event handler: %v", err)
		}
	})

	// Set up event handlers for SitePeerings (to detect direct site peering changes)
	peeringReg, err := sitePeeringInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { requestUpdate() },
		UpdateFunc: func(oldObj, newObj interface{}) { requestUpdate() },
		DeleteFunc: func(obj interface{}) { requestUpdate() },
	})
	if err != nil {
		return fmt.Errorf("failed to add site peering event handler: %w", err)
	}

	handlerCleanups = append(handlerCleanups, func() {
		if err := sitePeeringInformer.RemoveEventHandler(peeringReg); err != nil {
			klog.V(4).Infof("Failed to remove site peering event handler: %v", err)
		}
	})

	assignmentReg, err := assignmentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { requestUpdate() },
		UpdateFunc: func(oldObj, newObj interface{}) { requestUpdate() },
		DeleteFunc: func(obj interface{}) { requestUpdate() },
	})
	if err != nil {
		return fmt.Errorf("failed to add site gateway pool assignment event handler: %w", err)
	}

	handlerCleanups = append(handlerCleanups, func() {
		if err := assignmentInformer.RemoveEventHandler(assignmentReg); err != nil {
			klog.V(4).Infof("Failed to remove assignment event handler: %v", err)
		}
	})

	poolPeeringReg, err := poolPeeringInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { requestUpdate() },
		UpdateFunc: func(oldObj, newObj interface{}) { requestUpdate() },
		DeleteFunc: func(obj interface{}) { requestUpdate() },
	})
	if err != nil {
		return fmt.Errorf("failed to add gateway pool peering event handler: %w", err)
	}

	handlerCleanups = append(handlerCleanups, func() {
		if err := poolPeeringInformer.RemoveEventHandler(poolPeeringReg); err != nil {
			klog.V(4).Infof("Failed to remove pool peering event handler: %v", err)
		}
	})

	// Initial configuration
	requestUpdate()

	// Wait for context cancellation
	<-ctx.Done()

	return ctx.Err()
}

// cleanupNodeNetworkingOnShutdown conditionally removes managed networking state during node-agent shutdown.
func cleanupNodeNetworkingOnShutdown(cfg *config, state *wireGuardState) {
	if cfg == nil || state == nil {
		return
	}

	// Wait for the WebSocket and HTTP push goroutines to finish their
	// graceful close handshakes before tearing down tunnel interfaces.
	// The WS close frame must be sent over the tunnel before we delete it.
	state.mu.Lock()
	transportWg := state.statusTransportWg
	state.mu.Unlock()

	if transportWg != nil {
		klog.V(2).Info("Waiting for status transport goroutines to finish graceful close...")
		transportWg.Wait()
		klog.V(2).Info("Status transport goroutines finished")
	}

	state.mu.Lock()
	mainLinkManager := state.linkManager
	mainWGManager := state.wireguardManager
	routeManager := state.routeManager
	gatewayPolicyManager := state.gatewayPolicyManager
	masqueradeManager := state.masqueradeManager
	mssClampManager := state.mssClampManager
	healthCheckMgr := state.healthCheckManager

	gatewayLinkManagers := make(map[string]*unboundednetnetlink.LinkManager, len(state.gatewayLinkManagers))
	for ifaceName, manager := range state.gatewayLinkManagers {
		gatewayLinkManagers[ifaceName] = manager
	}

	gatewayWGManagers := make(map[string]*unboundednetnetlink.WireGuardManager, len(state.gatewayWireguardManagers))
	for ifaceName, manager := range state.gatewayWireguardManagers {
		gatewayWGManagers[ifaceName] = manager
	}

	geneveManagers := make(map[string]*unboundednetnetlink.LinkManager, len(state.geneveInterfaces))
	for ifName, lm := range state.geneveInterfaces {
		geneveManagers[ifName] = lm
	}
	state.mu.Unlock()

	// Stop healthcheck manager
	if healthCheckMgr != nil {
		healthCheckMgr.Stop()
		klog.Info("Healthcheck manager stopped")
	}

	if cfg.RemoveConfigurationOnShutdown {
		// Routes
		if routeManager != nil {
			if err := routeManager.RemoveAllRoutes(); err != nil {
				klog.Errorf("Failed to cleanup managed netlink routes on shutdown: %v", err)
			} else {
				klog.Info("Cleaned up managed netlink routes on shutdown")
			}
		}

		// Clean up route table and ip rule
		if cfg.RouteTableID != 0 && cfg.RouteTableID != 254 {
			if err := unboundednetnetlink.FlushRouteTable(cfg.RouteTableID); err != nil {
				klog.Errorf("Failed to flush route table %d on shutdown: %v", cfg.RouteTableID, err)
			}

			if err := unboundednetnetlink.RemoveIPRule(cfg.RouteTableID, 32765, 0, 0); err != nil {
				klog.Errorf("Failed to remove ip rule for table %d on shutdown: %v", cfg.RouteTableID, err)
			}
		}

		// Gateway policy routing rules
		if gatewayPolicyManager != nil {
			if err := gatewayPolicyManager.Cleanup(); err != nil {
				klog.Errorf("Failed to cleanup gateway policy routing rules on shutdown: %v", err)
			} else {
				klog.Info("Cleaned up gateway policy routing rules on shutdown")
			}
		}

		// Masquerade rules
		if masqueradeManager != nil {
			if err := masqueradeManager.Cleanup(); err != nil {
				klog.Errorf("Failed to cleanup masquerade rules on shutdown: %v", err)
			} else {
				klog.Info("Cleaned up masquerade rules on shutdown")
			}
		}

		// MSS clamp rules
		if mssClampManager != nil {
			if err := mssClampManager.Cleanup(); err != nil {
				klog.Errorf("Failed to cleanup MSS clamp rules on shutdown: %v", err)
			} else {
				klog.Info("Cleaned up MSS clamp rules on shutdown")
			}
		}

		// WireGuard managers (close wgctrl clients)
		if mainWGManager != nil {
			if err := mainWGManager.Close(); err != nil {
				klog.Warningf("Failed to close main WireGuard manager on shutdown: %v", err)
			}
		}

		for ifaceName, manager := range gatewayWGManagers {
			if manager == nil {
				continue
			}

			if err := manager.Close(); err != nil {
				klog.Warningf("Failed to close gateway WireGuard manager %s on shutdown: %v", ifaceName, err)
			}
		}

		// WireGuard interfaces (delete links)
		if mainLinkManager != nil {
			if err := mainLinkManager.DeleteLink(); err != nil {
				klog.Errorf("Failed to delete main WireGuard interface on shutdown: %v", err)
			} else {
				klog.Info("Deleted main WireGuard interface on shutdown")
			}
		}

		for ifaceName, manager := range gatewayLinkManagers {
			if manager == nil {
				continue
			}

			if err := manager.DeleteLink(); err != nil {
				klog.Errorf("Failed to delete gateway WireGuard interface %s on shutdown: %v", ifaceName, err)
				continue
			}

			removeTunnelForwardAccept(ifaceName)
			klog.Infof("Deleted gateway WireGuard interface %s on shutdown", ifaceName)
		}

		// GENEVE interfaces
		for ifName, lm := range geneveManagers {
			if lm == nil {
				continue
			}

			if err := lm.DeleteLink(); err != nil {
				klog.Errorf("Failed to delete GENEVE interface %s on shutdown: %v", ifName, err)
				continue
			}

			removeTunnelForwardAccept(ifName)
			klog.Infof("Deleted GENEVE interface %s on shutdown", ifName)
		}

		// Sweep for any remaining managed tunnel interfaces (wg*, gn*, ip*, vxlan*)
		links, err := netlink.LinkList()
		if err != nil {
			klog.Errorf("Failed to list links for tunnel interface sweep on shutdown: %v", err)
		} else {
			for _, link := range links {
				name := link.Attrs().Name
				if !isManagedTunnelInterface(name) {
					continue
				}

				if err := netlink.LinkDel(link); err != nil {
					klog.Warningf("Failed to delete managed tunnel interface %s on shutdown: %v", name, err)
				} else {
					removeTunnelForwardAccept(name)
					klog.Infof("Deleted managed tunnel interface %s on shutdown", name)
				}
			}
		}
	}
}

func startGatewayNodeHeartbeat(ctx context.Context, dynamicClient dynamic.Interface, nodeName string, state *wireGuardState, interval time.Duration) {
	if dynamicClient == nil || nodeName == "" || state == nil {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state.mu.Lock()
			isGateway := state.isGatewayNode
			localPools := append([]string{}, state.localGatewayPools...)
			state.mu.Unlock()

			if !isGateway || len(localPools) == 0 {
				continue
			}

			statusPatch := map[string]interface{}{
				"status": map[string]interface{}{
					"lastUpdated": time.Now().UTC().Format(time.RFC3339),
				},
			}
			if err := patchGatewayNodeStatus(ctx, dynamicClient, nodeName, statusPatch); err != nil {
				klog.Errorf("GatewayPoolNode heartbeat status patch failed for %s: %v", nodeName, err)
			}
		}
	}
}

// shouldReconcileGatewayNodeUpdate returns true when a GatewayNode update should
// trigger a full wireguard/gateway reconcile.
//
// Heartbeat-only updates (status.lastUpdated changes with no spec/routes delta)
// are intentionally ignored to avoid reconcile storms. Stale-route expiration is
// handled by the periodic reconcile ticker in watchSiteAndConfigureWireGuard.
func shouldReconcileGatewayNodeUpdate(oldObj, newObj interface{}) bool {
	oldUnstr, ok := oldObj.(*unstructured.Unstructured)
	if !ok {
		return true
	}

	newUnstr, ok := newObj.(*unstructured.Unstructured)
	if !ok {
		return true
	}

	oldGatewayNode, err := parseGatewayNode(oldUnstr)
	if err != nil {
		return true
	}

	newGatewayNode, err := parseGatewayNode(newUnstr)
	if err != nil {
		return true
	}

	if !reflect.DeepEqual(oldGatewayNode.Spec, newGatewayNode.Spec) {
		return true
	}

	if !reflect.DeepEqual(oldGatewayNode.Status.Routes, newGatewayNode.Status.Routes) {
		return true
	}

	// Only heartbeat changed.
	return false
}

// findMySiteFromCRDs finds which site this node belongs to by searching CRDs.
// For regular nodes, it searches SiteNodeSlices by WireGuard public key.
// For gateway nodes, it searches GatewayPool status by WireGuard public key.
// Returns empty string if not found in any CRD (node not yet added).
func findMySiteFromCRDs(sliceInformer, gatewayPoolInformer cache.SharedIndexInformer, myPubKey string) string {
	// First, check SiteNodeSlices (for regular nodes)
	for _, item := range sliceInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		slice, err := parseSiteNodeSlice(unstr)
		if err != nil {
			continue
		}

		for _, node := range slice.Nodes {
			if node.WireGuardPublicKey == myPubKey {
				return slice.SiteName
			}
		}
	}

	// If not found in slices, check GatewayPool status (for gateway nodes)
	for _, item := range gatewayPoolInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		pool, err := parseGatewayPool(unstr)
		if err != nil {
			continue
		}

		for _, gwNode := range pool.Status.Nodes {
			if gwNode.WireGuardPublicKey == myPubKey {
				if gwNode.SiteName != "" {
					return gwNode.SiteName
				}

				return ""
			}
		}
	}

	return ""
}

// isGatewayNodeFromCRDs checks if this node is a gateway node by searching GatewayPool status.
func isGatewayNodeFromCRDs(gatewayPoolInformer cache.SharedIndexInformer, myPubKey string) bool {
	for _, item := range gatewayPoolInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		pool, err := parseGatewayPool(unstr)
		if err != nil {
			continue
		}

		for _, gwNode := range pool.Status.Nodes {
			if gwNode.WireGuardPublicKey == myPubKey {
				return true
			}
		}
	}

	return false
}

// waitForSiteMembership waits for this node to appear in a SiteNodeSlice or GatewayPool.
// This ensures the site controller has processed this node before we continue.
// Returns the site name once found, or empty string for gateway nodes without a site.
func waitForSiteMembership(ctx context.Context, sliceInformer, gatewayPoolInformer cache.SharedIndexInformer, myPubKey string) (string, error) {
	// Check immediately first
	siteName := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, myPubKey)
	if siteName != "" {
		klog.Infof("Node found in site %q", siteName)
		return siteName, nil
	}

	klog.Info("Node not yet in any SiteNodeSlice or GatewayPool, waiting for site controller to add it...")

	// Set up a channel to receive notifications
	foundCh := make(chan string, 1)

	// Add event handlers to detect when we appear in a slice or pool
	sliceHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if siteName := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, myPubKey); siteName != "" {
				select {
				case foundCh <- siteName:
				default:
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if siteName := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, myPubKey); siteName != "" {
				select {
				case foundCh <- siteName:
				default:
				}
			}
		},
	}

	poolHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if siteName := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, myPubKey); siteName != "" {
				select {
				case foundCh <- siteName:
				default:
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if siteName := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, myPubKey); siteName != "" {
				select {
				case foundCh <- siteName:
				default:
				}
			}
		},
	}

	// Add handlers and get registration handles for removal
	sliceReg, err := sliceInformer.AddEventHandler(sliceHandler)
	if err != nil {
		return "", fmt.Errorf("failed to add slice event handler: %w", err)
	}

	defer func() {
		if err := sliceInformer.RemoveEventHandler(sliceReg); err != nil {
			klog.V(4).Infof("Failed to remove slice event handler: %v", err)
		}
	}()

	poolReg, err := gatewayPoolInformer.AddEventHandler(poolHandler)
	if err != nil {
		return "", fmt.Errorf("failed to add pool event handler: %w", err)
	}

	defer func() {
		if err := gatewayPoolInformer.RemoveEventHandler(poolReg); err != nil {
			klog.V(4).Infof("Failed to remove gateway pool event handler: %v", err)
		}
	}()

	// Wait for the node to appear or context to be cancelled
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case siteName := <-foundCh:
		klog.Infof("Node found in site %q", siteName)
		return siteName, nil
	}
}

// getManageCniPluginFromCRDs checks if the site has manageCniPlugin enabled by looking up the site CRD.
// Returns true (the default) if the site doesn't exist or doesn't have the field set.
func getManageCniPluginFromCRDs(siteInformer cache.SharedIndexInformer, siteName string) bool {
	if siteName == "" {
		return true // Default to true if no site
	}

	for _, item := range siteInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		site, err := parseSite(unstr)
		if err != nil {
			continue
		}

		if site.Name == siteName {
			if site.Spec.ManageCniPlugin == nil {
				return true // Default
			}

			return *site.Spec.ManageCniPlugin
		}
	}

	return true // Site not found, default to true
}

var configureWireGuardFunc = configureWireGuard

// updateWireGuardFromSlices reads Site and SiteNodeSlices from the informer caches and configures WireGuard
// It uses per-node podCIDRs from slices and gateway pools for routing
// It also looks up gateway pools referenced by the site and adds gateway peers with all remote sites' CIDRs
// For gateway nodes, it adds all nodes from all sites as peers (pubkey only, no endpoint)
// When manageCniPlugin is false on non-gateway nodes, same-site peers are skipped but
// directly peered remote-site peers are still included for podCIDR routing.
// Sites that are directly peered via SitePeering objects are treated as if they were the same site
func updateWireGuardFromSlices(ctx context.Context, dynamicClient dynamic.Interface, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer cache.SharedIndexInformer, cfg *config, mySiteName, privKey, myPubKey string, manageCniPlugin bool, state *wireGuardState) error {
	// Parse all sites
	var allSites []unboundednetv1alpha1.Site

	siteMap := make(map[string]*unboundednetv1alpha1.Site)

	for _, item := range siteInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		site, err := parseSite(unstr)
		if err != nil {
			klog.Warningf("Failed to parse Site: %v", err)
			continue
		}

		allSites = append(allSites, *site)
		siteMap[site.Name] = site
	}

	healthCheckProfiles := make(map[string]healthcheck.HealthCheckSettings)
	healthCheckProfileSources := make(map[string]string)
	siteHealthCheckProfileNames := make(map[string]string)
	peeringSiteHealthCheckProfileNames := make(map[string]string)
	peeringSiteHealthCheckSourcePeering := make(map[string]string)
	assignmentPoolHealthCheckProfileNames := make(map[string]string)
	assignmentPoolHealthCheckSourceAssignment := make(map[string]string)
	assignmentSiteHealthCheckProfileNames := make(map[string]string)
	assignmentSiteHealthCheckSourceAssignment := make(map[string]string)
	poolPeeringHealthCheckProfileNames := make(map[string]string)
	poolPeeringHealthCheckSourcePeering := make(map[string]string)
	poolHealthCheckProfileNames := make(map[string]string)

	// Per-scope tunnelMTU overrides collected from CRDs.
	siteTunnelMTUs := make(map[string]int)
	peeringSiteTunnelMTUs := make(map[string]int)
	assignmentSiteTunnelMTUs := make(map[string]int)
	assignmentPoolTunnelMTUs := make(map[string]int)
	poolTunnelMTUs := make(map[string]int)

	// Per-scope tunnel protocol overrides collected from CRDs.
	siteTunnelProtocols := make(map[string]string)
	peeringSiteTunnelProtocols := make(map[string]string)
	assignmentSiteTunnelProtocols := make(map[string]string)
	assignmentPoolTunnelProtocols := make(map[string]string)
	poolTunnelProtocols := make(map[string]string)

	for siteName, site := range siteMap {
		siteScope := healthCheckLogScope(siteGVR, siteName)

		enabled, profile := healthCheckProfileFromSettings(site.Spec.HealthCheckSettings, siteScope)
		if !enabled {
			continue
		}

		profileName := healthCheckProfileNameForSite(siteName)
		siteHealthCheckProfileNames[siteName] = profileName
		healthCheckProfiles[profileName] = profile
		healthCheckProfileSources[profileName] = siteScope
	}

	// Collect site-level tunnelMTU and tunnelProtocol overrides (separate
	// from healthcheck loop because these apply even when healthcheck is disabled).
	for siteName, site := range siteMap {
		if v := tunnelMTUFromSpec(site.Spec.TunnelMTU); v > 0 {
			siteTunnelMTUs[siteName] = v
		}

		if site.Spec.TunnelProtocol != nil {
			siteTunnelProtocols[siteName] = string(*site.Spec.TunnelProtocol)
		}
	}
	// Parse all SitePeerings in stable name order so conflict handling is deterministic.
	sitePeerings := make([]unboundednetv1alpha1.SitePeering, 0, len(sitePeeringInformer.GetStore().List()))
	for _, item := range sitePeeringInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		peering, err := parseSitePeering(unstr)
		if err != nil {
			klog.Warningf("Failed to parse SitePeering: %v", err)
			continue
		}

		if !unboundednetv1alpha1.SpecEnabled(peering.Spec.Enabled) {
			continue
		}

		sitePeerings = append(sitePeerings, *peering)
	}

	sort.Slice(sitePeerings, func(i, j int) bool {
		return sitePeerings[i].Name < sitePeerings[j].Name
	})

	assignments := make([]unboundednetv1alpha1.SiteGatewayPoolAssignment, 0, len(assignmentInformer.GetStore().List()))
	for _, item := range assignmentInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		assignment, err := parseSiteGatewayPoolAssignment(unstr)
		if err != nil {
			klog.Warningf("Failed to parse SiteGatewayPoolAssignment: %v", err)
			continue
		}

		if !unboundednetv1alpha1.SpecEnabled(assignment.Spec.Enabled) {
			continue
		}

		assignments = append(assignments, *assignment)
	}

	sort.Slice(assignments, func(i, j int) bool {
		return assignments[i].Name < assignments[j].Name
	})

	poolPeerings := make([]unboundednetv1alpha1.GatewayPoolPeering, 0, len(poolPeeringInformer.GetStore().List()))
	for _, item := range poolPeeringInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		peering, err := parseGatewayPoolPeering(unstr)
		if err != nil {
			klog.Warningf("Failed to parse GatewayPoolPeering: %v", err)
			continue
		}

		if !unboundednetv1alpha1.SpecEnabled(peering.Spec.Enabled) {
			continue
		}

		poolPeerings = append(poolPeerings, *peering)
	}

	sort.Slice(poolPeerings, func(i, j int) bool {
		return poolPeerings[i].Name < poolPeerings[j].Name
	})

	// Parse all site peerings and build a set of sites peered with our site
	peeredSites := make(map[string]bool)
	networkPeeredSites := make(map[string]bool) // all peered sites regardless of meshNodes
	allowedGatewayPools := make(map[string]bool)

	if mySiteName != "" {
		// Include our own site for local/same-site handling, independent of Peering CRs.
		peeredSites[mySiteName] = true
		networkPeeredSites[mySiteName] = true
	}

	for _, peering := range sitePeerings {
		// Check if our site is in this peering (skip if we have no site)
		mySiteInPeering := false

		if mySiteName != "" {
			for _, siteName := range peering.Spec.Sites {
				if siteName == mySiteName {
					mySiteInPeering = true
					break
				}
			}
		}

		// If our site is in this peering, add all other sites
		if mySiteInPeering {
			remoteSites := make([]string, 0, len(peering.Spec.Sites))
			peeringHealthCheckProfileName := ""

			peeringScope := healthCheckLogScope(sitePeeringGVR, peering.Name)
			if enabled, profile := healthCheckProfileFromSettings(peering.Spec.HealthCheckSettings, peeringScope); enabled {
				peeringHealthCheckProfileName = healthCheckProfileNameForSitePeering(peering.Name)
				healthCheckProfiles[peeringHealthCheckProfileName] = profile
				healthCheckProfileSources[peeringHealthCheckProfileName] = peeringScope
			}

			for _, siteName := range peering.Spec.Sites {
				if siteName == mySiteName {
					continue
				}
				// Always track network-peered sites so gateway pool peerings
				// between pools in peered sites can use internalIPs.
				networkPeeredSites[siteName] = true

				// Only add to mesh peeredSites when meshNodes is true (default).
				meshNodes := true
				if peering.Spec.MeshNodes != nil {
					meshNodes = *peering.Spec.MeshNodes
				}

				if meshNodes {
					peeredSites[siteName] = true
				}

				if peeringHealthCheckProfileName != "" {
					if existing, ok := peeringSiteHealthCheckProfileNames[siteName]; ok && existing != peeringHealthCheckProfileName {
						keptFrom := peeringSiteHealthCheckSourcePeering[siteName]
						klog.Warningf("Conflicting health check settings for peered site %q: keeping profile %q from SitePeering %q; ignoring profile %q from SitePeering %q", siteName, existing, keptFrom, peeringHealthCheckProfileName, peering.Name)
					} else {
						peeringSiteHealthCheckProfileNames[siteName] = peeringHealthCheckProfileName
						peeringSiteHealthCheckSourcePeering[siteName] = peering.Name
					}
				}
				// Collect peering-level tunnelMTU override for each remote site.
				if v := tunnelMTUFromSpec(peering.Spec.TunnelMTU); v > 0 {
					if _, exists := peeringSiteTunnelMTUs[siteName]; !exists {
						peeringSiteTunnelMTUs[siteName] = v
					}
				}
				// Collect peering-level tunnelProtocol override for each remote site.
				if peering.Spec.TunnelProtocol != nil {
					if _, exists := peeringSiteTunnelProtocols[siteName]; !exists {
						peeringSiteTunnelProtocols[siteName] = string(*peering.Spec.TunnelProtocol)
					}
				}

				remoteSites = append(remoteSites, siteName)
			}

			if len(remoteSites) > 0 {
				klog.V(4).Infof("Site %s is directly peered with remote sites: %v (via SitePeering %s)", mySiteName, remoteSites, peering.Name)
			}
		}
	}

	for _, assignment := range assignments {
		mySiteAssigned := false

		for _, siteName := range assignment.Spec.Sites {
			if siteName == mySiteName {
				mySiteAssigned = true
				break
			}
		}

		if !mySiteAssigned {
			continue
		}

		for _, poolName := range assignment.Spec.GatewayPools {
			if poolName == "" {
				continue
			}

			allowedGatewayPools[poolName] = true
		}

		mergeAssignmentHealthCheckState(
			assignment,
			mySiteName,
			nil,
			healthCheckProfiles,
			healthCheckProfileSources,
			assignmentPoolHealthCheckProfileNames,
			assignmentPoolHealthCheckSourceAssignment,
			assignmentSiteHealthCheckProfileNames,
			assignmentSiteHealthCheckSourceAssignment,
		)

		// Collect assignment-level tunnelMTU overrides per site and per pool.
		if v := tunnelMTUFromSpec(assignment.Spec.TunnelMTU); v > 0 {
			for _, siteName := range assignment.Spec.Sites {
				siteName = strings.TrimSpace(siteName)
				if siteName == "" || siteName == mySiteName {
					continue
				}

				if _, exists := assignmentSiteTunnelMTUs[siteName]; !exists {
					assignmentSiteTunnelMTUs[siteName] = v
				}
			}

			for _, poolName := range assignment.Spec.GatewayPools {
				poolName = strings.TrimSpace(poolName)
				if poolName == "" {
					continue
				}

				if _, exists := assignmentPoolTunnelMTUs[poolName]; !exists {
					assignmentPoolTunnelMTUs[poolName] = v
				}
			}
		}

		// Collect assignment-level tunnelProtocol overrides per site and per pool.
		if assignment.Spec.TunnelProtocol != nil {
			tunnelProtoStr := string(*assignment.Spec.TunnelProtocol)
			for _, siteName := range assignment.Spec.Sites {
				siteName = strings.TrimSpace(siteName)
				if siteName == "" || siteName == mySiteName {
					continue
				}

				if _, exists := assignmentSiteTunnelProtocols[siteName]; !exists {
					assignmentSiteTunnelProtocols[siteName] = tunnelProtoStr
				}
			}
			// Key by siteName|poolName to match the lookup in resolveTunnelProtocolsOnPeers.
			for _, siteName := range assignment.Spec.Sites {
				siteName = strings.TrimSpace(siteName)
				if siteName == "" {
					continue
				}

				for _, poolName := range assignment.Spec.GatewayPools {
					poolName = strings.TrimSpace(poolName)
					if poolName == "" {
						continue
					}

					compositeKey := siteName + "|" + poolName
					if _, exists := assignmentPoolTunnelProtocols[compositeKey]; !exists {
						assignmentPoolTunnelProtocols[compositeKey] = tunnelProtoStr
					}
				}
			}
		}
	}

	// Build a map of gateway pool names to their specs for quick lookup.
	// This map is NOT filtered by allowedGatewayPools so that gateway nodes
	// can discover peered pools via pool-to-pool peerings (e.g. Peering B
	// connecting site1-gwpool to site2-gwpool without listing any sites).
	// The allowedGatewayPools filter is applied downstream when selecting
	// poolsToIterate for non-gateway nodes.
	gatewayPoolMap := make(map[string]*unboundednetv1alpha1.GatewayPool)

	for _, item := range gatewayPoolInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		pool, err := parseGatewayPool(unstr)
		if err != nil {
			klog.Warningf("Failed to parse GatewayPool: %v", err)
			continue
		}

		gatewayPoolMap[pool.Name] = pool

		poolScope := healthCheckLogScope(gatewayPoolGVR, pool.Name)
		if enabled, profile := healthCheckProfileFromSettings(pool.Spec.HealthCheckSettings, poolScope); enabled {
			profileName := healthCheckProfileNameForGatewayPool(pool.Name)
			if existing, ok := healthCheckProfiles[profileName]; ok && !healthCheckProfilesEqual(existing, profile) {
				klog.Warningf("Conflicting health check settings for gateway pool %q; keeping first profile values", pool.Name)
			} else {
				healthCheckProfiles[profileName] = profile
				poolHealthCheckProfileNames[pool.Name] = profileName
				healthCheckProfileSources[profileName] = poolScope
			}
		}
		// Collect pool-level tunnelMTU override.
		if v := tunnelMTUFromSpec(pool.Spec.TunnelMTU); v > 0 {
			poolTunnelMTUs[pool.Name] = v
		}
		// Collect pool-level tunnelProtocol override.
		if pool.Spec.TunnelProtocol != nil {
			poolTunnelProtocols[pool.Name] = string(*pool.Spec.TunnelProtocol)
		}
	}

	gatewayNodeMap := make(map[string]*unboundednetv1alpha1.GatewayPoolNode)

	for _, item := range gatewayNodeInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		gatewayNode, err := parseGatewayNode(unstr)
		if err != nil {
			klog.Warningf("Failed to parse GatewayNode: %v", err)
			continue
		}

		if gatewayNode.Name == "" {
			continue
		}

		gatewayNodeMap[gatewayNode.Name] = gatewayNode
	}

	// Build set of gateway node public keys from GatewayPool status.
	// This is used to exclude gateway nodes from mesh peers (they use dedicated gateway interfaces).
	gatewayNodePubKeys := make(map[string]bool)

	for _, pool := range gatewayPoolMap {
		for _, gwNode := range pool.Status.Nodes {
			if gwNode.WireGuardPublicKey != "" {
				gatewayNodePubKeys[gwNode.WireGuardPublicKey] = true
			}
		}
	}

	// Check if this node is a gateway node by checking if its public key is in any GatewayPool status.
	// This avoids an API call to fetch the node's labels on every reconcile.
	isGatewayNode := gatewayNodePubKeys[myPubKey]
	if isGatewayNode {
		klog.V(3).Infof("Node public key found in GatewayPool status - this is a gateway node")
	}

	gatewayPoolsForNode := make(map[string]*unboundednetv1alpha1.GatewayPool)

	var myGatewayPort int32

	if isGatewayNode {
		for _, pool := range gatewayPoolMap {
			for _, gwNode := range pool.Status.Nodes {
				if gwNode.WireGuardPublicKey == myPubKey {
					gatewayPoolsForNode[pool.Name] = pool

					if gwNode.GatewayWireguardPort != 0 {
						myGatewayPort = gwNode.GatewayWireguardPort
					}

					break
				}
			}
		}
	}

	connectedSiteSet := make(map[string]bool)

	if isGatewayNode {
		for _, pool := range gatewayPoolsForNode {
			for _, siteName := range pool.Status.ConnectedSites {
				if siteName != "" {
					connectedSiteSet[siteName] = true
				}
			}
		}
	}

	peeredPoolSet := make(map[string]bool)

	if isGatewayNode && len(gatewayPoolsForNode) > 0 {
		for _, peering := range poolPeerings {
			containsPool := false

			for _, poolName := range peering.Spec.GatewayPools {
				if gatewayPoolsForNode[poolName] != nil {
					containsPool = true
					break
				}
			}

			if !containsPool {
				continue
			}

			peeringHealthCheckProfileName := ""

			peeringScope := healthCheckLogScope(gatewayPoolPeeringGVR, peering.Name)
			if enabled, profile := healthCheckProfileFromSettings(peering.Spec.HealthCheckSettings, peeringScope); enabled {
				peeringHealthCheckProfileName = healthCheckProfileNameForGatewayPoolPeering(peering.Name)
				healthCheckProfiles[peeringHealthCheckProfileName] = profile
				healthCheckProfileSources[peeringHealthCheckProfileName] = peeringScope
			}

			for _, poolName := range peering.Spec.GatewayPools {
				if gatewayPoolsForNode[poolName] != nil {
					continue
				}

				if poolName != "" {
					peeredPoolSet[poolName] = true
					if peeringHealthCheckProfileName != "" {
						if existing, ok := poolPeeringHealthCheckProfileNames[poolName]; ok && existing != peeringHealthCheckProfileName {
							keptFrom := poolPeeringHealthCheckSourcePeering[poolName]
							klog.Warningf("Conflicting health check settings for peered gatewayPool %q: keeping profile %q from GatewayPoolPeering %q; ignoring profile %q from GatewayPoolPeering %q", poolName, existing, keptFrom, peeringHealthCheckProfileName, peering.Name)
						} else {
							poolPeeringHealthCheckProfileNames[poolName] = peeringHealthCheckProfileName
							poolPeeringHealthCheckSourcePeering[poolName] = peering.Name
						}
					}
				}
			}
		}
	}

	// If this node is a gateway, start the health server and apply taint (only once each)
	if isGatewayNode {
		state.gatewayTaintApplied.Do(func() {
			klog.Info("This node is a gateway node, applying gateway taint")

			if err := taintGatewayNode(ctx, state.clientset, state.nodeName); err != nil {
				klog.Errorf("Failed to taint gateway node: %v", err)
			}
		})
	}

	localGatewayPools := make([]string, 0, len(gatewayPoolsForNode))
	hasExternalGatewayPool := false

	for poolName := range gatewayPoolsForNode {
		localGatewayPools = append(localGatewayPools, poolName)
		if pool := gatewayPoolsForNode[poolName]; pool != nil && normalizeGatewayPoolType(pool.Spec.Type) == gatewayPoolTypeExternal {
			hasExternalGatewayPool = true
		}
	}

	sort.Strings(localGatewayPools)

	assignedSiteSet := make(map[string]bool)

	localGatewayPoolSet := make(map[string]struct{}, len(localGatewayPools))
	for _, poolName := range localGatewayPools {
		localGatewayPoolSet[poolName] = struct{}{}
	}

	if isGatewayNode && len(localGatewayPools) > 0 {
		for _, assignment := range assignments {
			poolMatch := false

			for _, poolName := range assignment.Spec.GatewayPools {
				if _, ok := localGatewayPoolSet[poolName]; ok {
					poolMatch = true
					break
				}
			}

			if !poolMatch {
				continue
			}

			mergeAssignmentHealthCheckState(
				assignment,
				mySiteName,
				localGatewayPoolSet,
				healthCheckProfiles,
				healthCheckProfileSources,
				assignmentPoolHealthCheckProfileNames,
				assignmentPoolHealthCheckSourceAssignment,
				assignmentSiteHealthCheckProfileNames,
				assignmentSiteHealthCheckSourceAssignment,
			)

			// Collect gateway-node assignment tunnelMTU overrides per site and per pool.
			if v := tunnelMTUFromSpec(assignment.Spec.TunnelMTU); v > 0 {
				for _, siteName := range assignment.Spec.Sites {
					siteName = strings.TrimSpace(siteName)
					if siteName == "" || siteName == mySiteName {
						continue
					}

					if _, exists := assignmentSiteTunnelMTUs[siteName]; !exists {
						assignmentSiteTunnelMTUs[siteName] = v
					}
				}

				for _, poolName := range assignment.Spec.GatewayPools {
					poolName = strings.TrimSpace(poolName)
					if poolName == "" {
						continue
					}

					if _, exists := assignmentPoolTunnelMTUs[poolName]; !exists {
						assignmentPoolTunnelMTUs[poolName] = v
					}
				}
			}

			// Collect gateway-node assignment tunnelProtocol overrides per site and per pool.
			if assignment.Spec.TunnelProtocol != nil {
				tunnelProtoStr := string(*assignment.Spec.TunnelProtocol)
				for _, siteName := range assignment.Spec.Sites {
					siteName = strings.TrimSpace(siteName)
					if siteName == "" || siteName == mySiteName {
						continue
					}

					if _, exists := assignmentSiteTunnelProtocols[siteName]; !exists {
						assignmentSiteTunnelProtocols[siteName] = tunnelProtoStr
					}
				}

				for _, siteName := range assignment.Spec.Sites {
					siteName = strings.TrimSpace(siteName)
					if siteName == "" {
						continue
					}

					for _, poolName := range assignment.Spec.GatewayPools {
						poolName = strings.TrimSpace(poolName)
						if poolName == "" {
							continue
						}

						compositeKey := siteName + "|" + poolName
						if _, exists := assignmentPoolTunnelProtocols[compositeKey]; !exists {
							assignmentPoolTunnelProtocols[compositeKey] = tunnelProtoStr
						}
					}
				}
			}

			for _, siteName := range assignment.Spec.Sites {
				siteName = strings.TrimSpace(siteName)
				if siteName == "" {
					continue
				}

				assignedSiteSet[siteName] = true
			}
		}
	}

	if klog.V(4).Enabled() {
		profileNames := make([]string, 0, len(healthCheckProfiles))
		for profileName := range healthCheckProfiles {
			profileNames = append(profileNames, profileName)
		}

		sort.Strings(profileNames)
		klog.V(4).Infof("Resolved %d health check profile(s) for site %q", len(profileNames), mySiteName)

		for _, profileName := range profileNames {
			profile := healthCheckProfiles[profileName]
			klog.V(4).Infof(
				"Health check profile %q from %s: detectMultiplier=%d receiveInterval=%v transmitInterval=%v",
				profileName,
				healthCheckProfileSources[profileName],
				profile.DetectMultiplier,
				profile.ReceiveInterval,
				profile.TransmitInterval,
			)
		}
	}

	// Collect CIDRs from non-network-peered remote sites (nodeCidrs only; pod CIDRs are per-node and tracked elsewhere)
	// Network-peered sites (including meshNodes=false) should not install nodeCIDR routes via gateways.
	var otherSitesNodeCIDRs []string

	for _, site := range allSites {
		// Skip our own site and any network-peered sites (if we have a site)
		if mySiteName != "" && networkPeeredSites[site.Name] {
			continue
		}

		otherSitesNodeCIDRs = append(otherSitesNodeCIDRs, site.Spec.NodeCidrs...)
	}

	otherSitesNodeCIDRs = dedupeStrings(otherSitesNodeCIDRs)
	shouldLogOtherSitesNodeCIDRs := false

	state.mu.Lock()
	if !reflect.DeepEqual(state.otherSitesNodeCIDRs, otherSitesNodeCIDRs) {
		shouldLogOtherSitesNodeCIDRs = true

		state.otherSitesNodeCIDRs = append([]string(nil), otherSitesNodeCIDRs...)
	}
	state.mu.Unlock()

	if shouldLogOtherSitesNodeCIDRs && len(otherSitesNodeCIDRs) > 0 {
		klog.V(2).Infof("Collected node CIDRs from other sites: %v", otherSitesNodeCIDRs)
	}

	// Get all slices from the informer cache
	items := sliceInformer.GetStore().List()

	// Parse all slices
	var allSlices []unboundednetv1alpha1.SiteNodeSlice

	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		slice, err := parseSiteNodeSlice(unstr)
		if err != nil {
			klog.Warningf("Failed to parse SiteNodeSlice: %v", err)
			continue
		}

		allSlices = append(allSlices, *slice)
	}

	// Collect global pod CIDR pools from gateway pools relevant to this node.
	// These are the supernets that cover individual node podCIDRs.
	// Used for:
	// 1. Intra-site routing on mesh interface (when manageCniPlugin is true)
	// 2. Filtering out redundant routes on gateway nodes (always needed)
	// For non-gateway nodes, only include pools from peerings involving our site.
	// For gateway nodes, include all pools (peered pool selection handles scoping).
	var sitePodCIDRs []string

	for _, pool := range gatewayPoolMap {
		if !isGatewayNode && !allowedGatewayPools[pool.Name] {
			continue
		}

		sitePodCIDRs = append(sitePodCIDRs, pool.Spec.RoutedCidrs...)
	}

	// Collect gateway peers from gateway pools for dedicated gateway interfaces.
	// Non-gateway nodes: connect to pools from peerings involving their site
	// Gateway nodes: connect only to peered pools (other pools in the same Peering)
	var gatewayPeers []gatewayPeerInfo
	{
		var poolsToIterate map[string]*unboundednetv1alpha1.GatewayPool
		if isGatewayNode {
			// Gateway nodes only connect to peered pools (not their own pool)
			poolsToIterate = make(map[string]*unboundednetv1alpha1.GatewayPool, len(peeredPoolSet))
			for poolName := range peeredPoolSet {
				if pool := gatewayPoolMap[poolName]; pool != nil {
					poolsToIterate[poolName] = pool
				}
			}
		} else {
			// Non-gateway nodes: only connect to pools from peerings involving their site
			poolsToIterate = make(map[string]*unboundednetv1alpha1.GatewayPool, len(allowedGatewayPools))
			for poolName := range allowedGatewayPools {
				if pool := gatewayPoolMap[poolName]; pool != nil {
					poolsToIterate[poolName] = pool
				}
			}
		}

		staleAfter := gatewayNodeHeartbeatInterval * 4
		now := time.Now().UTC()

		for _, pool := range poolsToIterate {
			poolType := normalizeGatewayPoolType(pool.Spec.Type)

			// Build routed CIDRs from pool spec and node CIDRs from sites
			// connected/reachable via this pool (not all other sites).
			poolSiteNodeCIDRs := poolReachableNodeCIDRs(pool, siteMap, mySiteName)
			fallbackRoutedCIDRs := buildGatewayPoolRoutedCIDRs(pool, poolSiteNodeCIDRs)

			// Add each gateway node as a peer with the combined CIDRs as allowed IPs
			for _, gwNode := range pool.Status.Nodes {
				// Skip self (match by public key)
				if gwNode.WireGuardPublicKey == myPubKey {
					continue
				}
				// Skip nodes without WireGuard keys
				if gwNode.WireGuardPublicKey == "" {
					klog.V(3).Infof("Skipping gateway peer %s: no pubkey", gwNode.Name)
					continue
				}

				if poolType == "External" && len(gwNode.ExternalIPs) == 0 {
					klog.V(3).Infof("Skipping gateway peer %s: external pool requires external IPs", gwNode.Name)
					continue
				}

				if poolType == "Internal" && len(gwNode.InternalIPs) == 0 && len(gwNode.ExternalIPs) == 0 {
					klog.V(3).Infof("Skipping gateway peer %s: internal pool requires internal or external IPs", gwNode.Name)
					continue
				}

				peerGatewayNode := gatewayNodeMap[gwNode.Name]
				// Never install NodeCidr routes from network-peered sites via gateways.
				excludedNodeCIDRSites := networkPeeredSites
				routedCidrs, routeDistances, learnedRoutes := routedCIDRsForGatewayPeer(peerGatewayNode, fallbackRoutedCIDRs, mySiteName, localGatewayPools, excludedNodeCIDRSites, now, staleAfter)

				gatewayPeers = append(gatewayPeers, gatewayPeerInfo{
					Name:                   gwNode.Name,
					SiteName:               gwNode.SiteName,
					PoolName:               pool.Name,
					HealthCheckProfileName: poolPeeringHealthCheckProfileNames[pool.Name],
					PoolType:               poolType,
					WireGuardPublicKey:     gwNode.WireGuardPublicKey,
					GatewayWireguardPort:   gwNode.GatewayWireguardPort,
					InternalIPs:            gwNode.InternalIPs,
					ExternalIPs:            gwNode.ExternalIPs,
					HealthEndpoints:        gwNode.HealthEndpoints,
					RoutedCidrs:            routedCidrs,
					RouteDistances:         routeDistances,
					LearnedRoutes:          learnedRoutes,
					PodCIDRs:               gwNode.PodCIDRs,
					SkipPodCIDRRoutes:      !manageCniPlugin && !isGatewayNode && gwNode.SiteName == mySiteName,
				})
			}
		}
	}

	if isGatewayNode && dynamicClient != nil && len(localGatewayPools) > 0 {
		selectedPoolName := localGatewayPools[0]
		selectedPool := gatewayPoolMap[selectedPoolName]
		localRoutes := buildGatewayNodeRoutesForAssignedSitesStatus(selectedPoolName, selectedPool, assignments, siteMap)
		routes := mergeGatewayNodeAdvertisedRoutes(localRoutes, gatewayPeers, selectedPoolName)
		klog.V(2).Infof("Gateway route advertisement: pool=%s, %d local + %d merged routes, %d assignments",
			selectedPoolName, len(localRoutes), len(routes), len(assignments))

		if err := syncGatewayNodeRoutesStatus(ctx, dynamicClient, gatewayNodeInformer, cfg.NodeName, routes); err != nil {
			klog.Warningf("Failed to publish GatewayNode routes for node %s: %v", cfg.NodeName, err)
		}
	} else if isGatewayNode {
		klog.V(2).Infof("Gateway route advertisement skipped: dynamicClient=%v, localGatewayPools=%v",
			dynamicClient != nil, localGatewayPools)
	}

	// Collect all mesh peers from allSlices using a unified loop.
	// For non-gateway nodes: peers from peered sites (intra-site + directly peered)
	// For gateway nodes: peers from peered sites + connected sites (remote peers)
	//   plus same-pool gateway nodes (for gateway-to-gateway connectivity)
	// SiteName on each peer drives endpoint and routing decisions in configureWireGuard.
	var peers []meshPeerInfo

	for _, slice := range allSlices {
		// Non-gateway: only peered sites (including own site)
		if !isGatewayNode {
			if !peeredSites[slice.SiteName] {
				continue
			}
		} else {
			// Gateway: include only sites explicitly assigned to this gateway pool,
			// and only when the site is local or directly connected.
			if !shouldIncludeSliceForGatewayNode(slice.SiteName, mySiteName, connectedSiteSet, assignedSiteSet, hasExternalGatewayPool) {
				klog.V(3).Infof("Ignoring %d node(s) from site %s for gateway mesh peering: site is not assigned or not eligible for this gateway pool", len(slice.Nodes), slice.SiteName)
				continue
			}
		}
		// When manageCniPlugin is false, skip same-site peers for non-gateway nodes.
		// Keep directly peered remote sites so unmanaged sites can still route to managed remote pods.
		if !manageCniPlugin && !isGatewayNode && slice.SiteName == mySiteName {
			continue
		}

		for _, node := range slice.Nodes {
			// Skip self
			if node.WireGuardPublicKey == myPubKey {
				continue
			}
			// Skip gateway nodes on mesh interface -- they get dedicated gateway interfaces
			if gatewayNodePubKeys[node.WireGuardPublicKey] {
				klog.V(3).Infof("Skipping peer %s on mesh interface: gateway node (has dedicated gateway interface)", node.Name)
				continue
			}
			// Skip nodes without WireGuard keys or internal IPs
			if node.WireGuardPublicKey == "" || len(node.InternalIPs) == 0 {
				klog.V(3).Infof("Skipping peer %s: no pubkey or IPs", node.Name)
				continue
			}
			// Skip nodes without podCIDRs (not yet allocated)
			if len(node.PodCIDRs) == 0 {
				klog.V(3).Infof("Skipping peer %s: no podCIDRs assigned yet", node.Name)
				continue
			}

			peers = append(peers, meshPeerInfo{
				Name:               node.Name,
				SiteName:           slice.SiteName,
				WireGuardPublicKey: node.WireGuardPublicKey,
				InternalIPs:        node.InternalIPs,
				PodCIDRs:           node.PodCIDRs,
				SkipPodCIDRRoutes:  !manageCniPlugin && isGatewayNode && slice.SiteName == mySiteName,
			})
		}
	}

	// For gateway nodes: also add same-pool gateway nodes as mesh peers.
	// This enables gateway-to-gateway communication for pods on gateway nodes.
	if isGatewayNode {
		addedGwKeys := make(map[string]bool)

		for poolName, pool := range gatewayPoolsForNode {
			for _, gwNode := range pool.Status.Nodes {
				if gwNode.WireGuardPublicKey == myPubKey || gwNode.WireGuardPublicKey == "" {
					continue
				}

				if addedGwKeys[gwNode.WireGuardPublicKey] {
					continue
				}

				if len(gwNode.PodCIDRs) == 0 {
					klog.V(3).Infof("Skipping same-pool gateway peer %s: no podCIDRs", gwNode.Name)
					continue
				}

				addedGwKeys[gwNode.WireGuardPublicKey] = true
				peers = append(peers, meshPeerInfo{
					Name:                   gwNode.Name,
					SiteName:               gwNode.SiteName,
					HealthCheckProfileName: poolHealthCheckProfileNames[poolName],
					WireGuardPublicKey:     gwNode.WireGuardPublicKey,
					InternalIPs:            gwNode.InternalIPs,
					PodCIDRs:               gwNode.PodCIDRs,
				})
				klog.V(4).Infof("Gateway node (same pool %s): added gateway peer %s on mesh interface, podCIDRs %v", poolName, gwNode.Name, gwNode.PodCIDRs)
			}
		}
	}

	if !manageCniPlugin && !isGatewayNode {
		klog.V(2).Info("manageCniPlugin is false - skipping same-site tunnel peers for non-gateway nodes")
	}

	var (
		nonMasqCIDRs     []string
		sitePodCIDRPools []string
	)

	if mySiteName != "" {
		if site, ok := siteMap[mySiteName]; ok {
			nonMasqCIDRs = site.Spec.NonMasqueradeCIDRs
			for _, assignment := range site.Spec.PodCidrAssignments {
				sitePodCIDRPools = append(sitePodCIDRPools, assignment.CidrBlocks...)
			}
		}
	}

	nonMasqCIDRs = dedupeStrings(nonMasqCIDRs)
	sitePodCIDRPools = dedupeStrings(sitePodCIDRPools)

	// Phase 1: Read state and check for changes (under lock).
	// Keep the lock scope narrow -- only hold it for the fast differential
	// comparison and role-context update, then release before expensive
	// netlink operations so getNodeStatus() is not blocked.
	state.mu.Lock()

	prevIsGatewayNode := state.isGatewayNode
	prevMyGatewayPort := state.myGatewayPort
	prevLocalGatewayPools := append([]string(nil), state.localGatewayPools...)
	roleChanged := prevIsGatewayNode != isGatewayNode ||
		prevMyGatewayPort != myGatewayPort ||
		!strSliceEqual(prevLocalGatewayPools, localGatewayPools)

	// Check if tunnel configuration needs to change
	wgChanged := roleChanged ||
		!meshPeersEqual(state.peers, peers) ||
		!strSliceEqual(state.sitePodCIDRs, sitePodCIDRs) ||
		!gatewayPeersEqual(state.gatewayPeers, gatewayPeers) ||
		!healthCheckProfileMapEqual(state.healthCheckProfiles, healthCheckProfiles) ||
		!stringMapEqual(state.siteHealthCheckProfileNames, siteHealthCheckProfileNames) ||
		!stringMapEqual(state.peeringSiteHealthCheckProfiles, peeringSiteHealthCheckProfileNames) ||
		!stringMapEqual(state.assignmentSiteHealthCheckNames, assignmentSiteHealthCheckProfileNames) ||
		!stringMapEqual(state.assignmentPoolHealthCheckNames, assignmentPoolHealthCheckProfileNames) ||
		!stringMapEqual(state.poolHealthCheckProfileNames, poolHealthCheckProfileNames) ||
		!intMapEqual(state.siteTunnelMTUs, siteTunnelMTUs) ||
		!intMapEqual(state.peeringSiteTunnelMTUs, peeringSiteTunnelMTUs) ||
		!intMapEqual(state.assignmentSiteTunnelMTUs, assignmentSiteTunnelMTUs) ||
		!intMapEqual(state.assignmentPoolTunnelMTUs, assignmentPoolTunnelMTUs) ||
		!intMapEqual(state.poolTunnelMTUs, poolTunnelMTUs) ||
		!stringMapEqual(state.siteTunnelProtocols, siteTunnelProtocols) ||
		!stringMapEqual(state.peeringSiteTunnelProtocols, peeringSiteTunnelProtocols) ||
		!stringMapEqual(state.assignmentSiteTunnelProtocols, assignmentSiteTunnelProtocols) ||
		!stringMapEqual(state.assignmentPoolTunnelProtocols, assignmentPoolTunnelProtocols) ||
		!stringMapEqual(state.poolTunnelProtocols, poolTunnelProtocols)

	if !wgChanged {
		klog.V(3).Info("State unchanged, skipping tunnel update")
		state.mu.Unlock()
		syncMasqueradeRules(state, nonMasqCIDRs)

		return nil
	}

	// Log added and removed peers
	logMeshPeerChanges(state.peers, peers)
	logGatewayPeerChanges(state.gatewayPeers, gatewayPeers)

	if roleChanged {
		klog.V(3).Infof(
			"Gateway role context changed: isGateway %t -> %t, gatewayPort %d -> %d, localPools %v -> %v",
			prevIsGatewayNode, isGatewayNode, prevMyGatewayPort, myGatewayPort, prevLocalGatewayPools, localGatewayPools,
		)
	}

	// Update role context before configureWireGuard so route programming uses current role.
	state.isGatewayNode = isGatewayNode
	state.myGatewayPort = myGatewayPort

	state.localGatewayPools = append([]string(nil), localGatewayPools...)
	state.sitePodCIDRPools = append([]string(nil), sitePodCIDRPools...)
	state.sitePodCIDRs = append([]string(nil), sitePodCIDRs...)

	state.mu.Unlock()

	// Phase 2: Expensive network operations (lock-free).
	// The tunnel/WG config functions access state.geneveInterfaces,
	// state.vxlanManager, state.wireguardManager, state.routeManager, etc.
	// These are only written by this reconciliation goroutine (single writer),
	// and getNodeStatus() does not read them, so lock-free access is safe.

	// Resolve tunnel protocol on each peer, then split by tunnel protocol.
	// Only split if GENEVE is actually configured (non-empty interface name);
	// otherwise all peers flow through WireGuard.
	resolveTunnelProtocolsOnPeers(peers, gatewayPeers, mySiteName, peeredSites, networkPeeredSites,
		isGatewayNode, hasExternalGatewayPool, localGatewayPools,
		cfg.PreferredPrivateEncap, cfg.PreferredPublicEncap,
		siteTunnelProtocols, peeringSiteTunnelProtocols,
		assignmentPoolTunnelProtocols, poolTunnelProtocols)

	var (
		wgMeshPeers    []meshPeerInfo
		wgGatewayPeers []gatewayPeerInfo
		tunnelRoutes   []unboundednetnetlink.DesiredRoute
		tunnelHCPeers  map[string]bool
	)

	// Swap health check enabled maps with fresh instances under the lock.
	// The status server may be iterating the old maps under state.mu;
	// replacing the pointers here ensures the old maps have no concurrent
	// writer, and this (the only reconciliation) goroutine exclusively
	// owns the new maps until Phase 3.

	state.mu.Lock()
	state.meshPeerHealthCheckEnabled = make(map[string]bool)
	state.gatewayPeerHealthCheckEnabled = make(map[string]bool)
	state.mu.Unlock()

	geneveEnabled := cfg.GeneveInterfaceName != ""
	if geneveEnabled {
		// Reset pending BPF entries for this reconcile cycle.
		if cfg.TunnelDataplane == "ebpf" {
			state.mu.Lock()
			state.pendingBPFEntries = nil
			state.allGatewayPeersForSupernets = gatewayPeers
			state.mu.Unlock()
		}

		var (
			tunnelMeshPeers    []meshPeerInfo
			tunnelGatewayPeers []gatewayPeerInfo
			vxlanMeshPeers     []meshPeerInfo
			vxlanGatewayPeers  []gatewayPeerInfo
		)

		wgMeshPeers, wgGatewayPeers, tunnelMeshPeers, tunnelGatewayPeers, vxlanMeshPeers, vxlanGatewayPeers = filterPeersByTunnelProtocol(peers, gatewayPeers)

		// Configure per-peer tunnel (GENEVE/IPIP/None) peers.
		var tunnelErr error

		tunnelRoutes, tunnelHCPeers, tunnelErr = configureTunnelPeers(ctx, cfg, tunnelMeshPeers, tunnelGatewayPeers,
			mySiteName, peeredSites, networkPeeredSites,
			siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
			assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
			poolHealthCheckProfileNames,
			siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs,
			assignmentPoolTunnelMTUs, poolTunnelMTUs, state)
		if tunnelErr != nil {
			klog.Warningf("GENEVE configuration failed (WireGuard will still be configured): %v", tunnelErr)
		}

		// Configure VXLAN peers (single shared interface with route-based encap).
		if len(vxlanMeshPeers) > 0 || len(vxlanGatewayPeers) > 0 {
			vxlanRoutes, vxlanHCPeers, vxlanErr := configureVXLANPeers(ctx, cfg, vxlanMeshPeers, vxlanGatewayPeers,
				mySiteName, peeredSites,
				siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames,
				assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames,
				poolHealthCheckProfileNames,
				siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs,
				assignmentPoolTunnelMTUs, poolTunnelMTUs, state)
			if vxlanErr != nil {
				klog.Warningf("VXLAN configuration failed: %v", vxlanErr)
			} else {
				tunnelRoutes = append(tunnelRoutes, vxlanRoutes...)

				if tunnelHCPeers == nil {
					tunnelHCPeers = vxlanHCPeers
				} else {
					for k, v := range vxlanHCPeers {
						tunnelHCPeers[k] = v
					}
				}
			}
		}
	} else {
		wgMeshPeers = peers
		wgGatewayPeers = gatewayPeers
	}

	// Configure WireGuard with WG peers, merging tunnel routes into
	// the unified route manager's SyncRoutes call.
	if err := configureWireGuardFunc(ctx, cfg, privKey, wgMeshPeers, wgGatewayPeers, mySiteName, peeredSites, networkPeeredSites, gatewayNodePubKeys, siteHealthCheckProfileNames, peeringSiteHealthCheckProfileNames, assignmentSiteHealthCheckProfileNames, assignmentPoolHealthCheckProfileNames, poolHealthCheckProfileNames, siteTunnelMTUs, peeringSiteTunnelMTUs, assignmentSiteTunnelMTUs, assignmentPoolTunnelMTUs, poolTunnelMTUs, tunnelRoutes, tunnelHCPeers, state); err != nil {
		// Restore previous role context on failure so state remains coherent.
		state.mu.Lock()
		state.isGatewayNode = prevIsGatewayNode
		state.myGatewayPort = prevMyGatewayPort
		state.localGatewayPools = prevLocalGatewayPools
		state.mu.Unlock()

		return err
	}

	// When using eBPF dataplane, add WireGuard peer CIDRs to the BPF map
	// so that unbounded0 routes are redirected to the WG interface.
	// WG peers get redirect-only (no set_tunnel_key -- WG handles its own encap).
	if cfg.TunnelDataplane == "ebpf" {
		addWireGuardPeersToBPFMap(cfg, state, wgMeshPeers, wgGatewayPeers)
		// Final reconcile: merge all pending entries (GENEVE + VXLAN + WG)
		// into a single BPF map update. This avoids per-protocol Reconcile
		// calls overwriting each other's entries.
		reconcilePendingBPFEntries(state)
		// Remove tunnel devices that no peers are using. This avoids leaving
		// stale interfaces (geneve0, vxlan0, ipip0) on nodes where the
		// tunnel protocol has been changed.
		cleanupUnusedTunnelDevices(peers, gatewayPeers, wgGatewayPeers)
		// Re-apply rp_filter=0 on active tunnel interfaces. Deleting
		// interfaces during cleanup can cause the kernel to reset
		// rp_filter on remaining interfaces.
		reapplyRPFilterOnActiveTunnels(peers, gatewayPeers, wgGatewayPeers)
	}

	// Phase 3: Update state fields that getNodeStatus() reads (brief lock).
	state.mu.Lock()
	state.peers = peers
	state.gatewayPeers = gatewayPeers
	state.sitePodCIDRs = sitePodCIDRs
	state.sitePodCIDRPools = sitePodCIDRPools
	state.healthCheckProfiles = copyHealthCheckProfileMap(healthCheckProfiles)
	state.siteHealthCheckProfileNames = copyStringMap(siteHealthCheckProfileNames)
	state.peeringSiteHealthCheckProfiles = copyStringMap(peeringSiteHealthCheckProfileNames)
	state.assignmentSiteHealthCheckNames = copyStringMap(assignmentSiteHealthCheckProfileNames)
	state.assignmentPoolHealthCheckNames = copyStringMap(assignmentPoolHealthCheckProfileNames)
	state.poolHealthCheckProfileNames = copyStringMap(poolHealthCheckProfileNames)
	state.siteTunnelMTUs = copyStringIntMap(siteTunnelMTUs)
	state.peeringSiteTunnelMTUs = copyStringIntMap(peeringSiteTunnelMTUs)
	state.assignmentSiteTunnelMTUs = copyStringIntMap(assignmentSiteTunnelMTUs)
	state.assignmentPoolTunnelMTUs = copyStringIntMap(assignmentPoolTunnelMTUs)
	state.poolTunnelMTUs = copyStringIntMap(poolTunnelMTUs)
	state.siteTunnelProtocols = copyStringMap(siteTunnelProtocols)
	state.peeringSiteTunnelProtocols = copyStringMap(peeringSiteTunnelProtocols)
	state.assignmentSiteTunnelProtocols = copyStringMap(assignmentSiteTunnelProtocols)
	state.assignmentPoolTunnelProtocols = copyStringMap(assignmentPoolTunnelProtocols)
	state.poolTunnelProtocols = copyStringMap(poolTunnelProtocols)
	state.reconcileCount++
	state.mu.Unlock()

	syncMasqueradeRules(state, nonMasqCIDRs)

	return nil
}

func syncMasqueradeRules(state *wireGuardState, nonMasqCIDRs []string) {
	if state.masqueradeManager == nil {
		return
	}

	// Detect default gateway interfaces for IPv4 and IPv6.
	defaultIfaceV4 := unboundednetnetlink.DetectDefaultGateway(netlink.FAMILY_V4)
	defaultIfaceV6 := unboundednetnetlink.DetectDefaultGateway(netlink.FAMILY_V6)

	if defaultIfaceV4 == "" && defaultIfaceV6 == "" {
		klog.Warning("No default gateway found - masquerade rules will not be applied")
		return
	}

	// Sync masquerade rules
	// - Local destinations (--dst-type LOCAL) are automatically skipped
	// - nonMasqCIDRs: user-configured CIDRs that should not be masqueraded
	// - Only traffic leaving via default gateway interface is masqueraded
	if err := state.masqueradeManager.SyncRules(defaultIfaceV4, defaultIfaceV6, nonMasqCIDRs); err != nil {
		klog.Errorf("Failed to sync masquerade rules: %v", err)
	} else {
		klog.V(4).Infof("Synced masquerade rules for default gateway (v4: %s, v6: %s)", defaultIfaceV4, defaultIfaceV6)
	}
}

// logMeshPeerChanges logs which mesh peers were added or removed
func logMeshPeerChanges(oldPeers, newPeers []meshPeerInfo) {
	// Build maps for quick lookup
	oldMap := make(map[string]struct{})
	for _, p := range oldPeers {
		oldMap[p.Name] = struct{}{}
	}

	newMap := make(map[string]struct{})
	for _, p := range newPeers {
		newMap[p.Name] = struct{}{}
	}

	// Find removed peers
	for _, p := range oldPeers {
		if _, exists := newMap[p.Name]; !exists {
			if p.TunnelProtocol != "" {
				klog.Infof("Mesh peer removed: %s (protocol: %s)", p.Name, p.TunnelProtocol)
			} else {
				klog.Infof("Mesh peer removed: %s", p.Name)
			}
		}
	}

	// Find added peers
	for _, p := range newPeers {
		if _, exists := oldMap[p.Name]; !exists {
			if p.TunnelProtocol != "" {
				klog.Infof("Mesh peer added: %s (site: %s, protocol: %s)", p.Name, p.SiteName, p.TunnelProtocol)
			} else {
				klog.Infof("Mesh peer added: %s (site: %s)", p.Name, p.SiteName)
			}
		}
	}
}

// logGatewayPeerChanges logs which gateway peers were added or removed
func logGatewayPeerChanges(oldPeers, newPeers []gatewayPeerInfo) {
	// Build maps for quick lookup
	oldMap := make(map[string]struct{})
	for _, p := range oldPeers {
		oldMap[p.Name] = struct{}{}
	}

	newMap := make(map[string]struct{})
	for _, p := range newPeers {
		newMap[p.Name] = struct{}{}
	}

	// Find removed peers
	for _, p := range oldPeers {
		if _, exists := newMap[p.Name]; !exists {
			if p.TunnelProtocol != "" {
				klog.Infof("Gateway peer removed: %s (protocol: %s)", p.Name, p.TunnelProtocol)
			} else {
				klog.Infof("Gateway peer removed: %s", p.Name)
			}
		}
	}

	// Find added peers
	for _, p := range newPeers {
		if _, exists := oldMap[p.Name]; !exists {
			if p.TunnelProtocol != "" {
				klog.Infof("Gateway peer added: %s (routing %d CIDRs, protocol: %s)", p.Name, len(p.RoutedCidrs), p.TunnelProtocol)
			} else {
				klog.Infof("Gateway peer added: %s (routing %d CIDRs)", p.Name, len(p.RoutedCidrs))
			}
		}
	}
}

func pruneCoveredAllowedCIDRs(values []string) []string {
	type parsedCIDR struct {
		normalized string
		network    *net.IPNet
		ones       int
		bits       int
	}

	parsed := make([]parsedCIDR, 0, len(values))
	invalid := make(map[string]struct{})

	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}

		normalized := normalizeCIDR(trimmed)
		if normalized == "" {
			invalid[trimmed] = struct{}{}
			continue
		}

		_, network, err := net.ParseCIDR(normalized)
		if err != nil || network == nil {
			invalid[trimmed] = struct{}{}
			continue
		}

		ones, bits := network.Mask.Size()
		parsed = append(parsed, parsedCIDR{
			normalized: normalized,
			network:    network,
			ones:       ones,
			bits:       bits,
		})
	}

	keep := make(map[string]struct{})

	for i := range parsed {
		entry := parsed[i]
		covered := false

		for j := range parsed {
			if i == j {
				continue
			}

			candidate := parsed[j]
			if entry.bits != candidate.bits {
				continue
			}

			if candidate.ones >= entry.ones {
				continue
			}

			if !candidate.network.Contains(entry.network.IP) {
				continue
			}

			covered = true

			break
		}

		if !covered {
			keep[entry.normalized] = struct{}{}
		}
	}

	out := setToSortedStrings(keep)
	if len(invalid) > 0 {
		out = append(out, setToSortedStrings(invalid)...)
	}

	return out
}

func setToSortedStrings(set map[string]struct{}) []string {
	if len(set) == 0 {
		return []string{}
	}

	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}

	sort.Strings(values)

	return values
}
