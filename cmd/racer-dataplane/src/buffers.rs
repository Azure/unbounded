// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! NUMA-local, registration-ready transient 4 MiB buffers.
//!
//! Construct `Pools` before `Workers::start`, then call `for_worker` in its pinned
//! factory. Setup requires Linux 5.14+ (`MADV_POPULATE_WRITE`) and mbind permission.
//! A slot returns to the free pool only when its final live holder releases it.
//! Network single-flight coordination is independent of payload storage.
//!
//! Registration and I/O have separate lifetimes. io_uring registrations own a
//! `MemoryLease` and use `MemoryLease::buffers`; RDMA registrations own a lease
//! for `MemoryLease::region` per protection domain. Drivers own registration
//! indices and lkeys/rkeys and must deregister before releasing their leases.
//!
//! Raw asynchronous access is unsafe: a submitted read/receive must own a `Fill`,
//! and a write/send must own a `Buffer`, until completion or proven quiescence.
//! Cancellation alone is insufficient. A lease prevents unmapping, not recycling
//! or conflicting access. Safe wrappers must enforce this even on future drop or
//! shutdown failure; leaking is safer than reuse.

use crate::workers::{NumaNodeId, WorkerContext};
use std::collections::{BTreeMap, HashMap};
use std::io;
use std::marker::PhantomData;
use std::num::NonZeroUsize;
use std::ptr::NonNull;
use std::rc::Rc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::task::Waker;

pub const BUFFER_SIZE: usize = 4 * 1024 * 1024;

#[derive(Clone, Copy, Debug)]
pub struct Config {
    pub buffers_per_node: NonZeroUsize,
    /// Coordination capacity, independent of payload storage. Includes detached terminals.
    pub network_flights: NonZeroUsize,
    /// Includes unpolled and terminal network consumer leases.
    pub consumers_per_flight: NonZeroUsize,
}
impl Config {
    pub fn new(buffers_per_node: NonZeroUsize) -> Self {
        Self {
            buffers_per_node,
            network_flights: NonZeroUsize::new(128).unwrap(),
            consumers_per_flight: NonZeroUsize::new(64).unwrap(),
        }
    }
}

/// Complete value identity bound to private transport staging, never a cache key.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct Key([u8; 32]);
impl Key {
    pub const fn new(identity: [u8; 32]) -> Self {
        Self(identity)
    }
}

/// Setup-only registry. All workers on a node share one allocation and free list.
pub struct Pools {
    config: Config,
    nodes: Mutex<BTreeMap<NumaNodeId, Arc<Node>>>,
}
impl Pools {
    pub fn new(config: Config) -> Self {
        Self {
            config,
            nodes: Mutex::new(BTreeMap::new()),
        }
    }
    #[cfg(test)]
    pub(crate) fn test_for_worker(&self, placement: &WorkerContext) -> io::Result<WorkerPool> {
        self.nodes
            .lock()
            .unwrap()
            .entry(placement.numa_node_id())
            .or_insert_with(|| {
                Arc::new(Node::new(self.config, placement.numa_node_id(), |_| Ok(())).unwrap())
            });
        self.for_worker(placement)
    }
    /// Call from the pinned factory; the driver must preserve this CPU affinity.
    pub fn for_worker(&self, placement: &WorkerContext) -> io::Result<WorkerPool> {
        // SAFETY: sched_getcpu has no pointer arguments.
        let cpu = unsafe { libc::sched_getcpu() };
        if cpu < 0 {
            return Err(io::Error::last_os_error());
        }
        if cpu as usize != placement.cpu_id().0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "worker is not on its assigned CPU",
            ));
        }
        let mut nodes = self.nodes.lock().unwrap();
        let node = placement.numa_node_id();
        if let std::collections::btree_map::Entry::Vacant(entry) = nodes.entry(node) {
            entry.insert(Arc::new(Node::new(self.config, node, Mapping::bind)?));
        }
        let pool = WorkerPool {
            node: Rc::new(nodes[&node].clone()),
        };
        placement.bind_pool(&pool)?;
        Ok(pool)
    }
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/storage/buffers.rs"
));

