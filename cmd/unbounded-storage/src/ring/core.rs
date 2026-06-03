// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared low-level io_uring engine backing every facade in this
//! module.
//!
//! [`RingCore`] owns exactly one `io_uring` ring plus the bookkeeping
//! every facade needs: a slot map keyed by a monotonic `user_data`, a
//! sparse table of registered buffers, a registered file table, and a
//! FIFO back-pressure queue bounded by `queue_depth`. It is
//! `!Send + !Sync` by construction (a `PhantomData<*const ()>` pins it
//! to the thread that built it), mirroring the original disk and socket
//! rings this module consolidates.
//!
//! ## Completion model
//!
//! Each in-flight op owns a [`Slot`] holding its eventual CQE result and
//! the [`Waker`] of the task awaiting it, keyed by `user_data`.
//! [`RingCore::progress`] pushes queued SQEs, drains every available
//! CQE, fills the matching slot, and wakes the awaiting future plus one
//! parked back-pressure waiter.
//!
//! ## SEND_ZC two-CQE semantics
//!
//! A `IORING_OP_SEND_ZC` submission yields *two* CQEs: a send-completion
//! carrying `IORING_CQE_F_MORE` (byte count) followed by a notification
//! that clears `F_MORE` and signals the source buffer is released.
//! [`RingCore::progress`] records the byte count on the first (F_MORE)
//! completion without resolving the slot, and only resolves the op on
//! the final (no F_MORE) notification. Slots created with
//! `expects_more == true` are the only ones that should ever see a
//! F_MORE completion.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::ffi::c_void;
use std::future::Future;
use std::io;
use std::marker::PhantomData;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::ptr::NonNull;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use io_uring::{IoUring, cqueue, opcode, squeue};

use super::network::SockAddr;

/// Setup flags applied when building the underlying `IoUring`. Each
/// facade fills this from its own config; `StorageRing` enables
/// `IOPOLL` in production, `NetworkRing` never does (the kernel rejects
/// `IOPOLL` for socket ops).
#[derive(Copy, Clone, Debug, Default)]
pub(crate) struct RingSetup {
    pub iopoll: bool,
    pub single_issuer: bool,
    pub defer_taskrun: bool,
}

/// One io_uring ring plus the shared slot / buffer / back-pressure
/// machinery every facade in this module is built on. Pinned to the
/// thread that constructed it.
pub(crate) struct RingCore {
    ring: RefCell<IoUring>,
    /// Per-in-flight-op state keyed by `user_data`. Inserted at submit
    /// time, removed in [`Self::progress`] once the op resolves.
    slots: RefCell<HashMap<u64, Rc<Slot>>>,
    /// Sparse table of registered buffer regions. Each call to
    /// [`Self::register_buffer`] appends one entry; per-I/O the buffer
    /// index is resolved by locating a pointer inside one region.
    registered: RefCell<Vec<RegisteredBuf>>,
    /// Number of live (submitted, not yet reaped) ops. Bounded by
    /// `queue_depth` via the `submit_waiters` back-pressure queue.
    submitted: Cell<u32>,
    /// High-water mark of `submitted` since the core was created,
    /// recorded at increment time in [`Self::submit`]. Lets tests
    /// observe that ops actually went in-flight even when each op
    /// completes within its own poll - the transient peak is invisible
    /// to an external sample of `submitted`, which can return to zero
    /// before it is read.
    peak_submitted: Cell<u32>,
    /// FIFO of wakers parked because `submitted == queue_depth` at
    /// submit time. Drained one-per-completion in [`Self::progress`].
    submit_waiters: RefCell<Vec<Waker>>,
    next_user_data: Cell<u64>,
    /// Count of F_MORE (intermediate SEND_ZC) completions observed.
    /// Used only by tests to assert the two-CQE path executed.
    more_completions: Cell<u64>,
    setup: RingSetup,
    queue_depth: u32,
    _no_send: PhantomData<*const ()>,
}

/// A single registered buffer region. `Copy` so the table can be cloned
/// cheaply for the replace-whole-table registration form.
#[derive(Copy, Clone)]
pub(crate) struct RegisteredBuf {
    base: NonNull<u8>,
    len: usize,
}

