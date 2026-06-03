// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::future::Future;
use std::marker::PhantomData;
use std::pin::Pin;
use std::rc::Rc;

use crate::bufferpool::types::{Error, PageRef, StripeKey};

/// `Box<dyn Future + 'a>` alias used by [`StreamSrc`] to keep the
/// trait dyn-compatible without depending on the `futures` crate.
pub type LocalBoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + 'a>>;

/// `'static` variant of [`LocalBoxFuture`]. Returned by
/// [`StreamSrc::fetch_page_owned`] so a windowed reader can park
/// several in-flight fetch futures in the same struct as the
/// `StreamSrc` they were issued against (a borrowing future could
/// not be stored alongside its own borrow source).
pub type StaticBoxFuture<T> = Pin<Box<dyn Future<Output = T> + 'static>>;

/// Type-erased view of a [`crate::bufferpool::Pool`]'s per-stream
/// state. The implementor (`StreamSrcImpl` in `pool.rs`) owns an
/// `Rc<PoolInner>` plus an `Rc<R>` clone of the request, so the
/// trait object hides the `T, S, R` generics from `ReadStream`,
/// [`crate::bufferpool::WindowedRead`], and `PageGuard`.
pub(super) trait StreamSrc {
    #[allow(dead_code)]
    fn page_size(&self) -> usize;
    fn base(&self) -> *mut u8;
    #[allow(dead_code)]
    fn key(&self) -> StripeKey;
    fn fetch_page<'a>(&'a self, page_no: u64) -> LocalBoxFuture<'a, Result<u32, Error>>;
    /// Owned (`'static`) counterpart to [`StreamSrc::fetch_page`].
    /// Drives the exact same single-flight machine but captures
    /// `Rc` clones of the pool internals and request instead of
    /// borrowing `self`, so the returned future can outlive (and be
    /// stored away from) this trait object.
    ///
    /// `speculative` controls how the leader acquires its backing
    /// page. A speculative (prefetch) fetch must never park on the
    /// free list: if no page is immediately free it backs off,
    /// reverts its slot to `Idle`, and resolves to
    /// [`Error::PrefetchBackoff`] so the caller can release the
    /// prefetch budget and retry later. A non-speculative (head)
    /// fetch allocates blocking, parking on the free list until a
    /// page frees. Allocation happens lazily on first poll, so a
    /// future that is launched but never polled holds no page.
    fn fetch_page_owned(
        &self,
        page_no: u64,
        speculative: bool,
    ) -> StaticBoxFuture<Result<u32, Error>>;
    fn release_guard(&self, page_no: u64);
    fn decrement_stream(&self);
    /// Global speculative-prefetch budget. The pool maintains one
    /// counter shared by every windowed stream; this reserves a
    /// slot if the live count is `< max`, returning whether it did.
    fn try_reserve_prefetch(&self, max: usize) -> bool;
    /// Release one reservation taken by [`StreamSrc::try_reserve_prefetch`].
    fn release_prefetch(&self);
}

/// Single consumer surface; one page at a time, awaited explicitly.
pub struct ReadStream<'pool> {
    src: Rc<dyn StreamSrc + 'pool>,
    cursor: u64,
    end: u64,
    page_size: u64,
    _life: PhantomData<&'pool ()>,
}

impl<'pool> std::fmt::Debug for ReadStream<'pool> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReadStream")
            .field("cursor", &self.cursor)
            .field("end", &self.end)
            .field("page_size", &self.page_size)
            .finish()
    }
}

impl<'pool> ReadStream<'pool> {
    pub(super) fn new(
        src: Rc<dyn StreamSrc + 'pool>,
        offset: u64,
        end: u64,
        page_size: usize,
    ) -> Self {
        Self {
            src,
            cursor: offset,
            end,
            page_size: page_size as u64,
            _life: PhantomData,
        }
    }

