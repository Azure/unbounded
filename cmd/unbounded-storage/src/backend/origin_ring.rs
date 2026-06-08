// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Where an origin backend gets the [`NetworkRing`] it drives its
//! cache-miss fetches on.
//!
//! An origin fetch (a ranged GET or a length HEAD) dials the origin over
//! a [`NetworkRing`] and self-pumps via [`NetHandle`]'s `'static`
//! futures (each future calls `progress()` on its own ring inside
//! `poll`). Which ring a fetch may touch depends entirely on which
//! thread runs it, because a `NetworkRing` is single-threaded
//! (`Rc<RefCell<..>>`, `!Sync`):
//!
//! - [`OriginRing::Shard`]: the pool-transport backend runs on the
//!   pinned shard thread. It reuses that shard's own socket ring, the
//!   very ring the shard's tick hook progresses. All access is on one
//!   thread, so the shared ring stays sound under cooperative
//!   interleaving.
//! - [`OriginRing::WorkerLocal`]: the RPC handler's backend serves a
//!   peer cache-miss on an ephemeral `fabric-rpc-worker` thread (one OS
//!   thread per inbound RPC; see `fabric::rpc`). It must NOT touch the
//!   shard ring: the shard thread progresses that ring concurrently, and
//!   a cross-thread borrow of its `RefCell` panics with "already
//!   borrowed". Instead each worker thread lazily builds its OWN ring in
//!   thread-local storage and drives the fetch entirely there.
//!
//! The worker-local ring registers the per-thread scratch backing as
//! fixed buffer index 0 (via [`FixedRegion`]) so an origin fetch can
//! `recv_fixed` response bytes directly into scratch pages with no
//! intermediate heap copy. The region is threaded through [`OriginRing`]
//! as a `(base, len)` pair because the scratch `Backing`'s Drop carrier
//! cannot be re-synthesized from raw parts across the thread boundary.

use std::cell::RefCell;
use std::io;
use std::rc::Rc;

use crate::ring::{NetHandle, NetworkRing};

/// A raw fixed-buffer region (`base`, `len`) to register as fixed buffer
/// index 0 on a worker-local ring. `len` is `page_size * page_count` of
/// the scratch backing. Carried by value (not as a `&Backing`) so it can
/// cross into the worker thread without the backing's Drop carrier.
#[derive(Clone, Copy)]
pub struct FixedRegion {
    pub base: *mut u8,
    pub len: usize,
}

/// Source of the [`NetHandle`] an origin backend uses for one fetch.
///
/// Cloned into the backend at construction. [`OriginRing::handle`]
/// resolves it to a concrete handle on the calling thread; see the
/// module docs for why the two variants exist.
#[derive(Clone)]
pub enum OriginRing {
    /// Reuse the shard's socket ring. Sound only on the shard thread.
    Shard(Rc<RefCell<NetworkRing>>),
    /// Use (and lazily create) a ring private to the current thread,
    /// built with `queue_depth` submission/completion slots. When
    /// `region` is `Some`, it is registered as fixed buffer index 0 on
    /// first use so origin fetches can `recv_fixed` into scratch pages.
    WorkerLocal {
        queue_depth: u32,
        region: Option<FixedRegion>,
    },
}

impl OriginRing {
    /// Obtain a [`NetHandle`] for issuing origin ops on the current
    /// thread.
    ///
    /// `Shard` clones the shard ring. `WorkerLocal` returns a handle
    /// over this thread's ring, building it on first use; the ring
    /// lives in thread-local storage and is reused by every fetch that
    /// later runs on the same thread.
    pub fn handle(&self) -> io::Result<NetHandle> {
        match self {
            OriginRing::Shard(ring) => Ok(NetHandle::new(Rc::clone(ring))),
            OriginRing::WorkerLocal {
                queue_depth,
                region,
            } => worker_local_handle(*queue_depth, *region),
        }
    }
}

thread_local! {
    /// The current thread's private origin ring, created on first
    /// [`OriginRing::WorkerLocal`] use. An ephemeral `fabric-rpc-worker`
    /// thread drops it when the thread exits; a reused worker thread
    /// keeps it for subsequent requests.
    static WORKER_RING: RefCell<Option<Rc<RefCell<NetworkRing>>>> = const { RefCell::new(None) };
}

fn worker_local_handle(queue_depth: u32, region: Option<FixedRegion>) -> io::Result<NetHandle> {
    WORKER_RING.with(|cell| {
        if let Some(ring) = cell.borrow().as_ref() {
            return Ok(NetHandle::new(Rc::clone(ring)));
        }
        let net = NetworkRing::new(queue_depth)?;
        if let Some(region) = region {
            net.register_region(region.base, region.len)?;
        }
        let ring = Rc::new(RefCell::new(net));
        *cell.borrow_mut() = Some(Rc::clone(&ring));
        Ok(NetHandle::new(ring))
    })
}

#[cfg(test)]
mod tests {
    use std::rc::Rc;

    use super::*;

    /// A `Shard` ring hands back a handle over the very ring it was
    /// built from: the handle's `Rc` shares the same allocation, so the
    /// shard thread and the backend drive one ring.
    #[test]
    fn shard_handle_shares_the_shard_ring() {
        let ring = Rc::new(RefCell::new(NetworkRing::new(8).expect("ring")));
        let origin = OriginRing::Shard(Rc::clone(&ring));
        // Two handles drawn from the same shard source must alias the
        // same ring allocation as the original `Rc`.
        let before = Rc::strong_count(&ring);
        let _h1 = origin.handle().expect("handle");
        let _h2 = origin.handle().expect("handle");
        assert!(
            Rc::strong_count(&ring) > before,
            "shard handles must clone the shared ring Rc"
        );
    }

    /// A `WorkerLocal` ring is private to the thread that asks for it:
    /// repeated calls on one thread reuse a single ring, while a second
    /// thread builds an entirely separate one. This is the invariant
    /// that keeps the RPC worker off the shard ring.
    #[test]
    fn worker_local_ring_is_one_per_thread() {
        let queue_depth = 8;
        let origin = OriginRing::WorkerLocal {
            queue_depth,
            region: None,
        };

        // Same thread: the second call reuses the cached ring, so the
        // handles point at the same allocation.
        let h1 = origin.handle().expect("handle");
        let h2 = origin.handle().expect("handle");
        assert!(
            h1.same_ring(&h2),
            "repeated WorkerLocal handles on one thread must share a ring"
        );

        // A different thread builds its own ring, distinct from this
        // thread's. `OriginRing` is intentionally `!Send` (the `Shard`
        // variant holds an `Rc`), so the worker rebuilds its source from
        // the `Copy` queue depth rather than moving the enum across the
        // boundary.
        let joined = std::thread::spawn(move || {
            let other = OriginRing::WorkerLocal {
                queue_depth,
                region: None,
            };
            let a = other.handle().expect("handle");
            let b = other.handle().expect("handle");
            assert!(a.same_ring(&b), "worker thread must reuse its own ring");
            // Hand back the pointer identity so the parent can assert
            // the two threads did not share a ring.
            a.ring_addr()
        })
        .join()
        .expect("worker thread");

        assert_ne!(
            h1.ring_addr(),
            joined,
            "distinct threads must not share a WorkerLocal ring"
        );
    }
}