/// Per-op completion slot. `result` is the i32 CQE `res`; for SEND_ZC
/// it is filled by the first (F_MORE) completion and the slot is only
/// resolved once `notified` flips on the final notification CQE.
pub(crate) struct Slot {
    result: Cell<Option<i32>>,
    /// Set once the final (non-F_MORE) completion has been seen. For
    /// single-CQE ops this is set together with `result`.
    notified: Cell<bool>,
    /// True only for SEND_ZC ops, which legitimately produce a F_MORE
    /// completion before their final notification.
    expects_more: bool,
    waker: RefCell<Option<Waker>>,
    /// Memory the in-flight op references, kept alive until the CQE is
    /// reaped. Never read after construction; held purely to own the
    /// allocation for the kernel's benefit.
    _resource: OpResource,
}

impl Slot {
    fn new(expects_more: bool, resource: OpResource) -> Self {
        Self {
            result: Cell::new(None),
            notified: Cell::new(false),
            expects_more,
            waker: RefCell::new(None),
            _resource: resource,
        }
    }

    /// True once the op's final CQE has been reaped.
    pub(crate) fn is_done(&self) -> bool {
        self.notified.get()
    }

    /// The op's CQE `res`, or `-EIO` if somehow resolved without one.
    pub(crate) fn result(&self) -> i32 {
        self.result.get().unwrap_or(-libc::EIO)
    }

    /// Register the waker to be notified when the op resolves.
    pub(crate) fn set_waker(&self, w: Waker) {
        *self.waker.borrow_mut() = Some(w);
    }
}

/// Owned, heap-stable memory an in-flight op references. The kernel
/// reads/writes through the raw pointer captured in the SQE until the
/// op's CQE is reaped, so the backing allocation must outlive the op,
/// not merely the awaiting future (which may be dropped/cancelled
/// first). The slot keeps this in the slots map so the memory survives
/// until [`RingCore::progress`] reaps the completion.
///
/// The payload fields are never read: they exist purely to keep the
/// allocation alive for the kernel until the op's CQE is reaped, so the
/// `dead_code` lint is expected here.
#[allow(dead_code)]
pub(crate) enum OpResource {
    /// Op points into a long-lived registered region (READ/WRITE_FIXED,
    /// recv_fixed, send_zc_fixed) or carries no buffer (accept); nothing
    /// to own.
    None,
    /// Owned sockaddr for `connect`.
    Addr(Box<SockAddr>),
    /// Owned source buffer for a plain `send`.
    Buf(Vec<u8>),
    /// Owned destination buffer for a plain `recv`, shared with the
    /// awaiting future via `Rc` so the future can read the bytes once
    /// the op resolves while the slot keeps the allocation alive for the
    /// kernel until the CQE is reaped (even if the future drops first).
    RecvBuf(Rc<RefCell<Vec<u8>>>),
}

