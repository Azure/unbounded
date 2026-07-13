// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Stripe-to-shard ownership routing for cross-shard fan-out.
//!
//! [`owner_shard`] is the content-hash fallback: "which shard owns this
//! stripe key?" with no topology input. It is still used by the binary's
//! off-path [`crate::bufferpool::PoolGroup`] router closure and as the
//! spread function whenever NUMA-local routing cannot place a stripe.
//!
//! [`numa_local_shard`] is the NUMA-aware data-path router: it maps a
//! stripe key to the drive that backs it (via [`disk_for`], the same
//! function the local store uses), looks up that drive's NUMA node, and
//! picks a serving shard *on that node* (spreading across the node's
//! shards with a secondary hash). This keeps the disk read, checksum,
//! and `SEND_ZC` backing on one NUMA node while still spreading load
//! across its cores. When the drive's node has no serving shard (or the
//! drive is unpinned / its node is unknown) it falls back to
//! [`owner_shard`] and flags the pick as cross-NUMA.
//!
//! [`FanoutTable`] is the per-shard routing surface the coordinator
//! consults on the data path: given a stripe key and its in-stripe
//! offset it answers whether this shard owns the stripe (serve locally,
//! zero round-trip) or, if a peer owns it, hands back that peer's
//! [`FetchChannel`] and the `buf_index` its backing was registered at on
//! this shard's socket ring (so `SEND_ZC` can target the owner's page
//! directly).

use std::collections::BTreeMap;
use std::sync::Arc;

use crate::bufferpool::StripeKey;
use crate::fanout::FetchChannel;
use crate::storage::disk_for;
use crate::storage::disks::CacheDirectorySet;

/// Hash a [`StripeKey`] into a shard index in `0..shard_count`.
///
/// The first eight bytes of the 32-byte key are interpreted as a
/// little-endian `u64`; that distributes uniformly under a
/// content-addressed key (which is already a hash) and avoids pulling
/// in a hash crate. `shard_count` must be non-zero.
///
/// This is the topology-free fallback. The data path prefers
/// [`numa_local_shard`], which only falls back here when a stripe's
/// drive has no co-located serving shard.
pub fn owner_shard(key: &StripeKey, shard_count: usize) -> usize {
    debug_assert!(
        shard_count > 0,
        "owner_shard requires a non-zero shard count"
    );
    let bytes = &key.0[..8];
    let h = u64::from_le_bytes(bytes.try_into().expect("8 bytes"));
    (h as usize) % shard_count
}

/// Maps each NUMA node to the serving shards pinned to it.
///
/// Built once at shard-layer bring-up from the peer set (every peer
/// carries its `worker.numa`). Shards whose NUMA node is unknown
/// (`None`) are still counted in [`shard_count`](Self::shard_count) and
/// remain reachable through the [`owner_shard`] fallback, but they are
/// not associated with any node bucket.
pub struct NumaShardTable {
    /// Total number of serving shards in the fan-out group, including
    /// shards with an unknown NUMA node.
    shard_count: usize,
    /// `numa node -> sorted, deduped shard indices on that node`.
    by_node: BTreeMap<u16, Vec<usize>>,
}

impl NumaShardTable {
    /// Build the table from `(shard_index, numa)` pairs covering every
    /// shard in the group exactly once. The number of pairs becomes
    /// [`shard_count`](Self::shard_count); pairs with `Some(numa)` are
    /// bucketed by node.
    pub fn from_shards(shards: impl IntoIterator<Item = (usize, Option<u16>)>) -> Self {
        let mut shard_count = 0usize;
        let mut by_node: BTreeMap<u16, Vec<usize>> = BTreeMap::new();

        for (shard_index, numa) in shards {
            shard_count += 1;

            if let Some(node) = numa {
                by_node.entry(node).or_default().push(shard_index);
            }
        }

        for shards in by_node.values_mut() {
            shards.sort_unstable();
            shards.dedup();
        }

        Self {
            shard_count,
            by_node,
        }
    }

    /// Number of shards in the fan-out group (including shards with an
    /// unknown NUMA node).
    pub fn shard_count(&self) -> usize {
        self.shard_count
    }