struct Mapping {
    address: NonNull<u8>,
    len: usize,
    node: NumaNodeId,
    registration: Mutex<()>,
}
// SAFETY: the address is stable until the last owner drops. Only exclusive fills
// and pinned immutable readers provide safe access; raw access has the contract above.
unsafe impl Send for Mapping {}
unsafe impl Sync for Mapping {}
impl Mapping {
    fn new(count: usize, node: NumaNodeId) -> io::Result<Self> {
        let len = count
            .checked_mul(BUFFER_SIZE)
            .filter(|&len| len <= isize::MAX as usize)
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "buffer pool too large"))?;
        // SAFETY: anonymous mapping, no existing address or file involved.
        let address = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if address == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        let Some(address) = NonNull::new(address.cast()) else {
            // SAFETY: mmap succeeded; release the unusable null-address mapping.
            unsafe {
                libc::munmap(address, len);
            }
            return Err(io::Error::other("mmap returned a null address"));
        };
        Ok(Self {
            address,
            len,
            node,
            registration: Mutex::new(()),
        })
    }
    fn bind(&self) -> io::Result<()> {
        // Linux decrements maxnode before copying the mask. Include an extra bit.
        let maxnode =
            self.node.0.checked_add(2).ok_or_else(|| {
                io::Error::new(io::ErrorKind::InvalidInput, "NUMA node too large")
            })?;
        let bits = libc::c_ulong::BITS as usize;
        let mut mask = vec![0 as libc::c_ulong; maxnode.div_ceil(bits)];
        mask[self.node.0 / bits] |= 1 << (self.node.0 % bits);
        // SAFETY: live untouched mapping; mask holds maxnode bits rounded to words.
        let result = unsafe {
            libc::syscall(
                libc::SYS_mbind,
                self.address.as_ptr(),
                self.len,
                libc::MPOL_BIND | libc::MPOL_F_STATIC_NODES,
                mask.as_ptr(),
                maxnode,
                0u32,
            )
        };
        if result != 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }
    fn prefault(&self) -> io::Result<()> {
        // SAFETY: operates on our whole live mapping, reporting population errors.
        if unsafe {
            libc::madvise(
                self.address.as_ptr().cast(),
                self.len,
                libc::MADV_POPULATE_WRITE,
            )
        } != 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }
    fn pointer(&self, index: usize) -> *mut u8 {
        debug_assert!(index < self.len / BUFFER_SIZE);
        // SAFETY: internal indices are bounded by the allocated slot count.
        unsafe { self.address.as_ptr().add(index * BUFFER_SIZE) }
    }
}
impl Drop for Mapping {
    fn drop(&mut self) {
        // SAFETY: the last mapping owner is gone; no safe payload borrows remain.
        unsafe {
            libc::munmap(self.address.as_ptr().cast(), self.len);
        }
    }
}

/// Keeps memory mapped, but does not reserve slots or grant payload access.
#[derive(Clone)]
pub struct MemoryLease {
    mapping: Arc<Mapping>,
}
/// Borrowed registration descriptor. Copying a pointer does not extend its lifetime.
/// Raw I/O requires the ownership and access guarantees in the module documentation.
#[derive(Clone, Copy, Debug)]
pub struct Region<'a> {
    pub address: *mut u8,
    pub len: usize,
    _owner: PhantomData<&'a Mapping>,
}
#[derive(Clone, Copy, Debug)]
pub struct BufferRegion<'a> {
    pub index: usize,
    pub offset: usize,
    pub region: Region<'a>,
}
impl MemoryLease {
    pub(crate) fn registration_lock(&self) -> std::sync::MutexGuard<'_, ()> {
        self.mapping.registration.lock().unwrap()
    }
    pub fn numa_node_id(&self) -> NumaNodeId {
        self.mapping.node
    }
    pub fn region(&self) -> Region<'_> {
        Region {
            address: self.mapping.address.as_ptr(),
            len: self.mapping.len,
            _owner: PhantomData,
        }
    }
    pub fn buffers(&self) -> impl ExactSizeIterator<Item = BufferRegion<'_>> {
        (0..self.mapping.len / BUFFER_SIZE).map(|index| BufferRegion {
            index,
            offset: index * BUFFER_SIZE,
            region: Region {
                address: self.mapping.pointer(index),
                len: BUFFER_SIZE,
                _owner: PhantomData,
            },
        })
    }
    #[cfg(test)]
    pub(crate) fn test_residency(&self, populated: bool) {
        // SAFETY: sysconf has no pointer arguments.
        let page_size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as usize;
        let mut residency = vec![0u8; self.mapping.len / page_size];
        // SAFETY: mapping remains owned; output contains one byte per page.
        let result = unsafe {
            libc::mincore(
                self.mapping.address.as_ptr().cast(),
                self.mapping.len,
                residency.as_mut_ptr(),
            )
        };
        assert_eq!(result, 0, "mincore: {}", io::Error::last_os_error());
        assert!(
            residency
                .into_iter()
                .all(|status| (status & 1 != 0) == populated)
        );
    }
}

