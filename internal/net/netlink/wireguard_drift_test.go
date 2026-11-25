// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// fakeWireGuardClient keeps one device in memory and applies ConfigureDevice
// the way the kernel does for the fields WireGuardManager uses.
type fakeWireGuardClient struct {
	device       *wgtypes.Device
	deviceErr    error
	configureErr error
	configures   []wgtypes.Config
}

func (f *fakeWireGuardClient) Device(name string) (*wgtypes.Device, error) {
	if f.deviceErr != nil {
		return nil, f.deviceErr
	}

	if f.device == nil {
		return nil, errors.New("device does not exist")
	}

	device := *f.device
	device.Name = name
	device.Peers = slices.Clone(f.device.Peers)

	return &device, nil
}

func (f *fakeWireGuardClient) ConfigureDevice(_ string, cfg wgtypes.Config) error {
	f.configures = append(f.configures, cfg)
	if f.configureErr != nil {
		return f.configureErr
	}

	if f.device == nil {
		f.device = &wgtypes.Device{}
	}

	if cfg.PrivateKey != nil {
		f.device.PrivateKey = *cfg.PrivateKey
		f.device.PublicKey = cfg.PrivateKey.PublicKey()
	}

	if cfg.ListenPort != nil {
		f.device.ListenPort = *cfg.ListenPort
	}

	if cfg.ReplacePeers {
		f.device.Peers = nil
	}

	for _, pc := range cfg.Peers {
		idx := slices.IndexFunc(f.device.Peers, func(p wgtypes.Peer) bool { return p.PublicKey == pc.PublicKey })
		if pc.Remove {
			if idx >= 0 {
				f.device.Peers = slices.Delete(f.device.Peers, idx, idx+1)
			}

			continue
		}

		if idx < 0 {
			if pc.UpdateOnly {
				continue
			}

			f.device.Peers = append(f.device.Peers, wgtypes.Peer{PublicKey: pc.PublicKey})
			idx = len(f.device.Peers) - 1
		}

		peer := &f.device.Peers[idx]
		if pc.Endpoint != nil {
			peer.Endpoint = pc.Endpoint
		}

		if pc.ReplaceAllowedIPs {
			peer.AllowedIPs = nil
		}

		peer.AllowedIPs = append(peer.AllowedIPs, pc.AllowedIPs...)

		if pc.PersistentKeepaliveInterval != nil {
			peer.PersistentKeepaliveInterval = *pc.PersistentKeepaliveInterval
		}
	}

	return nil
}

func (f *fakeWireGuardClient) Close() error { return nil }

func (f *fakeWireGuardClient) peer(t *testing.T, key string) *wgtypes.Peer {
	t.Helper()

	pubKey, err := wgtypes.ParseKey(key)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}

	for i := range f.device.Peers {
		if f.device.Peers[i].PublicKey == pubKey {
			return &f.device.Peers[i]
		}
	}

	return nil
}

func newTestKey(t *testing.T) wgtypes.Key {
	t.Helper()

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return key
}

// driftTestSetup configures a manager for a worker's per-gateway interface with
// one gateway peer that has an explicit endpoint and one peer without one.
type driftTestSetup struct {
	client     *fakeWireGuardClient
	manager    *WireGuardManager
	privateKey string
	gateway    WireGuardPeer
	roaming    WireGuardPeer
}