    /// The serving shards pinned to `numa`, or `None` if that node has
    /// no shard.
    fn shards_on(&self, numa: u16) -> Option<&[usize]> {
        self.by_node.get(&numa).map(Vec::as_slice)
    }
}

/// The outcome of [`numa_local_shard`]: the chosen serving shard and
/// whether the choice had to leave the drive's NUMA node.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ShardPick {
    /// Index of the serving shard to route to.
    pub shard: usize,
    /// `true` when the pick could not be placed on the drive's NUMA
    /// node (the node has no serving shard, or the drive is unpinned /
    /// its node is unknown while disks are present). Drives the
    /// cross-NUMA fetch metric.
    pub cross_numa: bool,
}

/// Route a stripe to a serving shard, preferring a shard co-located on
/// the NUMA node of the drive that backs the stripe.
///
/// `drive_numa` is the live, path-sorted drive -> NUMA-node table
/// published by the [`DiskChannelDirectory`] (index `i` is the NUMA node
/// of the drive [`disk_for`] selects when `num_disks == drive_numa.len()`).
/// When it is empty (no disks open, or none pinned to a node on this dev
/// box) there is no topology to honor, so we spread with [`owner_shard`]
/// and do *not* count that as cross-NUMA.
pub fn numa_local_shard(
    key: &StripeKey,
    stripe_off: u64,
    drive_numa: &[Option<u16>],
    numa_shards: &NumaShardTable,
) -> ShardPick {
    let shard_count = numa_shards.shard_count();

    // No drive topology: nothing to be local to. Spread, not cross-NUMA.
    if drive_numa.is_empty() {
        return ShardPick {
            shard: owner_shard(key, shard_count),
            cross_numa: false,
        };
    }

    let drive = disk_for(key, stripe_off, drive_numa.len());
    let target = drive_numa[drive];

    match target.and_then(|node| numa_shards.shards_on(node)) {
        Some(local) if !local.is_empty() => ShardPick {
            shard: local[intra_node_pick(key, local.len())],
            cross_numa: false,
        },
        // The drive's node has no serving shard, or the drive is
        // unpinned / its node unknown: spread across all shards and flag
        // the cross-NUMA hop.
        _ => ShardPick {
            shard: owner_shard(key, shard_count),
            cross_numa: true,
        },
    }
}