// Separate ownership traffic from adjacent slots and their metadata locks.
#[repr(align(128))]
struct Padded<T>(T);
#[derive(Default)]
struct SlotInfo {
    value: Option<Key>,
    checksum: Option<u64>,
    len: Option<usize>,
}
struct Slot {
    refs: Padded<AtomicUsize>,
    info: Padded<Mutex<SlotInfo>>,
}
struct Node {
    mapping: Arc<Mapping>,
    slots: Box<[Slot]>,
    free: Mutex<Vec<usize>>,
    flights: NetworkFlights,
}
impl Node {
    fn new(
        config: Config,
        node: NumaNodeId,
        bind: impl FnOnce(&Mapping) -> io::Result<()>,
    ) -> io::Result<Self> {
        Self::initialized(config, node, |mapping| {
            bind(mapping)?;
            mapping.prefault()
        })
    }
    fn initialized(
        config: Config,
        node: NumaNodeId,
        initialize: impl FnOnce(&Mapping) -> io::Result<()>,
    ) -> io::Result<Self> {
        let count = config.buffers_per_node.get();
        let mapping = Mapping::new(count, node)?;
        initialize(&mapping)?;
        Ok(Self {
            mapping: Arc::new(mapping),
            slots: (0..count)
                .map(|_| Slot {
                    refs: Padded(AtomicUsize::new(0)),
                    info: Padded(Mutex::new(SlotInfo::default())),
                })
                .collect(),
            free: Mutex::new((0..count).rev().collect()),
            flights: NetworkFlights {
                registry: Mutex::new(HashMap::new()),
                count: Arc::new(AtomicUsize::new(0)),
                capacity: config.network_flights.get(),
                consumers_per_flight: config.consumers_per_flight.get(),
                #[cfg(test)]
                all_network: Mutex::new(Vec::new()),
            },
        })
    }
    fn release(&self, index: usize) {
        // Only an existing holder can retain. The final release makes the index
        // available under the free-list lock, which synchronizes the next allocation.
        // Acquire observes all other holders' releases before recycling memory.
        if self.slots[index].refs.0.fetch_sub(1, Ordering::AcqRel) == 1 {
            self.free.lock().unwrap().push(index);
        }
    }
}
fn retain(refs: &AtomicUsize) {
    if refs.fetch_add(1, Ordering::Relaxed) >= isize::MAX as usize {
        std::process::abort();
    }
}