impl RingCore {
    /// Build a ring with submission/completion depth `queue_depth`,
    /// applying the requested setup flags. If `build` fails and any flag
    /// was set, fall back to a plain ring of the same depth (mirrors the
    /// socket ring's fallback for kernels that reject the flags).
    pub(crate) fn new(queue_depth: u32, setup: RingSetup) -> io::Result<Self> {
        if queue_depth == 0 {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        let mut builder = IoUring::builder();
        if setup.iopoll {
            builder.setup_iopoll();
        }
        if setup.single_issuer {
            builder.setup_single_issuer();
        }
        if setup.defer_taskrun {
            builder.setup_defer_taskrun();
        }
        let any_flag = setup.iopoll || setup.single_issuer || setup.defer_taskrun;
        let ring = match builder.build(queue_depth) {
            Ok(r) => r,
            Err(e) => {
                if any_flag {
                    IoUring::new(queue_depth)?
                } else {
                    return Err(e);
                }
            }
        };
        Ok(Self {
            ring: RefCell::new(ring),
            slots: RefCell::new(HashMap::new()),
            registered: RefCell::new(Vec::new()),
            submitted: Cell::new(0),
            peak_submitted: Cell::new(0),
            submit_waiters: RefCell::new(Vec::new()),
            next_user_data: Cell::new(1),
            more_completions: Cell::new(0),
            setup,
            queue_depth,
            _no_send: PhantomData,
        })
    }

    /// Register `[base, base + len)` as the next buffer slot, returning
    /// its index. Uses the replace-whole-table form (io_uring 0.6 only
    /// exposes that): unregister any existing table, then register the
    /// extended one. Registration is expected before any I/O is in
    /// flight.
    pub(crate) fn register_buffer(&self, base: *mut u8, len: usize) -> io::Result<u16> {
        let nn = NonNull::new(base).ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
        let mut new_regs = self.registered.borrow().clone();
        let idx = new_regs.len();
        new_regs.push(RegisteredBuf { base: nn, len });
        let iovs: Vec<libc::iovec> = new_regs
            .iter()
            .map(|r| libc::iovec {
                iov_base: r.base.as_ptr() as *mut c_void,
                iov_len: r.len,
            })
            .collect();
        // SAFETY: every (base, len) was provided by a caller that owns
        // the region for the lifetime of the ring; `new_regs` stays
        // parallel to the kernel-side table.
        unsafe {
            let ring = self.ring.borrow();
            let submitter = ring.submitter();
            if !self.registered.borrow().is_empty() {
                let _ = submitter.unregister_buffers();
            }
            submitter.register_buffers(&iovs)?;
        }
        *self.registered.borrow_mut() = new_regs;
        Ok(idx as u16)
    }

    /// Replace the registered file table with `fds`. The Nth fd becomes
    /// `types::Fixed(N)`. Uses the replace-whole-table form: drop any
    /// existing table first (the kernel rejects a second register).
    pub(crate) fn register_files(&self, fds: &[RawFd]) -> io::Result<()> {
        let ring = self.ring.borrow();
        let submitter = ring.submitter();
        let _ = submitter.unregister_files();
        submitter.register_files(fds)?;
        Ok(())
    }

    /// Base pointer of registered region `idx`, if present. Used by
    /// `NetworkRing::recv_fixed`, whose opcode needs an absolute pointer
    /// rather than a fixed buf_index.
    pub(crate) fn registered_base(&self, idx: u16) -> Option<NonNull<u8>> {
        self.registered.borrow().get(idx as usize).map(|r| r.base)
    }

    /// Locate the registered region that fully contains `[ptr, ptr+len)`
    /// and return its buffer index. `-EFAULT` if no region matches.
    pub(crate) fn resolve_buf_index(&self, ptr: *const u8, len: usize) -> io::Result<u16> {
        let regs = self.registered.borrow();
        if regs.is_empty() {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        let start = ptr as usize;
        let end = start
            .checked_add(len)
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;
        for (idx, reg) in regs.iter().enumerate() {
            let base = reg.base.as_ptr() as usize;
            if start >= base && end <= base + reg.len {
                return Ok(idx as u16);
            }
        }
        Err(io::Error::from_raw_os_error(libc::EFAULT))
    }

    /// Hand out the next `user_data`. Starts at 1 and wraps to 1, so 0
    /// is never used (reserved for fire-and-forget cancel SQEs).
    pub(crate) fn alloc_user_data(&self) -> u64 {
        let v = self.next_user_data.get();
        let next = v.checked_add(1).unwrap_or(1);
        self.next_user_data.set(next);
        v
    }

    /// Await a free submission slot, then push `sqe` and register its
    /// slot. `resource` is any owned memory the SQE points into; it is
    /// held on the slot so it outlives the kernel's access. `expects_more`
    /// must be `true` only for SEND_ZC.
    ///
    /// Any raw pointer `sqe` captures must address either `resource` or a
    /// long-lived registered region, so it stays valid until the CQE is
    /// reaped. The ring is `!Send` so no other thread can drop the slot.
    pub(crate) async fn submit_op(
        &self,
        sqe: squeue::Entry,
        expects_more: bool,
        resource: OpResource,
    ) -> io::Result<Rc<Slot>> {
        SubmitSlot { core: self }.await;
        self.submit_now(sqe, expects_more, resource)
    }

    /// Push `sqe` and register its slot *without* awaiting a free slot.
    /// Used by the `'static` handle path (which, like the original
    /// socket handle, performs submission synchronously inside a borrow
    /// block). On a full SQ the push fails and bookkeeping is rolled
    /// back. See [`Self::submit_op`] for the pointer-validity contract.
    pub(crate) fn submit_now(
        &self,
        sqe: squeue::Entry,
        expects_more: bool,
        resource: OpResource,
    ) -> io::Result<Rc<Slot>> {
        let ud = sqe.get_user_data();
        let slot = Rc::new(Slot::new(expects_more, resource));
        self.slots.borrow_mut().insert(ud, Rc::clone(&slot));
        self.submitted.set(self.submitted.get() + 1);
        if self.submitted.get() > self.peak_submitted.get() {
            self.peak_submitted.set(self.submitted.get());
        }
        // SAFETY: see the contract on the public submit methods.
        let push_res = unsafe {
            let mut ring = self.ring.borrow_mut();
            ring.submission().push(&sqe)
        };
        if push_res.is_err() {
            self.slots.borrow_mut().remove(&ud);
            self.submitted.set(self.submitted.get().saturating_sub(1));
            if let Some(w) = pop_front_waker(&mut self.submit_waiters.borrow_mut()) {
                w.wake();
            }
            return Err(io::Error::from_raw_os_error(libc::ENOMEM));
        }
        // Kick the kernel so the op starts even if progress() is not
        // called for a while.
        {
            let ring = self.ring.borrow();
            let _ = ring.submitter().submit();
        }
        Ok(slot)
    }

    /// Push queued SQEs and drain every available CQE, resolving and
    /// waking ops and freeing one back-pressure waiter per completion.
    /// Returns whether any work happened (SQEs submitted or CQEs
    /// reaped). Submit errors of EBUSY/EAGAIN are tolerated; others
    /// surface.
    pub(crate) fn progress(&self) -> io::Result<bool> {
        let submitted_n = {
            let ring = self.ring.borrow();
            match ring.submitter().submit() {
                Ok(n) => n as u32,
                Err(e) => {
                    let raw = e.raw_os_error().unwrap_or(0);
                    if raw == libc::EBUSY || raw == libc::EAGAIN {
                        0
                    } else {
                        return Err(e);
                    }
                }
            }
        };

        // With DEFER_TASKRUN the kernel only runs deferred completion
        // task work (notably the SEND_ZC notification CQE) on an enter
        // that requests GETEVENTS. A plain submit() (want == 0) does not,
        // so flush events non-blockingly before draining.
        if self.setup.defer_taskrun {
            self.flush_events();
        }

        let mut to_wake: Vec<Waker> = Vec::new();
        let mut drained = 0u32;
        {
            let mut ring = self.ring.borrow_mut();
            let mut cq = ring.completion();
            cq.sync();
            while let Some(cqe) = cq.next() {
                drained += 1;
                let ud = cqe.user_data();
                let res = cqe.result();
                let more = cqe_is_more(cqe.flags());

                let Some(slot) = self.slots.borrow().get(&ud).cloned() else {
                    // Unknown / already-removed user_data (e.g. the
                    // user_data-0 cancel SQE). Ignore defensively.
                    continue;
                };
                if more {
                    // First SEND_ZC completion: record the byte count but
                    // do NOT resolve; the buffer is still in use until the
                    // notification arrives.
                    debug_assert!(
                        slot.expects_more,
                        "F_MORE completion on an op that did not expect one",
                    );
                    self.more_completions.set(self.more_completions.get() + 1);
                    slot.result.set(Some(res));
                    continue;
                }
                // Final completion (single-CQE ops, or the SEND_ZC
                // notification). Keep any byte count already recorded by
                // the F_MORE completion.
                if slot.result.get().is_none() {
                    slot.result.set(Some(res));
                }
                slot.notified.set(true);
                self.slots.borrow_mut().remove(&ud);
                let n = self.submitted.get();
                self.submitted.set(n.saturating_sub(1));
                if let Some(w) = slot.waker.borrow_mut().take() {
                    to_wake.push(w);
                }
                if let Some(w) = pop_front_waker(&mut self.submit_waiters.borrow_mut()) {
                    to_wake.push(w);
                }
            }
        }

        for w in to_wake {
            w.wake();
        }
        Ok(submitted_n > 0 || drained > 0)
    }

    /// Non-blocking `io_uring_enter(GETEVENTS)` with nothing to submit
    /// and `min_complete == 0`: runs deferred completion task work (e.g.
    /// the SEND_ZC notification under DEFER_TASKRUN) and returns
    /// immediately without waiting.
    fn flush_events(&self) {
        // `IORING_ENTER_GETEVENTS == 0x1`. The crate's `sys` module is
        // private, so the bit is restated here (mirrors the inline tests
        // that restate `IORING_CQE_F_MORE`).
        const IORING_ENTER_GETEVENTS: u32 = 0x1;
        let ring = self.ring.borrow();
        // SAFETY: to_submit and min_complete are both 0, so this submits
        // nothing and never blocks; it only flushes ready completions.
        let _ = unsafe {
            ring.submitter()
                .enter::<libc::sigset_t>(0, 0, IORING_ENTER_GETEVENTS, None)
        };
    }

    /// Best-effort cancel of the in-flight op named by
    /// `target_user_data` (`IORING_OP_ASYNC_CANCEL`). The cancel SQE
    /// carries `user_data` 0 (never handed out), so its own CQE matches
    /// no slot and is ignored by [`Self::progress`]. Silently skipped if
    /// the ring is busy: correctness never depends on the cancel
    /// landing, since the slot stays registered until reaped.
    pub(crate) fn cancel(&self, target_user_data: u64) {
        let entry = opcode::AsyncCancel::new(target_user_data)
            .build()
            .user_data(0);
        if let Ok(mut ring) = self.ring.try_borrow_mut() {
            // SAFETY: AsyncCancel references no user memory; it only
            // names another op by its user_data.
            let _ = unsafe { ring.submission().push(&entry) };
        }
    }

    /// Cancel the in-flight op named by `target_user_data` and BLOCK
    /// until its slot is reaped. Unlike [`Self::cancel`] (best-effort,
    /// may never land), this guarantees the kernel is done with the op's
    /// memory before returning.
    ///
    /// Required by drop paths for ops that write directly into
    /// caller-owned memory which may be reused the instant this returns
    /// (the fixed-buffer RECV path: the destination bufferpool page is
    /// handed back to the free list right after the fetch future drops).
    /// A best-effort cancel is unsound there because the kernel could
    /// still complete the RECV into the page after it has been
    /// reassigned to a different key.
    ///
    /// Bounded: `IORING_OP_ASYNC_CANCEL` aborts a pending RECV promptly
    /// (it completes with `-ECANCELED`), so only a handful of CQEs are
    /// drained before the slot is done. Each iteration blocks on a real
    /// completion, so the spin guard is a safety net, not a busy-wait.
    pub(crate) fn cancel_and_drain(&self, target_user_data: u64, slot: &Slot) {
        const MAX_DRAIN_ITERS: u32 = 4096;
        let mut cancel_submitted = false;
        let mut iters = 0u32;
        while !slot.is_done() {
            if !cancel_submitted {
                let entry = opcode::AsyncCancel::new(target_user_data)
                    .build()
                    .user_data(0);
                if let Ok(mut ring) = self.ring.try_borrow_mut() {
                    // SAFETY: AsyncCancel references no user memory; it
                    // only names another op by its user_data.
                    cancel_submitted = unsafe { ring.submission().push(&entry) }.is_ok();
                }
            }
            // Blocking enter: submit queued SQEs (including the cancel)
            // and wait for at least one completion. Draining unrelated
            // completions here frees SQ space so the cancel can land if
            // its earlier push failed on a full ring.
            {
                let ring = self.ring.borrow();
                let _ = ring.submitter().submit_and_wait(1);
            }
            let _ = self.progress();
            iters += 1;
            debug_assert!(
                iters < MAX_DRAIN_ITERS,
                "cancel_and_drain exceeded its iteration bound",
            );
            if iters >= MAX_DRAIN_ITERS {
                break;
            }
        }
    }

    /// Number of live (submitted, not yet reaped) ops. Used by tests to
    /// assert the back-pressure bound.
    pub(crate) fn in_flight(&self) -> u32 {
        self.submitted.get()
    }

    /// High-water mark of in-flight ops since creation. Used by the
    /// back-pressure tests to confirm ops actually went in-flight
    /// without racing the per-poll completion that returns `in_flight`
    /// to zero before an external sample can read it.
    pub(crate) fn peak_in_flight(&self) -> u32 {
        self.peak_submitted.get()
    }

    /// Configured submission/completion depth.
    pub(crate) fn queue_depth(&self) -> u32 {
        self.queue_depth
    }

    /// Count of F_MORE (intermediate SEND_ZC) completions observed.
    /// Used by tests to assert the two-CQE path executed.
    pub(crate) fn more_completions(&self) -> u64 {
        self.more_completions.get()
    }
}

impl Drop for RingCore {
    fn drop(&mut self) {
        // Best-effort: release the kernel-pinned buffer/file tables
        // promptly rather than waiting for ring-fd teardown.
        let sub = self.ring.get_mut().submitter();
        let _ = sub.unregister_buffers();
        let _ = sub.unregister_files();
    }
}

/// Back-pressure park: yields `Pending` while `submitted ==
/// queue_depth`, registering the caller's waker so [`RingCore::progress`]
/// can resume it when a slot frees up.
struct SubmitSlot<'a> {
    core: &'a RingCore,
}

impl Future for SubmitSlot<'_> {
    type Output = ();
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let core = self.core;
        if core.submitted.get() < core.queue_depth {
            return Poll::Ready(());
        }
        // Opportunistically drive completions to free a slot before
        // parking.
        let _ = core.progress();
        if core.submitted.get() < core.queue_depth {
            return Poll::Ready(());
        }
        core.submit_waiters.borrow_mut().push(cx.waker().clone());
        Poll::Pending
    }
}

