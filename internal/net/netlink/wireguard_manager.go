// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"bytes"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
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

// WireGuard drift reasons reported by RepairDrift and used as the reason label
// of the WireGuardDriftRepairs metric.
const (
	WireGuardDriftEndpoint       = "endpoint"
	WireGuardDriftAllowedIPs     = "allowed_ips"
	WireGuardDriftKeepalive      = "keepalive"
	WireGuardDriftMissingPeer    = "missing_peer"
	WireGuardDriftUnexpectedPeer = "unexpected_peer"
	WireGuardDriftListenPort     = "listen_port"
	WireGuardDriftPrivateKey     = "private_key"
)

// WireGuardClient is the subset of *wgctrl.Client that WireGuardManager uses.
type WireGuardClient interface {
	Device(name string) (*wgtypes.Device, error)
	ConfigureDevice(name string, cfg wgtypes.Config) error
	Close() error
}

// WireGuardDrift is one difference between the kernel configuration of a
// WireGuard interface and the configuration the manager last applied.
type WireGuardDrift struct {
	// PublicKey is empty for device-level drift (listen port, private key).
	PublicKey string
	Reason    string
	Current   string
	Desired   string
}

// wireGuardDesiredState is the configuration most recently passed to Configure.
type wireGuardDesiredState struct {
	privateKey wgtypes.Key
	listenPort int
	peers      map[wgtypes.Key]WireGuardPeer
}

// WireGuardManager manages WireGuard device configuration using wgctrl
type WireGuardManager struct {
	mu        sync.Mutex
	ifaceName string
	client    WireGuardClient
	// desired is the configuration RepairDrift enforces. It is recorded even
	// when applying it fails, so the next RepairDrift retries it.
	desired *wireGuardDesiredState
	// endpointDrift holds the drifted endpoint last repaired for each peer, so
	// a peer that keeps roaming away is not logged at the default level on
	// every heartbeat.
	endpointDrift map[wgtypes.Key]endpointDriftRecord
}

// NewWireGuardManager creates a new WireGuardManager for the specified interface
func NewWireGuardManager(ifaceName string) (*WireGuardManager, error) {
	client, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create wgctrl client: %w", err)
	}

	return NewWireGuardManagerWithClient(ifaceName, client), nil
}

// NewWireGuardManagerWithClient creates a WireGuardManager that uses the given
// client, for example a fake in tests.
func NewWireGuardManagerWithClient(ifaceName string, client WireGuardClient) *WireGuardManager {
	return &WireGuardManager{
		ifaceName: ifaceName,
		client:    client,
	}
}

// InterfaceName returns the name of the managed WireGuard interface.
func (wm *WireGuardManager) InterfaceName() string {
	return wm.ifaceName
}

// Close releases resources held by the WireGuardManager
func (wm *WireGuardManager) Close() error {
	if wm.client != nil {
		return wm.client.Close()
	}

	return nil
}

// Configure applies the WireGuard configuration to the interface using delta updates.
// It compares the current peer configuration with the desired configuration and only
// makes changes where necessary (add new peers, remove stale peers, update changed peers).
// When nothing differs it makes no kernel write, so repeated calls are cheap.
// The configuration is recorded for RepairDrift even when applying it fails.
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

	desired := newWireGuardDesiredState(privKey, listenPort, peers)
	wm.desired = desired

	// Get current device state to calculate delta
	currentDevice, err := wm.client.Device(wm.ifaceName)
	if err != nil {
		// Device might not exist yet or have no config, do a full configure
		klog.V(3).Infof("Could not get current device state for %s, doing full configuration: %v", wm.ifaceName, err)
		return wm.configureFullReplace(desired)
	}

	delta := wm.computeDelta(currentDevice, desired)

	// If no changes needed, skip the API call
	if delta.empty() {
		klog.V(3).Infof("WireGuard device %s: no peer changes needed", wm.ifaceName)
		return nil
	}

	if err := wm.applyDelta(desired, delta, start); err != nil {
		return err
	}

	klog.V(4).Infof("Configured WireGuard device %s with %d peers (added: %d, updated: %d, removed: %d)",
		wm.ifaceName, len(desired.peers), delta.added, delta.updated, delta.removed)

	return nil
}

