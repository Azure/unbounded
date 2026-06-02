// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::sync::Arc;

use crate::bufferpool::traits::Req;
use crate::runtime::WorkerIdx;

/// Static metadata about one NUMA shard. Cheap to clone; held by
/// `PoolGroup` so cross-shard routing can answer "which shard
/// owns request R?" without touching the shard's `Pool` (which
/// lives only inside the shard thread).
#[derive(Clone, Debug)]
pub struct ShardDescriptor {
    /// Worker slot this shard runs on. Matches the `WorkerIdx`
    /// passed to `Threading::spawn_pinned` for the shard's thread.
    pub worker_idx: WorkerIdx,
    /// NUMA node this shard is pinned against, if the runtime is
    /// NUMA-aware. `None` for the TCP-fallback / non-pinning case.
    pub numa: Option<u16>,
}

/// Routing function: `req -> shard index`. Returns an index into
/// the `PoolGroup`'s shard table. Implementations should be pure
/// (no I/O, no interior mutability) so a single request always
/// routes to the same shard for the lifetime of the group.
pub trait ShardRouter<R: Req>: Send + Sync + 'static {
    fn route(&self, req: &R) -> usize;
}

impl<R, F> ShardRouter<R> for F
where
    R: Req,
    F: Fn(&R) -> usize + Send + Sync + 'static,
{
    fn route(&self, req: &R) -> usize {
        (self)(req)
    }
}

/// Per-process fan-out over NUMA shards. `R` is the bufferpool
/// request type; the embedder selects the shard for each request
/// via [`ShardRouter`].
///
/// Construction: build a `Vec<ShardDescriptor>` mirroring whatever
/// per-shard threads the embedder spawned, then wrap in a
/// `PoolGroup` together with a router closure. The descriptors are
/// authoritative for membership; the router is authoritative for
/// "which one for this request."
pub struct PoolGroup<R: Req + 'static> {
    shards: Arc<[ShardDescriptor]>,
    router: Arc<dyn ShardRouter<R>>,
}

impl<R: Req + 'static> PoolGroup<R> {
    pub fn new<S>(shards: Vec<ShardDescriptor>, router: S) -> Self
    where
        S: ShardRouter<R>,
    {
        assert!(!shards.is_empty(), "PoolGroup requires at least one shard");
        Self {
            shards: Arc::from(shards.into_boxed_slice()),
            router: Arc::new(router),
        }
    }

    pub fn shards(&self) -> &[ShardDescriptor] {
        &self.shards
    }

    pub fn len(&self) -> usize {
        self.shards.len()
    }

    /// Returns `true` iff [`Self::len`] is zero. Provided to match
    /// clippy's `len_zero` lint expectation; `PoolGroup::new`
    /// panics on empty input so in practice this always returns
    /// `false`.
    pub fn is_empty(&self) -> bool {
        self.shards.is_empty()
    }

    /// Resolve a request to the descriptor of the shard that owns
    /// it. Routing is delegated to the embedder's `ShardRouter`;
    /// `PoolGroup` only enforces that the returned index is in
    /// range and panics otherwise (a routing bug is a programming
    /// error, not a runtime error).
    pub fn route(&self, req: &R) -> &ShardDescriptor {
        let idx = self.router.route(req);
        assert!(
            idx < self.shards.len(),
            "ShardRouter returned out-of-range index: {idx} >= {}",
            self.shards.len(),
        );
        &self.shards[idx]
    }
}

impl<R: Req + 'static> Clone for PoolGroup<R> {
    fn clone(&self) -> Self {
        Self {
            shards: self.shards.clone(),
            router: self.router.clone(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::types::StripeKey;

    #[derive(Clone)]
    struct K(u8);
    impl Req for K {
        fn key(&self) -> StripeKey {
            StripeKey([self.0; 32])
        }
    }

    #[test]
    fn route_dispatches_by_closure() {
        let shards = vec![
            ShardDescriptor {
                worker_idx: WorkerIdx(0),
                numa: Some(0),
            },
            ShardDescriptor {
                worker_idx: WorkerIdx(1),
                numa: Some(1),
            },
        ];
        let g: PoolGroup<K> = PoolGroup::new(shards, |k: &K| (k.0 as usize) % 2);
        assert_eq!(g.len(), 2);
        assert_eq!(g.route(&K(0)).worker_idx, WorkerIdx(0));
        assert_eq!(g.route(&K(1)).worker_idx, WorkerIdx(1));
        assert_eq!(g.route(&K(4)).worker_idx, WorkerIdx(0));
    }

    #[test]
    #[should_panic(expected = "out-of-range index")]
    fn route_panics_on_out_of_range_index() {
        let shards = vec![ShardDescriptor {
            worker_idx: WorkerIdx(0),
            numa: None,
        }];
        let g: PoolGroup<K> = PoolGroup::new(shards, |_: &K| 7);
        let _ = g.route(&K(0));
    }

    #[test]
    #[should_panic(expected = "at least one shard")]
    fn new_panics_on_empty() {
        let _: PoolGroup<K> = PoolGroup::new(Vec::new(), |_: &K| 0);
    }

    #[test]
    fn descriptor_clone_and_len() {
        let shards = vec![ShardDescriptor {
            worker_idx: WorkerIdx(3),
            numa: None,
        }];
        let g: PoolGroup<K> = PoolGroup::new(shards, |_: &K| 0);
        assert_eq!(g.len(), 1);
        assert!(!g.is_empty());
        let g2 = g.clone();
        assert_eq!(g2.shards()[0].worker_idx, WorkerIdx(3));
    }
}
