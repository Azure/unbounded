// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestManagerProbesViaInterface runs a session whose probes leave through a
// named interface and checks that it comes up.
func TestManagerProbesViaInterface(t *testing.T) {
	portA := freePort(t)
	portB := freePort(t)
	changes := make(chan stateEvt, 20)

	mgrA, err := NewManager("node-a", portA, func(peer string, newState, oldState SessionState) {
		changes <- stateEvt{peer, newState, oldState}
	})
	if err != nil {
		t.Fatal(err)
	}

	mgrB, err := NewManager("node-b", portB, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, m := range []*Manager{mgrA, mgrB} {
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer m.Stop()
	}

	settings := HealthCheckSettings{TransmitInterval: 50 * time.Millisecond, ReceiveInterval: 50 * time.Millisecond, DetectMultiplier: 3}
	if err := mgrA.AddPeerVia("node-b", net.ParseIP("127.0.0.1"), "lo", settings); err != nil {
		t.Fatal(err)
	}

	mgrA.mu.Lock()
	sess := mgrA.sessions["node-b"]
	mgrA.mu.Unlock()
	sess.mu.Lock()
	sess.port = portB
	sess.mu.Unlock()

	waitForState(t, changes, "node-b", StateUp, 5*time.Second)
}

func TestSessionWriteViaUnknownInterface(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck

	s := newSession(sessionConfig{peerHostname: "peer", viaIf: "no-such-if0", conn: conn, settings: DefaultSettings()})
	if err := s.write([]byte("x"), &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9}); err == nil {
		t.Fatal("expected an error for a missing interface")
	}
}

func TestAddPeerViaKeepsSessionWhenUnchanged(t *testing.T) {
	m, err := NewManager("mgr-via-same", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	ip := net.ParseIP("10.0.0.1")
	if err := m.AddPeerVia("peer", ip, "unbounded0", DefaultSettings()); err != nil {
		t.Fatal(err)
	}

	m.mu.RLock()
	first := m.sessions["peer"]
	m.mu.RUnlock()

	if err := m.AddPeerVia("peer", ip, "unbounded0", DefaultSettings()); err != nil {
		t.Fatal(err)
	}

	m.mu.RLock()
	second := m.sessions["peer"]
	m.mu.RUnlock()

	if first != second {
		t.Fatal("re-adding an unchanged peer replaced its session")
	}
}

func TestAddPeerViaReplacesSessionWhenInterfaceChanges(t *testing.T) {
	m, err := NewManager("mgr-via", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	ip := net.ParseIP("10.0.0.1")
	if err := m.AddPeer("peer", ip, DefaultSettings()); err != nil {
		t.Fatal(err)
	}

	if err := m.AddPeerVia("peer", ip, "unbounded0", DefaultSettings()); err != nil {
		t.Fatal(err)
	}

	m.mu.RLock()
	got := m.sessions["peer"].viaIf
	m.mu.RUnlock()

	if got != "unbounded0" {
		t.Fatalf("session viaIf = %q, want unbounded0", got)
	}
}