// RepairDrift reads the kernel configuration of the interface once, compares it
// with the configuration last passed to Configure, and re-applies only what
// differs. It returns the differences it repaired and makes no kernel write when
// nothing has drifted. It does nothing before the first Configure or after
// ResetDesired.
//
// Endpoints are enforced only for peers configured with one. A peer configured
// without an endpoint keeps whatever endpoint WireGuard learned by roaming,
// because that is how peers behind NAT are reached.
func (wm *WireGuardManager) RepairDrift() ([]WireGuardDrift, error) {
	start := time.Now()

	wm.mu.Lock()
	defer wm.mu.Unlock()

	if wm.desired == nil || wm.client == nil {
		return nil, nil
	}

	device, err := wm.client.Device(wm.ifaceName)
	if err != nil {
		return nil, fmt.Errorf("read WireGuard device %s: %w", wm.ifaceName, err)
	}

	delta := wm.computeDelta(device, wm.desired)
	if delta.empty() {
		wm.pruneEndpointDrift(nil)
		return nil, nil
	}

	if err := wm.applyDelta(wm.desired, delta, start); err != nil {
		return nil, fmt.Errorf("repair WireGuard drift on %s: %w", wm.ifaceName, err)
	}

	wm.reportDriftRepairs(delta.drifts)

	return delta.drifts, nil
}

// ResetDesired stops RepairDrift from enforcing a configuration, for example
// after the interface was deleted on purpose.
func (wm *WireGuardManager) ResetDesired() {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	wm.desired = nil
	wm.endpointDrift = nil
}

// endpointDriftLogInterval bounds how often the same drifted endpoint of a peer
// is logged at the default level. A peer that keeps roaming away from its
// configured endpoint is repaired on every check, but logged at most this often.
const endpointDriftLogInterval = 10 * time.Minute

type endpointDriftRecord struct {
	endpoint string
	logged   time.Time
}

// reportDriftRepairs logs and counts repaired drift.
func (wm *WireGuardManager) reportDriftRepairs(drifts []WireGuardDrift) {
	now := time.Now()
	endpointDrifted := make(map[wgtypes.Key]bool)

	for _, drift := range drifts {
		WireGuardDriftRepairs.WithLabelValues(wm.ifaceName, drift.Reason).Inc()

		subject := "device"
		if drift.PublicKey != "" {
			subject = "peer " + shortWireGuardKey(drift.PublicKey)
		}

		if drift.Reason != WireGuardDriftEndpoint {
			klog.Infof("Repaired WireGuard drift on %s: %s %s %s -> %s",
				wm.ifaceName, subject, drift.Reason, drift.Current, drift.Desired)

			continue
		}

		key, err := wgtypes.ParseKey(drift.PublicKey)
		if err != nil {
			continue
		}

		endpointDrifted[key] = true

		if wm.endpointDrift == nil {
			wm.endpointDrift = make(map[wgtypes.Key]endpointDriftRecord)
		}

		previous, seen := wm.endpointDrift[key]
		if seen && previous.endpoint == drift.Current && now.Sub(previous.logged) < endpointDriftLogInterval {
			klog.V(2).Infof("Repaired WireGuard drift on %s again: %s endpoint %s -> %s",
				wm.ifaceName, subject, drift.Current, drift.Desired)

			continue
		}

		wm.endpointDrift[key] = endpointDriftRecord{endpoint: drift.Current, logged: now}
		klog.Infof("Repaired WireGuard drift on %s: %s endpoint %s -> %s",
			wm.ifaceName, subject, drift.Current, drift.Desired)
	}

	wm.pruneEndpointDrift(endpointDrifted)
}

