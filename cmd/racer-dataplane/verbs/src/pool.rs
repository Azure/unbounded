//! Registered page buffers and their ownership states.
//!
//! See `designs/racer-verbs.md`, "Page buffer pool". One registered region
//! per device is split into equal page buffers. Each buffer has exactly one
//! state at a time, kept in an atomic array so other threads can observe it.
//!
//! The type system enforces who may touch the memory:
//!
//! - [`SendBuffer`] (`Staging`) gives `&mut [u8]` and [`RegisteredPage`]
//!   (`Owned`) gives `&[u8]`. They are the only ways Rust code can see page
//!   memory, and they exist only while no memory window covers the buffer and
//!   no work request uses it.
//! - Every other state is held by the owner thread as a [`Buffer`] token,
//!   which has no slice or pointer API at all. The owner moves a token to
//!   `Owned` only after the fence (receive completion, then a fenced
//!   `LOCAL_INV` completion) and to `Staging` only from `Free`.
//!
//! Lifetime: [`RegisteredPage`] and [`SendBuffer`] hold an `Arc<PoolShared>`.
//! [`Pool`] owns the region. When the pool is dropped while any page or send
//! buffer is still alive, or while any buffer is `Leaked` or `Quarantined`,
//! the region is leaked on purpose (never deregistered) and the charge is
//! quarantined, so the memory stays valid and counted for as long as anything
//! may use it.

use std::{
    rc::Rc,
    sync::{
        Arc, Mutex, MutexGuard, PoisonError,
        atomic::{AtomicU8, Ordering},
    },
    task::Waker,
};

use futures::task::AtomicWaker;

use crate::{Error, GuardOwner, Result, ffi::NativeRegion};

/// Ownership state of one page buffer.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BufferState {
    /// On the free list. No CPU or NIC access.
    Free,

    /// Held by a [`SendBuffer`]: CPU writes, no NIC access.
    Staging,

    /// Owner posted a `WRITE_WITH_IMM` from it: NIC reads.
    Sending,

    /// Owner posted a `BIND_MW` over it.
    Binding,

    /// The window's rkey was sent in a `Grant`: the peer may write.
    Exposed,

    /// Owner posted the fenced `LOCAL_INV`: the peer may write until its CQE.
    Invalidating,

    /// Held by a [`RegisteredPage`]: CPU reads, no NIC access.
    Owned,

    /// Its link is breaking: the NIC may still write until the QP is gone.
    Quarantined,

    /// Destroy failed: never reused, counted forever.
    Leaked,
}

const STATES: [BufferState; 9] = [
    BufferState::Free,
    BufferState::Staging,
    BufferState::Sending,
    BufferState::Binding,
    BufferState::Exposed,
    BufferState::Invalidating,
    BufferState::Owned,
    BufferState::Quarantined,
    BufferState::Leaked,
];

impl BufferState {
    fn code(self) -> u8 {
        STATES.iter().position(|s| *s == self).unwrap() as u8
    }

    fn from_code(code: u8) -> Self {
        STATES[code as usize]
    }

    /// States the owner holds as a [`Buffer`] token.
    fn owner_held(self) -> bool {
        matches!(
            self,
            Self::Sending | Self::Binding | Self::Exposed | Self::Invalidating | Self::Quarantined
        )
    }
}

/// Lock ignoring poison. Critical sections here never panic midway.
fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

/// State shared by the owner and every page or send buffer.
pub(crate) struct PoolShared {
    /// Start of the registered memory. Valid while any `Arc` exists (see the
    /// module docs).
    base: *mut u8,

    buffer_len: usize,

    states: Box<[AtomicU8]>,

    free: Mutex<Vec<u32>>,

    /// The owner, woken when a buffer returns.
    owner: AtomicWaker,

    /// Clients waiting for a free buffer in [`PoolShared::poll_stage`].
    waiters: Mutex<Vec<Waker>>,
}

