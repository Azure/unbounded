// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestManagerAddRemovePeer(t *testing.T) {
	m, err := NewManager("mgr-test", 0, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ip := net.ParseIP("10.0.0.1")
	if err := m.AddPeer("peer-1", ip, DefaultSettings()); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	// Duplicate with same settings should be idempotent (no error).
	if err := m.AddPeer("peer-1", ip, DefaultSettings()); err != nil {
		t.Errorf("expected idempotent AddPeer, got error: %v", err)
	}

	status, err := m.GetPeerStatus("peer-1")
	if err != nil {
		t.Fatalf("GetPeerStatus: %v", err)
	}

	if status.State != StateDown {
		t.Errorf("expected Down, got %s", status.State)
	}

	// Remove peer.
	if err := m.RemovePeer("peer-1"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}

	// Should be gone.
	if _, err := m.GetPeerStatus("peer-1"); err == nil {
		t.Error("expected error after remove")
	}

	// Remove nonexistent.
	if err := m.RemovePeer("ghost"); err == nil {
		t.Error("expected error for nonexistent peer")
	}
}

func TestManagerGetAllPeerStatuses(t *testing.T) {
	m, err := NewManager("mgr-test2", 0, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_ = m.AddPeer("a", net.ParseIP("10.0.0.1"), DefaultSettings())
	_ = m.AddPeer("b", net.ParseIP("10.0.0.2"), DefaultSettings())

	all := m.GetAllPeerStatuses()
	if len(all) != 2 {
		t.Errorf("expected 2 peers, got %d", len(all))
	}

	if _, ok := all["a"]; !ok {
		t.Error("peer a missing")
	}

	if _, ok := all["b"]; !ok {
		t.Error("peer b missing")
	}
}

func TestManagerUpdatePeerSettings(t *testing.T) {
	m, err := NewManager("mgr-test3", 0, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_ = m.AddPeer("peer-x", net.ParseIP("10.0.0.5"), DefaultSettings())

	newSettings := HealthCheckSettings{
		TransmitInterval: 2 * time.Second,
		ReceiveInterval:  2 * time.Second,
		DetectMultiplier: 5,
	}
	if err := m.UpdatePeerSettings("peer-x", newSettings); err != nil {
		t.Fatalf("UpdatePeerSettings: %v", err)
	}

	// Update nonexistent.
	if err := m.UpdatePeerSettings("ghost", newSettings); err == nil {
		t.Error("expected error for nonexistent peer")
	}
}

func freePort(t *testing.T) int {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()

	return port
}

func TestManagerFullFlow(t *testing.T) {
	portA := freePort(t)
	portB := freePort(t)

	stateChangesA := make(chan stateEvt, 20)
	stateChangesB := make(chan stateEvt, 20)

	mgrA, err := NewManager("node-a", portA, func(peer string, newState, oldState SessionState) {
		stateChangesA <- stateEvt{peer, newState, oldState}
	})
	if err != nil {
		t.Fatalf("NewManager A: %v", err)
	}

	mgrB, err := NewManager("node-b", portB, func(peer string, newState, oldState SessionState) {
		stateChangesB <- stateEvt{peer, newState, oldState}
	})
	if err != nil {
		t.Fatalf("NewManager B: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := mgrA.Start(ctx); err != nil {
		t.Fatalf("mgrA.Start: %v", err)
	}
	defer mgrA.Stop()

	if err := mgrB.Start(ctx); err != nil {
		t.Fatalf("mgrB.Start: %v", err)
	}
	defer mgrB.Stop()

	settings := HealthCheckSettings{
		TransmitInterval: 50 * time.Millisecond,
		ReceiveInterval:  50 * time.Millisecond,
		DetectMultiplier: 3,
	}

	// A peers with B, B peers with A.
	if err := mgrA.AddPeer("node-b", net.ParseIP("127.0.0.1"), settings); err != nil {
		t.Fatalf("mgrA.AddPeer: %v", err)
	}

	// Fix B's peer port to match A's listening port.
	mgrA.mu.Lock()
	sessAB := mgrA.sessions["node-b"]
	mgrA.mu.Unlock()
	sessAB.mu.Lock()
	sessAB.port = portB
	sessAB.mu.Unlock()

	if err := mgrB.AddPeer("node-a", net.ParseIP("127.0.0.1"), settings); err != nil {
		t.Fatalf("mgrB.AddPeer: %v", err)
	}

	mgrB.mu.Lock()
	sessBA := mgrB.sessions["node-a"]
	mgrB.mu.Unlock()
	sessBA.mu.Lock()
	sessBA.port = portA
	sessBA.mu.Unlock()

	// Both should transition to Up.
	waitForState(t, stateChangesA, "node-b", StateUp, 5*time.Second)
	waitForState(t, stateChangesB, "node-a", StateUp, 5*time.Second)

	// Verify status.
	statusA, err := mgrA.GetPeerStatus("node-b")
	if err != nil {
		t.Fatalf("GetPeerStatus: %v", err)
	}

	if statusA.State != StateUp {
		t.Errorf("expected A->B Up, got %s", statusA.State)
	}
}

type stateEvt struct {
	peer     string
	newState SessionState
	oldState SessionState
}

func TestScopedPeersUseRemoteHostnameAndIndependentReplies(t *testing.T) {
	port := freePort(t)

	server, err := NewManager("node-b", port, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}

	defer server.Stop()

	changes := make(chan stateEvt, 20)

	client, err := NewManager("node-a", freePort(t), func(peer string, state, old SessionState) {
		changes <- stateEvt{peer, state, old}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	settings := HealthCheckSettings{TransmitInterval: 50 * time.Millisecond, ReceiveInterval: 50 * time.Millisecond, DetectMultiplier: 3}
	for _, peer := range []string{"node-b/assignment/pool-a", "node-b/transit/pool-b"} {
		if err := client.AddScopedPeer(peer, "node-b", net.ParseIP("127.0.0.1"), settings); err != nil {
			t.Fatal(err)
		}

		client.sessions[peer].port = port
	}
	// Registration before Start must retain the separate sockets.
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	first := "node-b/assignment/pool-a"
	second := "node-b/transit/pool-b"

	waitForState(t, changes, first, StateUp, 5*time.Second)
	deadline := time.After(5 * time.Second)

	for {
		status, err := client.GetPeerStatus(second)
		if err != nil {
			t.Fatal(err)
		}

		if status.State == StateUp {
			break
		}

		select {
		case <-deadline:
			t.Fatal("second scoped session did not receive its own replies")
		case <-time.After(10 * time.Millisecond):
		}
	}

	client.sessions[first].stop()
	// A reply to another socket must not refresh the stopped session.
	before, err := client.GetPeerStatus(first)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	after, err := client.GetPeerStatus(first)
	if err != nil || after.PacketsReceived != before.PacketsReceived {
		t.Fatalf("another relationship satisfied the stopped session: %v, %v", after, err)
	}
}

func waitForState(t *testing.T, ch chan stateEvt, peer string, want SessionState, timeout time.Duration) {
	t.Helper()

	deadline := time.After(timeout)

	for {
		select {
		case evt := <-ch:
			if evt.peer == peer && evt.newState == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for peer %q to reach state %s", peer, want)
		}
	}
}