// pruneEndpointDrift forgets log records of peers that are no longer desired.
// Records of peers that did not drift this time are kept, so a peer that
// alternates between drifted and repaired is still logged only occasionally.
func (wm *WireGuardManager) pruneEndpointDrift(keep map[wgtypes.Key]bool) {
	for key := range wm.endpointDrift {
		if keep[key] {
			continue
		}

		if _, desired := wm.desired.peers[key]; !desired {
			delete(wm.endpointDrift, key)
		}
	}
}

func shortWireGuardKey(key string) string {
	return key[:min(8, len(key))]
}

func newWireGuardDesiredState(privKey wgtypes.Key, listenPort int, peers []WireGuardPeer) *wireGuardDesiredState {
	desired := &wireGuardDesiredState{
		privateKey: privKey,
		listenPort: listenPort,
		peers:      make(map[wgtypes.Key]WireGuardPeer, len(peers)),
	}

	for _, p := range peers {
		pubKey, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			klog.Warningf("Failed to parse public key %s: %v", p.PublicKey, err)
			continue
		}

		p.AllowedIPs = slices.Clone(p.AllowedIPs)
		desired.peers[pubKey] = p
	}

	return desired
}

// wireGuardDelta is the set of changes that brings a device to the desired state.
type wireGuardDelta struct {
	peerConfigs             []wgtypes.PeerConfig
	added, updated, removed int
	deviceChanged           bool
	drifts                  []WireGuardDrift
}

func (d wireGuardDelta) empty() bool {
	return len(d.peerConfigs) == 0 && !d.deviceChanged
}

// computeDelta compares the device with the desired state.
func (wm *WireGuardManager) computeDelta(device *wgtypes.Device, desired *wireGuardDesiredState) wireGuardDelta {
	var delta wireGuardDelta

	if device.PrivateKey != desired.privateKey {
		delta.deviceChanged = true
		delta.drifts = append(delta.drifts, WireGuardDrift{
			Reason:  WireGuardDriftPrivateKey,
			Current: "public key " + device.PublicKey.String(),
			Desired: "public key " + desired.privateKey.PublicKey().String(),
		})
	}

	if desired.listenPort > 0 && device.ListenPort != desired.listenPort {
		delta.deviceChanged = true
		delta.drifts = append(delta.drifts, WireGuardDrift{
			Reason:  WireGuardDriftListenPort,
			Current: strconv.Itoa(device.ListenPort),
			Desired: strconv.Itoa(desired.listenPort),
		})
	}

	currentPeers := make(map[wgtypes.Key]wgtypes.Peer, len(device.Peers))
	for _, p := range device.Peers {
		currentPeers[p.PublicKey] = p
	}

	desiredKeys := make([]wgtypes.Key, 0, len(desired.peers))
	for key := range desired.peers {
		desiredKeys = append(desiredKeys, key)
	}

	slices.SortFunc(desiredKeys, func(a, b wgtypes.Key) int { return bytes.Compare(a[:], b[:]) })

	// Add or update peers that should exist
	for _, pubKey := range desiredKeys {
		want := desired.peers[pubKey]

		current, exists := currentPeers[pubKey]
		if exists {
			drifts := wm.peerDrifts(current, want)
			if len(drifts) == 0 {
				continue
			}

			peerConfig, err := wm.buildPeerConfig(want)
			if err != nil {
				klog.Warningf("Failed to build peer config for %s: %v", want.PublicKey, err)
				continue
			}
			// UpdateOnly ensures we only update, not add
			peerConfig.UpdateOnly = true
			delta.peerConfigs = append(delta.peerConfigs, *peerConfig)
			delta.updated++
			delta.drifts = append(delta.drifts, drifts...)

			continue
		}

		peerConfig, err := wm.buildPeerConfig(want)
		if err != nil {
			klog.Warningf("Failed to build peer config for %s: %v", want.PublicKey, err)
			continue
		}

		delta.peerConfigs = append(delta.peerConfigs, *peerConfig)
		delta.added++
		delta.drifts = append(delta.drifts, WireGuardDrift{
			PublicKey: want.PublicKey,
			Reason:    WireGuardDriftMissingPeer,
			Current:   "absent",
			Desired:   "present",
		})
	}

	// Remove peers that should not exist
	for _, p := range device.Peers {
		if _, exists := desired.peers[p.PublicKey]; exists {
			continue
		}

		delta.peerConfigs = append(delta.peerConfigs, wgtypes.PeerConfig{
			PublicKey: p.PublicKey,
			Remove:    true,
		})
		delta.removed++
		delta.drifts = append(delta.drifts, WireGuardDrift{
			PublicKey: p.PublicKey.String(),
			Reason:    WireGuardDriftUnexpectedPeer,
			Current:   "present",
			Desired:   "absent",
		})
	}

	return delta
}

