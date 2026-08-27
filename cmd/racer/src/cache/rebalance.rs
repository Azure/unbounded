use std::rc::Rc;
use std::time::{Duration, Instant};

use crate::layout;
use crate::runtime::{self, CoreId};
use crate::server::Worker;

use super::store::Chunk;
use super::{Cache, Local, at, chunk_slots, pool};

/// How often core 0 reconsiders how much of the free tail the cache holds.
pub(super) const REBALANCE: Duration = Duration::from_secs(1);
/// The share of `policy.cache_index_bytes` the cache builds before serving a read. The policy
/// is a ceiling, not an allocation, and paying it up front costs DRAM and startup latency. The
/// rebalance grows the cache out of the pool instead.
pub(super) const OPEN_SHARE: u64 = 64;
/// Maximum proportional growth from the free pool in one interval.
const GROWTH_SHARE: u64 = 4;

/// The tail chunks nobody is holding, plus the bookkeeping the rebalance needs.
#[derive(Default)]
pub(crate) struct Pool {
    pub(super) free: Vec<u64>,
    pub(super) evicted: u64,
    pub(super) missed: u64,
    pub(super) sampled: Option<Instant>,
    pub(super) running: bool,
}

/// What one core's store did with what it holds, gathered by the rebalance.
#[derive(Clone, Copy, Default)]
struct Census {
    evicted: u64,
    missed: u64,
    slots: u64,
    chunks: u64,
}

/// How many of `chunks` the cache opens holding. One class, so this is only the index budget
/// talking: the rest of the tail stays in the pool until demand asks for it.
pub(super) fn plan_chunks(chunks: u64, budget: u64) -> u64 {
    chunks.min(budget / chunk_slots() as u64)
}

/// Whether the cache is short of media: it is evicting, or it is holding nothing and missing.
pub(super) fn wants(evicted: u64, missed: u64, bytes: u64) -> bool {
    evicted > 0 || (bytes == 0 && missed > 0)
}

impl Cache {
    /// The tail this cache was given, and the part of it nobody is holding.
    pub fn tail_bytes(&self, worker: &Worker) -> (u64, u64) {
        let idle = pool(worker, |p| p.free.len() as u64).unwrap_or(0);
        (
            self.tail.1 * layout::CHUNK_BYTES,
            idle * layout::CHUNK_BYTES,
        )
    }

    fn census_in(l: &Local) -> Census {
        Census {
            evicted: l.store.evicted,
            missed: l.stats.misses,
            slots: l.store.slots.len() as u64,
            chunks: l.store.chunks.len() as u64,
        }
    }

    /// Grow the cache out of the free tail while it is short of media.
    pub(super) async fn rebalance(&'static self, worker: Rc<Worker>) {
        let cores = runtime::cores();
        let mut sum = Census::default();
        let mut chunks = Vec::<u64>::with_capacity(cores);
        for c in 0..cores {
            let c = CoreId::new(c).expect("a worker index is a worker");
            let row = at(c, |_, l| Cache::census_in(l)).await;
            sum.evicted += row.evicted;
            sum.missed += row.missed;
            sum.slots += row.slots;
            chunks.push(row.chunks);
        }

        let Some((evicted, missed)) = pool(&worker, |p| {
            let d = |now: u64, then: &mut u64| -> u64 {
                let out = now.saturating_sub(*then);
                *then = now;
                out
            };
            (d(sum.evicted, &mut p.evicted), d(sum.missed, &mut p.missed))
        }) else {
            return;
        };

        let bytes = chunks.iter().sum::<u64>() * layout::CHUNK_BYTES;
        if !wants(evicted, missed, bytes) {
            return;
        }
        let cost = chunk_slots() as u64;
        if sum.slots + cost > self.budget {
            return;
        }
        let room = (self.budget - sum.slots) / cost;
        let idle: Vec<u64> = pool(&worker, |p| {
            let grow = (chunks.iter().sum::<u64>() / GROWTH_SHARE)
                .clamp(1, room)
                .min(p.free.len() as u64);
            let n = p.free.len() - grow as usize;
            p.free.split_off(n)
        })
        .unwrap_or_default();
        for off in idle {
            self.place(&mut chunks, Chunk { off }).await;
        }
    }

    /// Cores that can hold a slot, emptiest first.
    fn order(&self, chunks: &[u64]) -> Vec<CoreId> {
        let mut v: Vec<CoreId> = (0..self.shards)
            .map(|c| CoreId::new(c).expect("a shard is a worker"))
            .collect();
        v.sort_by_key(|&c| chunks.get(c.index()).copied().unwrap_or(0));
        v
    }

    async fn place(&'static self, chunks: &mut [u64], c: Chunk) {
        let order = self.order(chunks);
        let Some(&core) = order.first() else {
            return;
        };
        at(core, move |_, l| l.store.push_chunk(c)).await;
        chunks[core.index()] += 1;
    }
}
