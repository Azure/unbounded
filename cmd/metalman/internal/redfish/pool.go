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
	client *Client
	user   string
	pass   string
}

// NewPool returns a new empty client pool.
func NewPool() *Pool {
	return &Pool{clients: make(map[string]*entry)}
}

// Get returns a Client for the given BMC. If a cached Client exists with
// matching credentials, it is returned. If credentials have changed or no
// cached Client exists, a new one is created (closing the old one).
func (p *Pool) Get(ctx context.Context, url, certSHA256, user, pass, deviceID string) (*Client, error) {
	p.mu.Lock()

	if e, ok := p.clients[url]; ok {
		if e.user == user && e.pass == pass {
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
	p.clients[url] = &entry{client: c, user: user, pass: pass}
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