// applyDelta writes the delta to the device without replacing other peers.
func (wm *WireGuardManager) applyDelta(desired *wireGuardDesiredState, delta wireGuardDelta, start time.Time) error {
	privKey, listenPort := desired.privateKey, desired.listenPort
	cfg := wgtypes.Config{
		PrivateKey:   &privKey,
		ListenPort:   &listenPort,
		ReplacePeers: false, // Only apply the specific peer changes
		Peers:        delta.peerConfigs,
	}

	if err := wm.client.ConfigureDevice(wm.ifaceName, cfg); err != nil {
		WireGuardConfigureErrors.WithLabelValues(wm.ifaceName).Inc()
		WireGuardConfigureDuration.WithLabelValues(wm.ifaceName).Observe(time.Since(start).Seconds())

		return fmt.Errorf("failed to configure WireGuard device: %w", err)
	}

	WireGuardPeers.WithLabelValues(wm.ifaceName).Set(float64(len(desired.peers)))
	WireGuardPeersAdded.Add(float64(delta.added))
	WireGuardPeersRemoved.Add(float64(delta.removed))
	WireGuardConfigureDuration.WithLabelValues(wm.ifaceName).Observe(time.Since(start).Seconds())

	return nil
}

// configureFullReplace does a full peer replacement (used on first configuration)
func (wm *WireGuardManager) configureFullReplace(desired *wireGuardDesiredState) error {
	peerConfigs := make([]wgtypes.PeerConfig, 0, len(desired.peers))
	for _, peer := range desired.peers {
		peerConfig, err := wm.buildPeerConfig(peer)
		if err != nil {
			klog.Warningf("Failed to build peer config for %s: %v", peer.PublicKey, err)
			continue
		}

		peerConfigs = append(peerConfigs, *peerConfig)
	}

	privKey, listenPort := desired.privateKey, desired.listenPort
	cfg := wgtypes.Config{
		PrivateKey:   &privKey,
		ListenPort:   &listenPort,
		ReplacePeers: true,
		Peers:        peerConfigs,
	}

	if err := wm.client.ConfigureDevice(wm.ifaceName, cfg); err != nil {
		return fmt.Errorf("failed to configure WireGuard device: %w", err)
	}

	klog.V(4).Infof("Configured WireGuard device %s with %d peers (full replace)", wm.ifaceName, len(peerConfigs))

	return nil
}

// peerNeedsUpdate checks if a peer configuration has changed and needs updating
func (wm *WireGuardManager) peerNeedsUpdate(current wgtypes.Peer, desired WireGuardPeer) bool {
	return len(wm.peerDrifts(current, desired)) > 0
}

