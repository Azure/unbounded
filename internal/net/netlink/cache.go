// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

// NetlinkCache maintains an in-memory cache of network links, addresses,
// and routes, updated via netlink subscribe events with periodic full
// resync as a safety net. Analogous to a Kubernetes informer.
type NetlinkCache struct {
	mu sync.RWMutex

	linksByName  map[string]netlink.Link
	linksByIndex map[int]netlink.Link
	addrsByLink  map[int][]netlink.Addr
	routes       []netlink.Route

	resyncInterval time.Duration

	// Route batch support: when routeBatchDepth > 0, route events are
	// suppressed and a single RouteList runs when the batch completes.
	batchMu         sync.Mutex
	routeBatchDepth int
	routeBatchDirty bool
}

// NewNetlinkCache creates a new cache with the given resync interval.
func NewNetlinkCache(resyncInterval time.Duration) *NetlinkCache {
	return &NetlinkCache{
		linksByName:    make(map[string]netlink.Link),
		linksByIndex:   make(map[int]netlink.Link),
		addrsByLink:    make(map[int][]netlink.Addr),
		resyncInterval: resyncInterval,
	}
}

// Start subscribes to netlink events, performs an initial full sync, and
// launches the background event-processing goroutine. Subscribe channels
// are opened before the initial list to avoid missing changes between
// List and Subscribe.
func (c *NetlinkCache) Start(ctx context.Context) error {
	// 1. Subscribe to events BEFORE listing to avoid race condition
	linkCh := make(chan netlink.LinkUpdate, 64)
	addrCh := make(chan netlink.AddrUpdate, 64)
	routeCh := make(chan netlink.RouteUpdate, 64)
	done := make(chan struct{})

	if err := netlink.LinkSubscribe(linkCh, done); err != nil {
		return fmt.Errorf("failed to subscribe to link events: %w", err)
	}

	if err := netlink.AddrSubscribe(addrCh, done); err != nil {
		close(done)
		return fmt.Errorf("failed to subscribe to addr events: %w", err)
	}

	if err := netlink.RouteSubscribe(routeCh, done); err != nil {
		close(done)
		return fmt.Errorf("failed to subscribe to route events: %w", err)
	}

	// 2. Initial full sync
	if err := c.resync(); err != nil {
		close(done)
		return fmt.Errorf("initial netlink sync failed: %w", err)
	}

	klog.Infof("Netlink cache initialized: %d links, %d routes", len(c.linksByName), len(c.routes))

	// 3. Start event processing and periodic resync
	go c.run(ctx, linkCh, addrCh, routeCh, done)

	return nil
}

// run processes subscribe events and triggers periodic full resyncs.
func (c *NetlinkCache) run(ctx context.Context, linkCh chan netlink.LinkUpdate, addrCh chan netlink.AddrUpdate, routeCh chan netlink.RouteUpdate, done chan struct{}) {
	defer close(done)

	resyncTicker := time.NewTicker(c.resyncInterval)
	defer resyncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case update := <-linkCh:
			c.handleLinkUpdate(update)
		case update := <-addrCh:
			c.handleAddrUpdate(update)
		case update := <-routeCh:
			c.handleRouteUpdate(update)
		case <-resyncTicker.C:
			if err := c.resync(); err != nil {
				klog.Warningf("Netlink cache resync failed: %v", err)
			}
		}
	}
}

// resync performs a full list of all links, addresses, and routes.
func (c *NetlinkCache) resync() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("LinkList: %w", err)
	}

	allAddrs := make(map[int][]netlink.Addr)

	for _, link := range links {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			klog.V(4).Infof("Netlink cache: failed to list addrs for %s: %v", link.Attrs().Name, err)
			continue
		}

		if len(addrs) > 0 {
			allAddrs[link.Attrs().Index] = addrs
		}
	}

	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("RouteList: %w", err)
	}

	c.mu.Lock()
	c.linksByName = make(map[string]netlink.Link, len(links))

	c.linksByIndex = make(map[int]netlink.Link, len(links))
	for _, link := range links {
		c.linksByName[link.Attrs().Name] = link
		c.linksByIndex[link.Attrs().Index] = link
	}

	c.addrsByLink = allAddrs
	c.routes = routes
	c.mu.Unlock()

	return nil
}

// handleLinkUpdate applies an incremental link add/delete/rename event.
func (c *NetlinkCache) handleLinkUpdate(update netlink.LinkUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()

	link := update.Link
	name := link.Attrs().Name
	idx := link.Attrs().Index

	if update.Header.Type == syscall.RTM_DELLINK {
		delete(c.linksByName, name)
		delete(c.linksByIndex, idx)
		delete(c.addrsByLink, idx)
	} else {
		// For rename: remove old name if index existed with different name
		if old, ok := c.linksByIndex[idx]; ok && old.Attrs().Name != name {
			delete(c.linksByName, old.Attrs().Name)
		}

		c.linksByName[name] = link
		c.linksByIndex[idx] = link
	}
}

