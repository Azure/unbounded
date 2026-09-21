// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"fmt"
	"net"
	"sync"

	"k8s.io/klog/v2"

	pb "github.com/Azure/unbounded/internal/net/healthcheck/proto"
)

// Manager manages multiple peer health check sessions.
// It coordinates the listener and all active sessions, and dispatches
// state change callbacks when peer health transitions occur.
type Manager struct {
	localHostname string
	port          int
	onStateChange StateChangeFunc

	listener *Listener
	conn     net.PacketConn

	mu       sync.RWMutex
	peerMu   sync.Mutex
	sessions map[string]*session

	ctx    context.Context
	cancel context.CancelFunc
}

// NewManager creates a Manager that will listen on the given UDP port
// and invoke onStateChange whenever a peer transitions state.
func NewManager(localHostname string, port int, onStateChange StateChangeFunc) (*Manager, error) {
	listener, err := NewListener(localHostname, port)
	if err != nil {
		return nil, err
	}

	return &Manager{
		localHostname: localHostname,
		port:          port,
		onStateChange: onStateChange,
		listener:      listener,
		sessions:      make(map[string]*session),
	}, nil
}

// Start begins the listener and all registered sessions.
func (m *Manager) Start(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)
	m.ctx = ctx

	// Open the shared UDP connection for sending probes.
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", m.port))
	if err != nil {
		return fmt.Errorf("healthcheck manager: bind port %d: %w", m.port, err)
	}

	m.conn = conn

	// Wire the listener to use the same connection and route replies.
	m.listener.conn = conn
	m.listener.SetHandler(m.handleReply)

	// Start the listener read loop in a goroutine.
	go func() {
		if err := m.listener.Start(ctx); err != nil {
			klog.Errorf("healthcheck listener exited with error: %v", err)
		}
	}()

	// Start any sessions that were added before Start was called.
	m.mu.RLock()

	for _, s := range m.sessions {
		s.conn = conn
		s.start(ctx)
	}

	m.mu.RUnlock()

	klog.Infof("healthcheck manager started for %s on port %d", m.localHostname, m.port)

	return nil
}

// Stop gracefully shuts down the listener and all sessions.
func (m *Manager) Stop() {
	m.peerMu.Lock()
	defer m.peerMu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}

	m.mu.RLock()

	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}

	m.mu.RUnlock()

	for _, s := range sessions {
		s.stop()
	}

	m.listener.Stop()
	klog.Info("healthcheck manager stopped")
}

// AddPeer registers a new peer for health checking. If the manager is
// already running, the session starts immediately.
func (m *Manager) AddPeer(peerHostname string, overlayIP net.IP, settings HealthCheckSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}

	m.peerMu.Lock()
	defer m.peerMu.Unlock()

	m.mu.Lock()
	if existing, exists := m.sessions[peerHostname]; exists {
		if existing.overlayIP.Equal(overlayIP) {
			existing.updateSettings(settings)
			m.mu.Unlock()

			return nil
		}

		delete(m.sessions, peerHostname)
		m.mu.Unlock()
		// Callbacks may query the manager while stop waits for their completion.
		existing.stop()
		klog.V(4).Infof("healthcheck: updating peer %s (%s -> %s)", peerHostname, existing.overlayIP, overlayIP)
		m.mu.Lock()
	}
	defer m.mu.Unlock()

	s := newSession(sessionConfig{
		peerHostname:  peerHostname,
		overlayIP:     overlayIP,
		port:          m.port,
		localHostname: m.localHostname,
		settings:      settings,
		onChange:      m.onStateChange,
		conn:          m.conn,
	})
	m.sessions[peerHostname] = s

	// If manager is already running (conn is set), start the session now.
	if m.conn != nil {
		s.conn = m.conn
		s.start(m.ctx)
	}

	updatePeerGauges(m.sessions)
	klog.Infof("healthcheck: added peer %s (%s)", peerHostname, overlayIP)

	return nil
}

// RemovePeer stops and removes a peer session.
func (m *Manager) RemovePeer(peerHostname string) error {
	m.peerMu.Lock()
	defer m.peerMu.Unlock()

	m.mu.Lock()

	s, exists := m.sessions[peerHostname]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("peer %q not found", peerHostname)
	}

	delete(m.sessions, peerHostname)
	updatePeerGauges(m.sessions)
	m.mu.Unlock()

	s.stop()
	klog.Infof("healthcheck: removed peer %s", peerHostname)

	return nil
}

// UpdatePeerSettings modifies the health check parameters for an existing peer.
func (m *Manager) UpdatePeerSettings(peerHostname string, settings HealthCheckSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	s, exists := m.sessions[peerHostname]

	if !exists {
		return fmt.Errorf("peer %q not found", peerHostname)
	}

	s.updateSettings(settings)

	return nil
}

// GetPeerSettings returns a copy of the currently applied session settings.
func (m *Manager) GetPeerSettings(peerHostname string) (HealthCheckSettings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, exists := m.sessions[peerHostname]
	if !exists {
		return HealthCheckSettings{}, fmt.Errorf("peer %q not found", peerHostname)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.settings, nil
}

// GetPeerStatus returns the current health status for a single peer.
func (m *Manager) GetPeerStatus(peerHostname string) (*PeerStatus, error) {
	m.mu.RLock()
	s, exists := m.sessions[peerHostname]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("peer %q not found", peerHostname)
	}

	return s.status(), nil
}

// GetAllPeerStatuses returns the current health status for all peers.
func (m *Manager) GetAllPeerStatuses() map[string]*PeerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*PeerStatus, len(m.sessions))
	for hostname, s := range m.sessions {
		result[hostname] = s.status()
	}

	return result
}

func (m *Manager) handleReply(pkt *pb.HealthCheckPacket, from net.Addr) {
	m.mu.RLock()
	s, exists := m.sessions[pkt.SourceHostname]
	m.mu.RUnlock()

	if !exists {
		klog.V(4).Infof("healthcheck: reply from unknown peer %q (%s)", pkt.SourceHostname, from)
		return
	}

	s.receiveReply(pkt)
}
