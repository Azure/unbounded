// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

type trackedTLSConnection struct {
	state      http.ConnState
	old        bool
	expiration *time.Timer
}

// HTTP/1.1 completes active responses before closing an old trust context.
// Both listeners share this tracker so replica drain claims cover every context.
type tlsConnections struct {
	mu          sync.Mutex
	connections map[net.Conn]trackedTLSConnection
	localExpiry func() time.Time
}

func (t *tlsConnections) state(conn net.Conn, state http.ConnState) {
	var expiry time.Time

	if state == http.StateActive {
		if t.localExpiry != nil {
			expiry = t.localExpiry()
		}

		if secure, ok := conn.(*tls.Conn); ok {
			for _, cert := range secure.ConnectionState().PeerCertificates {
				if expiry.IsZero() || cert.NotAfter.Before(expiry) {
					expiry = cert.NotAfter
				}
			}
		}
	}

	t.mu.Lock()
	if t.connections == nil {
		t.connections = make(map[net.Conn]trackedTLSConnection)
	}

	entry := t.connections[conn]

	old := entry.old
	if state == http.StateClosed || state == http.StateHijacked {
		if entry.expiration != nil {
			entry.expiration.Stop()
		}

		delete(t.connections, conn)
	} else {
		entry.state = state
		if entry.expiration == nil && !expiry.IsZero() {
			entry.expiration = time.AfterFunc(time.Until(expiry), func() { closeTLSResource(conn) })
		}

		t.connections[conn] = entry
	}

	closeOld := old && state == http.StateIdle
	t.mu.Unlock()

	if closeOld {
		closeTLSResource(conn)
	}
}

func (t *tlsConnections) rotate() {
	t.mu.Lock()

	var idle []net.Conn

	for conn, entry := range t.connections {
		entry.old = true

		t.connections[conn] = entry
		if entry.state == http.StateIdle {
			idle = append(idle, conn)
		}
	}
	t.mu.Unlock()

	for _, conn := range idle {
		closeTLSResource(conn)
	}
}

func closeTLSResource(resource io.Closer) {
	if err := resource.Close(); err != nil {
		log.Printf("close TLS resource: %v", err)
	}
}

func (t *tlsConnections) drained() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, entry := range t.connections {
		if entry.old {
			return false
		}
	}

	return true
}