    /// Next page in the stream. Returns `None` at EOF. The returned
    /// [`PageGuard`] borrows `&mut self`, so the caller must drop
    /// it before calling `next_page` again; this enforces the
    /// one-page-at-a-time contract at compile time.
    pub async fn next_page<'s>(&'s mut self) -> Option<Result<PageGuard<'s>, Error>> {
        if self.cursor >= self.end {
            return None;
        }

        let page_size = self.page_size;
        let page_no = self.cursor / page_size;
        let intra_off = (self.cursor - page_no * page_size) as u32;
        let max_in_page = page_size - intra_off as u64;
        let remaining = self.end - self.cursor;
        let intra_len = std::cmp::min(max_in_page, remaining) as u32;

        let page_idx = match self.src.fetch_page(page_no).await {
            Ok(pi) => pi,
            Err(e) => return Some(Err(e)),
        };

        let base = self.src.base();
        // SAFETY: `page_idx` was allocated out of the pool's pinned
        // backing whose lifetime exceeds the `Rc<dyn StreamSrc>`
        // we hold. `intra_off + intra_len <= page_size`. The byte
        // range is read-only for the lifetime of the guard; the
        // pool keeps the slot pinned via `consumer_holds`.
        let bytes =
            unsafe { base.add(page_idx as usize * page_size as usize + intra_off as usize) };
        let page_ref = PageRef {
            page_idx,
            offset: intra_off,
            len: intra_len,
        };

        // Clone the trait object for the guard. Coerce '`pool` to
        // `'s` (covariant lifetime).
        let src_for_guard: Rc<dyn StreamSrc + 's> = self.src.clone();
        let guard = PageGuard::new(src_for_guard, page_no, bytes, intra_len, page_ref);

        self.cursor += intra_len as u64;
        Some(Ok(guard))
    }

    /// Test-only: returns the current cursor.
    #[cfg(test)]
    #[allow(dead_code)]
    pub(crate) fn cursor(&self) -> u64 {
        self.cursor
    }
}

impl<'pool> Drop for ReadStream<'pool> {
    fn drop(&mut self) {
        self.src.decrement_stream();
    }
}

/// Read-only view onto one page (or sub-page intra-window slice).
/// Tied to `&'s mut ReadStream` so only one is outstanding per
/// stream at a time.
pub struct PageGuard<'a> {
    src: Rc<dyn StreamSrc + 'a>,
    page_no: u64,
    bytes: *const u8,
    len: u32,
    page_ref: PageRef,
    /// Invariant in `'a` so the borrow checker prevents extending
    /// the guard's lifetime.
    _life: PhantomData<&'a mut ()>,
}

impl<'a> std::fmt::Debug for PageGuard<'a> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PageGuard")
            .field("page_no", &self.page_no)
            .field("page_ref", &self.page_ref)
            .field("len", &self.len)
            .finish()
    }
}

impl<'a> PageGuard<'a> {
    /// Wrap an already-fetched page slice. The caller must have a
    /// consumer hold transferred for `page_no` (via
    /// `fetch_page`/`forget`); dropping this guard balances it
    /// through [`StreamSrc::release_guard`]. `bytes`/`len` must name
    /// a sub-range of the pinned backing page identified by
    /// `page_ref.page_idx`.
    pub(super) fn new(
        src: Rc<dyn StreamSrc + 'a>,
        page_no: u64,
        bytes: *const u8,
        len: u32,
        page_ref: PageRef,
    ) -> Self {
        Self {
            src,
            page_no,
            bytes,
            len,
            page_ref,
            _life: PhantomData,
        }
    }

    pub fn as_slice(&self) -> &[u8] {
        // SAFETY: `bytes` and `len` came from `next_page` above and
        // refer to a sub-range of a pinned backing page. The slot
        // is held pinned until this guard drops, so the bytes are
        // valid for the lifetime of `&self`.
        unsafe { std::slice::from_raw_parts(self.bytes, self.len as usize) }
    }

    pub fn page_ref(&self) -> PageRef {
        self.page_ref
    }

    pub fn len(&self) -> usize {
        self.len as usize
    }

    pub fn is_empty(&self) -> bool {
        self.len == 0
    }
}

impl<'a> Drop for PageGuard<'a> {
    fn drop(&mut self) {
        self.src.release_guard(self.page_no);
    }
}