/// Future that resolves to the op's CQE `res` once its slot is marked
/// done. Polling opportunistically drives `progress()` so a completion
/// the kernel already posted is reaped without an extra scheduler
/// round-trip. If dropped before completion the slot is intentionally
/// *not* removed (the kernel may still touch the op's memory); a
/// best-effort cancel bounds how long the op stays outstanding.
pub(crate) struct OpFut<'a> {
    core: &'a RingCore,
    user_data: u64,
    slot: Rc<Slot>,
}

impl<'a> OpFut<'a> {
    pub(crate) fn new(core: &'a RingCore, user_data: u64, slot: Rc<Slot>) -> Self {
        Self {
            core,
            user_data,
            slot,
        }
    }
}

impl Future for OpFut<'_> {
    type Output = i32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<i32> {
        let this = self.get_mut();
        if this.slot.is_done() {
            return Poll::Ready(this.slot.result());
        }
        let _ = this.core.progress();
        if this.slot.is_done() {
            return Poll::Ready(this.slot.result());
        }
        this.slot.set_waker(cx.waker().clone());
        Poll::Pending
    }
}

impl Drop for OpFut<'_> {
    fn drop(&mut self) {
        if !self.slot.is_done() {
            self.core.cancel(self.user_data);
        }
    }
}

