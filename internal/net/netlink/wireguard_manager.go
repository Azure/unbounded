// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/klog/v2"
)

// WireGuardPeer represents a WireGuard peer configuration
type WireGuardPeer struct {
	PublicKey           string
	Endpoint            string // host:port format
	AllowedIPs          []string
	PersistentKeepalive int // seconds, 0 to disable
}

// WireGuardManager manages WireGuard device configuration using wgctrl
type WireGuardManager struct {
	mu        sync.Mutex
	ifaceName string
	client    WireGuardClient
	desired   *wireGuardDesired
}

// WireGuardClient is the wgctrl operation set used by WireGuardManager.
type WireGuardClient interface {
	Device(string) (*wgtypes.Device, error)
	ConfigureDevice(string, wgtypes.Config) error
	Close() error
}

type wireGuardDesired struct {
	privateKey wgtypes.Key
	listenPort int
	peers      map[wgtypes.Key]WireGuardPeer
}

// NewWireGuardManager creates a new WireGuardManager for the specified interface
func NewWireGuardManager(ifaceName string) (*WireGuardManager, error) {
	client, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create wgctrl client: %w", err)
	}

	return NewWireGuardManagerWithClient(ifaceName, client), nil
}

// NewWireGuardManagerWithClient creates a manager backed by client. It is
// exported for component tests that need to model kernel WireGuard state.
func NewWireGuardManagerWithClient(ifaceName string, client WireGuardClient) *WireGuardManager {
	return &WireGuardManager{ifaceName: ifaceName, client: client}
}

// Close releases resources held by the WireGuardManager
func (wm *WireGuardManager) Close() error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	if wm.client != nil {
		return wm.client.Close()
	}

	return nil
}

// Configure applies the WireGuard configuration to the interface using delta updates.
// It compares the current peer configuration with the desired configuration and only
// makes changes where necessary (add new peers, remove stale peers, update changed peers).
// privateKey is the base64-encoded private key
// listenPort is the UDP port to listen on
// peers is the list of desired peer configurations
func (wm *WireGuardManager) Configure(privateKey string, listenPort int, peers []WireGuardPeer) error {
	start := time.Now()

	wm.mu.Lock()
	defer wm.mu.Unlock()

	// Parse private key
	privKey, err := wgtypes.ParseKey(privateKey)
	if err != nil {
		return fmt.Errorf("failed to parse private key (invalid format)")
	}

	desiredPeers := parseDesiredPeers(peers)

	// Get current device state to calculate delta
	currentDevice, err := wm.client.Device(wm.ifaceName)
	if err != nil {
		// Device might not exist yet or have no config, do a full configure
		klog.V(3).Infof("Could not get current device state for %s, doing full configuration: %v", wm.ifaceName, err)

		if err := wm.configureFullReplace(privKey, listenPort, desiredPeers); err != nil {
			return err
		}

		wm.desired = &wireGuardDesired{privateKey: privKey, listenPort: listenPort, peers: desiredPeers}

		return nil
	}

	if _, err := wm.applyDelta(currentDevice, privKey, listenPort, desiredPeers, start); err != nil {
		return err
	}

	wm.desired = &wireGuardDesired{privateKey: privKey, listenPort: listenPort, peers: desiredPeers}

	return nil
}

func parseDesiredPeers(peers []WireGuardPeer) map[wgtypes.Key]WireGuardPeer {
	desiredPeers := make(map[wgtypes.Key]WireGuardPeer, len(peers))

	for _, peer := range peers {
		pubKey, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			klog.Warningf("Failed to parse public key %s: %v", peer.PublicKey, err)
			continue
		}

		peer.AllowedIPs = append([]string(nil), peer.AllowedIPs...)
		desiredPeers[pubKey] = peer
	}

	return desiredPeers
}

// RepairDrift reconciles the live WireGuard device with the last successfully
// applied configuration. Endpoints with an empty desired value remain learned
// by WireGuard and are not enforced.
func (wm *WireGuardManager) RepairDrift() error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	if wm.desired == nil || wm.client == nil {
		return nil
	}

	currentDevice, err := wm.client.Device(wm.ifaceName)
	if err != nil {
		return fmt.Errorf("read WireGuard device %s: %w", wm.ifaceName, err)
	}

	changed, err := wm.applyDelta(currentDevice, wm.desired.privateKey, wm.desired.listenPort, wm.desired.peers, time.Now())
	if err == nil && changed {
		WireGuardDriftRepairs.WithLabelValues(wm.ifaceName).Inc()
	}

	return err
}

