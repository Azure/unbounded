//! Registered page buffers and their ownership states.
//!
//! See `designs/racer-verbs.md`, "Page buffer pool". One registered region
//! per device is split into equal page buffers. Each buffer has exactly one
//! state at a time, kept in an atomic array so other threads can observe it.
//!
//! The type system enforces who may touch the memory:
//!
//! - [`RegisteredPage`] (`Owned`) gives `&[u8]`. It is the only way Rust
//!   code can see page memory, and it exists only while no memory window
//!   covers the buffer and no work request uses it.
//! - Every other state is held by the owner thread as a [`Buffer`] token,
//!   which has no slice or pointer API at all. The owner moves a token to
//!   `Owned` only after the fence (receive completion, then a signaled
//!   `LOCAL_INV` completion).
//!
//! A rail may also hold a device pool: page buffers in caller memory reached
//! through a dma-buf (for example GPU memory). Its buffers use the same
//! states and the same fence, but a fenced device buffer becomes a
//! [`DevicePage`], which has no slice or pointer API: the CPU never reads or
//! copies device memory.
//!
//! Page buffers only receive. Writes read caller memory in place through the
//! device's implicit on-demand paging region, which every pool holds.
//!
//! Lifetime: [`RegisteredPage`] holds an `Arc<PoolShared>`. [`Pool`] owns
//! the regions. When the pool is dropped while any page is still alive, or
//! while any buffer is `Leaked` or `Quarantined`,
//! the region is leaked on purpose (never deregistered) and the charge is
//! quarantined, so the memory stays valid and counted for as long as anything
//! may use it.

use std::{
    collections::BTreeMap,
    rc::Rc,
    sync::{
        Arc, Mutex, MutexGuard, PoisonError,
        atomic::{AtomicU8, Ordering},
    },
    task::Waker,
};

use futures::task::AtomicWaker;

use crate::{DeviceMemory, Error, GuardOwner, Result, ffi::NativeRegion};

/// Ownership state of one page buffer.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BufferState {
    /// On the free list. No CPU or NIC access.
    Free,

    /// Owner posted a `BIND_MW` over it.
    Binding,

    /// The window's rkey was sent in a `Grant`: the peer may write.
    Exposed,

    /// Owner posted a signaled `LOCAL_INV`: the peer may write until its CQE.
    Invalidating,

    /// Held by a [`RegisteredPage`]: CPU reads, no NIC access.
    Owned,

    /// Its link is breaking: the NIC may still write until the QP is gone.
    Quarantined,

    /// Destroy failed: never reused, counted forever.
    Leaked,
}

const STATES: [BufferState; 7] = [
    BufferState::Free,
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
            Self::Binding | Self::Exposed | Self::Invalidating | Self::Quarantined
        )
    }
}

/// Lock ignoring poison. Critical sections here never panic midway.
fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

/// State shared by the owner and every received page.
pub(crate) struct PoolShared {
    /// Start of the registered memory. Valid while any `Arc` exists (see the
    /// module docs). Null for a device pool.
    base: *mut u8,

    /// The dma-buf owner of a device pool.
    memory: Option<DeviceMemory>,

    buffer_len: usize,

    states: Box<[AtomicU8]>,

    free: Mutex<Vec<u32>>,

    /// The owner, woken when a buffer returns.
    owner: AtomicWaker,
}

// SAFETY: `base` is only dereferenced through `RegisteredPage`, which
// exclusively holds one buffer index in a state with no NIC access, and the
// memory outlives every `Arc<PoolShared>`.
unsafe impl Send for PoolShared {}
unsafe impl Sync for PoolShared {}

impl PoolShared {
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

    /// Return buffer `index` to the free list and wake the owner.
    fn push(&self, index: u32) {
        self.set(index, BufferState::Free);
        lock(&self.free).push(index);
        self.owner.wake();
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

    /// The device pool it is in, if any.
    device: Option<u32>,
}

impl Buffer {
    /// True for a device pool buffer.
    pub(crate) fn is_device(&self) -> bool {
        self.device.is_some()
    }

    /// The device pool it is in, if any.
    pub(crate) fn device(&self) -> Option<u32> {
        self.device
    }
}

/// A rail's device pool. See the module docs.
struct DevicePool {
    shared: Arc<PoolShared>,

