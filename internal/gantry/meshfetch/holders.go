// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"net"
	"sync"
	"time"
)

// holdersPerChunk is how many recent requesters a member remembers per
// chunk.
const holdersPerChunk = 4

// holderMinAge is how long after its request a requester is assumed to
// hold the chunk: a member that asked moments ago is most likely still
// pulling it (often it is the very request being filled).
const holderMinAge = 20 * time.Second

// holderTTL bounds how long a requester is assumed to still hold a chunk
// in its content store (containerd may garbage collect it).
const holderTTL = 6 * time.Hour

// holders is a bounded directory of members that recently pulled a chunk
// for their own containerd, and so likely hold it in their content store.
// An owner that has the chunk neither in memory nor locally reads it from
// one of them before going to the origin.
type holders struct {
	mu   sync.Mutex
	m    map[chunkKey]*holderSet
	ring []chunkKey
	next int
	now  func() time.Time
	// minAge skips requesters recorded less than this long ago.
	minAge time.Duration
}

type holderSet struct {
	addrs [holdersPerChunk]string
	at    [holdersPerChunk]time.Time
	n     int
}

func newHolders(capacity int) *holders {
	return &holders{
		m:      make(map[chunkKey]*holderSet, capacity),
		ring:   make([]chunkKey, max(capacity, 1)),
		now:    time.Now,
		minAge: holderMinAge,
	}
}

// validHolder reports whether addr looks like a host:port worth dialing.
func validHolder(addr string) bool {
	if addr == "" || len(addr) > 255 {
		return false
	}

	_, port, err := net.SplitHostPort(addr)

	return err == nil && port != ""
}

// add records addr as a recent requester of k.
func (h *holders) add(k chunkKey, addr string) {
	if !validHolder(addr) {
		return
	}

	now := h.now()

	h.mu.Lock()
	defer h.mu.Unlock()

	set := h.m[k]
	if set == nil {
		// Reuse the oldest slot of the ring, dropping its key.
		if old := h.ring[h.next]; old.digest != "" {
			delete(h.m, old)
		}

		h.ring[h.next] = k
		h.next = (h.next + 1) % len(h.ring)

		set = &holderSet{}
		h.m[k] = set
	}

	for i := range set.n {
		if set.addrs[i] == addr {
			set.at[i] = now
			return
		}
	}

	// Replace the oldest entry when full.
	i := set.n
	if i == holdersPerChunk {
		i = 0

		for j := 1; j < holdersPerChunk; j++ {
			if set.at[j].Before(set.at[i]) {
				i = j
			}
		}
	} else {
		set.n++
	}

	set.addrs[i], set.at[i] = addr, now
}

// list returns the requesters of k other than self recorded between
// holderTTL and minAge ago, most recent first.
func (h *holders) list(k chunkKey, self string) []string {
	now := h.now()

	h.mu.Lock()
	defer h.mu.Unlock()

	cutoff, newest := now.Add(-holderTTL), now.Add(-h.minAge)

	set := h.m[k]
	if set == nil {
		return nil
	}

	type entry struct {
		addr string
		at   time.Time
	}

	var es []entry

	for i := range set.n {
		if set.addrs[i] != self && set.at[i].After(cutoff) && !set.at[i].After(newest) {
			es = append(es, entry{set.addrs[i], set.at[i]})
		}
	}

	// Insertion sort, newest first: at most holdersPerChunk entries.
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j].at.After(es[j-1].at); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}

	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.addr
	}

	return out
}

// len returns the number of chunks with recorded requesters.
func (h *holders) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.m)
}