// SAFETY: `base` is only dereferenced through `RegisteredPage` and
// `SendBuffer`, each of which exclusively holds one buffer index in a state
// with no NIC access, and the memory outlives every `Arc<PoolShared>`.
unsafe impl Send for PoolShared {}
unsafe impl Sync for PoolShared {}

impl PoolShared {
    /// Page buffer length.
    #[cfg_attr(not(test), allow(dead_code, reason = "used by tests and diagnostics"))]
    pub(crate) fn buffer_len(&self) -> usize {
        self.buffer_len
    }

    /// Number of page buffers.
    pub(crate) fn count(&self) -> usize {
        self.states.len()
    }

    /// Current state of buffer `index`.
    pub(crate) fn state(&self, index: u32) -> BufferState {
        BufferState::from_code(self.states[index as usize].load(Ordering::Acquire))
    }

    /// Number of buffers in `state`.
    pub(crate) fn count_in(&self, state: BufferState) -> usize {
        (0..self.states.len() as u32)
            .filter(|&i| self.state(i) == state)
            .count()
    }

    fn set(&self, index: u32, state: BufferState) {
        self.states[index as usize].store(state.code(), Ordering::Release);
    }

    /// Take a free buffer and move it to `state`.
    fn pop(&self, state: BufferState) -> Option<u32> {
        let index = lock(&self.free).pop()?;
        debug_assert_eq!(self.state(index), BufferState::Free);
        self.set(index, state);
        Some(index)
    }

    /// Return buffer `index` to the free list and wake waiters.
    fn push(&self, index: u32) {
        self.set(index, BufferState::Free);
        lock(&self.free).push(index);
        self.owner.wake();
        for waker in std::mem::take(&mut *lock(&self.waiters)) {
            waker.wake();
        }
    }

    /// Take a free buffer as a [`SendBuffer`] of `len` bytes, from any
    /// thread. `Pending` registers `waker` until a buffer returns.
    pub(crate) fn poll_stage(
        self: &Arc<Self>,
        len: usize,
        waker: &Waker,
    ) -> Option<Result<SendBuffer>> {
        if len == 0 || len > self.buffer_len {
            return Some(Err(Error::InvalidRange));
        }

        if let Some(buffer) = self.stage(len) {
            return Some(Ok(buffer));
        }

        lock(&self.waiters).push(waker.clone());

        // A buffer may have returned between the first try and registering.
        self.stage(len).map(Ok)
    }

    /// Take a free buffer as a [`SendBuffer`] of `len` bytes, if any.
    pub(crate) fn stage(self: &Arc<Self>, len: usize) -> Option<SendBuffer> {
        if len == 0 || len > self.buffer_len {
            return None;
        }

        let index = self.pop(BufferState::Staging)?;

        Some(SendBuffer {
            shared: self.clone(),
            index,
            len,
        })
    }

    /// Register the owner's waker for returned buffers.
    pub(crate) fn register_owner(&self, waker: &Waker) {
        self.owner.register(waker);
    }

    /// Pointer to buffer `index`.
    fn ptr(&self, index: u32) -> *mut u8 {
        // In bounds: index < count and the region holds count * buffer_len.
        unsafe { self.base.add(index as usize * self.buffer_len) }
    }
}

/// An owner-held page buffer in a state with possible NIC access.
///
/// No slice or pointer API: the owner can only post it, move it between
/// owner states, or turn it into a [`RegisteredPage`] after the fence.
#[must_use = "a dropped token loses track of a registered buffer"]
#[derive(Debug, Eq, PartialEq)]
pub(crate) struct Buffer {
    index: u32,
}

impl Buffer {
    /// Buffer index in the pool.
    #[cfg_attr(not(test), allow(dead_code, reason = "used by tests and diagnostics"))]
    pub(crate) fn index(&self) -> u32 {
        self.index
    }
}

/// The owner's side of one device's page buffers. Not `Send`.
pub(crate) struct Pool {
    shared: Arc<PoolShared>,