    /// `None` only during drop.
    region: Option<Rc<NativeRegion>>,
}

/// The owner's side of one device's page buffers. Not `Send`.
pub(crate) struct Pool {
    shared: Arc<PoolShared>,

    /// `None` only during drop.
    region: Option<Rc<NativeRegion>>,

    /// The device's implicit on-demand paging region: the local key for
    /// every write source. `None` only during drop.
    implicit: Option<Rc<NativeRegion>>,

    /// Device page buffers by caller id, once attached.
    device: BTreeMap<u32, DevicePool>,

    quota: Arc<GuardOwner>,
}

impl Pool {
    /// Split `region` into `region.length() / buffer_len` receive buffers,
    /// and write from caller memory through `implicit` (from
    /// [`NativeRegion::implicit`]).
    ///
    /// `buffer_len` must be nonzero and at most `u32::MAX`, the region must
    /// hold at least one buffer, and `implicit` must be implicit.
    pub(crate) fn new(
        region: Rc<NativeRegion>,
        implicit: Rc<NativeRegion>,
        buffer_len: usize,
        quota: Arc<GuardOwner>,
    ) -> Result<Self> {
        if !implicit.is_implicit() || buffer_len == 0 || buffer_len > u32::MAX as usize {
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
                memory: None,
                buffer_len,
                states: (0..count)
                    .map(|_| AtomicU8::new(BufferState::Free.code()))
                    .collect(),
                // Pop from the end, so reverse to hand out low indices first.
                free: Mutex::new((0..count as u32).rev().collect()),
                owner: AtomicWaker::new(),
            }),
            region: Some(region),
            implicit: Some(implicit),
            device: BTreeMap::new(),
            quota,
        })
    }

    /// Split the dma-buf `region` (from [`NativeRegion::dmabuf`]) into
    /// device page buffers of the host buffer length, as device pool `id`.
    /// A rail may hold several device pools (one per GPU, say), each with
    /// its own id.
    pub(crate) fn attach_device(&mut self, id: u32, region: Rc<NativeRegion>) -> Result<()> {
        if self.device.contains_key(&id) {
            return Err(Error::InvalidRequest);
        }

        let Some(memory) = region.memory().cloned() else {
            return Err(Error::InvalidConfiguration);
        };

        let buffer_len = self.shared.buffer_len;
        let count = region.length() / buffer_len;
        if count == 0 || count > u32::MAX as usize {
            return Err(Error::InvalidConfiguration);
        }

        self.device.insert(
            id,
            DevicePool {
                shared: Arc::new(PoolShared {
                    base: std::ptr::null_mut(),
                    memory: Some(memory),
                    buffer_len,
                    states: (0..count)
                        .map(|_| AtomicU8::new(BufferState::Free.code()))
                        .collect(),
                    free: Mutex::new((0..count as u32).rev().collect()),
                    owner: AtomicWaker::new(),
                }),
                region: Some(region),
            },
        );
        Ok(())
    }

    /// State shared with device pages of pool `id`, if attached.
    pub(crate) fn device_shared(&self, id: u32) -> Option<&Arc<PoolShared>> {
        self.device.get(&id).map(|d| &d.shared)
    }

    /// Register the owner's waker for buffers returned to either pool.
    pub(crate) fn register_owner(&self, waker: &Waker) {
        self.shared.register_owner(waker);
        for device in self.device.values() {
            device.shared.register_owner(waker);
        }
    }

    /// State shared by `buffer`'s pool.
    fn shared_of(&self, buffer: &Buffer) -> &Arc<PoolShared> {
        if let Some(id) = buffer.device {
            &self
                .device
                .get(&id)
                .expect("device buffer without pool")
                .shared
        } else {
            &self.shared
        }
    }

    /// The registered region `buffer` lives in.
    pub(crate) fn region_of(&self, buffer: &Buffer) -> &Rc<NativeRegion> {
        if let Some(id) = buffer.device {
            let device = self.device.get(&id).expect("device buffer without pool");
            device.region.as_ref().unwrap()
        } else {
            self.region()
        }
    }

    /// Take a free buffer of device pool `id` straight into an owner
    /// `state`. `None` if that pool is not attached or none is free.
    pub(crate) fn take_device(&self, id: u32, state: BufferState) -> Option<Buffer> {
        assert!(state.owner_held() && state != BufferState::Quarantined);
        let device = self.device.get(&id)?;
        device.shared.pop(state).map(|index| Buffer {
            index,
            device: Some(id),
        })
    }

    /// Fence done after a write into a device buffer: hand `len` bytes to
    /// the caller as a [`DevicePage`].
    pub(crate) fn own_device(
        &self,
        buffer: Buffer,
        len: usize,
    ) -> std::result::Result<DevicePage, Buffer> {
        if buffer.device.is_none() || len == 0 || len > self.shared.buffer_len {
            return Err(buffer);
        }

        let shared = self.shared_of(&buffer).clone();
        shared.set(buffer.index, BufferState::Owned);

        Ok(DevicePage {
            shared,
            index: buffer.index,
            len,
        })
    }

    /// State shared with received pages.
    pub(crate) fn shared(&self) -> &Arc<PoolShared> {
        &self.shared
    }

    /// The registered region.
    pub(crate) fn region(&self) -> &Rc<NativeRegion> {
        self.region.as_ref().unwrap()
    }

    /// The implicit on-demand paging region that write sources go through.
    pub(crate) fn implicit(&self) -> &Rc<NativeRegion> {
        self.implicit.as_ref().unwrap()
    }

    /// Byte offset of `buffer` in the region.
    pub(crate) fn offset(&self, buffer: &Buffer) -> u64 {
        buffer.index as u64 * self.shared.buffer_len as u64
    }

    /// Take a free buffer straight into an owner `state`.
    pub(crate) fn take(&self, state: BufferState) -> Option<Buffer> {
        assert!(state.owner_held() && state != BufferState::Quarantined);
        self.shared.pop(state).map(|index| Buffer {
            index,
            device: None,
        })
    }

    /// Move an owner-held buffer to another owner `state`.
    pub(crate) fn set(&self, buffer: &Buffer, state: BufferState) {
        assert!(state.owner_held());
        self.shared_of(buffer).set(buffer.index, state);
    }

    /// Fence done (or nothing was ever exposed): return to `Free`.
    pub(crate) fn release(&self, buffer: Buffer) {
        self.shared_of(&buffer).push(buffer.index);
    }

    /// Fence done after a write: hand `len` bytes to the CPU as `Owned`.
    pub(crate) fn own(
        &self,
        buffer: Buffer,
        len: usize,
    ) -> std::result::Result<RegisteredPage, Buffer> {
        if buffer.device.is_some() || len == 0 || len > self.shared.buffer_len {
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
        self.shared_of(&buffer)
            .set(buffer.index, BufferState::Leaked);
    }

    /// Received pages still alive outside the owner.
    pub(crate) fn outstanding(&self) -> usize {
        Arc::strong_count(&self.shared) - 1
            + self
                .device
                .values()
                .map(|d| Arc::strong_count(&d.shared) - 1)
                .sum::<usize>()
    }

    /// True if dropping now would deregister the region: no buffer is
    /// outside `Free`.
    pub(crate) fn idle(&self) -> bool {
        let free = |shared: &PoolShared| shared.count_in(BufferState::Free) == shared.count();
        self.outstanding() == 0
            && free(&self.shared)
            && self.device.values().all(|d| free(&d.shared))
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
            std::mem::forget(self.implicit.take());
            for device in self.device.values_mut() {
                std::mem::forget(device.region.take());
            }
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

/// A received device page: no window covers it and no work request uses it.
///
/// The bytes are `memory()[offset()..offset() + len()]`, in memory the CPU
/// never maps. Read them with the owner of the memory, such as a GPU kernel,
/// and keep this value alive until that work is done. `Send` and `Sync`.
/// Dropping it, on any thread, returns the buffer to the device pool, after
/// which the next peer write may land there.
pub struct DevicePage {
    shared: Arc<PoolShared>,

    index: u32,

    len: usize,
}

impl DevicePage {
    /// The memory the peer wrote into.
    pub fn memory(&self) -> &DeviceMemory {
        self.shared.memory.as_ref().expect("device pool memory")
    }

    /// Offset of the first received byte from the start of
    /// [`Self::memory`] (after [`crate::DmabufMemory::offset`]).
    pub fn offset(&self) -> u64 {
        self.index as u64 * self.shared.buffer_len as u64
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

impl std::fmt::Debug for DevicePage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DevicePage")
            .field("index", &self.index)
            .field("len", &self.len)
            .finish()
    }
}

impl Drop for DevicePage {
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
        let region = NativeRegion::with_access(
            device.clone(),
            count * LEN + 100,
            Access::PAGES,
            quota.clone(),
        )
        .unwrap();
        let implicit = NativeRegion::implicit(device, quota.clone()).unwrap();
        (Pool::new(region, implicit, LEN, quota).unwrap(), observer)
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
        let implicit = || NativeRegion::implicit(device.clone(), quota.clone()).unwrap();
        let small =
            NativeRegion::with_access(device.clone(), LEN - 1, Access::PAGES, quota.clone())
                .unwrap();
        assert_eq!(
            Pool::new(small, implicit(), LEN, quota.clone()).err(),
            Some(Error::InvalidConfiguration)
        );
        let region =
            NativeRegion::with_access(device.clone(), LEN, Access::PAGES, quota.clone()).unwrap();
        assert_eq!(
            Pool::new(region.clone(), implicit(), 0, quota.clone()).err(),
            Some(Error::InvalidConfiguration)
        );
        // Writes need the implicit region; an ordinary region is refused.
        let ordinary =
            NativeRegion::with_access(device.clone(), LEN, Access::PAGES, quota.clone()).unwrap();
        assert_eq!(
            Pool::new(region, ordinary, LEN, quota.clone()).err(),
            Some(Error::InvalidConfiguration)
        );
        drop(device);
        assert_eq!(sim.live_resources(), 0, "failed pools deregister");
    }

    #[test]
    /// The receive path: Free, Binding, Exposed, Invalidating, Owned, Free.
    fn receive_transitions_end_owned_then_free() {
        let sim = Simulation::new();
        let (pool, charge) = pool(&sim, 2);
        let shared = pool.shared().clone();
        assert_eq!(shared.count(), 2);
        assert_eq!(shared.buffer_len, LEN);
        assert_eq!(pool.region().length(), 2 * LEN + 100);
        let buffer = pool.take(BufferState::Binding).unwrap();
        assert_eq!(buffer.index, 0);
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
    /// Exhaustion refuses; a page dropped on another thread wakes the owner
    /// and its buffer is reusable.
    fn exhaustion_and_cross_thread_return() {
        let sim = Simulation::new();
        let (pool, _charge) = pool(&sim, 1);
        let shared = pool.shared().clone();
        let owner = Arc::new(Count(AtomicUsize::new(0)));
        shared.register_owner(&Waker::from(owner.clone()));
        let buffer = pool.take(BufferState::Binding).unwrap();
        assert!(pool.take(BufferState::Binding).is_none());
        let page = pool.own(buffer, 3).unwrap();
        std::thread::spawn(move || {
            assert_eq!(page.bytes(), [0; 3]);
            drop(page);
        })
        .join()
        .unwrap();
        assert_eq!(owner.0.load(Ordering::SeqCst), 1);
        let buffer = pool.take(BufferState::Binding).unwrap();
        pool.release(buffer);
        drop(shared);
        assert!(pool.idle());
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
        assert_eq!(sim.live_resources(), 3, "device and both regions stay");
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
            assert_eq!(sim.live_resources(), 3);
        }
    }

    #[test]
    /// A device pool shares the buffer states and fence, hands out
    /// `DevicePage` instead of bytes, keeps host and device buffers apart,
    /// and leaks its region with the host region when not idle.
    fn device_pool_buffers_and_pages() {
        let sim = Simulation::new();
        let device = self::device(&sim);
        let (quota, charge) = lifetime_tests::quota();
        let region =
            NativeRegion::with_access(device.clone(), LEN, Access::PAGES, quota.clone()).unwrap();
        let implicit = NativeRegion::implicit(device.clone(), quota.clone()).unwrap();
        let mut pool = Pool::new(region, implicit, LEN, quota).unwrap();
        let memory = crate::simulation::MemfdMemory::new(2 * LEN + 1, 0x9000).unwrap();
        let (quota, _observer) = lifetime_tests::quota();
        assert!(pool.take_device(0, BufferState::Binding).is_none());
        // Host regions are refused as device pools.
        let host =
            NativeRegion::with_access(device.clone(), LEN, Access::PAGES, quota.clone()).unwrap();
        assert_eq!(
            pool.attach_device(0, host),
            Err(Error::InvalidConfiguration)
        );
        let region = NativeRegion::dmabuf(device.clone(), memory.clone(), quota.clone()).unwrap();
        pool.attach_device(0, region.clone()).unwrap();
        assert_eq!(pool.attach_device(0, region), Err(Error::InvalidRequest));
        let shared = pool.device_shared(0).unwrap().clone();
        assert_eq!(shared.count(), 2);
        let buffer = pool.take_device(0, BufferState::Binding).unwrap();
        assert!(buffer.is_device());
        assert_eq!(buffer.device(), Some(0));
        assert!(pool.region_of(&buffer).is_device());
        assert_eq!(pool.region_of(&buffer).address(), 0x9000);
        pool.set(&buffer, BufferState::Invalidating);
        assert_eq!(shared.state(0), BufferState::Invalidating);
        assert_eq!(pool.shared().state(0), BufferState::Free, "host untouched");
        // A device buffer never becomes a host page with bytes.
        let buffer = pool.own(buffer, 10).unwrap_err();
        let page = pool.own_device(buffer, 10).unwrap();
        assert_eq!((page.offset(), page.len()), (0, 10));
        assert_eq!(shared.state(0), BufferState::Owned);
        let second = pool.take_device(0, BufferState::Binding).unwrap();
        let second = pool.own_device(second, LEN).unwrap();
        assert_eq!(second.offset(), LEN as u64);
        assert!(pool.take_device(0, BufferState::Binding).is_none());
        assert!(pool.take_device(1, BufferState::Binding).is_none());
        let host = pool.take(BufferState::Binding).unwrap();
        let host = pool.own_device(host, 1).unwrap_err();
        pool.release(host);
        drop(second);
        assert!(!pool.idle(), "a device page is outstanding");
        drop(shared);
        drop(pool);
        // Dropped while a device page lives: both regions leak with the charge.
        assert_eq!(page.memory().length(), 2 * LEN + 1);
        drop(page);
        assert_eq!(charge.get(), 1);
        assert!(
            Arc::strong_count(&memory) > 1,
            "leaked region keeps the owner"
        );
    }

    #[test]
    /// Several device pools on one rail keep their buffers apart: each id
    /// hands out its own buffers over its own memory, and the pool is idle
    /// only when every device pool is.
    fn device_pools_are_kept_apart_by_id() {
        let sim = Simulation::new();
        let device = self::device(&sim);
        let (quota, _charge) = lifetime_tests::quota();
        let region =
            NativeRegion::with_access(device.clone(), LEN, Access::PAGES, quota.clone()).unwrap();
        let implicit = NativeRegion::implicit(device.clone(), quota.clone()).unwrap();
        let mut pool = Pool::new(region, implicit, LEN, quota).unwrap();
        let (quota, _observer) = lifetime_tests::quota();
        let first = crate::simulation::MemfdMemory::new(LEN, 0x9000).unwrap();
        let second = crate::simulation::MemfdMemory::new(2 * LEN, 0xa000).unwrap();
        let attach = |pool: &mut Pool, id, memory| {
            let region = NativeRegion::dmabuf(device.clone(), memory, quota.clone()).unwrap();
            pool.attach_device(id, region)
        };
        attach(&mut pool, 3, first.clone()).unwrap();
        attach(&mut pool, 7, second.clone()).unwrap();
        assert_eq!(attach(&mut pool, 7, second), Err(Error::InvalidRequest));
        assert_eq!(pool.device_shared(3).unwrap().count(), 1);
        assert_eq!(pool.device_shared(7).unwrap().count(), 2);
        let a = pool.take_device(3, BufferState::Binding).unwrap();
        assert!(pool.take_device(3, BufferState::Binding).is_none());
        let b = pool.take_device(7, BufferState::Binding).unwrap();
        assert_eq!((a.device(), b.device()), (Some(3), Some(7)));
        assert_eq!(pool.region_of(&a).address(), 0x9000);
        assert_eq!(pool.region_of(&b).address(), 0xa000);
        let page = pool.own_device(b, 5).unwrap();
        assert_eq!(page.memory().length(), 2 * LEN);
        pool.release(a);
        assert!(!pool.idle(), "a page of pool 7 is outstanding");
        drop(page);
        assert!(pool.idle());
        drop(first);
    }
}