func newDriftTestSetup(t *testing.T) *driftTestSetup {
	t.Helper()

	s := &driftTestSetup{
		client:     &fakeWireGuardClient{},
		privateKey: newTestKey(t).String(),
		gateway: WireGuardPeer{
			PublicKey:           newTestKey(t).PublicKey().String(),
			Endpoint:            "1.2.3.4:51821",
			AllowedIPs:          []string{"10.0.0.1", "10.244.0.0/16"},
			PersistentKeepalive: 25,
		},
		roaming: WireGuardPeer{
			PublicKey:           newTestKey(t).PublicKey().String(),
			AllowedIPs:          []string{"10.0.0.2"},
			PersistentKeepalive: 25,
		},
	}
	s.manager = NewWireGuardManagerWithClient("wg51821", s.client)

	if err := s.manager.Configure(s.privateKey, 51821, []WireGuardPeer{s.gateway, s.roaming}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	s.client.configures = nil

	return s
}

func TestRepairDriftRestoresRoamedEndpoint(t *testing.T) {
	s := newDriftTestSetup(t)

	// The gateway's old mesh interface sent a handshake from port 51820 and
	// WireGuard roamed the endpoint there.
	s.client.peer(t, s.gateway.PublicKey).Endpoint = &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}

	drifts, err := s.manager.RepairDrift()
	if err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	want := []WireGuardDrift{{
		PublicKey: s.gateway.PublicKey,
		Reason:    WireGuardDriftEndpoint,
		Current:   "1.2.3.4:51820",
		Desired:   "1.2.3.4:51821",
	}}
	if !slices.Equal(drifts, want) {
		t.Fatalf("drifts = %+v, want %+v", drifts, want)
	}

	if got := s.client.peer(t, s.gateway.PublicKey).Endpoint.String(); got != "1.2.3.4:51821" {
		t.Fatalf("endpoint after repair = %s, want 1.2.3.4:51821", got)
	}

	if len(s.client.configures) != 1 {
		t.Fatalf("expected one ConfigureDevice call, got %d", len(s.client.configures))
	}

	cfg := s.client.configures[0]
	if cfg.ReplacePeers || len(cfg.Peers) != 1 || !cfg.Peers[0].UpdateOnly {
		t.Fatalf("expected an update-only write of the drifted peer, got %+v", cfg)
	}

	// Repaired: the next check writes nothing.
	drifts, err = s.manager.RepairDrift()
	if err != nil || len(drifts) != 0 || len(s.client.configures) != 1 {
		t.Fatalf("expected no further repair, got drifts=%+v err=%v writes=%d", drifts, err, len(s.client.configures))
	}
}

func TestRepairDriftKeepsLearnedEndpointWithoutDesiredEndpoint(t *testing.T) {
	s := newDriftTestSetup(t)

	learned := &net.UDPAddr{IP: net.ParseIP("5.6.7.8"), Port: 40000}
	s.client.peer(t, s.roaming.PublicKey).Endpoint = learned

	drifts, err := s.manager.RepairDrift()
	if err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	if len(drifts) != 0 || len(s.client.configures) != 0 {
		t.Fatalf("expected a learned endpoint to be left alone, got drifts=%+v writes=%d", drifts, len(s.client.configures))
	}

	if got := s.client.peer(t, s.roaming.PublicKey).Endpoint; !udpAddrsEqual(got, learned) {
		t.Fatalf("learned endpoint changed to %v", got)
	}
}

