// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Per-stripe single-flight bookkeeping.
//!
//! One [`StripeFetch`] lives in the pool's inflight map per active
//! [`StripeKey`]. Each [`PageSlot`] within it tracks the lifecycle of
//! one logical page within the stripe: which `page_idx` was allocated
//! out of the [`crate::memory::Backing`], whether the I/O is
//! still pending, how many [`crate::bufferpool::PageGuard`]s are
//! holding the bytes, and whether the tee `BlockStore::write_page`
//! is still draining.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::rc::Rc;
use std::task::Waker;

use crate::bufferpool::types::Error;
pub(super) struct StripeFetch {
    pub pages: HashMap<u64, Rc<PageSlot>>,
    /// Number of live `ReadStream`s subscribed to this stripe.
    pub stream_refcount: u32,
}

impl StripeFetch {
    pub fn new() -> Self {
        Self {
            pages: HashMap::new(),
            stream_refcount: 0,
        }
    }
}

pub(super) struct PageSlot {
    #[allow(dead_code)]
    pub page_no: u64,
    pub state: RefCell<SlotState>,
    /// Allocated page index in the backing. `None` until the leader
    /// successfully claims a free page.
    pub page_idx: Cell<Option<u32>>,
    /// Live `PageGuard`s currently holding the bytes.
    pub consumer_holds: Cell<u32>,
    /// True while the tee `BlockStore::write_page` is outstanding;
    /// the page index cannot be recycled until both this flag is
    /// false and `consumer_holds == 0`.
    pub tee_pending: Cell<bool>,
    /// True while this ready slot is on the pool's idle-page LRU and
    /// can be evicted to satisfy a later allocation.
    pub cached: Cell<bool>,
    /// Leader-owned decision for whether this page may be retained in
    /// the in-memory page cache after consumers and tee complete.
    pub page_cache_enabled: Cell<bool>,
}

impl PageSlot {
    pub fn new(page_no: u64) -> Self {
        Self {
            page_no,
            state: RefCell::new(SlotState::Idle),
            page_idx: Cell::new(None),
            consumer_holds: Cell::new(0),
            tee_pending: Cell::new(false),
            cached: Cell::new(false),
            page_cache_enabled: Cell::new(false),
        }
    }

    /// `true` when the slot's page index can safely be returned to
    /// the free list and the slot itself can be removed from the
    /// `StripeFetch`.
    pub fn is_recyclable(&self) -> bool {
        self.consumer_holds.get() == 0 && !self.tee_pending.get()
    }
}

pub(super) enum SlotState {
    /// Newly created; no leader has taken it yet.
    Idle,
    /// A leader is driving the I/O. `wakers` parks late subscribers
    /// (and the next-leader candidate) until state transitions away
    /// from `Loading`. If the leader's future is dropped before
    /// reaching `Ready`, the leader's RAII guard in `pool::fetch_page`
    /// resets the slot to `Idle` and wakes the parked list so the
    /// next subscriber takes over.
    Loading(Vec<Waker>),
    Ready,
    Error(Error),
}
