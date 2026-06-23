// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Placeholder `BlockStore` for embedders that have no local cache
//! tier yet. Every `read_page` reports a miss, so the pool always
//! falls through to `Transport::bulk_get`; `write_page` is a no-op,
//! so the tee silently drops bytes on the floor.
//!
//! This exists so the binary can construct a `Pool` per shard
//! before a production blockstore lands. Replace with a real
//! io_uring or NVMe-backed impl as soon as one is available.

use crate::bufferpool::traits::{BlockStore, Req};
use crate::bufferpool::types::{Error, PageRef};
use crate::memory::Backing;

#[derive(Default)]
pub struct NullBlockStore;

impl NullBlockStore {
    pub fn new() -> Self {
        Self
    }
}

impl BlockStore for NullBlockStore {
    fn register_pages(&self, _backing: &Backing) -> Result<(), Error> {
        Ok(())
    }

    async fn read_page<R: Req + ?Sized>(
        &self,
        _req: &R,
        _stripe_off: u64,
        _dst: PageRef,
    ) -> Result<bool, Error> {
        // Always-miss. Pool falls through to `Transport::bulk_get`.
        Ok(false)
    }

    async fn write_page<R: Req + ?Sized>(
        &self,
        _req: &R,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), Error> {
        // Drop the tee silently. A real blockstore will persist.
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::pin::pin;
    use std::task::{Context, Poll};

    use super::*;
    use crate::runtime::noop_waker;

    fn block_on<F: Future>(f: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = pin!(f);
        loop {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
        }
    }

    #[test]
    fn read_always_misses_write_is_noop() {
        let s = NullBlockStore::new();
        let req = crate::storage::StripeReq::new(crate::bufferpool::StripeKey([0; 32]));
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 0,
        };
        assert!(matches!(block_on(s.read_page(&req, 0, dst)), Ok(false)));
        assert!(block_on(s.write_page(&req, 0, dst)).is_ok());
    }

    #[test]
    fn register_pages_accepts_anything() {
        let s = NullBlockStore::new();
        let backing = Backing {
            base: std::ptr::null_mut(),
            page_size: 4096,
            page_count: 0,
            keepalive: std::sync::Arc::new(()),
        };
        assert!(s.register_pages(&backing).is_ok());
    }
}
