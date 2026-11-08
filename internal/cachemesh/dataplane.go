// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import "time"

// The methods in this file are the data plane. Each loads the current
// routing snapshot with one atomic load and takes no locks, reads no clock,
// and allocates nothing.

// Route routes key in keyspace ks from this member. Send the request with
// d.Header to d.Next, or serve it locally when d.Kind is KindSelf. Before
// this member is admitted, requests go to a seed (KindGateway); without
// seeds they go directly to the owner (KindDirect) once a view is known,
// otherwise the decision is KindNoRoute.
func (m *Mesh) Route(ks Keyspace, key []byte) Decision {
	return m.st.Load().Route(ks.seed, key)
}

// RouteHash routes a key whose hash is h = ks.Hash(key), for callers that
// already hashed the key.
func (m *Mesh) RouteHash(_ Keyspace, h uint64) Decision {
	return m.st.Load().RouteHash(h)
}

// RouteMany routes keys into out[:len(keys)]; out must be at least as long
// as keys. All keys are routed against the same snapshot.
func (m *Mesh) RouteMany(ks Keyspace, keys [][]byte, out []Decision) {
	m.st.Load().RouteMany(ks.seed, keys, out)
}

// Forward routes a request received with header h at a relay. On
// ErrTTLExhausted the request must not be sent on (serve it locally or
// fail it); on ErrBadHeader it must be rejected. A header showing that the
// sender has a newer view makes the control plane catch up.
func (m *Mesh) Forward(h Header) (Decision, error) {
	return m.st.Load().Forward(h)
}

// Observe reports the outcome of a request sent to p, feeding circuit
// breakers, outlier detection, and latency tracking. Report every request
// sent to a peer from a Decision, including hedges.
func (m *Mesh) Observe(p PeerRef, r Result, latency time.Duration) {
	m.health.Observe(p.Slot, r, latency)
}

// TryHedge reports whether a hedge of the request carrying h fits the hedge
// budget. Call it when d.HedgeAfter has passed without a response, and if
// it returns true send a copy to d.Alternate with FlagHedge set.
func (m *Mesh) TryHedge(h Header) bool {
	return m.st.Load().TryHedge(h)
}

// Stamp returns the expiry stamp for a value of ks issued now. The owner
// stamps values; copies keep the owner's stamp.
func (m *Mesh) Stamp(ks Keyspace) Stamp {
	return m.st.Load().Stamp(ks.seed, ks.ttl)
}

// Fresh reports whether a value of ks stamped with s may still be served:
// its epoch has not passed and ks has not been invalidated since.
func (m *Mesh) Fresh(ks Keyspace, s Stamp) bool {
	return m.st.Load().Fresh(ks.seed, s)
}
