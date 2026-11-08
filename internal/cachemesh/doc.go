// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package cachemesh makes routing decisions for a distributed,
// groupcache-style cache spread across up to 100,000 processes. It decides
// which member owns a key, which neighbor a request goes to next, which
// neighbors a process keeps connections to, when a cached metadata value
// has expired, and who is in the cluster. It holds no bytes and opens no
// connections: the application brings its own transports, storage, and
// requests. The design is in designs/cachemesh.md.
//
// # Members and the control plane
//
// Each process creates one Mesh with New and drives it with Run. Members
// find each other through Config.Seeds (control endpoints of existing
// members) or the leader Lease (Config.Lease, for example a
// *kubelease.Store). One member, elected through the Lease, publishes
// versioned membership views; every other member learns them over the
// control transport without contacting the API server. A member with a nil
// Lease can only join through seeds and never becomes leader.
//
// The control plane is message based. The application implements
// ControlTransport (Send one request, return one response) over HTTP, gRPC,
// or anything else, and calls Mesh.ServeControl for every request it
// receives at Config.Addr. Members are placed in a grid of cells; each keeps
// at most MaxNeighbors data-plane neighbors (its cell and the adjacent
// cells), which a NeighborWatcher learns about, so connections stay bounded
// at any cluster size.
//
// # Data plane
//
// Route returns a Decision for a key: serve locally (KindSelf), or send to
// Decision.Next with Decision.Header (32 bytes, see Header.Put and
// ParseHeader). A member that receives a request calls Forward with the
// header and either serves it or sends it on; requests reach the owner in
// at most D hops, where D is the grid dimension (four at 100k members).
// Requests for the same key converge on the same relays, so relays can cache
// hot values on the way back.
//
// Report every request outcome with Observe. Circuit breakers, outlier
// detection, and peak-EWMA latency steer routes away from bad peers, to a
// backup in the owner's cell when the owner itself is bad. When a request is
// slower than Decision.HedgeAfter, TryHedge admits a hedge to
// Decision.Alternate within a cluster-wide budget. During an ownership
// handoff Decision.PrevOwner names the previous owner, which the new owner
// can read through before going to the origin.
//
// Route, RouteHash, RouteMany, Forward, Observe, TryHedge, Stamp, and Fresh
// take no locks, read no clock, and allocate nothing; they read an
// immutable snapshot that Run replaces when the view changes.
//
// # Keyspaces and expiry
//
// A Keyspace names an independent set of keys. Content-addressed blobs use
// a TTL of zero: they never expire, only get evicted. Metadata keyspaces use
// a TTL of at least MinTTL. The owner stamps a value with Stamp and copies
// keep that stamp; Fresh reports whether it may still be served. Expiry is
// measured with a cluster epoch the leader publishes, so a copy held anywhere
// expires within TTL plus two epoch periods even if its holder is cut off.
// Invalidate makes every existing stamp of a keyspace stale.
//
// # Lifecycle
//
// Before Run, and until this member is admitted, Route sends requests
// through a seed (KindGateway) or directly to the owner once a view is known
// (KindDirect); with neither it returns KindNoRoute. Ready reports when the
// member is Active. Leave drains the member: its partitions move away and
// the leader removes it, after which Run returns nil. Canceling Run's
// context stops the member at once; the others detect the failure and
// remove it.
//
// Package memtransport provides an in-process ControlTransport and
// LeaseStore for tests and examples.
package cachemesh
