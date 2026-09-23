// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestTLSConnectionsDrainAfterActiveResponse(t *testing.T) {
	tracker := new(tlsConnections)

	active, peer := net.Pipe()
	defer active.Close()
	defer peer.Close()

	tracker.state(active, http.StateActive)
	tracker.rotate()

	if tracker.drained() {
		t.Fatal("active old connection reported drained")
	}

	if err := peer.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("unexpected read")
	} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatal("active response closed during trust update")
	}

	fresh, freshPeer := net.Pipe()
	defer fresh.Close()
	defer freshPeer.Close()

	tracker.state(fresh, http.StateNew)
	tracker.state(active, http.StateIdle)
	tracker.state(active, http.StateClosed)

	if !tracker.drained() {
		t.Fatal("fresh connection blocks old-context drain")
	}
}

func TestTLSConnectionsDisconnectAtCertificateExpiry(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()

	expiry := time.Now().Add(30 * time.Millisecond)
	tracker := &tlsConnections{localExpiry: func() time.Time { return expiry }}
	tracker.state(conn, http.StateActive)

	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("expired TLS connection remains open")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("expiration failed to close active connection")
	}

	tracker.state(conn, http.StateClosed)

	if len(tracker.connections) != 0 {
		t.Fatal("closed connection retained")
	}
}
