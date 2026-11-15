// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"sync"
)

// Pool manages Redfish clients for multiple BMCs. It reuses sessions
// to avoid creating and destroying a Redfish session on every reconcile.
// Safe for concurrent use.
type Pool struct {
	mu      sync.Mutex
	clients map[string]*entry
}

type entry struct {
	client      *Client
	fingerprint string
	user        string
	pass        string
}

// NewPool returns a new empty client pool.
func NewPool() *Pool {
	return &Pool{clients: make(map[string]*entry)}
}

// matches reports whether e can serve a request with the given certificate pin,
// credentials, and device ID. Rack slots are addressed by the URL, so the
// configured device ID does not apply to them.
func (e *entry) matches(certSHA256, user, pass, deviceID string) bool {
	if e.fingerprint != certSHA256 || e.user != user || e.pass != pass {
		return false
	}

	if e.client.rack != nil {
		return true
	}

	return deviceID == "" || e.client.deviceID == deviceID
}

// Get returns a Client for the given BMC. If a cached Client exists with
// matching certificate pin, credentials, and device ID, it is returned. If any
// of those have changed or no cached Client exists, a new one is created
// (closing the old one).
func (p *Pool) Get(ctx context.Context, url, certSHA256, user, pass, deviceID string) (*Client, error) {
	p.mu.Lock()

	if e, ok := p.clients[url]; ok {
		if e.matches(certSHA256, user, pass, deviceID) {
			p.mu.Unlock()
			return e.client, nil
		}

		e.client.Close()
		delete(p.clients, url)
	}

	p.mu.Unlock()

	c, err := Dial(ctx, url, certSHA256, user, pass, deviceID)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()

	// Another goroutine may have created an entry while we were dialing.
	if e, ok := p.clients[url]; ok {
		if e.matches(certSHA256, user, pass, deviceID) {
			// Use the existing entry; close our redundant connection.
			p.mu.Unlock()
			c.Close()

			return e.client, nil
		}

		// The cached entry no longer matches; close the stale entry.
		e.client.Close()
	}

	p.clients[url] = &entry{client: c, fingerprint: certSHA256, user: user, pass: pass}
	p.mu.Unlock()

	return c, nil
}

// Close closes all cached Clients.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for url, e := range p.clients {
		e.client.Close()
		delete(p.clients, url)
	}
}