/// True when a CQE carries `IORING_CQE_F_MORE` ("more completions are
/// coming"). For SEND_ZC the first completion sets this; the final
/// notification does not.
pub(crate) fn cqe_is_more(flags: u32) -> bool {
    cqueue::more(flags)
}

/// Pop the oldest waker from `v`, treating it as a FIFO. `Vec` is fine
/// here because the queue is bounded by `queue_depth` and only the
/// owning thread touches it.
fn pop_front_waker(v: &mut Vec<Waker>) -> Option<Waker> {
    if v.is_empty() {
        None
    } else {
        Some(v.remove(0))
    }
}

/// Map a CQE `res` into an `io::Result`: negative is `-errno`.
pub(crate) fn check_res(res: i32) -> io::Result<i32> {
    if res < 0 {
        Err(io::Error::from_raw_os_error(-res))
    } else {
        Ok(res)
    }
}

/// Build a single `libc::iovec` spanning `[base, base + len)`. Pure
/// helper so the (base, len) -> iovec mapping is unit-testable without
/// a kernel ring.
pub(crate) fn build_iovec(base: *mut u8, len: usize) -> libc::iovec {
    libc::iovec {
        iov_base: base as *mut c_void,
        iov_len: len,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const F_MORE: u32 = 2;

    #[test]
    fn cqe_more_classification() {
        assert!(cqe_is_more(F_MORE));
        assert!(cqe_is_more(F_MORE | 0x1));
        assert!(!cqe_is_more(0));
        // 0x1 is IORING_CQE_F_BUFFER, not F_MORE.
        assert!(!cqe_is_more(0x1));
    }

    #[test]
    fn build_iovec_maps_base_and_len() {
        let mut buf = [0u8; 64];
        let base = buf.as_mut_ptr();
        let iov = build_iovec(base, 64);
        assert_eq!(iov.iov_base, base as *mut c_void);
        assert_eq!(iov.iov_len, 64);
    }

    #[test]
    fn user_data_is_unique_and_monotonic() {
        let core = RingCore::new(8, RingSetup::default()).expect("core");
        let a = core.alloc_user_data();
        let b = core.alloc_user_data();
        let c = core.alloc_user_data();
        assert_eq!(b, a + 1);
        assert_eq!(c, b + 1);
    }

    #[test]
    fn check_res_maps_negative_to_errno() {
        assert_eq!(check_res(5).unwrap(), 5);
        let e = check_res(-libc::EIO).unwrap_err();
        assert_eq!(e.raw_os_error(), Some(libc::EIO));
    }
}