    /// `None` only during drop.
    region: Option<Rc<NativeRegion>>,

    quota: Arc<GuardOwner>,
}

impl Pool {
    /// Split `region` into `region.length() / buffer_len` page buffers.
    ///
    /// `buffer_len` must be nonzero and at most `u32::MAX`, and the region
    /// must hold at least one buffer.
    pub(crate) fn new(
        region: Rc<NativeRegion>,
        buffer_len: usize,
        quota: Arc<GuardOwner>,
    ) -> Result<Self> {
        if buffer_len == 0 || buffer_len > u32::MAX as usize {
            return Err(Error::InvalidConfiguration);
        }

        let count = region.length() / buffer_len;
        if count == 0 || count > u32::MAX as usize {
            return Err(Error::InvalidConfiguration);
        }

        let base = region.as_ptr();
        if base.is_null() {
            return Err(Error::Io);
        }

        Ok(Self {
            shared: Arc::new(PoolShared {
                base,
                buffer_len,
                states: (0..count)
                    .map(|_| AtomicU8::new(BufferState::Free.code()))
                    .collect(),
                // Pop from the end, so reverse to hand out low indices first.
                free: Mutex::new((0..count as u32).rev().collect()),
                owner: AtomicWaker::new(),
                waiters: Mutex::new(Vec::new()),
            }),
            region: Some(region),
            quota,
        })
    }

    /// State shared with page and send buffers.
    pub(crate) fn shared(&self) -> &Arc<PoolShared> {
        &self.shared
    }

    /// The registered region.
    pub(crate) fn region(&self) -> &Rc<NativeRegion> {
        self.region.as_ref().unwrap()
    }

    /// Byte offset of `buffer` in the region.
    pub(crate) fn offset(&self, buffer: &Buffer) -> u64 {
        buffer.index as u64 * self.shared.buffer_len as u64
    }

    /// Take a free buffer straight into an owner `state`.
    pub(crate) fn take(&self, state: BufferState) -> Option<Buffer> {
        assert!(state.owner_held() && state != BufferState::Quarantined);
        self.shared.pop(state).map(|index| Buffer { index })
    }

    /// Move an owner-held buffer to another owner `state`.
    pub(crate) fn set(&self, buffer: &Buffer, state: BufferState) {
        assert!(state.owner_held());
        self.shared.set(buffer.index, state);
    }

    /// Hand a send buffer to the owner for a posted write: `Staging` to
    /// `Sending`. Returns the token and the staged length.
    ///
    /// Fails with [`Error::InvalidRequest`] if it belongs to another pool.
    pub(crate) fn send(
        &self,
        staged: SendBuffer,
    ) -> std::result::Result<(Buffer, usize), (Error, SendBuffer)> {
        if !Arc::ptr_eq(&staged.shared, &self.shared) {
            return Err((Error::InvalidRequest, staged));
        }

        let staged = std::mem::ManuallyDrop::new(staged);
        let index = staged.index;
        let len = staged.len;
        // SAFETY: read once out of a `ManuallyDrop` that is never used again.
        drop(unsafe { std::ptr::read(&staged.shared) });
        self.shared.set(index, BufferState::Sending);

        Ok((Buffer { index }, len))
    }

    /// Fence done (or nothing was ever exposed): return to `Free`.
    pub(crate) fn release(&self, buffer: Buffer) {
        self.shared.push(buffer.index);
    }

    /// Fence done after a write: hand `len` bytes to the CPU as `Owned`.
    pub(crate) fn own(
        &self,
        buffer: Buffer,
        len: usize,
    ) -> std::result::Result<RegisteredPage, Buffer> {
        if len == 0 || len > self.shared.buffer_len {
            return Err(buffer);
        }

        self.shared.set(buffer.index, BufferState::Owned);

        Ok(RegisteredPage {
            shared: self.shared.clone(),
            index: buffer.index,
            len,
        })
    }