func TestRepairDriftNoDriftMakesNoWrites(t *testing.T) {
	s := newDriftTestSetup(t)

	for range 3 {
		drifts, err := s.manager.RepairDrift()
		if err != nil || len(drifts) != 0 {
			t.Fatalf("expected no drift, got drifts=%+v err=%v", drifts, err)
		}
	}

	if len(s.client.configures) != 0 {
		t.Fatalf("expected no ConfigureDevice calls, got %d", len(s.client.configures))
	}

	// Configure with unchanged input is equally cheap.
	if err := s.manager.Configure(s.privateKey, 51821, []WireGuardPeer{s.roaming, s.gateway}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if len(s.client.configures) != 0 {
		t.Fatalf("expected unchanged Configure to make no writes, got %d", len(s.client.configures))
	}
}

func TestRepairDriftRestoresMissingPeer(t *testing.T) {
	s := newDriftTestSetup(t)

	s.client.device.Peers = slices.DeleteFunc(s.client.device.Peers, func(p wgtypes.Peer) bool {
		return p.PublicKey.String() == s.gateway.PublicKey
	})

	drifts, err := s.manager.RepairDrift()
	if err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	if len(drifts) != 1 || drifts[0].Reason != WireGuardDriftMissingPeer {
		t.Fatalf("drifts = %+v, want one missing peer", drifts)
	}

	peer := s.client.peer(t, s.gateway.PublicKey)
	if peer == nil || peer.Endpoint.String() != "1.2.3.4:51821" {
		t.Fatalf("missing peer not restored with its endpoint: %+v", peer)
	}
}

func TestRepairDriftRemovesUnexpectedPeer(t *testing.T) {
	s := newDriftTestSetup(t)

	stranger := newTestKey(t).PublicKey()
	s.client.device.Peers = append(s.client.device.Peers, wgtypes.Peer{PublicKey: stranger})

	drifts, err := s.manager.RepairDrift()
	if err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	if len(drifts) != 1 || drifts[0].Reason != WireGuardDriftUnexpectedPeer || drifts[0].PublicKey != stranger.String() {
		t.Fatalf("drifts = %+v, want one unexpected peer", drifts)
	}

	if s.client.peer(t, stranger.String()) != nil {
		t.Fatalf("unexpected peer was not removed")
	}
}

func TestRepairDriftRestoresAllowedIPs(t *testing.T) {
	s := newDriftTestSetup(t)

	// Also roam the peer without a desired endpoint, to check that repairing
	// its allowed IPs does not touch its learned endpoint.
	learned := &net.UDPAddr{IP: net.ParseIP("5.6.7.8"), Port: 40000}
	roaming := s.client.peer(t, s.roaming.PublicKey)
	roaming.Endpoint = learned
	roaming.AllowedIPs = nil

	drifts, err := s.manager.RepairDrift()
	if err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	if len(drifts) != 1 || drifts[0].Reason != WireGuardDriftAllowedIPs {
		t.Fatalf("drifts = %+v, want one allowed IPs drift", drifts)
	}

	if drifts[0].Current != "[]" || drifts[0].Desired != "[10.0.0.2/32]" {
		t.Fatalf("unexpected drift description %+v", drifts[0])
	}

	roaming = s.client.peer(t, s.roaming.PublicKey)
	if len(roaming.AllowedIPs) != 1 || !udpAddrsEqual(roaming.Endpoint, learned) {
		t.Fatalf("unexpected peer after repair: %+v", roaming)
	}

	if s.client.configures[0].Peers[0].Endpoint != nil {
		t.Fatalf("repair of a peer without a desired endpoint must not write an endpoint")
	}
}

func TestRepairDriftRestoresKeepaliveAndListenPort(t *testing.T) {
	s := newDriftTestSetup(t)

	s.client.device.ListenPort = 40001
	s.client.peer(t, s.gateway.PublicKey).PersistentKeepaliveInterval = 0

	drifts, err := s.manager.RepairDrift()
	if err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	reasons := make([]string, 0, len(drifts))
	for _, drift := range drifts {
		reasons = append(reasons, drift.Reason)
	}

	if !slices.Equal(reasons, []string{WireGuardDriftListenPort, WireGuardDriftKeepalive}) {
		t.Fatalf("drift reasons = %v", reasons)
	}

	if s.client.device.ListenPort != 51821 {
		t.Fatalf("listen port = %d, want 51821", s.client.device.ListenPort)
	}

	if got := s.client.peer(t, s.gateway.PublicKey).PersistentKeepaliveInterval; got != 25*time.Second {
		t.Fatalf("keepalive = %v, want 25s", got)
	}
}

func TestRepairDriftClearsKeepaliveWhenDesiredIsZero(t *testing.T) {
	s := newDriftTestSetup(t)

	peer := s.roaming
	peer.PersistentKeepalive = 0

	if err := s.manager.Configure(s.privateKey, 51821, []WireGuardPeer{s.gateway, peer}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if got := s.client.peer(t, peer.PublicKey).PersistentKeepaliveInterval; got != 0 {
		t.Fatalf("keepalive = %v, want disabled", got)
	}

	s.client.configures = nil

	drifts, err := s.manager.RepairDrift()
	if err != nil || len(drifts) != 0 || len(s.client.configures) != 0 {
		t.Fatalf("expected a disabled keepalive to stay converged, got drifts=%+v err=%v writes=%d",
			drifts, err, len(s.client.configures))
	}
}

func TestRepairDriftDeviceReadError(t *testing.T) {
	s := newDriftTestSetup(t)
	s.client.deviceErr = errors.New("no such device")

	if _, err := s.manager.RepairDrift(); err == nil {
		t.Fatalf("expected an error when the device cannot be read")
	}

	if len(s.client.configures) != 0 {
		t.Fatalf("expected no writes when the device cannot be read")
	}
}

func TestRepairDriftWriteError(t *testing.T) {
	s := newDriftTestSetup(t)
	s.client.peer(t, s.gateway.PublicKey).Endpoint = &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	s.client.configureErr = errors.New("operation not permitted")

	drifts, err := s.manager.RepairDrift()
	if err == nil {
		t.Fatalf("expected an error when the repair cannot be written")
	}

	if len(drifts) != 0 {
		t.Fatalf("expected no drift reported as repaired, got %+v", drifts)
	}
}

func TestRepairDriftBeforeConfigureAndAfterReset(t *testing.T) {
	client := &fakeWireGuardClient{device: &wgtypes.Device{ListenPort: 1}}
	manager := NewWireGuardManagerWithClient("wg51820", client)

	if drifts, err := manager.RepairDrift(); err != nil || len(drifts) != 0 || len(client.configures) != 0 {
		t.Fatalf("expected no repair before Configure, got drifts=%+v err=%v", drifts, err)
	}

	s := newDriftTestSetup(t)
	s.manager.ResetDesired()
	s.client.peer(t, s.gateway.PublicKey).Endpoint = &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}

	if drifts, err := s.manager.RepairDrift(); err != nil || len(drifts) != 0 || len(s.client.configures) != 0 {
		t.Fatalf("expected no repair after ResetDesired, got drifts=%+v err=%v", drifts, err)
	}

	// A zero-value manager, as built by status tests, must not panic.
	if drifts, err := (&WireGuardManager{}).RepairDrift(); err != nil || len(drifts) != 0 {
		t.Fatalf("expected zero-value manager to do nothing, got drifts=%+v err=%v", drifts, err)
	}
}

func TestConfigureFailureStillRecordsDesiredState(t *testing.T) {
	client := &fakeWireGuardClient{configureErr: errors.New("transient")}
	manager := NewWireGuardManagerWithClient("wg51821", client)
	gateway := WireGuardPeer{
		PublicKey:  newTestKey(t).PublicKey().String(),
		Endpoint:   "1.2.3.4:51821",
		AllowedIPs: []string{"10.0.0.1"},
	}

	if err := manager.Configure(newTestKey(t).String(), 51821, []WireGuardPeer{gateway}); err == nil {
		t.Fatalf("expected Configure to fail")
	}

	client.configureErr = nil
	client.device = &wgtypes.Device{}

	drifts, err := manager.RepairDrift()
	if err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	if len(drifts) == 0 || client.peer(t, gateway.PublicKey) == nil {
		t.Fatalf("expected RepairDrift to apply the configuration that failed, got drifts=%+v", drifts)
	}
}

func TestConfigureDoesNotAliasCallerAllowedIPs(t *testing.T) {
	s := newDriftTestSetup(t)

	allowed := []string{"10.0.0.9"}
	peer := s.gateway
	peer.AllowedIPs = allowed

	if err := s.manager.Configure(s.privateKey, 51821, []WireGuardPeer{peer, s.roaming}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	allowed[0] = "10.0.0.10"
	s.client.configures = nil

	if drifts, err := s.manager.RepairDrift(); err != nil || len(drifts) != 0 {
		t.Fatalf("expected caller mutation not to affect the recorded state, got drifts=%+v err=%v", drifts, err)
	}
}

func TestReportDriftRepairsDampsRepeatedEndpointLogs(t *testing.T) {
	s := newDriftTestSetup(t)
	roamed := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	key, _ := wgtypes.ParseKey(s.gateway.PublicKey)

	s.client.peer(t, s.gateway.PublicKey).Endpoint = roamed
	if _, err := s.manager.RepairDrift(); err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	first, ok := s.manager.endpointDrift[key]
	if !ok || first.endpoint != "1.2.3.4:51820" {
		t.Fatalf("expected endpoint drift to be recorded, got %+v", s.manager.endpointDrift)
	}

	s.client.peer(t, s.gateway.PublicKey).Endpoint = roamed
	if _, err := s.manager.RepairDrift(); err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	if again := s.manager.endpointDrift[key]; !again.logged.Equal(first.logged) {
		t.Fatalf("expected repeated identical drift not to be logged again at the default level")
	}

	// Removing the peer from the desired state forgets its record.
	if err := s.manager.Configure(s.privateKey, 51821, []WireGuardPeer{s.roaming}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if _, err := s.manager.RepairDrift(); err != nil {
		t.Fatalf("RepairDrift: %v", err)
	}

	if _, ok := s.manager.endpointDrift[key]; ok {
		t.Fatalf("expected record of a peer that is no longer desired to be pruned")
	}
}