// handleAddrUpdate applies an incremental address add/remove event.
// The AddrUpdate.LinkAddress is a net.IPNet, not a full netlink.Addr,
// so incremental updates are approximate -- the periodic resync
// corrects any drift.
func (c *NetlinkCache) handleAddrUpdate(update netlink.AddrUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := update.LinkIndex
	if update.NewAddr {
		ipNet := update.LinkAddress
		addr := netlink.Addr{IPNet: &ipNet}
		c.addrsByLink[idx] = append(c.addrsByLink[idx], addr)
	} else {
		// Address removed -- filter out the matching address
		addrs := c.addrsByLink[idx]

		filtered := addrs[:0]
		for _, a := range addrs {
			if !a.IP.Equal(update.LinkAddress.IP) {
				filtered = append(filtered, a)
			}
		}

		c.addrsByLink[idx] = filtered
	}
}

// handleRouteUpdate triggers a full route resync on any route change.
// If a route batch is active, the resync is deferred until the batch ends.
func (c *NetlinkCache) handleRouteUpdate(_ netlink.RouteUpdate) {
	c.batchMu.Lock()
	if c.routeBatchDepth > 0 {
		c.routeBatchDirty = true
		c.batchMu.Unlock()

		return
	}
	c.batchMu.Unlock()

	c.resyncRoutes()
}

// resyncRoutes performs a full route list and replaces the cached routes.
func (c *NetlinkCache) resyncRoutes() {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		klog.V(4).Infof("Netlink cache: route resync failed: %v", err)
		return
	}

	c.mu.Lock()
	c.routes = routes
	c.mu.Unlock()
}

// BeginRouteBatch suppresses route event resyncs. Multiple calls nest --
// route resync runs when the outermost batch ends. Use this before making
// multiple route changes to coalesce them into a single RouteList.
func (c *NetlinkCache) BeginRouteBatch() {
	c.batchMu.Lock()
	c.routeBatchDepth++
	c.batchMu.Unlock()
}

// EndRouteBatch decrements the batch depth. When the outermost batch ends,
// if any route events arrived during the batch, a single RouteList resync
// is triggered.
func (c *NetlinkCache) EndRouteBatch() {
	c.batchMu.Lock()

	c.routeBatchDepth--
	if c.routeBatchDepth < 0 {
		c.routeBatchDepth = 0
	}

	shouldResync := c.routeBatchDepth == 0 && c.routeBatchDirty
	c.routeBatchDirty = false
	c.batchMu.Unlock()

	if shouldResync {
		c.resyncRoutes()
	}
}

// LinkByName returns the cached link with the given name.
func (c *NetlinkCache) LinkByName(name string) (netlink.Link, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	link, ok := c.linksByName[name]
	if !ok {
		return nil, fmt.Errorf("link %s not found in cache", name)
	}

	return link, nil
}

// LinkByIndex returns the cached link with the given index.
func (c *NetlinkCache) LinkByIndex(index int) (netlink.Link, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	link, ok := c.linksByIndex[index]
	if !ok {
		return nil, fmt.Errorf("link index %d not found in cache", index)
	}

	return link, nil
}

// LinkList returns all cached links.
func (c *NetlinkCache) LinkList() []netlink.Link {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]netlink.Link, 0, len(c.linksByName))
	for _, link := range c.linksByName {
		result = append(result, link)
	}

	return result
}

// HasLink returns true if a link with the given name exists in the cache.
func (c *NetlinkCache) HasLink(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.linksByName[name]

	return ok
}

// AddrList returns cached addresses for the given link and address family.
func (c *NetlinkCache) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	addrs := c.addrsByLink[link.Attrs().Index]
	if family == netlink.FAMILY_ALL {
		return append([]netlink.Addr(nil), addrs...), nil
	}

	var filtered []netlink.Addr

	for _, a := range addrs {
		if family == netlink.FAMILY_V4 && a.IP.To4() != nil {
			filtered = append(filtered, a)
		} else if family == netlink.FAMILY_V6 && a.IP.To4() == nil {
			filtered = append(filtered, a)
		}
	}

	return filtered, nil
}

// RouteList returns cached routes, optionally filtered by link.
func (c *NetlinkCache) RouteList(link netlink.Link, family int) ([]netlink.Route, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var result []netlink.Route

	for _, r := range c.routes {
		if link != nil && r.LinkIndex != link.Attrs().Index {
			continue
		}

		if family != netlink.FAMILY_ALL {
			if r.Dst != nil {
				isV4 := r.Dst.IP.To4() != nil
				if (family == netlink.FAMILY_V4 && !isV4) || (family == netlink.FAMILY_V6 && isV4) {
					continue
				}
			}
		}

		result = append(result, r)
	}

	return result, nil
}

// Stats returns cache statistics for debugging.
func (c *NetlinkCache) Stats() (links, addrs, routes int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	totalAddrs := 0
	for _, a := range c.addrsByLink {
		totalAddrs += len(a)
	}

	return len(c.linksByName), totalAddrs, len(c.routes)
}