/// Pick a slot in `0..n` for intra-node spread.
///
/// Uses key bytes `[8..16]` as a little-endian `u64`, independent of
/// both [`owner_shard`] (bytes `[..8]`) and [`disk_for`] (a checksum
/// over the whole key plus offset), so co-located shards see an even
/// share of a node's stripes. `n` must be non-zero.
fn intra_node_pick(key: &StripeKey, n: usize) -> usize {
    debug_assert!(n > 0, "intra_node_pick requires a non-zero slot count");
    let bytes = &key.0[8..16];
    let h = u64::from_le_bytes(bytes.try_into().expect("8 bytes"));
    (h as usize) % n
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
/// backing, fetch channel, and NUMA node. Indexed by shard index (the
/// position in the worker-index-sorted shard order). The entry for this
/// shard's own index is `None` (served locally).
///
/// Routing consults the live [`DiskChannelDirectory`] drive -> NUMA
/// table on every call so that a disk hot-swap is reflected immediately;
/// all shards share the same directory `Arc`, so they agree on the drive
/// topology that the local store also uses to place pages.
pub struct FanoutTable {
    own_shard_index: usize,
    /// `shard_index -> peer`; `None` at `own_shard_index`.
    peers: Vec<Option<FanoutPeer>>,
    /// NUMA node -> serving shards, for NUMA-local placement.
    numa_shards: NumaShardTable,
    /// Live drive -> NUMA table source, shared with the local store.
    disk_channels: Arc<CacheDirectorySet>,
}

impl FanoutTable {
    /// Build a table for the shard at `own_shard_index`. `peers` is
    /// indexed by shard index and must hold `None` at `own_shard_index`
    /// and `Some` for every other shard. `numa_shards` must describe the
    /// same group (one entry per shard) and `disk_channels` is the live
    /// drive -> NUMA directory shared with the local store.
    pub fn new(
        own_shard_index: usize,
        peers: Vec<Option<FanoutPeer>>,
        numa_shards: NumaShardTable,
        disk_channels: Arc<CacheDirectorySet>,
    ) -> Self {
        debug_assert!(own_shard_index < peers.len(), "own shard index in range");
        debug_assert!(
            peers[own_shard_index].is_none(),
            "own shard must not have a peer entry"
        );
        debug_assert_eq!(
            numa_shards.shard_count(),
            peers.len(),
            "numa table must cover every shard"
        );
        Self {
            own_shard_index,
            peers,
            numa_shards,
            disk_channels,
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

    /// Route a stripe key (at in-stripe byte offset `stripe_off`) to its
    /// owner, preferring a shard on the backing drive's NUMA node.
    ///
    /// `stripe_off` is the in-stripe offset of the requested slice (the
    /// frontend's `StripeSlice::intra_offset`); it selects the same
    /// backing drive the local store would read, so the serving shard
    /// lands on that drive's NUMA node.
    pub fn owner_of(&self, key: &StripeKey, stripe_off: u64) -> Owner<'_> {
        self.owner_of_cache(key, None, stripe_off)
    }

    pub fn owner_of_cache(
        &self,
        key: &StripeKey,
        cache_id: Option<&str>,
        stripe_off: u64,
    ) -> Owner<'_> {
        let disk_snapshot = self.disk_channels.snapshot(cache_id);
        let drive_numa = disk_snapshot
            .as_ref()
            .map(|snapshot| snapshot.drive_numa.as_slice())
            .unwrap_or_default();
        let pick = numa_local_shard(key, stripe_off, drive_numa, &self.numa_shards);

        if pick.cross_numa {
            crate::metrics::fanout_cross_numa_fetch();
        }

        let idx = pick.shard;
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

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a 32-byte key whose `[..8]` (owner_shard) and `[8..16]`
    /// (intra_node_pick) hash inputs are set independently.
    fn key_with(owner_bytes: u64, intra_bytes: u64) -> StripeKey {
        let mut k = [0u8; 32];
        k[..8].copy_from_slice(&owner_bytes.to_le_bytes());
        k[8..16].copy_from_slice(&intra_bytes.to_le_bytes());
        StripeKey(k)
    }

    #[test]
    fn owner_shard_is_in_range_and_stable() {
        let key = key_with(0x0123_4567_89ab_cdef, 0);
        for shard_count in 1..=8 {
            let a = owner_shard(&key, shard_count);
            let b = owner_shard(&key, shard_count);
            assert_eq!(a, b, "owner_shard must be deterministic");
            assert!(a < shard_count, "owner_shard in range");
        }
    }

    #[test]
    fn owner_shard_uses_low_eight_bytes() {
        // shard_count 16 keeps the low nibble of the LE u64.
        let key = key_with(5, 0);
        assert_eq!(owner_shard(&key, 16), 5);
    }

    #[test]
    fn intra_node_pick_is_in_range_and_uses_second_eight_bytes() {
        let key = key_with(0, 7);
        for n in 1..=8 {
            assert!(intra_node_pick(&key, n) < n);
        }
        assert_eq!(intra_node_pick(&key, 16), 7);
    }

    #[test]
    fn numa_table_counts_all_shards_and_buckets_known_nodes() {
        // Shards 0,2 on node 0; shard 1 on node 1; shard 3 unknown.
        let table =
            NumaShardTable::from_shards([(0, Some(0)), (1, Some(1)), (2, Some(0)), (3, None)]);
        assert_eq!(table.shard_count(), 4);
        assert_eq!(table.shards_on(0), Some(&[0usize, 2][..]));
        assert_eq!(table.shards_on(1), Some(&[1usize][..]));
        assert_eq!(table.shards_on(2), None);
    }

    #[test]
    fn numa_table_sorts_and_dedups_buckets() {
        let table = NumaShardTable::from_shards([(2, Some(0)), (0, Some(0)), (2, Some(0))]);
        assert_eq!(table.shards_on(0), Some(&[0usize, 2][..]));
    }

    #[test]
    fn empty_drive_topology_spreads_without_cross_numa() {
        // No disks open: spread via owner_shard, not flagged cross-NUMA.
        let table = NumaShardTable::from_shards([(0, Some(0)), (1, Some(0)), (2, Some(1))]);
        let key = key_with(7, 0);
        let pick = numa_local_shard(&key, 0, &[], &table);
        assert_eq!(pick.shard, owner_shard(&key, 3));
        assert!(!pick.cross_numa);
    }

    #[test]
    fn picks_a_shard_on_the_drive_numa_node() {
        // Two nodes, two drives. Force the key+offset onto a drive whose
        // node has shards and assert the pick is one of that node's
        // shards and not flagged cross-NUMA.
        let table =
            NumaShardTable::from_shards([(0, Some(0)), (1, Some(0)), (2, Some(1)), (3, Some(1))]);
        // Drive 0 -> node 0 (shards 0,1); drive 1 -> node 1 (shards 2,3).
        let drive_numa = [Some(0u16), Some(1u16)];

        // Probe several keys; each pick must be on its drive's node.
        for n in 0..64u64 {
            let key = key_with(n, n.wrapping_mul(2654435761));
            let pick = numa_local_shard(&key, 0, &drive_numa, &table);
            assert!(!pick.cross_numa, "node has shards => local");
            let drive = disk_for(&key, 0, drive_numa.len());
            let node = drive_numa[drive].unwrap();
            let allowed = table.shards_on(node).unwrap();
            assert!(
                allowed.contains(&pick.shard),
                "pick {} must be on node {}'s shards {:?}",
                pick.shard,
                node,
                allowed
            );
        }
    }

    #[test]
    fn intra_node_spread_covers_all_shards_on_a_node() {
        // A node with two shards must see both chosen across many keys.
        let table = NumaShardTable::from_shards([(0, Some(0)), (1, Some(0))]);
        let drive_numa = [Some(0u16)];
        let mut seen = [false; 2];
        for n in 0..256u64 {
            let key = key_with(0, n);
            let pick = numa_local_shard(&key, 0, &drive_numa, &table);
            assert!(!pick.cross_numa);
            seen[pick.shard] = true;
        }
        assert!(seen[0] && seen[1], "intra-node spread must hit both shards");
    }

    #[test]
    fn drive_on_node_without_shards_falls_back_cross_numa() {
        // Single drive on node 9, but no shard lives on node 9.
        let table = NumaShardTable::from_shards([(0, Some(0)), (1, Some(1))]);
        let drive_numa = [Some(9u16)];
        let key = key_with(3, 0);
        let pick = numa_local_shard(&key, 0, &drive_numa, &table);
        assert_eq!(pick.shard, owner_shard(&key, 2));
        assert!(pick.cross_numa, "no shard on drive's node => cross-NUMA");
    }

    #[test]
    fn unpinned_drive_falls_back_cross_numa() {
        // Drive present but its NUMA node is unknown (None).
        let table = NumaShardTable::from_shards([(0, Some(0)), (1, Some(1))]);
        let drive_numa = [None];
        let key = key_with(3, 0);
        let pick = numa_local_shard(&key, 0, &drive_numa, &table);
        assert_eq!(pick.shard, owner_shard(&key, 2));
        assert!(pick.cross_numa, "unpinned drive => cross-NUMA");
    }

    #[test]
    fn pick_is_deterministic_for_same_inputs() {
        let table = NumaShardTable::from_shards([(0, Some(0)), (1, Some(0)), (2, Some(1))]);
        let drive_numa = [Some(0u16), Some(1u16)];
        let key = key_with(42, 99);
        let a = numa_local_shard(&key, 8, &drive_numa, &table);
        let b = numa_local_shard(&key, 8, &drive_numa, &table);
        assert_eq!(a.shard, b.shard);
        assert_eq!(a.cross_numa, b.cross_numa);
    }

    #[test]
    fn fanout_table_single_shard_is_always_local() {
        let table = NumaShardTable::from_shards([(0, None)]);
        let fanout = FanoutTable::new(0, vec![None], table, CacheDirectorySet::new());
        let key = key_with(123, 456);
        assert!(matches!(fanout.owner_of(&key, 0), Owner::Local));
        assert_eq!(fanout.shard_count(), 1);
        assert_eq!(fanout.own_shard_index(), 0);
    }
}