func (wm *WireGuardManager) applyDelta(currentDevice *wgtypes.Device, privKey wgtypes.Key, listenPort int, desiredPeers map[wgtypes.Key]WireGuardPeer, start time.Time) (bool, error) {
	// Build map of current peers by public key
	currentPeers := make(map[wgtypes.Key]wgtypes.Peer)
	for _, p := range currentDevice.Peers {
		currentPeers[p.PublicKey] = p
	}

	// Calculate delta
	var (
		peerConfigs             []wgtypes.PeerConfig
		added, updated, removed int
	)

	// Add or update peers that should exist

	for pubKey, desired := range desiredPeers {
		current, exists := currentPeers[pubKey]
		if !exists {
			// New peer - add it
			peerConfig, err := wm.buildPeerConfig(desired)
			if err != nil {
				klog.Warningf("Failed to build peer config for %s: %v", desired.PublicKey, err)
				continue
			}

			peerConfigs = append(peerConfigs, *peerConfig)
			added++
		} else {
			// Existing peer - check if it needs updating
			if wm.peerNeedsUpdate(current, desired) {
				peerConfig, err := wm.buildPeerConfig(desired)
				if err != nil {
					klog.Warningf("Failed to build peer config for %s: %v", desired.PublicKey, err)
					continue
				}
				// UpdateOnly ensures we only update, not add
				peerConfig.UpdateOnly = true
				peerConfigs = append(peerConfigs, *peerConfig)
				updated++
			}
		}
	}

	// Remove peers that should not exist
	for pubKey := range currentPeers {
		if _, exists := desiredPeers[pubKey]; !exists {
			peerConfigs = append(peerConfigs, wgtypes.PeerConfig{
				PublicKey: pubKey,
				Remove:    true,
			})
			removed++
		}
	}

	deviceChanged := currentDevice.PrivateKey != privKey || currentDevice.ListenPort != listenPort

	// If no changes needed, skip the API call
	if len(peerConfigs) == 0 && !deviceChanged {
		klog.V(3).Infof("WireGuard device %s: no peer changes needed", wm.ifaceName)
		return false, nil
	}

	// Build device config with delta changes (ReplacePeers: false)
	cfg := wgtypes.Config{
		ReplacePeers: false, // Only apply the specific peer changes
		Peers:        peerConfigs,
	}

	if deviceChanged {
		if currentDevice.PrivateKey != privKey {
			cfg.PrivateKey = &privKey
		}

		if currentDevice.ListenPort != listenPort {
			cfg.ListenPort = &listenPort
		}
	}

	// Apply configuration
	if err := wm.client.ConfigureDevice(wm.ifaceName, cfg); err != nil {
		WireGuardConfigureErrors.WithLabelValues(wm.ifaceName).Inc()
		WireGuardConfigureDuration.WithLabelValues(wm.ifaceName).Observe(time.Since(start).Seconds())

		return false, fmt.Errorf("failed to configure WireGuard device: %w", err)
	}

	WireGuardPeers.WithLabelValues(wm.ifaceName).Set(float64(len(desiredPeers)))
	WireGuardPeersAdded.Add(float64(added))
	WireGuardPeersRemoved.Add(float64(removed))
	WireGuardConfigureDuration.WithLabelValues(wm.ifaceName).Observe(time.Since(start).Seconds())

	klog.V(4).Infof("Configured WireGuard device %s with %d peers (added: %d, updated: %d, removed: %d, device changed: %t)",
		wm.ifaceName, len(desiredPeers), added, updated, removed, deviceChanged)

	return true, nil
}

// configureFullReplace does a full peer replacement (used on first configuration)
func (wm *WireGuardManager) configureFullReplace(privKey wgtypes.Key, listenPort int, peers map[wgtypes.Key]WireGuardPeer) error {
	peerConfigs := make([]wgtypes.PeerConfig, 0, len(peers))
	for _, peer := range peers {
		peerConfig, err := wm.buildPeerConfig(peer)
		if err != nil {
			klog.Warningf("Failed to build peer config for %s: %v", peer.PublicKey, err)
			continue
		}

		peerConfigs = append(peerConfigs, *peerConfig)
	}

	cfg := wgtypes.Config{
		PrivateKey:   &privKey,
		ListenPort:   &listenPort,
		ReplacePeers: true,
		Peers:        peerConfigs,
	}

	if err := wm.client.ConfigureDevice(wm.ifaceName, cfg); err != nil {
		return fmt.Errorf("failed to configure WireGuard device: %w", err)
	}

	WireGuardPeers.WithLabelValues(wm.ifaceName).Set(float64(len(peers)))
	klog.V(4).Infof("Configured WireGuard device %s with %d peers (full replace)", wm.ifaceName, len(peerConfigs))

	return nil
}

