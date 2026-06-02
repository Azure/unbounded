// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-shard resource bundle handed to frontend/backend builders.
//!
//! [`ShardContext`] carries the cross-cutting, shard-local resources a
//! frontend or backend future needs that are *not* the bufferpool
//! `Pool` itself: the page geometry and the per-shard
//! [`NetworkRing`](crate::ring::NetworkRing).
//!
//! Builders receive `&ShardContext` together with an `Rc<Pool>` (handed
//! separately so the context need not be generic over the pool's type
//! parameters) and a `&mut ShardLoop` on which they register their
//! `pool.read` / connection futures. The shard bring-up registers the
//! socket ring's [`progress`](crate::ring::NetworkRing::progress)
//! as a [`ShardLoop`](crate::runtime::ShardLoop) tick hook, so a
//! builder only has to enqueue its futures; the loop drives the ring.
//!
//! The whole struct is Linux-gated because it is meaningless without
//! the socket ring, which is itself Linux-only (io_uring).

use std::cell::RefCell;
use std::rc::Rc;

use crate::ring::NetworkRing;

/// Shard-local resources shared by frontend and backend futures. See
/// the module docs for how builders consume it.
pub struct ShardContext {
    page_size: usize,
    socket: Rc<RefCell<NetworkRing>>,
}

impl ShardContext {
    /// Build a context over the shard's page geometry and socket ring.
    pub fn new(page_size: usize, socket: Rc<RefCell<NetworkRing>>) -> Self {
        Self { page_size, socket }
    }

    /// Bufferpool page size in bytes for this shard.
    pub fn page_size(&self) -> usize {
        self.page_size
    }

    /// The per-shard io_uring socket ring. Backend/frontend futures
    /// borrow it to issue connect / accept / recv / send ops; the shard
    /// loop drives its `progress()` via a tick hook.
    pub fn socket(&self) -> &Rc<RefCell<NetworkRing>> {
        &self.socket
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accessors_return_constructed_values() {
        // Ring construction needs a kernel io_uring; if it is
        // unavailable in this environment, skip gracefully rather than
        // hard-failing the suite.
        let socket = match NetworkRing::new(8) {
            Ok(s) => Rc::new(RefCell::new(s)),
            Err(e) => {
                eprintln!("accessors_return_constructed_values: ring unavailable: {e}; skipping");
                return;
            }
        };
        let ctx = ShardContext::new(4096, socket.clone());
        assert_eq!(ctx.page_size(), 4096);
        // The accessor hands back the same Rc the context was built
        // with (same allocation).
        assert!(Rc::ptr_eq(ctx.socket(), &socket));
    }
}
