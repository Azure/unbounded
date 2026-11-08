// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/cachemesh/memtransport"
)

// server is one process of an application that embeds cachemesh. In a real
// deployment the control transport would be an HTTP or gRPC endpoint whose
// handler calls Mesh.ServeControl, and the data plane would be the
// application's own RPC carrying the 32-byte cachemesh header.
type server struct {
	mesh  *cachemesh.Mesh
	peers map[string]*server // stands in for data-plane connections
	cache map[string]entry
}

type entry struct {
	value string
	stamp cachemesh.Stamp
}

var metaKS = cachemesh.MustKeyspace("layer-metadata", 10*time.Minute)

// get is the client side: route the key and send the request.
func (s *server) get(key string) (string, error) {
	d := s.mesh.Route(metaKS, []byte(key))

	switch d.Kind {
	case cachemesh.KindSelf:
		return s.serve(key), nil
	case cachemesh.KindNoRoute:
		// Not joined and no gateway known yet: go to the origin or fail.
		return "", errors.New("no route")
	default:
		// Relay, Owner, Backup, Direct, Gateway: send to d.Next.
		return s.send(d, key)
	}
}

// send sends a request to d.Next, observes the outcome, and hedges to
// d.Alternate when d.Next is slow and the hedge budget allows it.
func (s *server) send(d cachemesh.Decision, key string) (string, error) {
	var hdr [cachemesh.HeaderSize]byte

	d.Header.Put(&hdr)

	start := time.Now()
	v, err := s.peers[d.Next.Addr].handle(hdr[:], key)

	switch {
	case err == nil:
		s.mesh.Observe(d.Next, cachemesh.Success, time.Since(start))
		return v, nil
	case errors.Is(err, context.DeadlineExceeded):
		s.mesh.Observe(d.Next, cachemesh.Timeout, time.Since(start))
	default:
		s.mesh.Observe(d.Next, cachemesh.Failure, time.Since(start))
	}

	// A real client starts the hedge after d.HedgeAfter rather than after
	// a failure; both go through the same budget.
	if !d.Alternate.IsZero() && s.mesh.TryHedge(d.Header) {
		h := d.Header
		h.Flags |= cachemesh.FlagHedge
		h.Put(&hdr)

		return s.peers[d.Alternate.Addr].handle(hdr[:], key)
	}

	return "", err
}

// handle is the server side of the data plane: serve locally or relay.
func (s *server) handle(raw []byte, key string) (string, error) {
	h, err := cachemesh.ParseHeader(raw)
	if err != nil {
		return "", err
	}

	d, err := s.mesh.Forward(h)
	if err != nil {
		return "", err // ErrTTLExhausted or ErrBadHeader
	}

	if d.Kind == cachemesh.KindSelf {
		return s.serve(key), nil
	}

	// A relay may cache the response on the way back (not shown).
	return s.send(d, key)
}

// serve returns a cached value while it is Fresh, refilling it otherwise.
// The owner stamps values; copies keep the owner's stamp.
func (s *server) serve(key string) string {
	if e, ok := s.cache[key]; ok && s.mesh.Fresh(metaKS, e.stamp) {
		return e.value
	}

	e := entry{value: "metadata of " + key, stamp: s.mesh.Stamp(metaKS)}
	s.cache[key] = e

	return e.value
}

func Example() {
	net := memtransport.NewNetwork()
	lease := memtransport.NewLeaseStore() // in production: kubelease.New(...)
	peers := map[string]*server{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := range 3 {
		addr := fmt.Sprintf("node-%d:7946", i)

		var seeds []string
		if i > 0 {
			seeds = []string{"node-0:7946"}
		}

		m, err := cachemesh.New(cachemesh.Config{
			ID:        fmt.Sprintf("node-%d", i),
			Addr:      addr,
			Transport: net.Transport(addr),
			Seeds:     seeds,
			Lease:     lease,
			Neighbors: cachemesh.NeighborFunc(func(added, removed []cachemesh.PeerRef) {
				// Open connections to added peers, close removed ones.
			}),
			Logger:        slog.New(slog.DiscardHandler),
			PartitionBits: 12,
			Tuning:        fastTuning(), // short timers for the example
		})
		if err != nil {
			panic(err)
		}

		net.Register(addr, m.ServeControl)
		peers[addr] = &server{mesh: m, peers: peers, cache: map[string]entry{}}

		go func() { _ = m.Run(ctx) }()

		if i == 0 {
			for !m.Leading() {
				time.Sleep(10 * time.Millisecond)
			}
		}
	}

	// Wait until every member is admitted and all agree on the view.
	for {
		ready := 0

		for _, s := range peers {
			if s.mesh.Ready() && s.mesh.Members() == 3 && s.mesh.Version() == peers["node-0:7946"].mesh.Version() {
				ready++
			}
		}

		if ready == 3 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	client := peers["node-1:7946"]

	v, err := client.get("sha256:abc")
	fmt.Println(v, err)

	owner := client.mesh.Route(metaKS, []byte("sha256:abc")).Owner
	_, cached := peers[owner.Addr].cache["sha256:abc"]
	fmt.Println("cached at owner:", cached)

	// Output:
	// metadata of sha256:abc <nil>
	// cached at owner: true
}
