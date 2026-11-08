// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package cachemesh makes routing decisions for a distributed,
// groupcache-style cache spread across up to 100,000 processes. It decides
// which member owns a key, which neighbor a request goes to next, which
// neighbors a process keeps connections to, when a cached metadata value
// has expired, and who is in the cluster. It holds no values and opens no
// connections: the application brings its own transports, storage, and
// requests. The design is in designs/cachemesh.md.
//
// # Members and the control plane
//
// Each process creates one Mesh with New and drives it with Run. Members
// find each other through Config.Seeds (control endpoints of existing
// members) or the leader Lease (Config.Lease, for example a
// *kubelease.Store; seeds published in the Lease are used like configured
// ones). One member, elected through the Lease, publishes versioned
// membership views; every other member learns them over the control
// transport, and only a few candidates ever call the API server. A member
// with a nil Lease can only join through seeds and never becomes leader.
//
// Set Config.ExpectedSize to roughly the cluster size. It sets how often
// members without a view read the Lease, and so how long the first member
// of a new cluster takes to bootstrap (about ExpectedSize/20 seconds) and
// how hard a cold-restarting cluster hits the API server (about 20 reads
// per second when the value is right). See Config.ExpectedSize.
//
// # Wiring the control transport
//
// The control plane is request/response. Implement ControlTransport (Send
// one request to an Endpoint, return its response) over HTTP, gRPC, or
// anything else, and call Mesh.ServeControl for every request received at
// Config.Addr; write back the bytes it returns. ServeControl answers
// malformed input itself, so a non-nil error means only that the Mesh is
// not running (ErrNotRunning) or the message is too large
// (ErrMessageTooLarge); report it as a transport failure. Bound request and
// response bodies with MaxControlMessageSize. Send must honor its context:
// Run waits for outstanding calls when it returns. See the HTTP example.
//
// # Data plane
//
// Members are placed in a grid of cells; each keeps at most MaxNeighbors
// data-plane neighbors (its cell and the adjacent cells), which a
// NeighborWatcher learns about, so connections stay bounded at any cluster
// size. Requests travel hop by hop over the application's data-plane
// transport, carrying the 32-byte Header (Header.Put, ParseHeader). A
// PeerRef names a member by ID and control Addr; map it to the data-plane
// address with your own convention (for example the same host with another
// port).
//
// The origin calls Route (or RouteHash, RouteMany); a member that receives
// a request calls ParseHeader and Forward. Both return a Decision; act on
// its Kind:
//
//   - KindSelf: serve the request here. If Owner is not this member
//     (FlagBackup set), this member stands in for an unusable owner.
//     PrevOwner, when set, is the previous owner during a handoff: on a
//     miss, read through from it before going to the origin.
//   - KindRelay, KindOwner, KindBackup, KindDirect: send to Next with
//     Header. Relays may cache responses on the way back.
//   - KindGateway: this process is not (yet) a member; send to Next, a
//     member that relays on its behalf.
//   - KindNoRoute: no view and no gateway yet; fail the request or fetch
//     from the origin.
//
// Forward returns ErrTTLExhausted when the request must not travel further
// (serve it here or fail it) and ErrBadHeader for a malformed header
// (reject it). Report every outcome of a request sent to a peer, hedges
// included, with Observe: circuit breakers, outlier ejection, and
// peak-EWMA latency then steer routes away from bad peers, to a backup in
// the owner's cell when the owner itself is bad. If no response arrives
// within HedgeAfter and Alternate is not zero, ask TryHedge; when it
// returns true, send a copy to Alternate with FlagHedge set. After a
// failure, Alternate is also the retry target.
//
// Route, RouteHash, RouteMany, Forward, Observe, TryHedge, Stamp, and Fresh
// are safe for concurrent use, take no locks, read no clock, and allocate
// nothing; they read an immutable snapshot that Run replaces when the view
// changes. They work before Run, routing through seeds.
//
// # Keyspaces and expiry
//
// A Keyspace names an independent set of keys with its own placement.
// Content-addressed blobs use a TTL of zero: they never expire, only get
// evicted, and Fresh always reports true. Metadata keyspaces use a TTL of
// at least MinTTL (5 minutes). The owner stamps a value with Stamp when it
// fetches it from the origin, every copy keeps that stamp, and each holder
// checks Fresh before serving it. Expiry is measured with a cluster epoch
// the leader publishes, so a copy held anywhere expires within TTL plus
// two epoch periods even if its holder is cut off. Invalidate makes every
// existing stamp of a keyspace stale and waits until this member has seen
// it.
//
// # Lifecycle
//
// Run may be called once; it returns ctx.Err() when ctx ends and nil after
// Leave completes. Until this member is admitted, Route sends requests
// through a seed (KindGateway). Ready reports when the member is Active.
// Leave drains the member: its partitions move away and the leader removes
// it, after which Run returns nil. Canceling Run's context stops the member
// at once; the others detect the failure and remove it. Hooks and the
// NeighborWatcher are called without the Mesh lock and may call Mesh
// methods. The zero Mesh is not usable; use New.
//
// Package memtransport provides an in-process ControlTransport and
// LeaseStore for tests and examples.
package cachemesh