/// Thread-local ownership avoids a node-wide Arc update on every request.
/// ```compile_fail
/// use racer_dataplane::buffers::WorkerPool;
/// fn send<T: Send>() {}
/// send::<WorkerPool>();
/// ```
#[derive(Clone)]
pub struct WorkerPool {
    #[allow(clippy::redundant_allocation)]
    node: Rc<Arc<Node>>,
}
/// Logical shard view; allocation and network flights remain shared by the NUMA node.
pub struct ShardPool {
    pool: WorkerPool,
    shard: crate::sharding::ShardId,
}
impl ShardPool {
    pub fn shard_id(&self) -> crate::sharding::ShardId {
        self.shard
    }
    pub(crate) fn pool(&self) -> &WorkerPool {
        &self.pool
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Exhausted;
impl std::fmt::Display for Exhausted {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("buffer pool exhausted; retry after live holders release buffers")
    }
}
impl std::error::Error for Exhausted {}
impl WorkerPool {
    pub fn numa_node_id(&self) -> NumaNodeId {
        self.node.mapping.node
    }
    pub fn memory_lease(&self) -> MemoryLease {
        MemoryLease {
            mapping: self.node.mapping.clone(),
        }
    }
    /// Allocate exclusive transient storage, or apply backpressure if every slot is held.
    pub fn private_fill(&self) -> Result<Fill, Exhausted> {
        self.allocate(None, 0)
    }
    /// Allocate private staging bound to a complete value identity for transport validation.
    pub fn stage(&self, key: Key) -> Result<Fill, Exhausted> {
        self.allocate(Some(key), 0)
    }
    /// Keep downstream progress capacity free while a receive depends on peers.
    /// Check and allocation share the NUMA free-list lock across all workers.
    pub(crate) fn stage_reserved(&self, key: Key, reserve: usize) -> Result<Fill, Exhausted> {
        self.allocate(Some(key), reserve)
    }
    fn allocate(&self, value: Option<Key>, reserve: usize) -> Result<Fill, Exhausted> {
        let index = {
            let mut free = self.node.free.lock().unwrap();
            if free.len() <= reserve {
                return Err(Exhausted);
            }
            free.pop().unwrap()
        };
        let slot = &self.node.slots[index];
        debug_assert_eq!(slot.refs.0.load(Ordering::Relaxed), 0);
        *slot.info.0.lock().unwrap() = SlotInfo {
            value,
            checksum: None,
            len: None,
        };
        slot.refs.0.store(1, Ordering::Relaxed);
        Ok(Fill {
            handle: Some(Handle {
                node: self.node.clone(),
                index,
            }),
        })
    }
    pub(crate) fn owns_fill(&self, fill: &Fill) -> bool {
        Arc::ptr_eq(&self.node, &fill.handle.as_ref().unwrap().node)
    }
    pub(crate) fn same_pool(&self, other: &Self) -> bool {
        Arc::ptr_eq(&self.node, &other.node)
    }
    pub(crate) fn for_shard(
        &self,
        context: &WorkerContext,
        shard: crate::sharding::ShardId,
    ) -> io::Result<ShardPool> {
        context.bind_pool(self)?;
        if !context.shard_ids().contains(&shard) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "foreign shard buffer view",
            ));
        }
        Ok(ShardPool {
            pool: self.clone(),
            shard,
        })
    }
    pub(crate) fn network_flight(&self, key: NetworkFlightKey) -> Result<NetworkFlight, Exhausted> {
        self.node.flights.join(key)
    }
}

struct Handle {
    #[allow(clippy::redundant_allocation)]
    node: Rc<Arc<Node>>,
    index: usize,
}
impl Handle {
    fn slot(&self) -> &Slot {
        &self.node.slots[self.index]
    }
    fn matches_key(&self, key: Key) -> bool {
        self.slot().info.0.lock().unwrap().value == Some(key)
    }
    fn region(&self) -> BufferRegion<'_> {
        BufferRegion {
            index: self.index,
            offset: self.index * BUFFER_SIZE,
            region: Region {
                address: self.node.mapping.pointer(self.index),
                len: BUFFER_SIZE,
                _owner: PhantomData,
            },
        }
    }
}
impl Clone for Handle {
    fn clone(&self) -> Self {
        retain(&self.slot().refs.0);
        Self {
            node: self.node.clone(),
            index: self.index,
        }
    }
}
impl Drop for Handle {
    fn drop(&mut self) {
        self.node.release(self.index);
    }
}