// peerNeedsUpdate checks if a peer configuration has changed and needs updating
func (wm *WireGuardManager) peerNeedsUpdate(current wgtypes.Peer, desired WireGuardPeer) bool {
	// Check endpoint change (keep current endpoint when desired is empty)
	if desired.Endpoint != "" {
		desiredEndpoint, err := net.ResolveUDPAddr("udp", desired.Endpoint)
		if err == nil {
			if current.Endpoint == nil || !udpAddrsEqual(current.Endpoint, desiredEndpoint) {
				return true
			}
		}
	}

	// Check persistent keepalive change
	desiredKeepalive := time.Duration(desired.PersistentKeepalive) * time.Second
	if current.PersistentKeepaliveInterval != desiredKeepalive {
		return true
	}

	// Check allowed IPs change
	if !wm.allowedIPsEqual(current.AllowedIPs, desired.AllowedIPs) {
		return true
	}

	return false
}

// allowedIPsEqual compares current allowed IPs with desired allowed IPs
func (wm *WireGuardManager) allowedIPsEqual(current []net.IPNet, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}

	// Build set of current allowed IPs as strings
	currentSet := make(map[string]bool)
	for _, ipnet := range current {
		currentSet[ipnet.String()] = true
	}

	// Check all desired IPs are in current set
	for _, cidr := range desired {
		// Normalize the CIDR string
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			addr, err := netip.ParseAddr(cidr)
			if err != nil {
				continue
			}

			if addr.Is4() {
				prefix = netip.PrefixFrom(addr, 32)
			} else {
				prefix = netip.PrefixFrom(addr, 128)
			}
		}

		ipNet := prefixToIPNet(prefix)
		if !currentSet[ipNet.String()] {
			return false
		}
	}

	return true
}

// udpAddrsEqual compares two UDP addresses for equality
func udpAddrsEqual(a, b *net.UDPAddr) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return a.IP.Equal(b.IP) && a.Port == b.Port
}

// buildPeerConfig creates a wgtypes.PeerConfig from a WireGuardPeer
func (wm *WireGuardManager) buildPeerConfig(peer WireGuardPeer) (*wgtypes.PeerConfig, error) {
	// Parse public key
	pubKey, err := wgtypes.ParseKey(peer.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse public key: %w", err)
	}

	// Parse endpoint if provided
	var endpoint *net.UDPAddr
	if peer.Endpoint != "" {
		endpoint, err = net.ResolveUDPAddr("udp", peer.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve endpoint %s: %w", peer.Endpoint, err)
		}
	}

	// Parse allowed IPs
	allowedIPs := make([]net.IPNet, 0, len(peer.AllowedIPs))
	for _, cidr := range peer.AllowedIPs {
		// Try parsing as netip.Prefix first (handles both IP and CIDR)
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			// Try parsing as just an IP address
			addr, err := netip.ParseAddr(cidr)
			if err != nil {
				klog.Warningf("Invalid allowed IP %s: %v", cidr, err)
				continue
			}
			// Convert to /32 or /128 prefix
			if addr.Is4() {
				prefix = netip.PrefixFrom(addr, 32)
			} else {
				prefix = netip.PrefixFrom(addr, 128)
			}
		}

		// Convert netip.Prefix to net.IPNet
		ipNet := prefixToIPNet(prefix)
		allowedIPs = append(allowedIPs, ipNet)
	}

	return &wgtypes.PeerConfig{
		PublicKey:                   pubKey,
		Endpoint:                    endpoint,
		ReplaceAllowedIPs:           true,
		AllowedIPs:                  allowedIPs,
		PersistentKeepaliveInterval: wm.keepaliveDuration(peer.PersistentKeepalive),
	}, nil
}

// keepaliveDuration converts seconds to a *time.Duration for PersistentKeepalive
func (wm *WireGuardManager) keepaliveDuration(seconds int) *time.Duration {
	d := time.Duration(max(seconds, 0)) * time.Second

	return &d
}

// prefixToIPNet converts a netip.Prefix to a net.IPNet
func prefixToIPNet(prefix netip.Prefix) net.IPNet {
	addr := prefix.Addr()
	bits := prefix.Bits()

	var mask net.IPMask
	if addr.Is4() {
		mask = net.CIDRMask(bits, 32)
	} else {
		mask = net.CIDRMask(bits, 128)
	}

	return net.IPNet{
		IP:   addr.AsSlice(),
		Mask: mask,
	}
}

// GetDevice returns the current WireGuard device configuration
func (wm *WireGuardManager) GetDevice() (*wgtypes.Device, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	if wm.client == nil {
		return nil, fmt.Errorf("WireGuard client for %s is unavailable", wm.ifaceName)
	}

	return wm.client.Device(wm.ifaceName)
}

// GetPeers returns the current list of peers configured on the device
func (wm *WireGuardManager) GetPeers() ([]wgtypes.Peer, error) {
	device, err := wm.GetDevice()
	if err != nil {
		return nil, fmt.Errorf("failed to get device: %w", err)
	}

	return device.Peers, nil
}