    /// Destroy failed: the buffer is never reused.
    pub(crate) fn leak(&self, buffer: Buffer) {
        self.shared.set(buffer.index, BufferState::Leaked);
    }

    /// Page and send buffers still alive outside the owner.
    pub(crate) fn outstanding(&self) -> usize {
        Arc::strong_count(&self.shared) - 1
    }

    /// True if dropping now would deregister the region: no buffer is
    /// outside `Free`.
    pub(crate) fn idle(&self) -> bool {
        self.outstanding() == 0 && self.shared.count_in(BufferState::Free) == self.shared.count()
    }
}

impl Drop for Pool {
    /// Deregister when idle. Otherwise leak the region and its charge: the
    /// NIC or a page may still use the memory.
    fn drop(&mut self) {
        if !self.idle() {
            self.quota.quarantine();
            std::mem::forget(self.quota.clone());
            std::mem::forget(self.region.take());
        }
    }
}

/// A received page: no window covers it and no work request uses it.
///
/// The only way to read received registered memory. `Send` and `Sync`.
/// Dropping it, on any thread, returns the buffer to the pool.
pub struct RegisteredPage {
    shared: Arc<PoolShared>,

    index: u32,

    len: usize,
}

impl RegisteredPage {
    /// The received bytes.
    pub fn bytes(&self) -> &[u8] {
        // SAFETY: state `Owned`: this value exclusively holds the buffer and
        // the NIC cannot write it; the memory outlives `shared`.
        unsafe { std::slice::from_raw_parts(self.shared.ptr(self.index), self.len) }
    }

    /// Number of received bytes.
    pub fn len(&self) -> usize {
        self.len
    }

    /// True if no bytes were received. Never true for a delivered page.
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }
}

impl std::fmt::Debug for RegisteredPage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RegisteredPage")
            .field("index", &self.index)
            .field("len", &self.len)
            .finish()
    }
}

impl Drop for RegisteredPage {
    fn drop(&mut self) {
        self.shared.push(self.index);
    }
}

/// A free page buffer staged for sending: CPU writes only, no NIC access.
///
/// `Send`. Dropping it returns the buffer to the pool.
pub struct SendBuffer {
    shared: Arc<PoolShared>,

    index: u32,

    len: usize,
}

impl SendBuffer {
    /// The staged bytes (zeroed or stale content; callers fill them).
    pub fn bytes_mut(&mut self) -> &mut [u8] {
        // SAFETY: state `Staging`: this value exclusively holds the buffer
        // and the NIC cannot access it.
        unsafe { std::slice::from_raw_parts_mut(self.shared.ptr(self.index), self.len) }
    }

    /// Read back the staged bytes.
    pub fn bytes(&self) -> &[u8] {
        // SAFETY: as in `bytes_mut`.
        unsafe { std::slice::from_raw_parts(self.shared.ptr(self.index), self.len) }
    }

    /// Shorten the staged length. Longer values are ignored.
    pub fn truncate(&mut self, len: usize) {
        self.len = self.len.min(len);
    }

    /// Staged length.
    pub fn len(&self) -> usize {
        self.len
    }

    /// True if nothing is staged. Empty buffers cannot be written.
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }
}

impl std::fmt::Debug for SendBuffer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SendBuffer")
            .field("index", &self.index)
            .field("len", &self.len)
            .finish()
    }
}

impl Drop for SendBuffer {
    fn drop(&mut self) {
        self.shared.push(self.index);
    }
}

#[cfg(test)]
mod tests {
    use std::{
        sync::atomic::AtomicUsize,
        task::{Wake, Waker},
    };

    use super::*;
    use crate::{
        ffi::{Access, NativeDevice, lifetime_tests},
        simulation::{Device, Simulation},
    };

    const LEN: usize = 4096;

    fn device(sim: &Simulation) -> Rc<NativeDevice> {
        let node = sim
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = node.enter();
        Rc::new(crate::ffi::discover().unwrap().remove(0))
    }

