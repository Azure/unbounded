// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"sync"
)

type trackedTLSConnection struct {
	state http.ConnState
	old   bool
}

// HTTP/1.1 completes active responses before closing an old trust context.
// Both listeners share this tracker so replica drain claims cover every context.
type tlsConnections struct {
	mu          sync.Mutex
	connections map[net.Conn]trackedTLSConnection
}

func (t *tlsConnections) state(conn net.Conn, state http.ConnState) {
	t.mu.Lock()
	if t.connections == nil {
		t.connections = make(map[net.Conn]trackedTLSConnection)
	}

	old := t.connections[conn].old
	if state == http.StateClosed || state == http.StateHijacked {
		delete(t.connections, conn)
	} else {
		t.connections[conn] = trackedTLSConnection{state: state, old: old}
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