/// Exclusive initialized storage. Recycled bytes may belong to an earlier value;
/// write the entire published prefix before calling `publish`.
/// ```compile_fail
/// use racer_dataplane::buffers::Fill;
/// fn publish_while_borrowed(mut fill: Fill) {
///     let bytes = fill.as_mut_slice();
///     let buffer = fill.publish(1);
///     bytes[0] = 42;
/// }
/// ```
#[must_use]
pub struct Fill {
    handle: Option<Handle>,
}
impl Fill {
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        let handle = self.handle.as_ref().unwrap();
        // SAFETY: only this Fill grants byte access, and it pins initialized storage.
        unsafe {
            std::slice::from_raw_parts_mut(handle.node.mapping.pointer(handle.index), BUFFER_SIZE)
        }
    }
    pub fn region(&self) -> BufferRegion<'_> {
        self.handle.as_ref().unwrap().region()
    }
    pub(crate) fn matches_key(&self, key: Key) -> bool {
        self.handle.as_ref().unwrap().matches_key(key)
    }
    pub fn publish(mut self, len: usize) -> io::Result<Buffer> {
        if len > BUFFER_SIZE {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "published length exceeds buffer capacity",
            ));
        }
        let handle = self.handle.take().unwrap();
        handle.slot().info.0.lock().unwrap().len = Some(len);
        Ok(Buffer { handle, len })
    }
    /// Freeze bytes with their admission-time CRC64; this does not install a cache entry.
    pub fn publish_checked(self, len: usize, checksum: u64) -> io::Result<Buffer> {
        self.handle
            .as_ref()
            .unwrap()
            .slot()
            .info
            .0
            .lock()
            .unwrap()
            .checksum = Some(checksum);
        self.publish(len)
    }
    pub fn split_destination(self) -> (PublicationAuthority, Destination) {
        let authority = PublicationAuthority {
            handle: Some(self.handle.as_ref().unwrap().clone()),
        };
        (authority, Destination { fill: self })
    }
    pub(crate) fn into_compute(mut self) -> ComputeWrite {
        let handle = self.handle.take().unwrap();
        retain(&handle.slot().refs.0);
        ComputeWrite {
            node: Some(Arc::clone(&handle.node)),
            index: handle.index,
            _exclusive: PhantomData,
        }
    }
}

/// Writable transport storage without publication authority.
/// ```compile_fail
/// use racer_dataplane::buffers::Destination;
/// fn publish(destination: Destination) { destination.publish(1); }
/// ```
/// ```compile_fail
/// use racer_dataplane::buffers::{Destination, Fill};
/// fn escape(destination: Destination) -> Fill { destination.fill }
/// ```
/// ```compile_fail
/// use racer_dataplane::buffers::{Destination, Fill};
/// fn escape(destination: Destination) -> Fill { destination.into() }
/// ```
/// ```compile_fail
/// use racer_dataplane::buffers::Destination;
/// fn escape(destination: Destination) { destination.into_compute(); }
/// ```
#[must_use]
pub struct Destination {
    fill: Fill,
}
/// Affine authority, pinning the slot even after destination cancellation. Only
/// Destination grants byte access. Dropping authority cannot release active I/O.
/// ```compile_fail
/// use racer_dataplane::buffers::{Destination, PublicationAuthority};
/// fn duplicate(authority: PublicationAuthority, destination: Destination) {
///     let first = authority.reunite(destination);
///     let second = authority.reunite(destination);
/// }
/// ```
/// ```compile_fail
/// use racer_dataplane::buffers::{Destination, PublicationAuthority};
/// fn forge(destination: Destination) {
///     let authority = PublicationAuthority { handle: None };
///     authority.reunite(destination);
/// }
/// ```
#[must_use]
pub struct PublicationAuthority {
    handle: Option<Handle>,
}
impl std::fmt::Debug for Destination {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Destination").finish_non_exhaustive()
    }
}
impl std::fmt::Debug for PublicationAuthority {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PublicationAuthority")
            .finish_non_exhaustive()
    }
}
impl PublicationAuthority {
    /// Recover storage only from the matching destination. Mismatch returns both unchanged.
    pub fn reunite(mut self, destination: Destination) -> Result<Fill, (Self, Destination)> {
        let origin = self.handle.as_ref().unwrap();
        let actual = destination.fill.handle.as_ref().unwrap();
        if origin.index != actual.index || !Arc::ptr_eq(&origin.node, &actual.node) {
            return Err((self, destination));
        }
        self.handle.take();
        Ok(destination.fill)
    }
}
impl Destination {
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        self.fill.as_mut_slice()
    }
    pub fn region(&self) -> BufferRegion<'_> {
        self.fill.region()
    }
}
mod writable_sealed {
    pub trait Sealed {}
}
/// Sealed exclusive storage accepted by transports. Type erasure preserves authority.
/// ```compile_fail
/// use racer_dataplane::buffers::{Destination, Fill, Writable};
/// fn escape(d: Destination) -> Fill { d.into_storage().into() }
/// ```
pub trait Writable: writable_sealed::Sealed + Sized {
    fn as_mut_slice(&mut self) -> &mut [u8];
    fn region(&self) -> BufferRegion<'_>;
    #[doc(hidden)]
    fn matches_key(&self, key: Key) -> bool;
    #[doc(hidden)]
    fn into_storage(self) -> WritableStorage;
    #[doc(hidden)]
    fn from_storage(storage: WritableStorage) -> Result<Self, WritableStorage>;
}
#[doc(hidden)]
pub struct WritableStorage(WritableKind);
enum WritableKind {
    Fill(Fill),
    Destination(Destination),
}
impl WritableStorage {
    pub(crate) fn region(&self) -> BufferRegion<'_> {
        match &self.0 {
            WritableKind::Fill(fill) => fill.region(),
            WritableKind::Destination(d) => d.region(),
        }
    }
    #[cfg(test)]
    pub(crate) fn as_mut_slice(&mut self) -> &mut [u8] {
        match &mut self.0 {
            WritableKind::Fill(fill) => fill.as_mut_slice(),
            WritableKind::Destination(d) => d.as_mut_slice(),
        }
    }
}
macro_rules! writable {
    ($ty:ident, $key:expr) => {
        impl writable_sealed::Sealed for $ty {}
        impl Writable for $ty {
            fn as_mut_slice(&mut self) -> &mut [u8] {
                self.as_mut_slice()
            }
            fn region(&self) -> BufferRegion<'_> {
                self.region()
            }
            fn matches_key(&self, key: Key) -> bool {
                ($key)(self, key)
            }
            fn into_storage(self) -> WritableStorage {
                WritableStorage(WritableKind::$ty(self))
            }
            fn from_storage(storage: WritableStorage) -> Result<Self, WritableStorage> {
                match storage.0 {
                    WritableKind::$ty(value) => Ok(value),
                    _ => Err(storage),
                }
            }
        }
    };
}
writable!(Fill, |fill: &Fill, key| fill.matches_key(key));
writable!(Destination, |destination: &Destination, key| destination
    .fill
    .matches_key(key));