    /// A pool of `count` buffers plus its charge observer.
    fn pool(sim: &Simulation, count: usize) -> (Pool, lifetime_tests::QuotaObserver) {
        let device = device(sim);
        let (quota, observer) = lifetime_tests::quota();
        let region =
            NativeRegion::with_access(device, count * LEN + 100, Access::PAGES, quota.clone())
                .unwrap();
        (Pool::new(region, LEN, quota).unwrap(), observer)
    }

    struct Count(AtomicUsize);

    impl Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    #[test]
    /// Bad buffer lengths and regions too small for one buffer are refused.
    fn configuration_is_validated() {
        let sim = Simulation::new();
        let device = device(&sim);
        let (quota, _) = lifetime_tests::quota();
        let small =
            NativeRegion::with_access(device.clone(), LEN - 1, Access::PAGES, quota.clone())
                .unwrap();
        assert_eq!(
            Pool::new(small, LEN, quota.clone()).err(),
            Some(Error::InvalidConfiguration)
        );
        let region = NativeRegion::with_access(device, LEN, Access::PAGES, quota.clone()).unwrap();
        assert_eq!(
            Pool::new(region, 0, quota).err(),
            Some(Error::InvalidConfiguration)
        );
        assert_eq!(sim.live_resources(), 0, "failed pools deregister");
    }

    #[test]
    /// The receive path: Free, Binding, Exposed, Invalidating, Owned, Free.
    fn receive_transitions_end_owned_then_free() {
        let sim = Simulation::new();
        let (pool, charge) = pool(&sim, 2);
        let shared = pool.shared().clone();
        assert_eq!(shared.count(), 2);
        assert_eq!(shared.buffer_len(), LEN);
        assert_eq!(pool.region().length(), 2 * LEN + 100);
        let buffer = pool.take(BufferState::Binding).unwrap();
        assert_eq!(buffer.index(), 0);
        assert_eq!(pool.offset(&buffer), 0);
        assert_eq!(shared.state(0), BufferState::Binding);
        pool.set(&buffer, BufferState::Exposed);
        assert_eq!(shared.state(0), BufferState::Exposed);
        pool.set(&buffer, BufferState::Invalidating);
        // Too long or empty: the token comes back untouched.
        let buffer = pool.own(buffer, LEN + 1).unwrap_err();
        let buffer = pool.own(buffer, 0).unwrap_err();
        assert_eq!(shared.state(0), BufferState::Invalidating);
        let page = pool.own(buffer, 10).unwrap();
        assert_eq!(shared.state(0), BufferState::Owned);
        assert_eq!(page.bytes(), [0; 10]);
        assert_eq!((page.len(), page.is_empty()), (10, false));
        assert_eq!(pool.outstanding(), 2, "the page and the test's clone");
        drop(page);
        assert_eq!(shared.state(0), BufferState::Free);
        assert!(
            !pool.idle(),
            "the test's shared clone counts as outstanding"
        );
        drop(shared);
        assert!(pool.idle());
        drop(pool);
        assert_eq!(charge.get(), 0);
        assert_eq!(sim.live_resources(), 0, "region and device are freed");
    }

    #[test]
    /// The send path: Staging, Sending, Free; and released grants go Free.
    fn send_and_release_transitions() {
        let sim = Simulation::new();
        let (pool, _charge) = pool(&sim, 2);
        let shared = pool.shared().clone();
        let mut staged = shared.stage(5).unwrap();
        assert_eq!(shared.state(staged.index), BufferState::Staging);
        staged.bytes_mut().copy_from_slice(b"hello");
        staged.truncate(9);
        assert_eq!(staged.len(), 5);
        staged.truncate(4);
        assert_eq!(staged.bytes(), b"hell");
        assert!(!staged.is_empty());
        let (buffer, len) = pool.send(staged).unwrap();
        assert_eq!(len, 4);
        assert_eq!(shared.state(buffer.index()), BufferState::Sending);
        assert_eq!(pool.outstanding(), 1, "only the test's clone");
        pool.release(buffer);
        let granted = pool.take(BufferState::Binding).unwrap();
        pool.release(granted);
        assert_eq!(shared.count_in(BufferState::Free), 2);
        // Bad staging lengths.
        assert!(shared.stage(0).is_none());
        assert!(shared.stage(LEN + 1).is_none());
        let waker = Waker::from(Arc::new(Count(AtomicUsize::new(0))));
        assert!(matches!(
            shared.poll_stage(0, &waker),
            Some(Err(Error::InvalidRange))
        ));
    }

