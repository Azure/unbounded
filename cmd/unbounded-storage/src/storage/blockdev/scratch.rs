// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Small registered-buffer scratch pool for btree/meta I/O.
//!
//! Why this exists: every page handed to [`BlockDevice::read`] or
//! [`BlockDevice::write`] must live inside a region previously
//! registered with the device's fixed-buffer table (otherwise the
//! io_uring backend's `READ_FIXED` / `WRITE_FIXED` SQEs fail with
//! `-EFAULT`). The bufferpool already owns a large, registered
//! hugepage backing, but that backing serves 2 MiB user data pages
//! and is sized to cache capacity; slicing it for the 4 KiB btree
//! /meta I/O path would entangle two unrelated lifecycles.
//!
//! [`ScratchPool`] is the explicit "small registered region for
//! structural I/O" that bufferpool is not. It owns one page-aligned
//! heap allocation of `page_size * page_count` bytes, registers it
//! with the device exactly once at construction, and hands out
//! [`ScratchPage`] leases that are guaranteed to fall inside the
//! registered region. Leases are RAII; dropping returns the page to
//! the free list and wakes one parked waiter.
//!
//! The pool is single-threaded by construction (held inside a
//! `!Send` device path), matching the rest of the storage shard.

use std::alloc::{Layout, alloc_zeroed, dealloc};
use std::cell::RefCell;
use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::ptr::NonNull;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use crate::storage::blockdev::BlockDevice;
use crate::storage::types::Error;

/// Owner of the scratch allocation. Registers itself with the
/// device at construction; pages are leased via
/// [`Self::acquire`].
///
/// `Rc<ScratchPool>` is the canonical handle: btree, meta, and
/// rebuild code clone the `Rc` rather than borrowing the pool.
pub struct ScratchPool {
    base: NonNull<u8>,
    page_size: usize,
    page_count: usize,
    layout: Layout,
    inner: RefCell<Inner>,
}

struct Inner {
    /// Indices of currently-free pages within the scratch region.
    free: VecDeque<u32>,
    /// Wakers parked on [`AcquireFut`] while [`Inner::free`] is
    /// empty. Drained one-per-release.
    waiters: VecDeque<Waker>,
}

impl ScratchPool {
    /// Allocate `page_count` page-aligned scratch pages of
    /// `page_size` bytes each and register the backing region with
    /// `device`. The page size MUST equal `device.page_size()`.
    ///
    /// Returns an `Rc` so the same pool can be shared between the
    /// btree (lookups, walks) and the engine (meta I/O) on the same
    /// shard.
    pub fn new<B: BlockDevice + ?Sized>(
        device: &B,
        page_size: usize,
        page_count: usize,
    ) -> Result<Rc<Self>, Error> {
        if page_size == 0 || page_count == 0 {
            return Err(Error::Io(libc::EINVAL));
        }
        if page_size != device.page_size() {
            return Err(Error::Io(libc::EINVAL));
        }
        let align = page_size.next_power_of_two().max(4096);
        let total = page_size
            .checked_mul(page_count)
            .ok_or(Error::Io(libc::EOVERFLOW))?;
        let layout = Layout::from_size_align(total, align).map_err(|_| Error::Io(libc::EINVAL))?;
        // SAFETY: layout has nonzero size (page_size >= 1 and
        // page_count >= 1, checked above).
        let raw = unsafe { alloc_zeroed(layout) };
        let base = NonNull::new(raw).ok_or(Error::Io(libc::ENOMEM))?;

        // Register with the device. The device is expected to track
        // multiple registrations; calling sites (engine, btree)
        // construct the scratch pool BEFORE the bufferpool's larger
        // backing is registered, but the order does not matter as
        // long as both regions end up in the device's table.
        device.register_buffers(base.as_ptr(), total)?;

        let mut free = VecDeque::with_capacity(page_count);
        for i in 0..page_count as u32 {
            free.push_back(i);
        }
        Ok(Rc::new(Self {
            base,
            page_size,
            page_count,
            layout,
            inner: RefCell::new(Inner {
                free,
                waiters: VecDeque::new(),
            }),
        }))
    }

    /// Page size (in bytes) of each scratch page. Equal to
    /// `device.page_size()` by construction.
    pub fn page_size(&self) -> usize {
        self.page_size
    }

    /// Total scratch pages backing this pool.
    #[allow(dead_code)]
    pub fn page_count(&self) -> usize {
        self.page_count
    }

    /// Asynchronously lease a scratch page. Resolves immediately if
    /// the free list is non-empty; otherwise parks until another
    /// holder drops a [`ScratchPage`].
    pub fn acquire(self: &Rc<Self>) -> AcquireFut {
        AcquireFut {
            pool: Some(self.clone()),
        }
    }

    fn try_pop(&self) -> Option<u32> {
        self.inner.borrow_mut().free.pop_front()
    }

