// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Stripe-to-shard ownership routing for cross-shard fan-out.
//!
//! [`owner_shard`] is the single source of truth for "which shard owns
//! this stripe key?" used by both the binary's [`crate::bufferpool::PoolGroup`]
//! router closure and the frontend serve path. [`FanoutTable`] is the
//! per-shard routing surface the coordinator consults on the data path:
//! given a stripe key it answers whether this shard owns the stripe
//! (serve locally, zero round-trip) or, if a peer owns it, hands back
//! that peer's [`FetchChannel`] and the `buf_index` its backing was
//! registered at on this shard's socket ring (so `SEND_ZC` can target
//! the owner's page directly).

use crate::bufferpool::StripeKey;
use crate::fanout::FetchChannel;

/// Hash a [`StripeKey`] into a shard index in `0..shard_count`.
///
/// The first eight bytes of the 32-byte key are interpreted as a
/// little-endian `u64`; that distributes uniformly under a
/// content-addressed key (which is already a hash) and avoids pulling
/// in a hash crate. `shard_count` must be non-zero.
pub fn owner_shard(key: &StripeKey, shard_count: usize) -> usize {
    debug_assert!(shard_count > 0, "owner_shard requires a non-zero shard count");
    let bytes = &key.0[..8];
    let h = u64::from_le_bytes(bytes.try_into().expect("8 bytes"));
    (h as usize) % shard_count
}

/// A peer shard the coordinator can fan a stripe out to: the Send
/// channel to its [`FetchService`](crate::fanout::FetchService) plus the
/// fixed-buffer index its backing occupies on this shard's socket ring.
pub struct FanoutPeer {
    /// Command channel to the owner shard's fetch service.
    pub channel: FetchChannel,
    /// Index this peer's backing was registered at on the local socket
    /// ring (via `register_region_indexed`), used as the `SEND_ZC`
    /// fixed-buffer index when serving the owner's pinned pages.
    pub buf_index: u16,
}

/// What to do with a stripe once its owner is known.
pub enum Owner<'a> {
    /// This shard owns the stripe; serve it from the local pool (fixed
    /// buffer index 0), no cross-shard round-trip.
    Local,
    /// A peer owns the stripe; fetch it over the peer's channel and
    /// `SEND_ZC` from its backing.
    Peer(&'a FanoutPeer),
}

/// Per-shard cross-shard routing surface.
///
/// Built once during shard bring-up after every shard has published its
/// backing and fetch channel. Indexed by shard index (the position in
/// the worker-index-sorted shard order, matching [`owner_shard`] and the
/// process [`PoolGroup`](crate::bufferpool::PoolGroup)). The entry for
/// this shard's own index is `None` (served locally).
pub struct FanoutTable {
    own_shard_index: usize,
    /// `shard_index -> peer`; `None` at `own_shard_index`.
    peers: Vec<Option<FanoutPeer>>,
}

impl FanoutTable {
    /// Build a table for the shard at `own_shard_index`. `peers` is
    /// indexed by shard index and must hold `None` at `own_shard_index`
    /// and `Some` for every other shard.
    pub fn new(own_shard_index: usize, peers: Vec<Option<FanoutPeer>>) -> Self {
        debug_assert!(own_shard_index < peers.len(), "own shard index in range");
        debug_assert!(
            peers[own_shard_index].is_none(),
            "own shard must not have a peer entry"
        );
        Self {
            own_shard_index,
            peers,
        }
    }

    /// Number of shards in the fan-out group.
    pub fn shard_count(&self) -> usize {
        self.peers.len()
    }

    /// This shard's index in the group.
    pub fn own_shard_index(&self) -> usize {
        self.own_shard_index
    }

    /// Route a stripe key to its owner.
    pub fn owner_of(&self, key: &StripeKey) -> Owner<'_> {
        let idx = owner_shard(key, self.peers.len());
        if idx == self.own_shard_index {
            return Owner::Local;
        }
        match &self.peers[idx] {
            Some(peer) => Owner::Peer(peer),
            // A missing peer entry for a non-self shard is a bring-up
            // bug; fall back to the local pool rather than panicking on
            // the data path (the local pool can still resolve the stripe
            // via its own transport, just without the fan-out win).
            None => Owner::Local,
        }
    }
}