    #[test]
    /// Exhaustion waits; a buffer dropped on another thread wakes waiters
    /// and the owner and is reusable.
    fn exhaustion_and_cross_thread_return() {
        let sim = Simulation::new();
        let (pool, _charge) = pool(&sim, 1);
        let shared = pool.shared().clone();
        let owner = Arc::new(Count(AtomicUsize::new(0)));
        shared.register_owner(&Waker::from(owner.clone()));
        let buffer = pool.take(BufferState::Binding).unwrap();
        assert!(pool.take(BufferState::Binding).is_none());
        let page = pool.own(buffer, 3).unwrap();
        let waiter = Arc::new(Count(AtomicUsize::new(0)));
        assert!(shared.poll_stage(3, &Waker::from(waiter.clone())).is_none());
        std::thread::spawn(move || {
            assert_eq!(page.bytes(), [0; 3]);
            drop(page);
        })
        .join()
        .unwrap();
        assert_eq!(waiter.0.load(Ordering::SeqCst), 1);
        assert_eq!(owner.0.load(Ordering::SeqCst), 1);
        let staged = shared.poll_stage(3, &Waker::from(waiter)).unwrap().unwrap();
        std::thread::spawn(move || drop(staged)).join().unwrap();
        drop(shared);
        assert!(pool.idle());
    }

    #[test]
    /// A send buffer from another pool is refused and returned intact.
    fn foreign_send_buffer_is_refused() {
        let sim = Simulation::new();
        let (left, _a) = pool(&sim, 1);
        let (right, _b) = pool(&sim, 1);
        let staged = right.shared().stage(1).unwrap();
        let (error, staged) = left.send(staged).unwrap_err();
        assert_eq!(error, Error::InvalidRequest);
        assert_eq!(right.shared().state(0), BufferState::Staging);
        drop(staged);
        assert!(right.idle() && left.idle());
    }

    #[test]
    /// Dropping the pool while a page is alive leaks the region and charge,
    /// so the page stays readable and counted.
    fn region_outlives_pages() {
        let sim = Simulation::new();
        let (pool, charge) = pool(&sim, 1);
        let buffer = pool.take(BufferState::Binding).unwrap();
        let page = pool.own(buffer, 4).unwrap();
        drop(pool);
        assert_eq!(page.bytes(), [0; 4]);
        drop(page);
        assert_eq!(charge.get(), 1, "leaked region keeps the guard");
        assert_eq!(sim.live_resources(), 2, "device and region stay");
    }

    #[test]
    /// A leaked (or quarantined) buffer keeps the region and guard forever.
    fn leaked_buffer_keeps_guard() {
        for state in [BufferState::Leaked, BufferState::Quarantined] {
            let sim = Simulation::new();
            let (pool, charge) = pool(&sim, 2);
            let buffer = pool.take(BufferState::Exposed).unwrap();
            if state == BufferState::Leaked {
                pool.leak(buffer);
            } else {
                pool.set(&buffer, BufferState::Quarantined);
                // The token is dropped with the pool (owner shutdown).
                let _ = buffer;
            }
            assert_eq!(pool.shared().state(0), state);
            assert!(pool.outstanding() == 0 && !pool.idle());
            drop(pool);
            assert_eq!(charge.get(), 1);
            assert_eq!(sim.live_resources(), 2);
        }
    }
}