// peerDrifts lists how a kernel peer differs from its desired configuration.
// The endpoint is compared only when the desired endpoint is set, so endpoints
// that WireGuard learned by roaming are kept.
func (wm *WireGuardManager) peerDrifts(current wgtypes.Peer, desired WireGuardPeer) []WireGuardDrift {
	var drifts []WireGuardDrift

	if desired.Endpoint != "" {
		desiredEndpoint, err := net.ResolveUDPAddr("udp", desired.Endpoint)
		if err == nil && !udpAddrsEqual(current.Endpoint, desiredEndpoint) {
			currentEndpoint := "none"
			if current.Endpoint != nil {
				currentEndpoint = current.Endpoint.String()
			}

			drifts = append(drifts, WireGuardDrift{
				PublicKey: desired.PublicKey,
				Reason:    WireGuardDriftEndpoint,
				Current:   currentEndpoint,
				Desired:   desiredEndpoint.String(),
			})
		}
	}

	desiredKeepalive := *wm.keepaliveDuration(desired.PersistentKeepalive)
	if current.PersistentKeepaliveInterval != desiredKeepalive {
		drifts = append(drifts, WireGuardDrift{
			PublicKey: desired.PublicKey,
			Reason:    WireGuardDriftKeepalive,
			Current:   current.PersistentKeepaliveInterval.String(),
			Desired:   desiredKeepalive.String(),
		})
	}

	if !wm.allowedIPsEqual(current.AllowedIPs, desired.AllowedIPs) {
		drifts = append(drifts, WireGuardDrift{
			PublicKey: desired.PublicKey,
			Reason:    WireGuardDriftAllowedIPs,
			Current:   prefixSetString(currentAllowedIPSet(current.AllowedIPs)),
			Desired:   prefixSetString(desiredAllowedIPSet(desired.AllowedIPs)),
		})
	}

	return drifts
}

// allowedIPsEqual reports whether current and desired allowed IPs are the same
// set of prefixes. Single IPs, host bits and invalid desired entries are
// normalized the same way buildPeerConfig and the kernel treat them.
func (wm *WireGuardManager) allowedIPsEqual(current []net.IPNet, desired []string) bool {
	return maps.Equal(currentAllowedIPSet(current), desiredAllowedIPSet(desired))
}

func currentAllowedIPSet(current []net.IPNet) map[netip.Prefix]bool {
	set := make(map[netip.Prefix]bool, len(current))

	for _, ipNet := range current {
		addr, ok := netip.AddrFromSlice(ipNet.IP)
		if !ok {
			continue
		}

		bits, _ := ipNet.Mask.Size()
		set[netip.PrefixFrom(addr.Unmap(), bits).Masked()] = true
	}

	return set
}

func desiredAllowedIPSet(desired []string) map[netip.Prefix]bool {
	set := make(map[netip.Prefix]bool, len(desired))

	for _, cidr := range desired {
		if prefix, ok := parseAllowedIP(cidr); ok {
			set[prefix.Masked()] = true
		}
	}

	return set
}

func prefixSetString(set map[netip.Prefix]bool) string {
	prefixes := make([]string, 0, len(set))
	for prefix := range set {
		prefixes = append(prefixes, prefix.String())
	}

	sort.Strings(prefixes)

	return "[" + strings.Join(prefixes, ",") + "]"
}

// parseAllowedIP parses a CIDR or a single IP, which becomes a /32 or /128.
func parseAllowedIP(cidr string) (netip.Prefix, bool) {
	if prefix, err := netip.ParsePrefix(cidr); err == nil {
		return prefix, true
	}

	addr, err := netip.ParseAddr(cidr)
	if err != nil {
		return netip.Prefix{}, false
	}

	return netip.PrefixFrom(addr, addr.BitLen()), true
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
		prefix, ok := parseAllowedIP(cidr)
		if !ok {
			klog.Warningf("Invalid allowed IP %s", cidr)
			continue
		}

		allowedIPs = append(allowedIPs, prefixToIPNet(prefix))
	}

	return &wgtypes.PeerConfig{
		PublicKey:                   pubKey,
		Endpoint:                    endpoint,
		ReplaceAllowedIPs:           true,
		AllowedIPs:                  allowedIPs,
		PersistentKeepaliveInterval: wm.keepaliveDuration(peer.PersistentKeepalive),
	}, nil
}

// keepaliveDuration converts seconds to a *time.Duration for PersistentKeepalive.
// It is never nil: zero disables keepalive, so a desired value of zero also
// clears a keepalive that is set in the kernel.
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
	return wm.client.Device(wm.ifaceName)
}

// GetPeers returns the current list of peers configured on the device
func (wm *WireGuardManager) GetPeers() ([]wgtypes.Peer, error) {
	device, err := wm.client.Device(wm.ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get device: %w", err)
	}

	return device.Peers, nil
}
