// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Windowed, internally-prefetching reader over a single stripe.
//!
//! [`WindowedRead`] is the lifetime-erased sibling of
//! [`crate::bufferpool::ReadStream`]. Where `ReadStream` awaits one
//! `fetch_page` at a time, `WindowedRead` keeps up to `window`
//! `fetch_page` futures outstanding ahead of its consumer cursor
//! (within one stripe) and drives them concurrently, while still
//! handing `PageGuard`s back to the caller strictly in cursor order,
//! one at a time. This lets a downstream consumer saturate the
//! fabric NIC instead of serializing on per-page round trips.
//!
//! Two budgets bound the speculation:
//!   - `window`: the per-stream depth, clamped by
//!     `Pool::read_windowed` to `[1, max_inflight_pages + 1]`.
//!   - the pool's global `max_inflight_pages` prefetch budget,
//!     reserved through [`StreamSrc::try_reserve_prefetch`].
//!
//! Head-of-line guarantee: the page at the cursor is always fetched
//! and never counts against either budget, and it is polled before
//! its speculative siblings on every drive pass, so it wins any
//! free-list allocation race. Speculative pages strictly ahead of
//! the head are the only ones that consume prefetch budget; bounding
//! them keeps prefetch from starving the head's `free.alloc().await`.

use std::collections::HashMap;
use std::future::Future;
use std::marker::PhantomData;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use crate::bufferpool::stream::{PageGuard, StaticBoxFuture, StreamSrc};
use crate::bufferpool::types::{Error, PageRef};

/// One outstanding fetch future tracked by a [`WindowedRead`].
struct PendingFetch {
    page_no: u64,
    fut: StaticBoxFuture<Result<u32, Error>>,
    /// Whether this page reserved a slot of the global prefetch
    /// budget at launch (true for speculative pages, false for the
    /// head). The reservation is held until the page is consumed by
    /// `next_page` or released in `Drop`.
    counted: bool,
}

/// A completed fetch awaiting consumption in cursor order. For an
/// `Ok` result the fetch transferred a consumer hold that pins the
/// slot; it is balanced by the `PageGuard` handed out on consume or
/// by `release_guard` on `Drop`.
struct ReadyPage {
    result: Result<u32, Error>,
    counted: bool,
}

/// Windowed consumer surface. Like [`crate::bufferpool::ReadStream`]
/// it yields one [`PageGuard`] at a time in cursor order; unlike it,
/// it prefetches ahead.
pub struct WindowedRead<'pool> {
    src: Rc<dyn StreamSrc + 'pool>,
    cursor: u64,
    end: u64,
    page_size: u64,
    /// Effective prefetch depth: max pages tracked (pending + ready)
    /// at once for this stream.
    window: usize,
    /// Cap passed to [`StreamSrc::try_reserve_prefetch`].
    max_inflight_pages: usize,
    pending: Vec<PendingFetch>,
    ready: HashMap<u64, ReadyPage>,
    _life: PhantomData<&'pool ()>,
}

impl<'pool> std::fmt::Debug for WindowedRead<'pool> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("WindowedRead")
            .field("cursor", &self.cursor)
            .field("end", &self.end)
            .field("page_size", &self.page_size)
            .field("window", &self.window)
            .field("pending", &self.pending.len())
            .field("ready", &self.ready.len())
            .finish()
    }
}

impl<'pool> WindowedRead<'pool> {
    pub(super) fn new(
        src: Rc<dyn StreamSrc + 'pool>,
        offset: u64,
        end: u64,
        page_size: usize,
        window: usize,
        max_inflight_pages: usize,
    ) -> Self {
        Self {
            src,
            cursor: offset,
            end,
            page_size: page_size as u64,
            window: window.max(1),
            max_inflight_pages,
            pending: Vec::new(),
            ready: HashMap::new(),
            _life: PhantomData,
        }
    }