/// Immutable transient bytes. Clones pin the slot without taking the free-list lock.
/// ```compile_fail
/// use racer_dataplane::buffers::Buffer;
/// fn modify(buffer: Buffer) { buffer.as_slice()[0] = 42; }
/// ```
#[must_use]
#[derive(Clone)]
pub struct Buffer {
    handle: Handle,
    len: usize,
}
impl Buffer {
    pub(crate) fn matches_key(&self, key: Key) -> bool {
        self.handle.matches_key(key)
    }
    pub fn as_slice(&self) -> &[u8] {
        // SAFETY: this handle pins immutable bytes until all holders release them.
        unsafe {
            std::slice::from_raw_parts(
                self.handle.node.mapping.pointer(self.handle.index),
                self.len,
            )
        }
    }
    /// Registration capacity is always 4 MiB; I/O must use `as_slice().len()`.
    pub fn region(&self) -> BufferRegion<'_> {
        self.handle.region()
    }
    pub fn checksum(&self) -> Option<u64> {
        self.handle.slot().info.0.lock().unwrap().checksum
    }
    pub(crate) fn compute_read(&self) -> ComputeRead {
        retain(&self.handle.slot().refs.0);
        ComputeRead {
            node: Some(Arc::clone(&self.handle.node)),
            index: self.handle.index,
            len: self.len,
        }
    }
}
// Sendable capabilities own one slot reference independently of the I/O worker.
pub(crate) struct ComputeWrite {
    node: Option<Arc<Node>>,
    index: usize,
    _exclusive: PhantomData<std::cell::Cell<()>>,
}
pub(crate) struct ComputeRead {
    node: Option<Arc<Node>>,
    index: usize,
    len: usize,
}
impl ComputeWrite {
    pub(crate) fn bytes(&mut self) -> &mut [u8] {
        // SAFETY: consumed Fill was the only mutable owner, transferred to this capability.
        unsafe {
            std::slice::from_raw_parts_mut(
                self.node.as_ref().unwrap().mapping.pointer(self.index),
                BUFFER_SIZE,
            )
        }
    }
    pub(crate) fn into_fill(mut self) -> Fill {
        Fill {
            handle: Some(Handle {
                node: Rc::new(self.node.take().unwrap()),
                index: self.index,
            }),
        }
    }
}
impl Drop for ComputeWrite {
    fn drop(&mut self) {
        if let Some(node) = self.node.take() {
            node.release(self.index);
        }
    }
}
impl ComputeRead {
    #[cfg(test)]
    pub(crate) fn bytes(&self) -> &[u8] {
        // SAFETY: this reference pins immutable published bytes until dropped.
        unsafe {
            std::slice::from_raw_parts(
                self.node.as_ref().unwrap().mapping.pointer(self.index),
                self.len,
            )
        }
    }
}
impl Drop for ComputeRead {
    fn drop(&mut self) {
        if let Some(node) = self.node.take() {
            node.release(self.index);
        }
    }
}