    fn release(&self, idx: u32) {
        let waker = {
            let mut inner = self.inner.borrow_mut();
            inner.free.push_back(idx);
            inner.waiters.pop_front()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    fn park(&self, w: Waker) {
        self.inner.borrow_mut().waiters.push_back(w);
    }

    /// SAFETY: the returned pointer is valid for `page_size` bytes
    /// for as long as the [`ScratchPage`] holding `idx` is alive.
    unsafe fn page_ptr(&self, idx: u32) -> *mut u8 {
        // SAFETY: `idx < page_count` by construction (only valid
        // indices ever enter the free list).
        unsafe { self.base.as_ptr().add(idx as usize * self.page_size) }
    }
}

impl Drop for ScratchPool {
    fn drop(&mut self) {
        // The device is expected to outlive the pool, but we have
        // no way to unregister a specific index through the trait;
        // the device's own `Drop` calls `unregister_buffers` for
        // every slot. Free the heap allocation here.
        // SAFETY: layout matches the one passed to `alloc_zeroed`.
        unsafe { dealloc(self.base.as_ptr(), self.layout) };
    }
}

/// RAII handle to one scratch page. Dereferences to a mutable byte
/// slice of length `pool.page_size()`. Dropping the handle returns
/// the page to the free list and wakes one parked waiter.
pub struct ScratchPage {
    pool: Rc<ScratchPool>,
    idx: u32,
}

impl ScratchPage {
    /// Pointer to the start of this scratch page.
    pub fn as_ptr(&self) -> *const u8 {
        // SAFETY: idx is owned by this guard until drop.
        unsafe { self.pool.page_ptr(self.idx) }
    }

    /// Mutable pointer to the start of this scratch page.
    pub fn as_mut_ptr(&mut self) -> *mut u8 {
        // SAFETY: idx is owned by this guard until drop.
        unsafe { self.pool.page_ptr(self.idx) }
    }

    /// Borrow the page as a mutable byte slice of length
    /// `pool.page_size()`.
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        let ps = self.pool.page_size;
        let p = self.as_mut_ptr();
        // SAFETY: the page is exclusively owned by this guard and
        // is `ps` bytes wide; no other reference into the same
        // region can exist while this borrow is live.
        unsafe { std::slice::from_raw_parts_mut(p, ps) }
    }

    /// Borrow the page as a shared byte slice of length
    /// `pool.page_size()`.
    pub fn as_slice(&self) -> &[u8] {
        let ps = self.pool.page_size;
        let p = self.as_ptr();
        // SAFETY: exclusive ownership of the page by this guard
        // makes a shared borrow trivially sound.
        unsafe { std::slice::from_raw_parts(p, ps) }
    }
}

impl Drop for ScratchPage {
    fn drop(&mut self) {
        self.pool.release(self.idx);
    }
}

/// Future returned by [`ScratchPool::acquire`].
pub struct AcquireFut {
    pool: Option<Rc<ScratchPool>>,
}

impl Future for AcquireFut {
    type Output = ScratchPage;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<ScratchPage> {
        let this = self.get_mut();
        let pool = this
            .pool
            .as_ref()
            .expect("AcquireFut polled after completion");
        if let Some(idx) = pool.try_pop() {
            let pool = this.pool.take().expect("checked above");
            return Poll::Ready(ScratchPage { pool, idx });
        }
        pool.park(cx.waker().clone());
        // Re-check after parking to close the race where a
        // release happened between try_pop and park.
        if let Some(idx) = pool.try_pop() {
            let pool = this.pool.take().expect("checked above");
            return Poll::Ready(ScratchPage { pool, idx });
        }
        Poll::Pending
    }
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::pin::pin;
    use std::task::{Context, Poll};

    use super::*;
    use crate::runtime::noop_waker;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};

    fn block_on<F: Future>(f: F) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut f = pin!(f);
        let mut spins = 0u32;
        loop {
            match f.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "stuck");
                }
            }
        }
    }

    #[test]
    fn acquire_release_round_trip() {
        let dev = MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 8,
            ..Default::default()
        });
        let pool = ScratchPool::new(&dev, 4096, 2).unwrap();
        let mut p1 = block_on(pool.acquire());
        let mut p2 = block_on(pool.acquire());
        // Different pages.
        assert_ne!(p1.as_mut_ptr(), p2.as_mut_ptr());
        // Each is 4096 bytes and writable.
        p1.as_mut_slice()[0] = 0xab;
        p2.as_mut_slice()[4095] = 0xcd;
        assert_eq!(p1.as_slice()[0], 0xab);
        assert_eq!(p2.as_slice()[4095], 0xcd);
        drop(p1);
        // A third acquire now succeeds (was empty before the drop).
        let _p3 = block_on(pool.acquire());
    }

    #[test]
    fn acquire_parks_when_empty_and_wakes_on_release() {
        let dev = MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 8,
            ..Default::default()
        });
        let pool = ScratchPool::new(&dev, 4096, 1).unwrap();
        let held = block_on(pool.acquire());
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let fut = pool.acquire();
        let mut fut = pin!(fut);
        assert!(matches!(fut.as_mut().poll(&mut cx), Poll::Pending));
        drop(held);
        // After release, the parked future must complete.
        let mut spins = 0u32;
        loop {
            if let Poll::Ready(_p) = fut.as_mut().poll(&mut cx) {
                break;
            }
            spins += 1;
            assert!(spins < 1_000_000, "stuck waiting on release");
        }
    }

    #[test]
    fn registers_with_device() {
        let dev = MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 8,
            ..Default::default()
        });
        let _pool = ScratchPool::new(&dev, 4096, 4).unwrap();
        assert!(dev.registered_base().is_some());
        assert_eq!(dev.registered_len(), 4096 * 4);
    }
}