    /// Next page in cursor order. Returns `None` at EOF. The
    /// returned [`PageGuard`] borrows `&mut self`, enforcing the
    /// one-page-at-a-time contract; the reader keeps prefetching
    /// ahead internally regardless.
    pub async fn next_page<'s>(&'s mut self) -> Option<Result<PageGuard<'s>, Error>> {
        if self.cursor >= self.end {
            return None;
        }

        let page_size = self.page_size;
        let head = self.cursor / page_size;
        let intra_off = (self.cursor - head * page_size) as u32;
        let max_in_page = page_size - intra_off as u64;
        let remaining = self.end - self.cursor;
        let intra_len = std::cmp::min(max_in_page, remaining) as u32;

        // Launch the head (always) plus as many speculative pages as
        // window + global budget permit, then drive every pending
        // future until the head completes.
        self.ensure_head_and_refill();
        DrivePending { w: self, head }.await;

        let entry = self
            .ready
            .remove(&head)
            .expect("head page must be ready after DrivePending resolves");
        // The page leaves the "ahead, unconsumed" set either way, so
        // release its budget reservation here (decrement-on-consume).
        if entry.counted {
            self.src.release_prefetch();
        }
        let page_idx = match entry.result {
            Ok(pi) => pi,
            // On error no consumer hold was transferred, so there is
            // nothing to balance; just surface it in order.
            Err(e) => return Some(Err(e)),
        };

        self.cursor += intra_len as u64;
        // Keep the pipeline full: launch prefetch for the advanced
        // cursor now. The futures make progress on the next
        // `next_page` poll.
        self.ensure_head_and_refill();

        let base = self.src.base();
        // SAFETY: identical invariants to `ReadStream::next_page`.
        // `page_idx` names a pinned backing page; the fetch left a
        // consumer hold pinning the slot for the guard's lifetime,
        // and `intra_off + intra_len <= page_size`.
        let bytes =
            unsafe { base.add(page_idx as usize * page_size as usize + intra_off as usize) };
        let page_ref = PageRef {
            page_idx,
            offset: intra_off,
            len: intra_len,
        };
        let src_for_guard: Rc<dyn StreamSrc + 's> = self.src.clone();
        let guard = PageGuard::new(src_for_guard, head, bytes, intra_len, page_ref);
        Some(Ok(guard))
    }

    /// Test-only: current cursor.
    #[cfg(test)]
    #[allow(dead_code)]
    pub(crate) fn cursor(&self) -> u64 {
        self.cursor
    }

    fn is_tracked(&self, page_no: u64) -> bool {
        self.ready.contains_key(&page_no) || self.pending.iter().any(|p| p.page_no == page_no)
    }

    /// Ensure the head page has an in-flight (or completed) fetch,
    /// then launch speculative fetches for subsequent pages while
    /// the window has room and the global prefetch budget allows.
    fn ensure_head_and_refill(&mut self) {
        if self.cursor >= self.end {
            return;
        }
        let head = self.cursor / self.page_size;
        // `end > cursor` here, so `end - 1` does not underflow.
        let last_page = (self.end - 1) / self.page_size;

        // The head is always fetchable and never counts against the
        // prefetch budget. It is launched non-speculative, so its
        // leader allocates blocking (parking on the free list if
        // needed) and `DrivePending` polls it before any speculative
        // sibling, so it wins the next free page under pressure. This
        // is what prevents a speculative fetch from starving the head
        // and deadlocking.
        if !self.is_tracked(head) {
            let fut = self.src.fetch_page_owned(head, false);
            self.pending.push(PendingFetch {
                page_no: head,
                fut,
                counted: false,
            });
        }

        let mut candidate = head + 1;
        while candidate <= last_page {
            if self.pending.len() + self.ready.len() >= self.window {
                break;
            }
            if self.is_tracked(candidate) {
                candidate += 1;
                continue;
            }
            // Speculative pages must reserve global budget; if it is
            // exhausted, stop speculating (the head still proceeds).
            if !self.src.try_reserve_prefetch(self.max_inflight_pages) {
                break;
            }
            // Launch speculative. Its leader allocates non-blocking and
            // backs off (resolving to `Error::PrefetchBackoff`) if no
            // page is free, so speculation never parks on the free list
            // ahead of any stream's head. `DrivePending` releases the
            // budget reservation when it observes that backoff.
            let fut = self.src.fetch_page_owned(candidate, true);
            self.pending.push(PendingFetch {
                page_no: candidate,
                fut,
                counted: true,
            });
            candidate += 1;
        }
    }
}