/// Network dependency identity, deliberately separate from stored ValueId.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct NetworkFlightKey {
    pub value: [u8; 32],
    pub routing: [u8; 32],
    pub version: u8,
    pub destination: u32,
    pub dependency: NetworkDependency,
}
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub enum NetworkDependency {
    Canonical { slot: u32 },
}
/// Bounded coordination storage, with no completed-value directory ownership.
struct NetworkFlights {
    registry: Mutex<HashMap<NetworkFlightKey, std::sync::Weak<NetworkState>>>,
    count: Arc<AtomicUsize>,
    capacity: usize,
    consumers_per_flight: usize,
    #[cfg(test)]
    all_network: Mutex<Vec<std::sync::Weak<NetworkState>>>,
}
struct NetworkState {
    inner: Mutex<NetworkInner>,
    count: Arc<AtomicUsize>,
}
struct NetworkInner {
    metadata: Option<crate::metadata::Metadata>,
    producer: bool,
    outcome: Option<Result<ComputeRead, Arc<crate::cache::Error>>>,
    file: Option<crate::allocator::FileValue>,
    next: usize,
    consumers: usize,
    wakers: HashMap<usize, Waker>,
}
impl Drop for NetworkState {
    fn drop(&mut self) {
        self.count.fetch_sub(1, Ordering::Release);
    }
}
/// Only producer cancellation permits takeover; typed failures reach every joiner.
pub(crate) struct NetworkFlight {
    state: Arc<NetworkState>,
    id: usize,
    producer: bool,
}
pub(crate) enum NetworkProgress {
    Metadata(crate::metadata::Metadata),
    Produce,
    Pending,
    Ready(Result<Buffer, Arc<crate::cache::Error>>),
    File(crate::allocator::FileValue),
}
impl NetworkFlights {
    fn join(&self, key: NetworkFlightKey) -> Result<NetworkFlight, Exhausted> {
        let mut registry = self.registry.lock().unwrap();
        registry.retain(|_, state| state.strong_count() != 0);
        let existing = registry
            .get(&key)
            .and_then(std::sync::Weak::upgrade)
            .filter(|state| {
                let inner = state.inner.lock().unwrap();
                inner.outcome.is_none() && inner.file.is_none() && inner.metadata.is_none()
            });
        let state = if let Some(state) = existing {
            state
        } else {
            if self.count.load(Ordering::Acquire) >= self.capacity {
                return Err(Exhausted);
            }
            self.count.fetch_add(1, Ordering::Relaxed);
            let state = Arc::new(NetworkState {
                inner: Mutex::new(NetworkInner {
                    metadata: None,
                    producer: false,
                    outcome: None,
                    file: None,
                    next: 0,
                    consumers: 0,
                    wakers: HashMap::new(),
                }),
                count: self.count.clone(),
            });
            registry.insert(key, Arc::downgrade(&state));
            #[cfg(test)]
            {
                let mut all = self.all_network.lock().unwrap();
                all.retain(|state| state.strong_count() != 0);
                all.push(Arc::downgrade(&state));
            }
            state
        };
        let mut inner = state.inner.lock().unwrap();
        if inner.consumers >= self.consumers_per_flight {
            return Err(Exhausted);
        }
        let id = inner.next;
        inner.next = id.checked_add(1).ok_or(Exhausted)?;
        inner.consumers += 1;
        drop(inner);
        #[cfg(test)]
        if id > 0 {
            if let Some(world) = crate::simulation::current() {
                world.event("network-join", "", format!("consumer={id}"));
            }
        }
        Ok(NetworkFlight {
            state,
            id,
            producer: false,
        })
    }
}
impl NetworkFlight {
    pub(crate) fn poll(&mut self, waker: &Waker) -> NetworkProgress {
        let cloned = waker.clone();
        let mut inner = self.state.inner.lock().unwrap();
        if let Some(metadata) = inner.metadata {
            drop(inner);
            drop(cloned);
            return NetworkProgress::Metadata(metadata);
        }
        if let Some(file) = &inner.file {
            let result = NetworkProgress::File(file.clone());
            drop(inner);
            drop(cloned);
            return result;
        }
        if let Some(outcome) = &inner.outcome {
            let result = NetworkProgress::Ready(match outcome {
                Ok(lease) => {
                    let node = lease.node.as_ref().unwrap();
                    retain(&node.slots[lease.index].refs.0);
                    Ok(Buffer {
                        handle: Handle {
                            node: Rc::new(node.clone()),
                            index: lease.index,
                        },
                        len: lease.len,
                    })
                }
                Err(error) => Err(error.clone()),
            });
            drop(inner);
            drop(cloned);
            return result;
        }
        if !inner.producer {
            inner.producer = true;
            self.producer = true;
        }
        if self.producer {
            drop(inner);
            drop(cloned);
            NetworkProgress::Produce
        } else {
            let old = inner.wakers.insert(self.id, cloned);
            drop(inner);
            drop(old);
            NetworkProgress::Pending
        }
    }
    pub(crate) fn finish(&mut self, result: Result<&Buffer, Arc<crate::cache::Error>>) {
        assert!(self.producer);
        let wakers = {
            let mut inner = self.state.inner.lock().unwrap();
            inner.outcome = Some(result.map(Buffer::compute_read));
            self.producer = false;
            std::mem::take(&mut inner.wakers)
        };
        wake_network(wakers);
    }
    pub(crate) fn finish_file(&mut self, value: &crate::allocator::FileValue) {
        assert!(self.producer);
        let wakers = {
            let mut inner = self.state.inner.lock().unwrap();
            inner.file = Some(value.clone());
            self.producer = false;
            std::mem::take(&mut inner.wakers)
        };
        wake_network(wakers);
    }
    pub(crate) fn finish_metadata(&mut self, value: crate::metadata::Metadata) {
        assert!(self.producer);
        let wakers = {
            let mut inner = self.state.inner.lock().unwrap();
            inner.metadata = Some(value);
            self.producer = false;
            std::mem::take(&mut inner.wakers)
        };
        wake_network(wakers);
    }
}
fn wake_network(wakers: HashMap<usize, Waker>) {
    for waker in wakers.into_values() {
        // Callbacks run outside shared locks; a panic must not strand survivors.
        if let Err(payload) =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| waker.wake()))
        {
            if let Err(payload) =
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| drop(payload)))
            {
                std::mem::forget(payload);
            }
        }
    }
}
impl Drop for NetworkFlight {
    fn drop(&mut self) {
        let (old, wakers) = {
            let mut inner = self.state.inner.lock().unwrap();
            #[cfg(test)]
            let skip = inner.consumers > 1
                && inner.outcome.is_none()
                && inner.file.is_none()
                && inner.metadata.is_none()
                && crate::simulation::current().is_some_and(|world| {
                    world.activate_mutant(
                        crate::simulation::history::Mutant::SkipCanceledFlightAccounting,
                    )
                });
            #[cfg(not(test))]
            let skip = false;
            if !skip {
                inner.consumers -= 1;
            }
            let old = inner.wakers.remove(&self.id);
            let wakers = if self.producer {
                inner.producer = false;
                std::mem::take(&mut inner.wakers)
            } else {
                HashMap::new()
            };
            (old, wakers)
        };
        drop(old);
        wake_network(wakers);
    }
}