impl<'pool> Drop for WindowedRead<'pool> {
    fn drop(&mut self) {
        // Balance consumer holds for ready-but-unconsumed `Ok` pages
        // and release any prefetch budget still reserved for them.
        let ready = std::mem::take(&mut self.ready);
        for (page_no, entry) in ready {
            if entry.result.is_ok() {
                self.src.release_guard(page_no);
            }
            if entry.counted {
                self.src.release_prefetch();
            }
        }
        // Dropping a pending future runs its own internal RAII
        // cleanup (LeaderGuard / ConsumerHold inside `fetch_page`);
        // we only owe back the prefetch-budget reservation it held.
        let pending = std::mem::take(&mut self.pending);
        for pf in pending {
            if pf.counted {
                self.src.release_prefetch();
            }
        }
        self.src.decrement_stream();
    }
}

/// Drives every outstanding fetch future concurrently with a single
/// `cx`, moving completions into `ready`. Polls the head future
/// first so it wins any free-list allocation race against
/// speculative siblings. Resolves once the head page is `ready`.
struct DrivePending<'a, 'pool> {
    w: &'a mut WindowedRead<'pool>,
    head: u64,
}

impl<'a, 'pool> Future for DrivePending<'a, 'pool> {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        // `DrivePending` holds only a `&mut` and a `u64`; it is `Unpin`.
        let this = self.get_mut();
        let head = this.head;
        let w = &mut *this.w;

        // Head-of-line: poll the head first so a freshly freed page
        // is claimed by the head's leader before any speculative
        // sibling in the same pass.
        if let Some(i) = w.pending.iter().position(|p| p.page_no == head) {
            if let Poll::Ready(res) = w.pending[i].fut.as_mut().poll(cx) {
                let pf = w.pending.swap_remove(i);
                w.ready.insert(
                    pf.page_no,
                    ReadyPage {
                        result: res,
                        counted: pf.counted,
                    },
                );
            }
        }

        // Drive the remaining (speculative) futures.
        let mut i = 0;
        while i < w.pending.len() {
            if w.pending[i].page_no == head {
                i += 1;
                continue;
            }
            match w.pending[i].fut.as_mut().poll(cx) {
                Poll::Ready(res) => {
                    let pf = w.pending.swap_remove(i);
                    // A speculative leader that found no free page
                    // backs off rather than parking. That is not a
                    // consumer-visible error: drop it and return its
                    // prefetch-budget reservation so it can be retried
                    // when pages free up. It transferred no consumer
                    // hold, so there is nothing else to balance. By the
                    // time the cursor could reach this page it has been
                    // removed here, so `ensure_head_and_refill`
                    // relaunches it as a blocking head; a head fetch is
                    // never speculative and so never backs off, which
                    // is why the head branch above never sees this.
                    if matches!(res, Err(Error::PrefetchBackoff)) {
                        debug_assert!(pf.counted);
                        if pf.counted {
                            w.src.release_prefetch();
                        }
                        continue;
                    }
                    w.ready.insert(
                        pf.page_no,
                        ReadyPage {
                            result: res,
                            counted: pf.counted,
                        },
                    );
                }
                Poll::Pending => i += 1,
            }
        }

        if w.ready.contains_key(&head) {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }
}
