//! Distinct accounting and lifetimes for plaintext, ciphertext, pipes, and kernel I/O.
//! Cache lookups retain the original encrypted page and immutable version metadata.
//! The memory cache never owns plaintext: local hits decrypt cached ciphertext
//! unless a reader still holds the page's plaintext, which is then shared.
//! Eviction releases idle leases; retirement hides entries without revoking owners.

use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Result;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PAGE_BYTES;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::VersionMetadata;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use racer_wire::control::CacheId;
use std::cell::RefCell;
use std::rc::Rc;
use std::sync::Arc;

/// Admission charge carried by every ciphertext allocation.
pub(crate) type CiphertextCharge = flow_control::Charge<AdmissionPolicy>;

/// Default direct-I/O alignment for ciphertext buffers when no store device
/// reports its own: 4 KiB memory, offset, and length units.
pub(crate) fn default_disk_alignment() -> page_alloc::Alignment {
    page_alloc::Alignment::new(4096, 4096, 4096).expect("valid default alignment")
}

/// Idle buffers each worker's pool keeps per capacity class.
///
/// Idle storage stays charged to [`ResourceClass::Pool`], so the bound is kept
/// to a few pages per class. This is a ceiling, not a steady state:
/// [`BufferPool::maintain_idle_buffers`] sheds half of the idle storage after
/// each window without a checkout, so an idle worker's pool empties within a
/// few windows while continuous reuse keeps it warm. Memory pressure sheds it
/// at once.
pub(crate) const POOL_IDLE_PER_CLASS: usize = 4;

/// Writable aligned ciphertext storage from one worker's pool. Dropping it
/// unfrozen returns the storage to that pool and releases its reservation.
pub struct CiphertextBuffer {
    buffer: DiskBuffer,
    /// Length through the next device length-unit boundary.
    padded: usize,
}
impl CiphertextBuffer {
    /// Requested length in bytes.
    pub fn len(&self) -> usize {
        self.buffer.len()
    }
    /// Always false: pools reject zero-length buffers.
    pub fn is_empty(&self) -> bool {
        false
    }
    /// Allocation capacity the reservation covers.
    pub fn capacity(&self) -> usize {
        self.buffer.capacity()
    }
    /// The Ciphertext reservation accounting for this storage.
    pub(crate) fn charge(&self) -> &CiphertextCharge {
        self.buffer.charge()
    }
    /// Bytes, which may hold stale ciphertext until fully written.
    pub fn as_slice(&self) -> &[u8] {
        &self.buffer
    }
    /// Exclusively borrow exactly the requested bytes.
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        &mut self.buffer
    }
    /// Swap the reservation for `charge`, which must cover the capacity.
    /// Returns the replaced charge, or `charge` back when it does not cover.
    pub(crate) fn replace_charge(
        &mut self,
        charge: CiphertextCharge,
    ) -> std::result::Result<CiphertextCharge, CiphertextCharge> {
        self.buffer.replace_charge(charge)
    }
    /// Zero the pad and freeze without copying. The returned buffer's length
    /// is the padded length; the second value is the ciphertext length.
    fn freeze(self) -> (DiskBuffer, usize) {
        let Self { mut buffer, padded } = self;
        let length = buffer.len();
        buffer.spare_mut()[length..padded].fill(0);
        buffer
            .set_len(padded)
            .expect("padded length is within capacity");
        (buffer, length)
    }
}
impl std::fmt::Debug for CiphertextBuffer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CiphertextBuffer")
            .field("length", &self.buffer.len())
            .finish_non_exhaustive()
    }
}
// SAFETY: the pooled allocation is exclusively owned, stable, and live until
// Drop, and is only exposed through these borrows.
unsafe impl racer_io::ring::IoBuffer for CiphertextBuffer {
    fn bytes(&self) -> racer_io::Result<&[u8]> {
        Ok(self.as_slice())
    }
    fn bytes_mut(&mut self) -> racer_io::Result<&mut [u8]> {
        Ok(self.as_mut_slice())
    }
}

/// Demand state for trimming one worker's idle pooled storage.
#[derive(Clone, Copy, Debug, Default)]
struct IdleDemand {
    window_start: Option<std::time::Instant>,
    takes_at_start: u64,
    idle_at_start: usize,
    constrained: bool,
    trimmed: u64,
}

/// Per-worker ciphertext and plaintext allocation front end.
///
/// Cloned handles share one [`page_alloc::buf::Pool`], so buffers return to
/// the worker that created them even when they are filled or dropped on a
/// shared crypto thread.
#[derive(Clone)]
pub struct BufferPool {
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    /// Direct-I/O alignment of this worker's store devices.
    alignment: page_alloc::Alignment,
    /// Every pooled buffer: aligned ciphertext, disk reads, and wipe-on-return
    /// plaintext. Idle storage is charged to [`ResourceClass::Pool`] and
    /// returns here from any thread, so a warm path reuses a mapping instead
    /// of mapping and unmapping a fresh one.
    disk: Rc<std::cell::RefCell<page_alloc::buf::Pool>>,
    demand: Rc<std::cell::Cell<IdleDemand>>,
}
/// Mutable staging buffer, not proof of authentication and not client-deliverable.
/// Fixed-size owned backing stays at the same address when this owner moves.
pub struct PlaintextBuffer(PlaintextBuf);
impl PlaintextBuffer {
    pub(crate) fn into_buf(self) -> PlaintextBuf {
        self.0
    }
    pub(crate) fn reservation(&self) -> &flow_control::Charge<AdmissionPolicy> {
        self.0.charge()
    }
}
// SAFETY: private fixed backing and reservation remain exclusively owned.
unsafe impl racer_io::ring::IoBuffer for PlaintextBuffer {
    fn bytes(&self) -> racer_io::Result<&[u8]> {
        Ok(&self.0)
    }
    fn bytes_mut(&mut self) -> racer_io::Result<&mut [u8]> {
        Ok(&mut self.0)
    }
}
/// Only page authentication/origin validation may create this publishable type.
#[derive(Clone)]
pub struct VerifiedPage {
    pub(crate) inner: Arc<VerifiedBytes>,
}
/// Authenticated page identity paired with one pooled, wipe-on-return payload.
pub(crate) struct VerifiedBytes {
    pub page: PageId,

    pub storage: PlaintextBuf,
}

/// Plaintext page storage checked out of a worker's buffer pool. The charge
/// covers the pooled capacity; the buffer is wiped before it is reused.
pub type PlaintextBuf = page_alloc::buf::Buf<PlaintextCharge>;

/// Admission charge backing pooled plaintext storage.
///
/// [`AdmissionPolicy`] lets only ciphertext charges back page allocator
/// storage, so slabs and aligned ciphertext pools can never hold plaintext.
/// This wrapper is the one sanctioned exception: it covers a pooled
/// plaintext buffer, which is always wiped before reuse or release.
pub struct PlaintextCharge(pub(crate) flow_control::Charge<AdmissionPolicy>);

impl page_alloc::Charge for PlaintextCharge {
    fn covers(&self, bytes: usize) -> bool {
        self.0.class() == ResourceClass::Plaintext && self.0.amount() >= bytes
    }

    fn requires_wipe(&self) -> bool {
        true
    }
}

impl std::ops::Deref for PlaintextCharge {
    type Target = flow_control::Charge<AdmissionPolicy>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl std::ops::Deref for VerifiedBytes {
    type Target = PlaintextBuf;

    /// Borrow allocation and accounting without exposing mutable verified contents.
    fn deref(&self) -> &Self::Target {
        &self.storage
    }
}

#[cfg(test)]
impl std::ops::DerefMut for VerifiedBytes {
    /// Permit malformed fixture construction without exposing mutable verified bytes.
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.storage
    }
}

/// Fixture plaintext: copies `bytes` into unpooled storage charged to
/// `admission` under `key` for the full buffer capacity.
#[cfg(test)]
pub(crate) fn fixture_plaintext(
    admission: &flow_control::Quotas<AdmissionPolicy>,
    key: Option<&racer_wire::control::CacheId>,
    bytes: &[u8],
) -> PlaintextBuf {
    let capacity = bytes
        .len()
        .max(1)
        .next_multiple_of(page_alloc::buf::GRANULE);
    let charge = admission
        .reserve(key, ResourceClass::Plaintext, capacity)
        .expect("fixture plaintext admission");
    fixture_storage(charge, bytes)
}

/// Fixture plaintext over a caller-made charge covering one granule-rounded
/// buffer of `bytes.len()`.
#[cfg(test)]
pub(crate) fn fixture_storage(
    charge: flow_control::Charge<AdmissionPolicy>,
    bytes: &[u8],
) -> PlaintextBuf {
    let mut storage = page_alloc::buf::Buf::unpooled(bytes.len(), 1, PlaintextCharge(charge), true)
        .expect("fixture buffer");
    storage.copy_from_slice(bytes);
    storage
}
/// A handle on immutable ciphertext.
///
/// Handles are pinning by default: each pinning handle, including every clone
/// of one, counts in [`CiphertextBytes::pins`] until dropped. Caches and write
/// queues ask [`Self::pins`] whether evicting their entry would free the bytes,
/// instead of reading the `Arc` strong count. Telemetry, sampling, and other
/// observers take [`Self::unpinned`] handles, which keep the bytes alive but
/// never make an entry look busy.
pub struct CiphertextPage {
    pub(crate) inner: Arc<CiphertextBytes>,
    pub(crate) provenance: Option<crate::telemetry::PeerProvenance>,
    /// Whether this handle counts in `inner.pins`.
    pub(crate) pinned: bool,
}
impl Clone for CiphertextPage {
    fn clone(&self) -> Self {
        if self.pinned {
            self.inner.pins.pin();
        }
        Self {
            inner: self.inner.clone(),
            provenance: self.provenance,
            pinned: self.pinned,
        }
    }
}
impl Drop for CiphertextPage {
    fn drop(&mut self) {
        if self.pinned {
            self.inner.pins.unpin();
        }
    }
}
/// Count of pinning [`CiphertextPage`] handles on one allocation. It starts at
/// one for the handle that constructs the bytes.
pub(crate) struct PinCount(std::sync::atomic::AtomicU32);
impl Default for PinCount {
    fn default() -> Self {
        Self(std::sync::atomic::AtomicU32::new(1))
    }
}
impl PinCount {
    fn pin(&self) {
        let previous = self.0.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        assert!(
            previous != 0 && previous != u32::MAX,
            "ciphertext pin count"
        );
    }
    fn unpin(&self) {
        let previous = self.0.fetch_sub(1, std::sync::atomic::Ordering::Release);
        debug_assert!(previous != 0, "ciphertext pin count underflow");
    }
    fn get(&self) -> u32 {
        self.0.load(std::sync::atomic::Ordering::Acquire)
    }
}
/// Ciphertext metadata remains separate from allocation ownership.
pub(crate) struct CiphertextBytes {
    pub envelope: PageEnvelope,

    pub storage: CiphertextStorage,

    /// Pinning handles; see [`CiphertextPage`].
    pub(crate) pins: PinCount,
}

/// Pooled disk read storage charged to the reading cache's Ciphertext class.
pub(crate) type DiskBuffer = page_alloc::buf::Buf<flow_control::Charge<AdmissionPolicy>>;

pub(crate) enum CiphertextStorage {
    /// Ciphertext copied or read into page allocator storage (a disk record,
    /// a TCP receive, or a fixture); `range` is its ciphertext.
    Slab {
        buffer: DiskBuffer,
        range: std::ops::Range<usize>,
    },
    /// Ciphertext produced directly into direct-I/O aligned storage.
    ///
    /// Invariant, established by construction: the bytes start at offset 0 of
    /// an allocation aligned to its pool's memory unit, and `buffer` (its
    /// whole logical length) holds the ciphertext followed only by zeros up
    /// to a length-unit boundary. The store may write it directly; see
    /// [`CiphertextPage::disk_body`].
    Aligned { buffer: DiskBuffer, length: usize },
    /// A received RDMA page still in registered pool memory. Request-scoped
    /// only: retaining paths copy it out (see `retainable`) so pool buffers
    /// are never pinned by caches or write queues. Dropping it on any thread
    /// returns the buffer to the pool.
    Registered {
        page: rdma_verbs::RegisteredPage,
        reservation: flow_control::Charge<AdmissionPolicy>,
        /// The first copy-out, shared by every later retaining path (memory
        /// cache, disk write queue) so a page is copied out at most once.
        retained: std::sync::OnceLock<CiphertextPage>,
    },
}

impl CiphertextBytes {
    pub(crate) fn reservation(&self) -> &flow_control::Charge<AdmissionPolicy> {
        match &self.storage {
            CiphertextStorage::Slab { buffer, .. } => buffer.charge(),
            CiphertextStorage::Aligned { buffer, .. } => buffer.charge(),
            CiphertextStorage::Registered { reservation, .. } => reservation,
        }
    }
    pub(crate) fn capacity(&self) -> usize {
        match &self.storage {
            CiphertextStorage::Slab { buffer, .. } => buffer.capacity(),
            CiphertextStorage::Aligned { buffer, .. } => buffer.capacity(),
            CiphertextStorage::Registered { page, .. } => page.len(),
        }
    }
}

#[cfg(test)]
impl CiphertextBytes {
    /// Mutable fixture bytes, for building malformed ciphertext.
    pub(crate) fn fixture_bytes_mut(&mut self) -> &mut [u8] {
        match &mut self.storage {
            CiphertextStorage::Slab { buffer, range } => &mut buffer[range.clone()],
            CiphertextStorage::Aligned { buffer, length } => &mut buffer[..*length],
            CiphertextStorage::Registered { .. } => panic!("registered ciphertext is immutable"),
        }
    }
    /// Resize fixture ciphertext within its buffer capacity, filling new
    /// bytes with `value`.
    pub(crate) fn fixture_resize(&mut self, length: usize, value: u8) {
        let (buffer, start, end) = match &mut self.storage {
            CiphertextStorage::Slab { buffer, range } => {
                let start = range.start;
                (buffer, start, &mut range.end)
            }
            CiphertextStorage::Aligned { buffer, length } => (buffer, 0, length),
            CiphertextStorage::Registered { .. } => panic!("registered ciphertext is immutable"),
        };
        let old = *end;
        let new = start + length;
        if new > buffer.len() {
            buffer.set_len(new).expect("fixture ciphertext capacity");
        }
        if new > old {
            buffer[old..new].fill(value);
        }
        *end = new;
    }
    /// Drop the last fixture byte.
    pub(crate) fn fixture_pop(&mut self) {
        let length = self.fixture_bytes_mut().len();
        self.fixture_resize(length - 1, 0);
    }
    /// Append one fixture byte.
    pub(crate) fn fixture_push(&mut self, value: u8) {
        let length = self.fixture_bytes_mut().len();
        self.fixture_resize(length + 1, value);
    }
    /// Replace the fixture's admission charge.
    pub(crate) fn fixture_replace_charge(&mut self, charge: flow_control::Charge<AdmissionPolicy>) {
        let replaced = match &mut self.storage {
            CiphertextStorage::Slab { buffer, .. } | CiphertextStorage::Aligned { buffer, .. } => {
                buffer.replace_charge(charge).is_ok()
            }
            CiphertextStorage::Registered { reservation, .. } => {
                *reservation = charge;
                true
            }
        };
        assert!(replaced, "fixture charge must cover the buffer");
    }
}

/// A buffer pool over fresh default-limit admission, for fixtures whose
/// crypto never encrypts against another admission.
#[cfg(test)]
pub(crate) fn fixture_buffer_pool() -> BufferPool {
    BufferPool::new(Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ))))
}

/// Fixture AEAD output of `charge.amount()` bytes in unpooled aligned
/// storage. The charge is topped up from `admission` to the buffer capacity.
#[cfg(test)]
pub(crate) fn fixture_encrypt_output(
    admission: &flow_control::Quotas<AdmissionPolicy>,
    mut charge: flow_control::Charge<AdmissionPolicy>,
) -> Box<CiphertextBuffer> {
    let alignment = default_disk_alignment();
    let length = charge.amount();
    let padded = padded_length(alignment, length).expect("fixture ciphertext length");
    let capacity = unpooled_capacity(alignment, padded);
    let extra = admission
        .reserve(charge.key(), ResourceClass::Ciphertext, capacity - length)
        .expect("fixture ciphertext admission");
    assert!(charge.merge(extra).is_ok(), "fixture charge merge");
    let mut buffer = page_alloc::buf::Buf::unpooled(padded, alignment.memory(), charge, false)
        .expect("fixture buffer");
    buffer.set_len(length).expect("fixture length");
    Box::new(CiphertextBuffer { buffer, padded })
}

/// Fixture ciphertext storage: copies `bytes` into unpooled storage charged
/// to `admission` under `key` for the full buffer capacity.
#[cfg(test)]
pub(crate) fn fixture_ciphertext_storage(
    admission: &flow_control::Quotas<AdmissionPolicy>,
    key: Option<&CacheId>,
    bytes: &[u8],
) -> CiphertextStorage {
    let capacity = bytes
        .len()
        .max(1)
        .next_multiple_of(page_alloc::buf::GRANULE);
    let charge = admission
        .reserve(key, ResourceClass::Ciphertext, capacity)
        .expect("fixture ciphertext admission");
    let mut buffer =
        page_alloc::buf::Buf::unpooled(bytes.len(), 1, charge, false).expect("fixture buffer");
    buffer.copy_from_slice(bytes);
    CiphertextStorage::Slab {
        buffer,
        range: 0..bytes.len(),
    }
}

impl BufferPool {
    /// Use the default 4 KiB disk alignment for aligned ciphertext.
    pub fn new(admission: Rc<flow_control::Quotas<AdmissionPolicy>>) -> Self {
        Self::with_alignment(admission, default_disk_alignment())
    }
    /// Create one worker's pools; `alignment` should match its store devices.
    pub fn with_alignment(
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        alignment: page_alloc::Alignment,
    ) -> Self {
        let disk = page_alloc::buf::Pool::new(
            page_alloc::buf::PoolConfig {
                alignment: alignment.memory(),
                max_idle_per_class: POOL_IDLE_PER_CLASS,
            },
            flow_control::QuotaPoolAccount::new(admission.shared(), ResourceClass::Pool),
        )
        .expect("disk alignment is a valid pool alignment");
        Self {
            admission,
            alignment,
            disk: Rc::new(std::cell::RefCell::new(disk)),
            demand: Default::default(),
        }
    }
    /// Direct-I/O alignment of aligned ciphertext buffers from this worker.
    #[cfg(test)]
    pub(crate) fn alignment(&self) -> page_alloc::Alignment {
        self.alignment
    }
    /// Idle and trimming counters of this worker's pool. `bytes` is the
    /// charged idle storage; `entries` is not tracked.
    #[cfg(test)]
    pub(crate) fn pool_stats(&self) -> page_alloc::PoolStats {
        let demand = self.demand.get();
        page_alloc::PoolStats {
            bytes: self.disk.borrow().idle_bytes(),
            max_depth: POOL_IDLE_PER_CLASS,
            trimmed: demand.trimmed,
            constrained: demand.constrained,
            ..Default::default()
        }
    }
    /// Release idle storage that went unused for a whole maintenance window:
    /// half of what was idle when the window opened, provided nothing was
    /// checked out since, or all of it while constrained. O(1) until
    /// the window elapses; returns bytes released.
    pub(crate) fn maintain_idle_buffers(&self, now: std::time::Instant) -> usize {
        let mut demand = self.demand.get();
        let Ok(mut pool) = self.disk.try_borrow_mut() else {
            return 0;
        };
        let takes = pool.takes();
        if demand.constrained {
            let released = pool.shed(usize::MAX);
            if released > 0 {
                demand.trimmed = demand.trimmed.saturating_add(1);
                self.demand.set(demand);
            }
            return released;
        }
        let Some(start) = demand.window_start else {
            demand.window_start = Some(now);
            demand.takes_at_start = takes;
            demand.idle_at_start = pool.idle_bytes();
            self.demand.set(demand);
            return 0;
        };
        let mut released = 0;
        if now.saturating_duration_since(start) >= page_alloc::DEFAULT_WINDOW {
            // Only storage idle since the window started counts as unused.
            let stale = pool.idle_bytes().min(demand.idle_at_start);
            if takes == demand.takes_at_start && stale > 0 {
                released = pool.shed(stale.div_ceil(2));
            }
            demand.window_start = Some(now);
            demand.takes_at_start = takes;
            demand.idle_at_start = pool.idle_bytes();
        }
        if released > 0 {
            demand.trimmed = demand.trimmed.saturating_add(1);
        }
        self.demand.set(demand);
        released
    }
    /// Release idle storage now, and at every maintenance while memory is
    /// constrained.
    pub(crate) fn constrain_idle_buffers(&self, constrained: bool) {
        let mut demand = self.demand.get();
        demand.constrained = constrained;
        self.demand.set(demand);
        if constrained {
            self.shed_disk_buffers();
        }
    }
    /// Discard idle disk read storage (`MADV_DONTNEED`) and release its
    /// [`ResourceClass::Pool`] charge. Returns the bytes released.
    pub(crate) fn shed_disk_buffers(&self) -> usize {
        self.disk
            .try_borrow_mut()
            .map_or(0, |mut pool| pool.shed(usize::MAX))
    }
    /// Idle bytes held by the disk read pool.
    #[cfg(test)]
    pub(crate) fn disk_idle_bytes(&self) -> usize {
        self.disk.borrow().idle_bytes()
    }
    /// Check out a pooled disk read buffer of exactly `length` bytes. The
    /// reservation must be a Ciphertext charge from this worker's admission
    /// covering the pooled capacity; contents are unspecified until the read.
    pub(crate) fn disk_buffer(
        &self,
        length: usize,
        reservation: flow_control::Charge<AdmissionPolicy>,
    ) -> Result<DiskBuffer> {
        if !self.admission.owns(&reservation) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(self.disk.borrow_mut().take(length, reservation, false)?)
    }
    /// Capacity a pooled disk read of `length` bytes is charged for.
    pub(crate) fn disk_capacity(&self, length: usize) -> Result<usize> {
        Ok(self.disk.borrow().capacity_for(length)?)
    }
    /// Release this worker's idle pooled storage and the admission it retains.
    pub(crate) fn reclaim_idle_buffers(&self) {
        self.shed_disk_buffers();
    }
    /// Admission a pooled aligned buffer of `length` bytes must be charged for.
    pub(crate) fn aligned_capacity(&self, length: usize) -> Result<usize> {
        self.disk_capacity(padded_length(self.alignment, length)?)
    }
    /// Check out aligned ciphertext storage for exactly `length` bytes.
    ///
    /// The reservation must be a Ciphertext charge from this worker's admission
    /// covering [`Self::aligned_capacity`]. Recycled storage is not re-zeroed;
    /// the producer must overwrite all `length` bytes before freezing.
    pub(crate) fn aligned_ciphertext(
        &self,
        reservation: CiphertextCharge,
        length: usize,
    ) -> Result<CiphertextBuffer> {
        if length == 0 || length > PAGE_BYTES as usize + 16 || !self.admission.owns(&reservation) {
            return Err(Error::InvalidConfiguration);
        }
        reservation.validate(ResourceClass::Ciphertext, self.aligned_capacity(length)?)?;
        let padded = padded_length(self.alignment, length)?;
        let mut buffer = self.disk.borrow_mut().take(padded, reservation, false)?;
        buffer.set_len(length)?;
        Ok(CiphertextBuffer { buffer, padded })
    }
    /// Check out aligned storage for AEAD output, replacing an exact-length
    /// reservation with one covering the pooled capacity when needed. Fails
    /// with the admission error when the larger charge is refused.
    pub fn encryption_output(
        &self,
        mut reservation: CiphertextCharge,
        length: usize,
    ) -> Result<CiphertextBuffer> {
        let capacity = self.aligned_capacity(length)?;
        if !self.admission.owns(&reservation) {
            return Err(Error::InvalidConfiguration);
        }
        reservation.validate(ResourceClass::Ciphertext, length)?;
        self.top_up(&mut reservation, capacity)?;
        self.aligned_ciphertext(reservation, length)
    }
    /// Grow `reservation` to at least `capacity` under its key, shedding idle
    /// pooled buffers once if admission refuses.
    fn top_up(&self, reservation: &mut CiphertextCharge, capacity: usize) -> Result<()> {
        let Some(needed) = capacity
            .checked_sub(reservation.amount())
            .filter(|n| *n > 0)
        else {
            return Ok(());
        };
        let key = reservation.key().cloned();
        let mut extra = self
            .admission
            .reserve(key.as_ref(), ResourceClass::Ciphertext, needed);
        if extra.is_err() {
            // Idle pooled buffers may be what pins this worker's admission.
            self.shed_disk_buffers();
            extra = self
                .admission
                .reserve(key.as_ref(), ResourceClass::Ciphertext, needed);
        }
        reservation
            .merge(extra?)
            .map_err(|_| Error::InvalidConfiguration)
    }
    pub fn plaintext(
        &self,
        reservation: flow_control::Charge<AdmissionPolicy>,
        length: usize,
    ) -> Result<PlaintextBuffer> {
        if length == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.plaintext_storage(reservation, length)
            .map(PlaintextBuffer)
    }
    /// Bytes charged for pooled plaintext of `length` bytes: the 4 KiB
    /// granule-rounded capacity the pool hands out.
    pub(crate) fn plaintext_capacity(&self, length: usize) -> Result<usize> {
        Ok(self.disk.borrow().capacity_for(length)?)
    }
    /// Check out pooled, wipe-on-return plaintext storage of exactly `length`
    /// bytes. `reservation` must be an owned Plaintext charge for at least
    /// `length`; it is topped up to the pooled capacity under the same key.
    /// Contents are unspecified until the producer overwrites them.
    pub fn plaintext_storage(
        &self,
        mut reservation: flow_control::Charge<AdmissionPolicy>,
        length: usize,
    ) -> Result<PlaintextBuf> {
        if length > PAGE_BYTES as usize {
            return Err(Error::InvalidConfiguration);
        }
        self.validate_reservation(
            &reservation,
            ResourceClass::Plaintext,
            length,
            reservation.key(),
        )?;
        let capacity = self.disk.borrow().capacity_for(length)?;
        if reservation.amount() < capacity {
            let extra = self.admission.reserve(
                reservation.key(),
                ResourceClass::Plaintext,
                capacity - reservation.amount(),
            )?;
            reservation
                .merge(extra)
                .map_err(|_| Error::InvalidConfiguration)?;
        } else {
            reservation.shrink(capacity)?;
        }
        Ok(self
            .disk
            .borrow_mut()
            .take(length, PlaintextCharge(reservation), true)?)
    }
    /// Copy received ciphertext into pooled storage. `reservation` must be an owned
    /// Ciphertext charge keyed to the page's cache covering at least
    /// `bytes.len()`; it is topped up (or shrunk) to the pooled capacity.
    pub fn ciphertext(
        &self,
        mut reservation: flow_control::Charge<AdmissionPolicy>,
        envelope: PageEnvelope,
        bytes: impl AsRef<[u8]>,
    ) -> Result<CiphertextPage> {
        let bytes = bytes.as_ref();
        envelope.validate()?;
        if bytes.len() != envelope.ciphertext_length as usize {
            return Err(Error::CorruptRecord);
        }
        self.validate_reservation(
            &reservation,
            ResourceClass::Ciphertext,
            bytes.len(),
            Some(&envelope.page.version.object.cache),
        )?;
        let capacity = self.disk_capacity(bytes.len())?;
        if reservation.amount() < capacity {
            self.top_up(&mut reservation, capacity)?;
        } else {
            reservation.shrink(capacity)?;
        }
        let mut buffer = self.disk_buffer(bytes.len(), reservation)?;
        buffer.copy_from_slice(bytes);
        Ok(CiphertextPage {
            pinned: true,
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                pins: Default::default(),
                envelope,
                storage: CiphertextStorage::Slab {
                    buffer,
                    range: 0..bytes.len(),
                },
            }),
        })
    }
    /// Wrap a received registered page without copying. The reservation
    /// covers exactly the received length, the page's visible capacity.
    pub(crate) fn registered_ciphertext(
        &self,
        reservation: flow_control::Charge<AdmissionPolicy>,
        envelope: PageEnvelope,
        page: rdma_verbs::RegisteredPage,
    ) -> Result<CiphertextPage> {
        envelope.validate()?;
        if page.len() != envelope.ciphertext_length as usize {
            return Err(Error::CorruptRecord);
        }
        self.validate_reservation(
            &reservation,
            ResourceClass::Ciphertext,
            page.len(),
            Some(&envelope.page.version.object.cache),
        )?;
        Ok(CiphertextPage {
            pinned: true,
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                pins: Default::default(),
                envelope,
                storage: CiphertextStorage::Registered {
                    page,
                    reservation,
                    retained: std::sync::OnceLock::new(),
                },
            }),
        })
    }
    /// Copy registered ciphertext into owned bytes under a fresh charge before
    /// retaining it; other storage is returned unchanged. Reserve before any
    /// cache borrow because admission may reclaim.
    ///
    /// Safety net: fills detach registered pages once with
    /// [`Self::detach_registered`] before publishing, so this normally sees
    /// already-detached pages.
    pub(crate) fn retainable(&self, ciphertext: CiphertextPage) -> Result<CiphertextPage> {
        if !ciphertext.is_registered() {
            return Ok(ciphertext);
        }
        if let Some(copy) = ciphertext.retained_copy()
            && self.admission.owns(copy.inner.reservation())
        {
            return Ok(copy);
        }
        // Cover the pooled size class so the copy lands in aligned storage.
        let length = ciphertext.bytes().len();
        let reservation = self.admission.reserve(
            Some(&ciphertext.envelope().page.version.object.cache),
            ResourceClass::Ciphertext,
            self.aligned_capacity(length).unwrap_or(length),
        )?;
        self.rehome_aligned(ciphertext, reservation)
    }
    /// Copy a registered RDMA page once into this worker's pooled aligned
    /// storage and drop the registered owner, so the memory cache and the
    /// store can share one disk-aligned page.
    ///
    /// Callers should check [`CiphertextPage::is_registered`] before reserving.
    /// `reservation` must be a Ciphertext charge for the page's cache covering
    /// [`Self::aligned_capacity`] of the ciphertext length; it is consumed in
    /// every outcome. Other pages are returned unchanged and the reservation is
    /// released. The registered buffer returns to its pool as soon as the last
    /// clone of `page` is dropped, which is here when the caller passes the only
    /// handle.
    pub(crate) fn detach_registered(
        &self,
        page: CiphertextPage,
        reservation: CiphertextCharge,
    ) -> Result<CiphertextPage> {
        if !page.is_registered() {
            return Ok(page);
        }
        // An earlier retaining path already copied it out under this
        // admission: share that copy and release `reservation`.
        if let Some(copy) = page.retained_copy()
            && self.admission.owns(copy.inner.reservation())
            && copy.inner.reservation().key() == Some(&page.envelope().page.version.object.cache)
        {
            return Ok(copy);
        }
        self.copy_aligned(&page, reservation)
    }
    /// Rehome `page` under `reservation`, copying into this worker's pooled
    /// aligned storage when the bytes cannot move in place and the
    /// reservation covers the pooled capacity. Otherwise behaves like
    /// [`CiphertextPage::rehome`].
    pub(crate) fn rehome_aligned(
        &self,
        mut page: CiphertextPage,
        reservation: CiphertextCharge,
    ) -> Result<CiphertextPage> {
        let length = page.bytes().len();
        let exclusive = !page.is_registered() && Arc::get_mut(&mut page.inner).is_some();
        if !exclusive
            && reservation.key() == Some(&page.envelope().page.version.object.cache)
            && self
                .aligned_capacity(length)
                .is_ok_and(|capacity| reservation.amount() >= capacity)
        {
            return self.copy_aligned(&page, reservation);
        }
        page.rehome(reservation)
    }
    /// Copy `page`'s bytes into a pooled aligned page, preserving its envelope,
    /// and provenance.
    fn copy_aligned(
        &self,
        page: &CiphertextPage,
        reservation: CiphertextCharge,
    ) -> Result<CiphertextPage> {
        let length = page.bytes().len();
        let mut buffer = self.aligned_ciphertext(reservation, length)?;
        buffer.as_mut_slice().copy_from_slice(page.bytes());
        let mut copy = self.aligned_ciphertext_page(buffer, page.envelope().clone())?;
        copy.provenance = page.provenance;
        page.record_copy_out(&copy);
        Ok(copy)
    }
    pub(crate) fn slab_ciphertext(
        &self,
        buffer: DiskBuffer,
        envelope: PageEnvelope,
        range: std::ops::Range<usize>,
    ) -> Result<CiphertextPage> {
        envelope.validate()?;
        if buffer.get(range.clone()).is_none() || range.len() != envelope.ciphertext_length as usize
        {
            return Err(Error::CorruptRecord);
        }
        self.validate_reservation(
            buffer.charge(),
            ResourceClass::Ciphertext,
            buffer.capacity(),
            Some(&envelope.page.version.object.cache),
        )?;
        // The read CQE has returned ownership. Freeze exposes no mutable bytes,
        // and the full padded allocation and charge outlive every ciphertext view.
        Ok(CiphertextPage {
            pinned: true,
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                pins: Default::default(),
                envelope,
                storage: CiphertextStorage::Slab { buffer, range },
            }),
        })
    }
    /// Freeze AEAD or receive output produced in this worker's aligned pool
    /// into a disk-aligned page without copying.
    ///
    /// Mirrors [`Self::ciphertext`]: the length must equal the envelope's
    /// ciphertext length, and the buffer's charge must be a Ciphertext
    /// reservation from this admission for the page's cache covering the full
    /// capacity. Freezing zero-fills the pad.
    pub(crate) fn aligned_ciphertext_page(
        &self,
        buffer: CiphertextBuffer,
        envelope: PageEnvelope,
    ) -> Result<CiphertextPage> {
        envelope.validate()?;
        let length = buffer.len();
        if length != envelope.ciphertext_length as usize {
            return Err(Error::CorruptRecord);
        }
        self.validate_reservation(
            buffer.charge(),
            ResourceClass::Ciphertext,
            buffer.capacity(),
            Some(&envelope.page.version.object.cache),
        )?;
        Ok(CiphertextPage::from_aligned(buffer, envelope))
    }
    fn entry_limit(&self) -> usize {
        self.admission.policy().limits().metadata_entries.get()
    }
    fn reclaim_buffers(&self) {
        self.reclaim_idle_buffers();
    }
    fn validate_reservation(
        &self,
        reservation: &flow_control::Charge<AdmissionPolicy>,
        class: ResourceClass,
        capacity: usize,
        cache: Option<&CacheId>,
    ) -> Result<()> {
        if !self.admission.owns(reservation) || cache.is_none() || reservation.key() != cache {
            return Err(Error::InvalidConfiguration);
        }
        reservation.validate(class, capacity).map_err(Into::into)
    }
    fn validate_page(&self, page: &PageResult) -> Result<()> {
        page.validate_metadata()?;
        let cache = Some(&page.plaintext.page().version.object.cache);
        self.validate_reservation(
            page.plaintext.inner.charge(),
            ResourceClass::Plaintext,
            page.plaintext.inner.capacity(),
            cache,
        )?;
        self.validate_reservation(
            page.ciphertext.inner.reservation(),
            ResourceClass::Ciphertext,
            page.ciphertext.inner.capacity(),
            cache,
        )
    }
    fn validate_ciphertext(&self, copy: &CiphertextCopy) -> Result<()> {
        copy.validate_metadata()?;
        self.validate_reservation(
            copy.ciphertext.inner.reservation(),
            ResourceClass::Ciphertext,
            copy.ciphertext.inner.capacity(),
            Some(&copy.metadata.version.object.cache),
        )
    }
}
#[cfg(test)]
thread_local! {
    static REGISTERED_COPY_OUTS: std::cell::Cell<u64> = const { std::cell::Cell::new(0) };
}
/// Count one copy of registered RDMA ciphertext out of its pool buffer.
#[cfg(test)]
fn note_registered_copy_out() {
    REGISTERED_COPY_OUTS.with(|count| count.set(count.get().wrapping_add(1)));
}
/// Registered RDMA pages copied out of pool buffers on this thread.
///
/// Thread-local because copies run on the owning worker's I/O thread. Test
/// instrumentation only.
#[cfg(test)]
pub(crate) fn registered_copy_outs() -> u64 {
    REGISTERED_COPY_OUTS.with(std::cell::Cell::get)
}
/// Capacity of an unpooled buffer of `padded` bytes at `alignment`.
fn unpooled_capacity(alignment: page_alloc::Alignment, padded: usize) -> usize {
    padded.next_multiple_of(alignment.memory().max(page_alloc::buf::GRANULE))
}
/// Bytes from the start of `length` through the next length-unit boundary.
fn padded_length(alignment: page_alloc::Alignment, length: usize) -> Result<usize> {
    let unit = alignment.length();
    length
        .checked_next_multiple_of(unit)
        .filter(|padded| length != 0 && *padded <= page_alloc::Alignment::MAX_TRANSFER_LENGTH)
        .ok_or(Error::InvalidConfiguration)
}
impl VerifiedPage {
    pub fn page(&self) -> &PageId {
        &self.inner.page
    }
    pub fn bytes(&self) -> &[u8] {
        &self.inner.storage
    }
}

impl CiphertextPage {
    /// Pinning handles on these bytes, including this one when it pins.
    pub(crate) fn pins(&self) -> u32 {
        self.inner.pins.get()
    }
    /// Whether this is the only pinning handle, so dropping it frees the
    /// bytes unless an unpinned observer still holds them.
    pub(crate) fn exclusive(&self) -> bool {
        self.pins() == 1
    }
    /// An observer handle that keeps the bytes alive without pinning them.
    #[cfg_attr(
        not(test),
        expect(dead_code, reason = "for telemetry and sampling observers")
    )]
    pub(crate) fn unpinned(&self) -> Self {
        Self {
            inner: self.inner.clone(),
            provenance: self.provenance,
            pinned: false,
        }
    }
    /// Rehome a completed receive before retaining it in another worker's cache.
    /// Admit the new charge first. Shared transport owners keep their original
    /// allocation and charge until fenced; only exclusive bytes can move in place.
    pub(crate) fn rehome(self, reservation: flow_control::Charge<AdmissionPolicy>) -> Result<Self> {
        self.rehome_aligned_to(reservation, default_disk_alignment())
    }
    /// Charge a rehomed copy needs to land in zero-padded storage aligned for
    /// `alignment`, so a disk write can use it without another copy.
    pub(crate) fn rehome_capacity(&self, alignment: page_alloc::Alignment) -> Result<usize> {
        padded_length(alignment, self.bytes().len())
            .map(|padded| unpooled_capacity(alignment, padded))
    }
    /// [`Self::rehome`], copying into storage aligned for `alignment` (the
    /// device's) when `reservation` covers [`Self::rehome_capacity`].
    pub(crate) fn rehome_aligned_to(
        mut self,
        reservation: flow_control::Charge<AdmissionPolicy>,
        alignment: page_alloc::Alignment,
    ) -> Result<Self> {
        reservation.validate(ResourceClass::Ciphertext, self.inner.capacity())?;
        if reservation.key() != Some(&self.envelope().page.version.object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        // Registered pages always copy out: retaining them would pin pool buffers.
        if !self.is_registered()
            && let Some(inner) = Arc::get_mut(&mut self.inner)
        {
            match &mut inner.storage {
                CiphertextStorage::Slab { buffer, .. } => {
                    buffer
                        .replace_charge(reservation)
                        .map_err(|_| Error::from(page_alloc::Error::InvalidConfiguration))?;
                }
                CiphertextStorage::Aligned { buffer, .. } => {
                    buffer
                        .replace_charge(reservation)
                        .map_err(|_| Error::from(page_alloc::Error::InvalidConfiguration))?;
                }
                CiphertextStorage::Registered { .. } => unreachable!(),
            }
            return Ok(self);
        }
        let length = self.bytes().len();
        // Prefer a zero-padded aligned copy the store can write without
        // another copy; otherwise copy into unaligned storage the charge covers.
        let copy = if let Ok(padded) = padded_length(alignment, length)
            && reservation.amount() >= unpooled_capacity(alignment, padded)
        {
            // Fresh allocations are zeroed, so the pad past `length` stays zero.
            let mut buffer =
                page_alloc::buf::Buf::unpooled(padded, alignment.memory(), reservation, false)?;
            buffer[..length].copy_from_slice(self.bytes());
            Self {
                pinned: true,
                inner: Arc::new(CiphertextBytes {
                    pins: Default::default(),
                    envelope: self.envelope().clone(),
                    storage: CiphertextStorage::Aligned { buffer, length },
                }),
                provenance: self.provenance,
            }
        } else {
            let mut buffer = page_alloc::buf::Buf::unpooled(length, 1, reservation, false)?;
            buffer.copy_from_slice(self.bytes());
            Self {
                pinned: true,
                inner: Arc::new(CiphertextBytes {
                    pins: Default::default(),
                    envelope: self.envelope().clone(),
                    storage: CiphertextStorage::Slab {
                        buffer,
                        range: 0..length,
                    },
                }),
                provenance: self.provenance,
            }
        };
        self.record_copy_out(&copy);
        Ok(copy)
    }
    /// Count a copy of registered ciphertext and remember the first one so
    /// the next retaining path for the same admission shares it. Other
    /// storage is not a registered copy-out and is ignored.
    pub(crate) fn record_copy_out(&self, copy: &Self) {
        if let CiphertextStorage::Registered { retained, .. } = &self.inner.storage {
            #[cfg(test)]
            note_registered_copy_out();
            let _ = retained.set(copy.clone());
        }
    }
    /// The owned copy an earlier retaining path made of this registered page.
    pub(crate) fn retained_copy(&self) -> Option<Self> {
        match &self.inner.storage {
            CiphertextStorage::Registered { retained, .. } => {
                retained.get().cloned().map(|mut copy| {
                    copy.provenance = self.provenance;
                    copy
                })
            }
            _ => None,
        }
    }
    /// Freeze aligned output into a page. Callers have validated the length
    /// against `envelope` and the buffer's Ciphertext charge.
    pub(crate) fn from_aligned(buffer: CiphertextBuffer, envelope: PageEnvelope) -> Self {
        let (buffer, length) = buffer.freeze();
        Self {
            pinned: true,
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                pins: Default::default(),
                envelope,
                storage: CiphertextStorage::Aligned { buffer, length },
            }),
        }
    }
    pub fn envelope(&self) -> &PageEnvelope {
        &self.inner.envelope
    }
    pub fn bytes(&self) -> &[u8] {
        match &self.inner.storage {
            CiphertextStorage::Slab { buffer, range } => &buffer[range.clone()],
            CiphertextStorage::Aligned { buffer, length } => &buffer[..*length],
            CiphertextStorage::Registered { page, .. } => page.bytes(),
        }
    }
    /// The zero-padded ciphertext body when it can be written to a device with
    /// `alignment` without copying.
    ///
    /// Returns `Some` only for pages produced into aligned storage (AEAD output,
    /// TCP receive, and aligned rehome copies) whose allocation address is a
    /// multiple of `alignment.memory()` and whose padded length is a multiple
    /// of `alignment.length()`. The slice starts with [`Self::bytes`] and is
    /// followed only by zeros. Callers must copy when this returns `None`.
    pub(crate) fn disk_body(&self, alignment: page_alloc::Alignment) -> Option<&[u8]> {
        let CiphertextStorage::Aligned { buffer, .. } = &self.inner.storage else {
            return None;
        };
        let padded: &[u8] = buffer;
        ((padded.as_ptr() as usize).is_multiple_of(alignment.memory())
            && padded.len().is_multiple_of(alignment.length()))
        .then_some(padded)
    }
    /// Whether [`Self::disk_body`] is available for `alignment`.
    #[cfg(test)]
    pub(crate) fn is_disk_aligned(&self, alignment: page_alloc::Alignment) -> bool {
        self.disk_body(alignment).is_some()
    }
    /// Whether the bytes still live in a registered RDMA pool buffer.
    pub(crate) fn is_registered(&self) -> bool {
        matches!(self.inner.storage, CiphertextStorage::Registered { .. })
    }
}
// SAFETY: shared ciphertext backing is immutable and retained by the owner.
unsafe impl racer_io::ring::SendBuffer for CiphertextPage {
    fn send_bytes(&self) -> racer_io::Result<&[u8]> {
        Ok(self.bytes())
    }
}
/// Credential-free page results shared by fills, memory, and flight completion.
/// Retain all three values for the same version until the last reader releases
/// them. Freshness-pointer eviction must not strand a cached page without length.
#[derive(Clone)]
pub struct PageResult {
    pub metadata: ObjectMetadata,
    pub plaintext: VerifiedPage,
    pub ciphertext: CiphertextPage,
}

/// Pending and completed peer copies retain metadata alongside original ciphertext.
/// The deadline is historical, not permission to advance the current-version pointer.
#[derive(Clone)]
pub struct CiphertextCopy {
    pub metadata: ObjectMetadata,
    pub ciphertext: CiphertextPage,
}

/// Structurally validated acquisition output, not proof of page AEAD. Only a
/// plaintext consumer may promote this to PageResult after authenticating bytes.
#[derive(Clone)]
pub struct UnverifiedPage {
    pub copy: CiphertextCopy,
    pub(crate) disk_token: Option<crate::store::ReadToken>,
}

#[derive(Clone)]
pub enum AcquiredPage {
    Ciphertext(UnverifiedPage),
    Plaintext(PageResult),
}
impl AcquiredPage {
    #[cfg(test)]
    pub(crate) fn verified(self) -> PageResult {
        match self {
            Self::Plaintext(page) => page,
            Self::Ciphertext(_) => panic!("expected verified page"),
        }
    }
    pub fn copy(&self) -> CiphertextCopy {
        match self {
            Self::Ciphertext(page) => page.copy.clone(),
            Self::Plaintext(page) => page.copy(),
        }
    }

    /// Validate the requested identity without cloning retained payload owners.
    pub fn validate_for(&self, page: &PageId) -> Result<()> {
        match self {
            Self::Plaintext(result) => result.validate_for(page),
            Self::Ciphertext(result) => {
                let copy = &result.copy;
                if &copy.ciphertext.envelope().page != page {
                    return Err(Error::CorruptRecord);
                }
                copy.validate_metadata()
            }
        }
    }
}
impl From<PageResult> for AcquiredPage {
    fn from(page: PageResult) -> Self {
        Self::Plaintext(page)
    }
}

impl PageResult {
    /// An internally consistent bundle still must match the requested flight page.
    /// This is structural validation, not a substitute for authentication.
    pub fn validate_for(&self, page: &PageId) -> Result<()> {
        if self.plaintext.page() != page {
            return Err(Error::CorruptRecord);
        }
        self.validate_metadata()
    }

    /// Structural agreement only. Authentication remains the fill/crypto boundary.
    pub fn validate_metadata(&self) -> Result<()> {
        self.metadata.validate()?;
        validate_ciphertext_length(&self.ciphertext)?;
        validate_association(
            &self.metadata,
            self.plaintext.page(),
            self.plaintext.bytes().len() as u64,
            self.ciphertext.envelope(),
        )
    }
    pub fn copy(&self) -> CiphertextCopy {
        CiphertextCopy {
            metadata: self.metadata.clone(),
            ciphertext: self.ciphertext.clone(),
        }
    }
}

impl CiphertextCopy {
    pub fn validate_metadata(&self) -> Result<()> {
        validate_ciphertext(&self.metadata, &self.ciphertext)
    }
}

fn validate_ciphertext(metadata: &ObjectMetadata, ciphertext: &CiphertextPage) -> Result<()> {
    metadata.validate()?;
    metadata.immutable().validate_page(ciphertext.envelope())?;
    validate_ciphertext_length(ciphertext)
}
fn validate_ciphertext_length(ciphertext: &CiphertextPage) -> Result<()> {
    if ciphertext.bytes().len() != ciphertext.envelope().ciphertext_length as usize {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}

fn validate_association(
    metadata: &ObjectMetadata,
    plaintext_page: &PageId,
    plaintext_length: u64,
    envelope: &PageEnvelope,
) -> Result<()> {
    metadata.immutable().validate_page(envelope)?;
    if plaintext_page != &envelope.page || plaintext_length != u64::from(envelope.plaintext_length)
    {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}

/// One authenticated page retained only as its original ciphertext.
///
/// The cache never owns plaintext. `plaintext` is a weak handle to the last
/// authenticated plaintext for this page; it upgrades only while a request or
/// stream still holds that buffer, letting a concurrent reader share it
/// without a second decrypt. It never keeps plaintext or its charge alive.
pub(crate) struct CachedPage {
    pub(crate) metadata: ObjectMetadata,
    pub(crate) ciphertext: CiphertextPage,
    plaintext: std::sync::Weak<VerifiedBytes>,
}
impl CachedPage {
    fn new(page: PageResult) -> Self {
        Self {
            plaintext: Arc::downgrade(&page.plaintext.inner),
            metadata: page.metadata,
            ciphertext: page.ciphertext,
        }
    }
    /// The verified bundle, if some other owner still holds its plaintext.
    fn live(&self) -> Option<PageResult> {
        let inner = self.plaintext.upgrade()?;
        Some(PageResult {
            metadata: self.metadata.clone(),
            plaintext: VerifiedPage { inner },
            ciphertext: self.ciphertext.clone(),
        })
    }
    pub(crate) fn copy(&self) -> CiphertextCopy {
        CiphertextCopy {
            metadata: self.metadata.clone(),
            ciphertext: self.ciphertext.clone(),
        }
    }
}

/// Memory cache lanes: one per cache, so one cache's entries are reclaimed
/// without scanning the others.
struct ByCache;
impl racer_core::cache::Grouping<PageId> for ByCache {
    type Id = CacheId;
    fn group(key: &PageId) -> Option<CacheId> {
        Some(key.version.object.cache.clone())
    }
}

/// Verified pages in tick order plus a per-version page set. The recency
/// index and its reclamation rules live in [`racer_core::cache`], which
/// the simulator drives with payload-free values.
#[derive(Default)]
struct Entries {
    lru: racer_core::cache::TickLru<PageId, CachedPage, std::hash::RandomState, ByCache>,
    versions: HashMap<ObjectVersion, HashSet<PageId>>,
}
impl std::ops::Deref for Entries {
    type Target = racer_core::cache::TickLru<PageId, CachedPage, std::hash::RandomState, ByCache>;
    fn deref(&self) -> &Self::Target {
        &self.lru
    }
}
impl std::ops::DerefMut for Entries {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.lru
    }
}
impl Entries {
    fn remove(&mut self, id: &PageId) -> Option<CachedPage> {
        let page = self.lru.remove(id)?;
        self.forget_version(id);
        Some(page)
    }
    /// Drop `id` from its version's page set after the LRU removed it.
    fn forget_version(&mut self, id: &PageId) {
        if let Some(pages) = self.versions.get_mut(&id.version) {
            pages.remove(id);
            if pages.is_empty() {
                self.versions.remove(&id.version);
            }
        }
    }
    fn insert(&mut self, page: PageResult) -> Result<()> {
        let id = page.plaintext.page().clone();
        let page = CachedPage::new(page);
        self.lru
            .insert(id.clone(), page)
            .map_err(|_| Error::Overloaded)?;
        self.versions
            .entry(id.version.clone())
            .or_default()
            .insert(id);
        Ok(())
    }
    /// Move `page` to the most recently used tick.
    fn touch(&mut self, page: &PageId) -> Result<()> {
        self.lru.touch(page).map_err(|_| Error::Overloaded)
    }
}
/// Unverified original ciphertext keyed by page, with a monotonic touch tick
/// per entry so idle expiry can find the least recently touched entries.
use racer_core::cache::Visit;
type CiphertextEntries =
    racer_core::cache::OrderedLru<PageId, UnverifiedPage, std::hash::RandomState, ByCache>;

/// Worker-local original ciphertext, both authenticated (with a weak handle to
/// any live plaintext) and not yet authenticated; independent of disk clock.
/// Plaintext is never retained here: a local hit decrypts the cached ciphertext
/// unless another owner still holds the page's plaintext.
pub struct MemoryCache {
    // BufferPool holds only shared handles; do not retain a second Rc layer.
    pool: BufferPool,
    entries: RefCell<Entries>,
    ciphertext_entries: RefCell<CiphertextEntries>,
    ciphertext_cursor: RefCell<Option<PageId>>,
    /// Last cache examined by a global reclaim's over-share window.
    pressure_cursor: RefCell<Option<CacheId>>,
    availability: Rc<crate::control::Availability>,
    expiry: RefCell<racer_core::cache::Expiry>,
    key_sweep: RefCell<KeySweep>,
}

/// Progress of a bounded pass that drops entries whose page key the node no
/// longer holds. A pass starts whenever the keyring generation changes and
/// walks each table in recency order; entries touched during the pass move
/// past the cursor and are still visited.
#[derive(Default)]
struct KeySweep {
    generation: Option<u64>,
    verified: Option<u64>,
    ciphertext: Option<u64>,
}

impl MemoryCache {
    /// Verified pages currently indexed, without touching them.
    #[cfg(test)]
    pub(crate) fn resident(&self) -> Vec<PageId> {
        self.entries.borrow().pages.keys().cloned().collect()
    }
    /// This worker's allocation front end.
    pub(crate) fn buffers(&self) -> &BufferPool {
        &self.pool
    }
    pub fn new(pool: BufferPool, availability: Rc<crate::control::Availability>) -> Self {
        Self {
            pool,
            entries: RefCell::new(Entries::default()),
            ciphertext_entries: RefCell::new(CiphertextEntries::default()),
            ciphertext_cursor: RefCell::new(None),
            pressure_cursor: RefCell::new(None),
            availability,
            expiry: RefCell::new(racer_core::cache::Expiry::default()),
            key_sweep: RefCell::default(),
        }
    }
    /// Published cache admission and modes shared with the read path.
    pub(crate) fn availability(&self) -> &Rc<crate::control::Availability> {
        &self.availability
    }
    fn available(&self, page: &CachedPage) -> bool {
        // A passthrough cache retains nothing, so residual entries from before
        // a mode change are never served.
        !self
            .availability
            .passthrough(&page.metadata.version.object.cache)
            && self.availability.page(
                &page.metadata.version.object.cache,
                page.ciphertext.envelope().key_id,
            )
    }
    /// The authenticated cached page, touched, after dropping it if its cache
    /// or key is no longer admitted.
    fn verified(&self, page: &PageId) -> Result<Option<std::cell::RefMut<'_, Entries>>> {
        let mut entries = self.entries.borrow_mut();
        let Some((_, entry)) = entries.pages.get(page) else {
            return Ok(None);
        };
        if !self.available(entry) {
            entries.remove(page);
            return Ok(None);
        }
        entries.touch(page)?;
        Ok(Some(entries))
    }
    /// A verified page whose plaintext some other owner still holds. A cached
    /// page whose plaintext was released is a miss here; callers decrypt it
    /// from [`Self::unverified`] instead.
    pub fn get(&self, page: &PageId) -> Result<Option<PageResult>> {
        Ok(self
            .verified(page)?
            .and_then(|entries| entries.pages[page].1.live()))
    }
    /// Original ciphertext for peers and disk, authenticated or not.
    pub fn ciphertext(&self, page: &PageId) -> Result<Option<CiphertextCopy>> {
        if let Some(entries) = self.verified(page)? {
            return Ok(Some(entries.pages[page].1.copy()));
        }
        Ok(self.unverified(page)?.map(|entry| entry.copy))
    }
    /// Cached ciphertext a local reader must authenticate and decrypt. An
    /// authenticated page is returned too, so a plaintext miss in
    /// [`Self::get`] decrypts from memory rather than refetching.
    pub(crate) fn unverified(&self, page: &PageId) -> Result<Option<UnverifiedPage>> {
        if let Some(entries) = self.verified(page)? {
            return Ok(Some(UnverifiedPage {
                copy: entries.pages[page].1.copy(),
                disk_token: None,
            }));
        }
        let mut entries = self.ciphertext_entries.borrow_mut();
        if entries.get(page).is_some_and(|entry| {
            self.availability.passthrough(&page.version.object.cache)
                || !self.availability.page(
                    &page.version.object.cache,
                    entry.copy.ciphertext.envelope().key_id,
                )
        }) {
            entries.remove(page);
        }
        entries.touch(page);
        Ok(entries.get(page).cloned())
    }
    pub(crate) fn publish_ciphertext(&self, mut page: UnverifiedPage) -> Result<()> {
        self.pool.validate_ciphertext(&page.copy)?;
        let id = &page.copy.ciphertext.envelope().page;
        // Passthrough pages live only as long as their in-flight readers.
        if self.availability.passthrough(&id.version.object.cache) {
            return Ok(());
        }
        if !self.availability.page(
            &id.version.object.cache,
            page.copy.ciphertext.envelope().key_id,
        ) {
            return Err(Error::MissingKey);
        }
        // A verified entry already owns this page's ciphertext. Retaining a
        // second copy would pin it and keep it from ever going idle.
        if self.entries.borrow().pages.contains_key(id)
            || self.ciphertext_entries.borrow().contains_key(id)
        {
            return Ok(());
        }
        page.copy.ciphertext = self.pool.retainable(page.copy.ciphertext)?;
        let id = &page.copy.ciphertext.envelope().page;
        let mut entries = self.ciphertext_entries.borrow_mut();
        if entries.contains_key(id) {
            return Ok(());
        }
        if entries.len() + self.entries.borrow().len() >= self.pool.entry_limit() {
            return Err(Error::Overloaded);
        }
        entries.insert(id.clone(), page);
        Ok(())
    }
    pub(crate) fn invalidate_ciphertext(&self, page: &UnverifiedPage) {
        let id = &page.copy.ciphertext.envelope().page;
        // A registered page was copied out on publish, so match its contents.
        let registered = page.copy.ciphertext.is_registered();
        let matches = |cached: &CiphertextPage| {
            Arc::ptr_eq(&cached.inner, &page.copy.ciphertext.inner)
                || (registered
                    && cached.envelope() == page.copy.ciphertext.envelope()
                    && cached.bytes() == page.copy.ciphertext.bytes())
        };
        let mut entries = self.ciphertext_entries.borrow_mut();
        if entries
            .get(id)
            .is_some_and(|entry| matches(&entry.copy.ciphertext))
        {
            entries.remove(id);
        }
        drop(entries);
        // Cached authenticated ciphertext that later fails to decrypt (memory
        // corruption or a revoked key) must not be served again.
        let mut entries = self.entries.borrow_mut();
        if entries
            .pages
            .get(id)
            .is_some_and(|(_, entry)| matches(&entry.ciphertext))
        {
            entries.remove(id);
        }
    }
    /// Validate matching identities and full-page bounds before retaining the bundle.
    pub fn publish(&self, page: PageResult) -> Result<()> {
        self.pool.validate_page(&page)?;
        // Passthrough pages live only as long as their in-flight readers.
        if self
            .availability
            .passthrough(&page.metadata.version.object.cache)
        {
            return Ok(());
        }
        self.publish_validated(page)
    }
    fn publish_validated(&self, mut page: PageResult) -> Result<()> {
        // Copy registered ciphertext out before retaining it; a duplicate is
        // rejected or ignored below and keeps the original entry.
        if page.ciphertext.is_registered()
            && !self
                .entries
                .borrow()
                .pages
                .contains_key(page.plaintext.page())
        {
            page.ciphertext = self.pool.retainable(page.ciphertext)?;
        }
        let id = page.plaintext.page();
        self.ciphertext_entries.borrow_mut().remove(id);
        if !self.availability.cache(&id.version.object.cache) {
            return Err(Error::Unavailable);
        }
        if !self.availability.page(
            &page.metadata.version.object.cache,
            page.ciphertext.envelope().key_id,
        ) {
            return Err(Error::MissingKey);
        }
        let mut entries = self.entries.borrow_mut();
        if let Some(known) = entries
            .versions
            .get(&page.metadata.version)
            .and_then(|pages| pages.iter().next())
            .and_then(|id| entries.pages.get(id))
            && !known
                .1
                .metadata
                .immutable()
                .compatible(&page.metadata.immutable())
        {
            return Err(Error::CorruptRecord);
        }
        if let Some((_, entry)) = entries.pages.get_mut(id) {
            match entry.plaintext.upgrade() {
                Some(live) if live.storage[..] != page.plaintext.inner.storage[..] => {
                    return Err(Error::CorruptRecord);
                }
                Some(_) => {}
                // Remember the newly decrypted plaintext so concurrent readers
                // can share it while it lives.
                None => entry.plaintext = Arc::downgrade(&page.plaintext.inner),
            }
            // A duplicate fill must not replace the original nonce/ciphertext
            // or turn its historical deadline into renewed freshness.
            return Ok(());
        }
        if entries.len() + self.ciphertext_entries.borrow().len() >= self.pool.entry_limit() {
            let id = entries.victim(idle).ok_or(Error::Overloaded)?;
            entries.remove(&id);
        }
        entries.insert(page)
    }
    pub(crate) fn metadata(&self, version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        if self.availability.passthrough(&version.object.cache) {
            return Ok(None);
        }
        let entries = self.entries.borrow();
        let found = entries
            .versions
            .get(version)
            .and_then(|pages| {
                pages
                    .iter()
                    .filter_map(|id| entries.pages.get(id))
                    .find(|(_, entry)| self.available(entry))
            })
            .map(|(_, entry)| entry.metadata.immutable());
        if found.is_some() {
            return Ok(found);
        }
        Ok(self
            .ciphertext_entries
            .borrow()
            .values()
            .find(|entry| {
                &entry.copy.metadata.version == version
                    && self.availability.page(
                        &version.object.cache,
                        entry.copy.ciphertext.envelope().key_id,
                    )
            })
            .map(|entry| entry.copy.metadata.immutable()))
    }
    /// Return released admission bytes, including any reserved final-page slack.
    /// Busy ciphertext protects a retained page; live plaintext does not, since
    /// the cache holds it only weakly.
    pub fn evict_idle(&self, bytes: usize) -> Result<usize> {
        let released = self.evict_idle_entries(bytes)?;
        self.pool.reclaim_buffers();
        Ok(released)
    }
    /// Like [`Self::evict_idle`], but keeps idle pooled buffers so later
    /// reads reuse them instead of allocating.
    pub fn evict_idle_entries(&self, bytes: usize) -> Result<usize> {
        let mut released = self.reclaim_ciphertext(None, bytes);
        let mut entries = self.entries.borrow_mut();
        let mut versions = Vec::new();
        entries.lru.reclaim(|id, entry| {
            if released >= bytes {
                return Visit::Stop;
            }
            if !idle(entry) {
                return Visit::Skip;
            }
            released = released.saturating_add(entry.ciphertext.inner.reservation().amount());
            versions.push(id.clone());
            Visit::Remove
        });
        for id in versions {
            entries.forget_version(&id);
        }
        Ok(released)
    }
    /// One bounded LRU pass over ciphertext. The callback may release a queued
    /// writer's sole extra ciphertext reference, never a reader. The cache
    /// holds no plaintext, so plaintext pressure releases nothing here.
    pub(crate) fn reclaim_idle(
        &self,
        class: ResourceClass,
        cache: Option<&CacheId>,
        bytes: usize,
        mut release_queued: impl FnMut(&PageId, &CiphertextPage) -> usize,
    ) -> usize {
        if !matches!(class, ResourceClass::Ciphertext) {
            return 0;
        }
        let mut released = self.reclaim_ciphertext(cache, bytes);
        let mut entries = self.entries.borrow_mut();
        let mut versions = Vec::new();
        entries.lru.reclaim(|id, entry| {
            if released >= bytes {
                return Visit::Stop;
            }
            if cache.is_some_and(|cache| cache != &entry.metadata.version.object.cache) {
                return Visit::Skip;
            }
            released = released.saturating_add(release_queued(id, &entry.ciphertext));
            if released >= bytes {
                return Visit::Last;
            }
            if !idle(entry) {
                return Visit::Skip;
            }
            released = released.saturating_add(entry.ciphertext.inner.reservation().amount());
            versions.push(id.clone());
            Visit::Remove
        });
        for id in versions {
            entries.forget_version(&id);
        }
        released
    }
    #[cfg(any(test, feature = "subscription-interop"))]
    pub(crate) fn remove_cache(&self, cache: &CacheId) -> Result<()> {
        self.remove_caches(&std::collections::HashSet::from([cache.clone()]))
    }
    /// Remove every entry belonging to any cache in `caches` in one pass over
    /// each table, so cost does not scale with the number of caches.
    pub(crate) fn remove_caches(&self, caches: &std::collections::HashSet<CacheId>) -> Result<()> {
        if caches.is_empty() {
            return Ok(());
        }
        self.ciphertext_entries
            .borrow_mut()
            .retain(|id| !caches.contains(&id.version.object.cache));
        let mut entries = self.entries.borrow_mut();
        let removed: Vec<_> = entries
            .pages
            .keys()
            .filter(|id| caches.contains(&id.version.object.cache))
            .cloned()
            .collect();
        for id in removed {
            entries.remove(&id);
        }
        Ok(())
    }
    /// Drop at most `budget` examined entries per table whose cache or page
    /// key is no longer admitted. Idle until the keyring generation changes,
    /// then resumes each call until both tables have been walked once.
    /// Returns the number of entries removed.
    pub(crate) fn sweep_unavailable(&self, budget: usize) -> usize {
        let mut sweep = self.key_sweep.borrow_mut();
        let generation = self.availability.key_generation();
        if generation != sweep.generation {
            *sweep = KeySweep {
                generation,
                verified: Some(0),
                ciphertext: Some(0),
            };
        }
        let mut removed = 0;
        if let Some(after) = sweep.verified {
            let mut entries = self.entries.borrow_mut();
            let (selected, next) = entries.expiring(after, u64::MAX, budget);
            sweep.verified = next;
            for (_, id) in selected {
                if !self.available(&entries.pages[&id].1) {
                    entries.remove(&id);
                    removed += 1;
                }
            }
        }
        if let Some(after) = sweep.ciphertext {
            let mut entries = self.ciphertext_entries.borrow_mut();
            let (selected, next) = entries.expiring(after, u64::MAX, budget);
            sweep.ciphertext = next;
            for (_, id) in selected {
                let cache = &id.version.object.cache;
                let key = entries[&id].copy.ciphertext.envelope().key_id;
                if self.availability.passthrough(cache) || !self.availability.page(cache, key) {
                    entries.remove(&id);
                    removed += 1;
                }
            }
        }
        removed
    }
    /// Release entries not inserted or hit for at least `racer_core::cache::IDLE_TTL`,
    /// skipping any still referenced elsewhere (retried on the next pass).
    /// Examines at most `racer_core::cache::EXPIRY_BATCH` entries per class per call and
    /// returns the number of entries released. Freed buffers return to the
    /// pools, whose own maintenance decays them.
    pub(crate) fn expire_idle(&self, now: std::time::Instant) -> usize {
        let mut expiry = self.expiry.borrow_mut();
        let verified_clock = self.entries.borrow().clock;
        let ciphertext_clock = self.ciphertext_entries.borrow().clock;
        let Some((verified_cutoff, ciphertext_cutoff)) =
            expiry.advance(now, verified_clock, ciphertext_clock)
        else {
            return 0;
        };
        let mut released = 0;
        if let Some(after) = expiry.verified {
            let mut entries = self.entries.borrow_mut();
            let (selected, next) =
                entries.expiring(after, verified_cutoff, racer_core::cache::EXPIRY_BATCH);
            expiry.verified = next;
            for (_, id) in selected {
                if idle(&entries.pages[&id].1) {
                    entries.remove(&id);
                    released += 1;
                }
            }
        }
        if let Some(after) = expiry.ciphertext {
            let mut entries = self.ciphertext_entries.borrow_mut();
            let (selected, next) =
                entries.expiring(after, ciphertext_cutoff, racer_core::cache::EXPIRY_BATCH);
            expiry.ciphertext = next;
            for (_, id) in selected {
                if entries[&id].copy.ciphertext.exclusive() {
                    entries.remove(&id);
                    released += 1;
                }
            }
        }
        released
    }
    /// Reclaim `bytes` of ciphertext for `owner`, or for global pressure when
    /// `owner` is `None`. An owner's deficit scans only that owner's entries.
    /// Global pressure first takes idle bytes from over-share caches in a
    /// bounded window, then falls back to the unweighted pass. So an
    /// under-share requester keeps its own pages unless the window's
    /// over-share bytes are busy, missing, or too few.
    ///
    /// `excess` reports a cache's bytes above its share (`Quotas::excess`).
    /// `reclaim_writer` releases queued writer copies and returns the bytes
    /// released. Plaintext is never retained, so plaintext pressure releases
    /// nothing.
    ///
    /// Work is bounded: at most `racer_core::cache::PRESSURE_WINDOW` caches, each with at most
    /// `racer_core::cache::RECLAIM_WINDOW` entries per kind, plus the existing unweighted pass.
    /// The window rotates instead of ranking all caches. The next call
    /// resumes after the last cache this call examined, so a pass that ends
    /// early does not skip the rest of its window. Busy or held entries are
    /// skipped as in [`Self::reclaim_idle`].
    pub(crate) fn reclaim(
        &self,
        class: ResourceClass,
        owner: Option<&CacheId>,
        bytes: usize,
        excess: impl Fn(&CacheId) -> usize,
        mut release_queued: impl FnMut(&PageId, &CiphertextPage) -> usize,
        mut reclaim_writer: impl FnMut(Option<&CacheId>, usize) -> usize,
    ) -> usize {
        if !matches!(class, ResourceClass::Ciphertext) || bytes == 0 {
            return 0;
        }
        if let Some(owner) = owner {
            let released = self.reclaim_cache(owner, bytes, &mut release_queued);
            if released >= bytes {
                return released;
            }
            return released.saturating_add(reclaim_writer(Some(owner), bytes - released));
        }
        let mut released = 0;
        for cache in self.pressure_window() {
            if released >= bytes {
                break;
            }
            // Resume after the last examined cache, not the last prefetched one.
            *self.pressure_cursor.borrow_mut() = Some(cache.clone());
            let want = excess(&cache).min(bytes - released);
            if want == 0 {
                continue;
            }
            let mut got = self.reclaim_cache(&cache, want, &mut release_queued);
            if got < want {
                got = got.saturating_add(reclaim_writer(Some(&cache), want - got));
            }
            released = released.saturating_add(got);
        }
        if released < bytes {
            released = released.saturating_add(self.reclaim_idle(
                class,
                None,
                bytes - released,
                &mut release_queued,
            ));
        }
        if released < bytes {
            released = released.saturating_add(reclaim_writer(None, bytes - released));
        }
        released
    }
    /// Up to `racer_core::cache::PRESSURE_WINDOW` caches with retained entries after the cursor,
    /// in rotating order. Does not move the cursor; `reclaim` moves it as it
    /// examines each cache. Cost is O(racer_core::cache::PRESSURE_WINDOW * log caches),
    /// independent of entry count.
    fn pressure_window(&self) -> Vec<CacheId> {
        let cursor = self.pressure_cursor.borrow();
        let mut caches = self.entries.borrow().lanes.window(cursor.as_ref());
        caches.extend(
            self.ciphertext_entries
                .borrow()
                .lanes
                .window(cursor.as_ref()),
        );
        // Wrapped order: caches after the cursor come first.
        let wrapped = |cache: &CacheId| cursor.as_ref().is_some_and(|cursor| cache <= cursor);
        caches.sort_by(|a, b| (wrapped(a), a).cmp(&(wrapped(b), b)));
        caches.dedup();
        caches.truncate(racer_core::cache::PRESSURE_WINDOW);
        caches
    }
    /// One bounded pass over a single cache's idle ciphertext, oldest first.
    /// Unlike a keyed [`Self::reclaim_idle`], it scans only that cache's
    /// entries, so other caches cannot use up its scan budget.
    fn reclaim_cache(
        &self,
        cache: &CacheId,
        bytes: usize,
        mut release_queued: impl FnMut(&PageId, &CiphertextPage) -> usize,
    ) -> usize {
        let mut released = 0;
        {
            let mut entries = self.ciphertext_entries.borrow_mut();
            for (tick, id) in entries.lanes.candidates(cache) {
                if released >= bytes {
                    break;
                }
                entries.lanes.advance(cache, tick);
                if !entries[&id].copy.ciphertext.exclusive() {
                    continue;
                }
                if let Some(entry) = entries.remove(&id) {
                    released += entry.copy.ciphertext.inner.reservation().amount();
                }
            }
        }
        let mut entries = self.entries.borrow_mut();
        for (tick, id) in entries.lanes.candidates(cache) {
            if released >= bytes {
                break;
            }
            entries.lanes.advance(cache, tick);
            let entry = &entries.pages[&id].1;
            released = released.saturating_add(release_queued(&id, &entry.ciphertext));
            if released >= bytes {
                break;
            }
            if !idle(entry) {
                continue;
            }
            released = released.saturating_add(entry.ciphertext.inner.reservation().amount());
            entries.remove(&id);
        }
        released
    }
    fn reclaim_ciphertext(&self, cache: Option<&CacheId>, bytes: usize) -> usize {
        let mut entries = self.ciphertext_entries.borrow_mut();
        let cursor = self.ciphertext_cursor.borrow().clone();
        let selected: Vec<_> = entries
            .range((
                cursor
                    .as_ref()
                    .map_or(std::ops::Bound::Unbounded, std::ops::Bound::Excluded),
                std::ops::Bound::Unbounded,
            ))
            .chain(
                entries
                    .iter()
                    .take_while(|(id, _)| cursor.as_ref().is_some_and(|cursor| *id <= cursor)),
            )
            .take(256)
            .map(|(id, _)| id.clone())
            .collect();
        let mut released = 0;
        for id in selected {
            if released >= bytes {
                break;
            }
            *self.ciphertext_cursor.borrow_mut() = Some(id.clone());
            if cache.is_some_and(|c| c != &id.version.object.cache)
                || !entries[&id].copy.ciphertext.exclusive()
            {
                continue;
            }
            if let Some(entry) = entries.remove(&id) {
                released += entry.copy.ciphertext.inner.reservation().amount();
            }
        }
        released
    }
}
/// No pinning handle outside the cache shares the retained ciphertext.
fn idle(entry: &CachedPage) -> bool {
    entry.ciphertext.exclusive()
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::model::Nonce;
    use crate::model::PageNumber;
    use crate::model::VersionMetadata;
    use racer_io::ring::IoBuffer;
    /// Owner and reclamation contracts over real allocations.
    mod owners {
        use super::*;
        use crate::lifecycle::Service;
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::RequestId;
        use crate::model::StrongEtag;
        use crate::model::WorkerId;
        use crate::runtime::RequestScope;
        use crate::security;
        use crate::security::CryptoClient;
        use crate::security::PageCrypto;
        use crate::security::PageCryptoEngine;
        use crate::worker::CryptoRuntime;
        use std::num::NonZeroUsize;
        use std::task::Context;
        use std::time::Duration;
        use std::time::Instant;

        const PLAIN: usize = PAGE_BYTES as usize;
        /// Pooled ciphertext charge for one full page: its tag rounds up a granule.
        const CIPHER: usize = PLAIN + page_alloc::buf::GRANULE;

        fn admission(pages: usize, dirty: usize) -> Rc<flow_control::Quotas<AdmissionPolicy>> {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.plaintext_bytes = NonZeroUsize::new(pages * PLAIN).unwrap();
            limits.ciphertext_bytes = NonZeroUsize::new(pages * CIPHER).unwrap();
            limits.dirty_bytes = NonZeroUsize::new(dirty).unwrap();
            limits.metadata_entries = NonZeroUsize::new(16).unwrap();
            Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)))
        }

        fn descriptor(cache: &CacheId, version: &str, length: usize) -> VersionMetadata {
            VersionMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: ObjectId {
                        cache: cache.clone(),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value(version),
                },
                length: length as u64,
            }
        }

        fn page_id(metadata: &VersionMetadata) -> PageId {
            PageId {
                version: metadata.version.clone(),
                number: PageNumber(0),
            }
        }

        fn allocated_page(
            admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
            cache: &CacheId,
            version: &str,
            length: usize,
        ) -> PageResult {
            let metadata = descriptor(cache, version, length);
            let id = page_id(&metadata);
            let reserved = crate::admission::reserve_fill(admission, cache, false).unwrap();
            let pool = BufferPool::new(admission.clone());
            let mut plaintext = pool.plaintext(reserved.plaintext, length).unwrap();
            plaintext.bytes_mut().unwrap().fill(7);
            // Structural fixture only: production allocation and owner layout, not an
            // authentication oracle (security/aead.rs performs this transfer).
            let plaintext = VerifiedPage {
                inner: Arc::new(VerifiedBytes {
                    page: id.clone(),
                    storage: plaintext.into_buf(),
                }),
            };
            let ciphertext = pool
                .ciphertext(
                    reserved.ciphertext,
                    PageEnvelope {
                        page_size: racer_wire::object::PageSize::DEFAULT,
                        cipher: crate::model::PageCipher::Xaes256Gcm,
                        page: id,
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        nonce: Nonce([2; 24]),
                        plaintext_length: length as u32,
                        ciphertext_length: (length + 16) as u32,
                    },
                    vec![9; length + 16],
                )
                .unwrap();
            PageResult {
                metadata: metadata.for_pin(),
                plaintext,
                ciphertext,
            }
        }

        fn occupancy(admission: &flow_control::Quotas<AdmissionPolicy>) -> [usize; 3] {
            [
                ResourceClass::Plaintext,
                ResourceClass::Ciphertext,
                ResourceClass::DirtyCiphertext,
            ]
            .map(|class| admission.used(class))
        }

        /// Duplicate readers charge once; the cache retains ciphertext and holds
        /// plaintext weakly, so a plaintext-only reader does not pin cached
        /// ciphertext; lookup removal is not a completion fence.
        #[test]
        fn duplicate_owners_charge_once_until_each_last_owner() {
            let env = racer_io::Env::host();
            let real = admission(2, CIPHER);
            let cache = CacheId(crate::test_support::security::CACHE.into());
            let memory = MemoryCache::new(
                BufferPool::new(real.clone()),
                crate::test_support::availability(&env),
            );
            let page = allocated_page(&real, &cache, "v1", PLAIN);
            let id = page.plaintext.page().clone();
            let delivered = page.plaintext.clone();
            memory.publish(page).unwrap();
            assert_eq!(occupancy(&real), [PLAIN, CIPHER, 0], "publish");

            let read = memory.get(&id).unwrap().unwrap();
            let again = memory.get(&id).unwrap().unwrap();
            assert!(Arc::ptr_eq(&read.plaintext.inner, &again.plaintext.inner));
            assert!(Arc::ptr_eq(&read.ciphertext.inner, &again.ciphertext.inner));
            drop(again);
            assert_eq!(occupancy(&real), [PLAIN, CIPHER, 0], "duplicate reader");
            assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
            drop(read.plaintext);
            assert_eq!(memory.evict_idle(usize::MAX), Ok(0), "ciphertext reader");
            drop(read.ciphertext);
            let plain = memory.get(&id).unwrap().unwrap().plaintext;
            assert!(Arc::ptr_eq(&plain.inner, &delivered.inner));
            // A plaintext reader does not pin the cached ciphertext.
            assert_eq!(memory.evict_idle(usize::MAX), Ok(CIPHER));
            assert_eq!(occupancy(&real), [PLAIN, 0, 0], "plaintext-only reader");
            drop(delivered);
            memory.remove_cache(&cache).unwrap();
            assert!(memory.get(&id).unwrap().is_none());
            assert_eq!(occupancy(&real), [PLAIN, 0, 0], "lookup removal");
            assert_eq!(plain.bytes().len(), PLAIN);
            assert_eq!(plain.bytes()[PLAIN - 1], 7);
            drop(plain);
            assert_eq!(occupancy(&real), [0, 0, 0], "last owner released");
        }

        /// Cache A over its fair share cannot reserve by evicting cache B: the local
        /// deficit wins over global room, busy A pages survive, and once idle only
        /// A's oldest ciphertext is reclaimed. Plaintext pressure cannot be relieved
        /// by the cache, which holds plaintext weakly.
        #[test]
        fn fair_reclamation_keeps_other_cache() {
            let env = racer_io::Env::host();
            for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
                let real = admission(4, CIPHER);
                let a = CacheId(crate::test_support::security::CACHE.into());
                let b = CacheId("44444444-4444-4444-8444-444444444444".into());
                let memory = MemoryCache::new(
                    BufferPool::new(real.clone()),
                    crate::test_support::availability_for(&env, vec![a.clone(), b.clone()]),
                );
                let mut ids = Vec::new();
                for (cache, version) in [(&b, "other"), (&a, "old"), (&a, "new")] {
                    let page = allocated_page(&real, cache, version, PLAIN);
                    ids.push(page.plaintext.page().clone());
                    memory.publish(page).unwrap();
                }
                let cipher = matches!(class, ResourceClass::Ciphertext);
                assert_eq!(occupancy(&real), [0, 3 * CIPHER, 0]);
                let amount = if cipher { CIPHER } else { PLAIN };
                if cipher {
                    assert!(matches!(
                        real.reserve(Some(&a), class, amount),
                        Err(flow_control::Error::Overloaded)
                    ));
                    let (owner, deficit) = real.reclamation(&a, class, amount).unwrap();
                    assert_eq!(owner, Some(a.clone()));
                    assert!(deficit > 0 && deficit <= amount);
                }
                // Readers of A's pages hold ciphertext and pin them.
                let reads: Vec<_> = ids[1..]
                    .iter()
                    .map(|id| memory.ciphertext(id).unwrap().unwrap())
                    .collect();
                assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_, _| 0), 0);
                assert_eq!(occupancy(&real), [0, 3 * CIPHER, 0], "busy pages survive");
                drop(reads);
                let retained = if cipher {
                    assert_eq!(
                        memory.reclaim_idle(class, Some(&a), amount, |_, _| 0),
                        amount
                    );
                    assert!(memory.ciphertext(&ids[1]).unwrap().is_none());
                    2
                } else {
                    assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_, _| 0), 0);
                    assert!(memory.ciphertext(&ids[1]).unwrap().is_some());
                    3
                };
                assert!(memory.ciphertext(&ids[0]).unwrap().is_some(), "other cache");
                assert!(memory.ciphertext(&ids[2]).unwrap().is_some());
                assert_eq!(occupancy(&real), [0, retained * CIPHER, 0]);
                let next = real.reserve(Some(&a), class, amount).unwrap();
                drop(next);
                assert_eq!(memory.evict_idle(usize::MAX), Ok(retained * CIPHER));
                assert_eq!(occupancy(&real), [0, 0, 0], "drained");
            }
        }

        fn scope() -> RequestScope {
            RequestScope::host(
                RequestId([1; 16]),
                Instant::now() + Duration::from_secs(600),
            )
            .unwrap()
        }

        /// A canceled caller is not a completion fence: page charges stay held after
        /// the caller drops its work and until the crypto completion is reaped, even
        /// after admission stops.
        #[test]
        fn canceled_crypto_holds_owners_until_completion_reap() {
            let env = racer_io::Env::host();
            let real = admission(1, CIPHER);
            let cache = CacheId(crate::test_support::security::CACHE.into());
            let reserved = crate::admission::reserve_fill(&real, &cache, false).unwrap();
            let buffers = BufferPool::new(real.clone());
            let plaintext = buffers.plaintext(reserved.plaintext, 3).unwrap();
            let (port, engine_port) = security::pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
            let client = Rc::new(CryptoClient::new(port));
            let crypto = PageCrypto::new(
                Rc::new(crate::test_support::security::keys(&env)),
                client.clone(),
                buffers.clone(),
            );
            let mut engine = PageCryptoEngine::new(CryptoRuntime {
                port: engine_port,
                ctx: racer_io::WorkerCtx::host(),
            });
            let scope = scope();
            let mut work = crypto.encrypt(
                page_id(&descriptor(&cache, "v1", 3)),
                racer_wire::object::PageSize::DEFAULT,
                plaintext,
                reserved.ciphertext,
                &scope,
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(work.as_mut().poll(&mut cx).is_pending());
            assert_eq!(client.outstanding(), 1);
            assert_eq!(occupancy(&real), [4096, PLAIN + 16, 0], "accepted crypto");
            scope.cancel().unwrap();
            assert!(work.as_mut().poll(&mut cx).is_pending());
            drop(work);
            real.stop();
            client.poll_budgeted(1).unwrap();
            assert_eq!(occupancy(&real), [4096, PLAIN + 16, 0], "canceled caller");
            assert_eq!(client.outstanding(), 1);
            engine.poll_budgeted(&mut cx, 1).unwrap();
            assert_eq!(
                occupancy(&real),
                [4096, PLAIN + 16, 0],
                "published, not reaped"
            );
            assert_eq!(client.outstanding(), 1);
            client.poll_budgeted(1).unwrap();
            assert_eq!(client.outstanding(), 0);
            assert_eq!(occupancy(&real), [0, 0, 0], "reaped");
            // Reaped plaintext returns to the worker pool as idle Pool bytes.
            buffers.reclaim_idle_buffers();
            assert!(
                ResourceClass::ALL
                    .iter()
                    .all(|class| real.used(*class) == 0)
            );
        }
    }
    mod cache {
        use super::*;
        use crate::model::ExpiresAt;

        #[test]
        #[ignore = "deterministic cache refill trace experiment"]
        fn reclaim_trace_experiment() {
            use sha2::{Digest, Sha256};
            let env = racer_io::Env::host();
            const WARMUP: usize = 2048;
            const MEASURE: usize = 20000;
            let cache_id = CacheId(crate::test_support::security::CACHE.into());
            let mut cdf = Vec::new();
            let mut total = 0.0;
            for rank in 1..=512 {
                total += (rank as f64).powf(-1.2);
                cdf.push(total);
            }
            for value in &mut cdf {
                *value /= total;
            }
            cdf[511] = 1.0;
            for workload in ["hot16_churn", "zipf512_1.2", "sequential512"] {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.plaintext_bytes = std::num::NonZeroUsize::new(51 * 8).unwrap();
                limits.ciphertext_bytes = std::num::NonZeroUsize::new(51 * 24).unwrap();
                limits.metadata_entries = std::num::NonZeroUsize::new(512).unwrap();
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
                let pool = BufferPool::new(admission.clone());
                let cache = MemoryCache::new(pool.clone(), crate::test_support::availability(&env));
                let mut random = 0x6a09_e667_f3bc_c909u64;
                let mut hash = Sha256::new();
                let mut hits = 0;
                let mut refills = 0;
                let mut warmup_refills = 0;
                let mut reclaims = 0;
                for request in 0..WARMUP + MEASURE {
                    let rank = match workload {
                        "hot16_churn" if request % 5 == 4 => 16 + (request / 5) % 496,
                        "hot16_churn" => (request / 5 * 4 + request % 5) % 16,
                        "zipf512_1.2" => {
                            random ^= random << 13;
                            random ^= random >> 7;
                            random ^= random << 17;
                            let draw = (random >> 11) as f64 / (1u64 << 53) as f64;
                            cdf.partition_point(|value| *value <= draw)
                        }
                        _ => request % 512,
                    };
                    hash.update((rank as u64).to_le_bytes());
                    let mut key = [0; 32];
                    key[..8].copy_from_slice(&(rank as u64).to_le_bytes());
                    let metadata = VersionMetadata {
                        content_type: None,
                        version: ObjectVersion {
                            object: crate::model::ObjectId {
                                cache: cache_id.clone(),
                                key: crate::model::CacheKey(key),
                            },
                            etag: crate::model::StrongEtag::test_value("trace-v1"),
                        },
                        length: 8,
                    };
                    let page = PageId {
                        version: metadata.version.clone(),
                        number: PageNumber(0),
                    };
                    if let Some(found) = cache.ciphertext(&page).unwrap() {
                        drop(found);
                        hits += usize::from(request >= WARMUP);
                    } else {
                        refills += usize::from(request >= WARMUP);
                        warmup_refills += usize::from(request < WARMUP);
                        let mut reserve =
                            |class, amount| match admission.reserve(Some(&cache_id), class, amount)
                            {
                                Ok(charge) => charge,
                                Err(flow_control::Error::Overloaded) => {
                                    assert!(
                                        cache.reclaim_idle(
                                            class,
                                            Some(&cache_id),
                                            amount,
                                            |_, _| 0
                                        ) >= amount
                                    );
                                    reclaims += usize::from(request >= WARMUP);
                                    admission.reserve(Some(&cache_id), class, amount).unwrap()
                                }
                                Err(error) => panic!("unexpected admission error: {error:?}"),
                            };
                        let cipher_charge = reserve(ResourceClass::Ciphertext, 24);
                        let plain_charge =
                            reserve(ResourceClass::Plaintext, page_alloc::buf::GRANULE);
                        let plaintext = VerifiedPage {
                            inner: Arc::new(VerifiedBytes {
                                page: page.clone(),
                                storage: crate::memory::fixture_storage(
                                    plain_charge,
                                    &(rank as u64).to_le_bytes(),
                                ),
                            }),
                        };
                        let ciphertext = pool
                            .ciphertext(
                                cipher_charge,
                                PageEnvelope {
                                    page_size: racer_wire::object::PageSize::DEFAULT,
                                    cipher: crate::model::PageCipher::Xaes256Gcm,
                                    page,
                                    key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                                    nonce: Nonce([2; 24]),
                                    plaintext_length: 8,
                                    ciphertext_length: 24,
                                },
                                vec![2; 24],
                            )
                            .unwrap();
                        cache
                            .publish(PageResult {
                                metadata: metadata.for_pin(),
                                plaintext,
                                ciphertext,
                            })
                            .unwrap();
                    }
                    let entries = cache.entries.borrow();
                    assert!(entries.len() <= 51);
                    // Plaintext is released once the publisher drops it.
                    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                    assert_eq!(
                        admission.used(ResourceClass::Ciphertext),
                        entries.len() * 24
                    );
                    assert!(admission.used(ResourceClass::Ciphertext) <= 1224);
                }
                assert_eq!(hits + refills, MEASURE);
                cache.evict_idle(usize::MAX).unwrap();
                assert!(cache.entries.borrow().pages.is_empty());
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                println!(
                    "reclaim_trace workload={workload} sha256={:x} warmup={WARMUP} measured={MEASURE} warmup_refills={warmup_refills} hits={hits} refills={refills} reclaims={reclaims} cleanup_plain=0 cleanup_cipher=0",
                    hash.finalize()
                );
            }
        }

        mod partial_reclaim {
            use super::*;

            fn publish(
                cache: &MemoryCache,
                admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
                name: &str,
            ) -> PageId {
                let page = bundle(admission, name);
                let id = page.plaintext.page().clone();
                cache.publish(page).unwrap();
                id
            }

            fn publish_ciphertext(
                cache: &MemoryCache,
                admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
                name: &str,
            ) -> PageId {
                let page = bundle(admission, name);
                let id = page.plaintext.page().clone();
                cache
                    .publish_ciphertext(UnverifiedPage {
                        copy: page.copy(),
                        disk_token: None,
                    })
                    .unwrap();
                id
            }

            /// Global reclaim with per-cache lanes and weighted over-share.
            mod pressure {
                use super::*;
                use std::cell::Cell;

                /// Pooled charge for one 19-byte fixture ciphertext.
                const CIPHER: usize = page_alloc::buf::GRANULE;

                /// Published cache IDs are UUIDs; derive a stable one per name.
                fn id(cache: &str) -> CacheId {
                    let hash = cache.bytes().fold(0x811c_9dc5_u32, |h, b| {
                        (h ^ u32::from(b)).wrapping_mul(0x0100_0193)
                    });
                    CacheId(format!("{hash:08x}-4444-4444-8444-444444444444"))
                }

                fn memory(env: &racer_io::Env, caches: &[&str], entries: usize) -> MemoryCache {
                    MemoryCache::new(
                        BufferPool::new(admission(entries)),
                        crate::test_support::availability_for(
                            env,
                            caches.iter().map(|cache| id(cache)).collect(),
                        ),
                    )
                }

                fn page_in(cache: &MemoryCache, owner: &str, name: &str) -> PageResult {
                    use crate::model::CacheKey;
                    use crate::model::ObjectId;
                    use crate::model::StrongEtag;
                    bundle_for(
                        &cache.pool.admission,
                        VersionMetadata {
                            content_type: None,
                            version: ObjectVersion {
                                object: ObjectId {
                                    cache: id(owner),
                                    key: CacheKey([0; 32]),
                                },
                                etag: StrongEtag::test_value(name),
                            },
                            length: 3,
                        },
                    )
                }

                /// Publish a verified entry; the plaintext charge leaves with the bundle.
                fn verified_in(cache: &MemoryCache, owner: &str, name: &str) -> PageId {
                    let page = page_in(cache, owner, name);
                    let id = page.plaintext.page().clone();
                    cache.publish(page).unwrap();
                    id
                }

                fn unverified_in(cache: &MemoryCache, owner: &str, name: &str) -> PageId {
                    let page = page_in(cache, owner, name);
                    let id = page.plaintext.page().clone();
                    cache
                        .publish_ciphertext(UnverifiedPage {
                            copy: page.copy(),
                            disk_token: None,
                        })
                        .unwrap();
                    id
                }

                fn resident(cache: &MemoryCache, owner: &str) -> usize {
                    let owner = id(owner);
                    let count = |lanes: &racer_core::cache::Lanes<CacheId, PageId>| {
                        lanes.0.get(&owner).map_or(0, |l| l.order.len())
                    };
                    count(&cache.entries.borrow().lanes)
                        + count(&cache.ciphertext_entries.borrow().lanes)
                }

                /// Every lane entry matches the global index, and every entry
                /// has exactly one lane slot under its own cache.
                fn assert_lanes(cache: &MemoryCache) {
                    let entries = cache.entries.borrow();
                    let mut lane_entries = 0;
                    for (owner, lane) in &entries.lanes.0 {
                        assert!(!lane.order.is_empty(), "empty lane {owner:?} kept");
                        for (tick, page) in &lane.order {
                            assert_eq!(&page.version.object.cache, owner);
                            assert_eq!(entries.pages[page].0, *tick);
                            assert_eq!(entries.lru.lru[tick], *page);
                            lane_entries += 1;
                        }
                    }
                    assert_eq!(lane_entries, entries.pages.len());
                    assert_eq!(entries.lru.lru.len(), entries.pages.len());
                    let ciphertext = cache.ciphertext_entries.borrow();
                    let mut lane_entries = 0;
                    for (owner, lane) in &ciphertext.lanes.0 {
                        assert!(!lane.order.is_empty(), "empty lane {owner:?} kept");
                        for (tick, page) in &lane.order {
                            assert_eq!(&page.version.object.cache, owner);
                            assert_eq!(ciphertext.ticks[page], *tick);
                            assert_eq!(ciphertext.order[tick], *page);
                            lane_entries += 1;
                        }
                    }
                    assert_eq!(lane_entries, ciphertext.len());
                    assert_eq!(ciphertext.ticks.len(), ciphertext.len());
                }

                /// Weighted model of `Quotas::excess` over retained entries.
                /// Shares divide `limit` pages by weight, as `flow::share` does.
                struct Shares<'a> {
                    cache: &'a MemoryCache,
                    limit: usize,
                    weights: Cell<[(u64, u64); 1]>,
                    calls: Cell<usize>,
                }
                impl Shares<'_> {
                    fn excess(&self, owner: &CacheId) -> usize {
                        self.calls.set(self.calls.get() + 1);
                        let [(low, high)] = self.weights.get();
                        let name = if *owner == id("low") { "low" } else { "high" };
                        let weight = if name == "low" { low } else { high };
                        let share = self.limit * weight as usize / (low + high) as usize;
                        resident(self.cache, name).saturating_sub(share) * CIPHER
                    }
                }

                #[test]
                fn global_reclaim_prefers_over_share_cache_after_live_reweight() {
                    let env = racer_io::Env::host();
                    let cache = memory(&env, &["low", "high"], 1000);
                    let admission = cache.pool.admission.clone();
                    // High pages are oldest, so an unweighted pass picks them first.
                    let high: Vec<_> = (0..20)
                        .map(|i| verified_in(&cache, "high", &format!("h{i}")))
                        .collect();
                    let low: Vec<_> = (0..80)
                        .map(|i| verified_in(&cache, "low", &format!("l{i}")))
                        .collect();
                    assert_lanes(&cache);
                    // Before the reweigh, low 900 and high 100: high is over share.
                    let shares = Shares {
                        cache: &cache,
                        limit: 100,
                        weights: Cell::new([(900, 100)]),
                        calls: Cell::new(0),
                    };
                    let reclaim = || {
                        cache.reclaim(
                            ResourceClass::Ciphertext,
                            None,
                            CIPHER,
                            |owner| shares.excess(owner),
                            |_, _| 0,
                            |_, _| 0,
                        )
                    };
                    assert_eq!(reclaim(), CIPHER);
                    assert_eq!(
                        (resident(&cache, "low"), resident(&cache, "high")),
                        (80, 19)
                    );
                    // Live reweigh to low 100, high 900 with residency 80:19.
                    shares.weights.set([(100, 900)]);
                    for round in 0..10 {
                        // Touch every low page before each high miss.
                        for page in &low {
                            let _ = cache.ciphertext(page).unwrap();
                        }
                        let used = admission.used(ResourceClass::Ciphertext);
                        assert_eq!(reclaim(), CIPHER, "round {round}");
                        // Released bytes are true quota charges.
                        assert_eq!(admission.used(ResourceClass::Ciphertext), used - CIPHER);
                        assert_eq!(resident(&cache, "high"), 19, "round {round}");
                        assert_eq!(resident(&cache, "low"), 79 - round);
                    }
                    for page in &high[1..] {
                        assert!(cache.ciphertext(page).unwrap().is_some());
                    }
                    assert_lanes(&cache);
                    // The window holds two caches, so each reclaim asks at most twice.
                    assert!(shares.calls.get() <= 2 * 11);
                }

                #[test]
                fn busy_over_share_pages_survive_and_fallback_releases_idle_bytes() {
                    let env = racer_io::Env::host();
                    let cache = memory(&env, &["low", "high"], 100);
                    let high = verified_in(&cache, "high", "h");
                    let low: Vec<_> = (0..3)
                        .map(|i| verified_in(&cache, "low", &format!("l{i}")))
                        .collect();
                    let unverified = unverified_in(&cache, "low", "u");
                    // Readers hold every low page.
                    let mut readers: Vec<_> = low
                        .iter()
                        .chain([&unverified])
                        .map(|page| cache.ciphertext(page).unwrap().unwrap())
                        .collect();
                    let writer_calls = RefCell::new(Vec::new());
                    let queued = Cell::new(0);
                    let reclaim = |bytes| {
                        cache.reclaim(
                            ResourceClass::Ciphertext,
                            None,
                            bytes,
                            |owner| usize::from(*owner == id("low")) * 4 * CIPHER,
                            |_, _| {
                                queued.set(queued.get() + 1);
                                0
                            },
                            |owner, bytes| {
                                writer_calls.borrow_mut().push((owner.cloned(), bytes));
                                0
                            },
                        )
                    };
                    // Over-share bytes are all busy: writer copies are asked
                    // for, then the unweighted fallback takes the idle page.
                    assert_eq!(reclaim(CIPHER), CIPHER);
                    assert!(cache.ciphertext(&high).unwrap().is_none());
                    assert_eq!(writer_calls.take(), vec![(Some(id("low")), CIPHER)]);
                    assert_eq!(resident(&cache, "low"), 4);
                    // Nothing is idle: one bounded call returns zero and asks
                    // the writer once per scope, without spinning.
                    queued.set(0);
                    assert_eq!(reclaim(CIPHER), 0);
                    assert_eq!(
                        writer_calls.take(),
                        vec![(Some(id("low")), CIPHER), (None, CIPHER)]
                    );
                    assert_eq!(resident(&cache, "low"), 4);
                    // One lane scan plus one fallback scan of the low pages.
                    assert_eq!(queued.get(), 2 * 3);
                    // Released readers make over-share bytes reclaimable.
                    readers.clear();
                    assert_eq!(reclaim(2 * CIPHER), 2 * CIPHER);
                    assert_eq!(resident(&cache, "low"), 2);
                    assert!(writer_calls.take().is_empty());
                    assert_lanes(&cache);
                }

                #[test]
                fn reclaim_credits_queued_and_writer_bytes() {
                    let env = racer_io::Env::host();
                    let cache = memory(&env, &["low", "high"], 100);
                    let low = verified_in(&cache, "low", "l");
                    // A queued writer shares the page, so it is not idle.
                    let copy = cache.ciphertext(&low).unwrap().unwrap();
                    let writer = RefCell::new(Vec::new());
                    let released = cache.reclaim(
                        ResourceClass::Ciphertext,
                        None,
                        2 * CIPHER,
                        |_| 4 * CIPHER,
                        |page, _| {
                            assert_eq!(page, &low);
                            5
                        },
                        |owner, bytes| {
                            writer.borrow_mut().push((owner.cloned(), bytes));
                            7
                        },
                    );
                    // Queued staging 5 and writer 7 count for the over-share
                    // cache, then again for the unweighted fallback.
                    assert_eq!(released, 2 * (5 + 7));
                    assert_eq!(
                        writer.take(),
                        vec![
                            (Some(id("low")), 2 * CIPHER - 5),
                            (None, 2 * CIPHER - 2 * 5 - 7),
                        ]
                    );
                    drop(copy);
                    // A keyed owner deficit scans only the owner and its writer.
                    let high = verified_in(&cache, "high", "h");
                    let released = cache.reclaim(
                        ResourceClass::Ciphertext,
                        Some(&id("low")),
                        CIPHER,
                        |_| panic!("keyed reclaim never weighs other caches"),
                        |_, _| 0,
                        |_, _| panic!("idle owner bytes suffice"),
                    );
                    assert_eq!(released, CIPHER);
                    assert!(cache.ciphertext(&high).unwrap().is_some());
                    assert_lanes(&cache);
                }

                #[test]
                fn plaintext_pressure_releases_nothing() {
                    let env = racer_io::Env::host();
                    let cache = memory(&env, &["low"], 100);
                    verified_in(&cache, "low", "l");
                    unverified_in(&cache, "low", "u");
                    for owner in [None, Some(&id("low"))] {
                        assert_eq!(
                            cache.reclaim(
                                ResourceClass::Plaintext,
                                owner,
                                usize::MAX,
                                |_| panic!("plaintext is never retained"),
                                |_, _| panic!("plaintext is never retained"),
                                |_, _| panic!("plaintext is never retained"),
                            ),
                            0
                        );
                    }
                    assert_eq!(resident(&cache, "low"), 2);
                    assert_eq!(cache.pool.admission.used(ResourceClass::Plaintext), 0);
                }

                #[test]
                fn lanes_track_insert_touch_remove_and_cache_removal() {
                    let env = racer_io::Env::host();
                    let cache = memory(&env, &["a", "b", "c"], 100);
                    let a = verified_in(&cache, "a", "a0");
                    let b = unverified_in(&cache, "b", "b0");
                    verified_in(&cache, "c", "c0");
                    unverified_in(&cache, "a", "a1");
                    assert_lanes(&cache);
                    // Touches move lane slots with the global index.
                    let _ = cache.ciphertext(&a).unwrap();
                    let _ = cache.ciphertext(&b).unwrap();
                    assert_lanes(&cache);
                    // Upgrading unverified ciphertext moves it between indexes.
                    cache.publish(page_in(&cache, "b", "b0")).unwrap();
                    assert_lanes(&cache);
                    assert_eq!(cache.evict_idle(CIPHER).unwrap(), CIPHER);
                    assert_lanes(&cache);
                    cache.remove_cache(&id("a")).unwrap();
                    assert_lanes(&cache);
                    assert_eq!(resident(&cache, "a"), 0);
                    assert!(!cache.entries.borrow().lanes.0.contains_key(&id("a")));
                    assert!(cache.evict_idle(usize::MAX).unwrap() > 0);
                    assert_lanes(&cache);
                    assert!(cache.entries.borrow().lanes.0.is_empty());
                    assert!(cache.ciphertext_entries.borrow().lanes.0.is_empty());
                }

                #[test]
                fn window_rotation_and_lane_scans_are_bounded() {
                    let env = racer_io::Env::host();
                    const CACHES: usize = 1000;
                    let names: Vec<_> = (0..CACHES).map(|i| format!("c{i:04}")).collect();
                    let mut all: Vec<&str> = names.iter().map(String::as_str).collect();
                    all.push("target");
                    let cache = memory(&env, &all, 4 * CACHES);
                    // Older busy entries of other caches fill a whole scan window.
                    let busy: Vec<_> = (0..racer_core::cache::RECLAIM_WINDOW + 44)
                        .map(|i| {
                            let page = verified_in(&cache, &names[i % 4], &format!("busy{i}"));
                            cache.ciphertext(&page).unwrap().unwrap()
                        })
                        .collect();
                    // One held entry per cache keeps every lane alive.
                    let held: Vec<_> = names
                        .iter()
                        .map(|name| {
                            let page = unverified_in(&cache, name, "held");
                            cache.ciphertext(&page).unwrap().unwrap()
                        })
                        .collect();
                    verified_in(&cache, "target", "t");
                    // The keyed global-window scan cannot see the newest page.
                    assert_eq!(
                        cache.reclaim_idle(
                            ResourceClass::Ciphertext,
                            Some(&id("target")),
                            CIPHER,
                            |_, _| 0
                        ),
                        0
                    );
                    // Its lane scan finds it at once.
                    assert_eq!(
                        cache.reclaim(
                            ResourceClass::Ciphertext,
                            Some(&id("target")),
                            CIPHER,
                            |_| 0,
                            |_, _| 0,
                            |_, _| 0
                        ),
                        CIPHER
                    );
                    // Each global reclaim weighs at most racer_core::cache::PRESSURE_WINDOW caches,
                    // and rotation reaches every cache within ceil(C / W) calls.
                    let seen = RefCell::new(std::collections::BTreeSet::new());
                    let rounds = CACHES.div_ceil(racer_core::cache::PRESSURE_WINDOW);
                    for _ in 0..rounds {
                        let calls = Cell::new(0);
                        let queued = Cell::new(0);
                        cache.reclaim(
                            ResourceClass::Ciphertext,
                            None,
                            usize::MAX,
                            |owner| {
                                calls.set(calls.get() + 1);
                                seen.borrow_mut().insert(owner.clone());
                                0
                            },
                            |_, _| {
                                queued.set(queued.get() + 1);
                                0
                            },
                            |_, _| 0,
                        );
                        assert!(calls.get() <= racer_core::cache::PRESSURE_WINDOW);
                        // Only the unweighted fallback examines verified entries.
                        assert!(queued.get() <= racer_core::cache::RECLAIM_WINDOW);
                    }
                    assert_eq!(seen.borrow().len(), CACHES);
                    assert_lanes(&cache);
                    drop((busy, held));
                }

                #[test]
                fn early_success_rotation_reaches_every_cache() {
                    let env = racer_io::Env::host();
                    // A multiple of the window, so a cursor that jumped to the
                    // last prefetched cache would revisit only every eighth.
                    const CACHES: usize = 2 * racer_core::cache::PRESSURE_WINDOW;
                    let names: Vec<_> = (0..CACHES).map(|i| format!("r{i:02}")).collect();
                    let all: Vec<&str> = names.iter().map(String::as_str).collect();
                    let cache = memory(&env, &all, 8 * CACHES);
                    // Each cache keeps a held entry, so its lane survives, and
                    // two idle entries, so every call can succeed at once.
                    let held: Vec<_> = names
                        .iter()
                        .map(|name| {
                            let page = unverified_in(&cache, name, "held");
                            for i in 0..2 {
                                unverified_in(&cache, name, &format!("idle{i}"));
                            }
                            cache.ciphertext(&page).unwrap().unwrap()
                        })
                        .collect();
                    for round in 0..2 {
                        let examined = RefCell::new(Vec::new());
                        for _ in 0..CACHES {
                            let released = cache.reclaim(
                                ResourceClass::Ciphertext,
                                None,
                                CIPHER,
                                |owner| {
                                    examined.borrow_mut().push(owner.clone());
                                    CIPHER
                                },
                                |_, _| panic!("unverified entries only"),
                                |_, _| panic!("idle bytes suffice"),
                            );
                            assert_eq!(released, CIPHER);
                        }
                        // One cache per call, each exactly once per round.
                        let mut examined = examined.take();
                        assert_eq!(examined.len(), CACHES);
                        examined.sort();
                        examined.dedup();
                        assert_eq!(examined.len(), CACHES, "round {round}");
                        for name in &names {
                            assert_eq!(resident(&cache, name), 2 - round, "round {round}");
                        }
                    }
                    assert_lanes(&cache);
                    drop(held);
                }

                /// Real `AdmissionPolicy` weights, `reclamation`, and `excess`
                /// drive `reclaim`. Extra charges with no payload stand in for
                /// other live or writer-owned bytes, so no 16 MiB pages are needed.
                #[test]
                fn real_quotas_choose_over_share_cache_after_live_reweight() {
                    let env = racer_io::Env::host();
                    let page = PAGE_BYTES as usize;
                    let class = ResourceClass::Ciphertext;
                    let mut limits = crate::test_support::cluster::config(false).limits;
                    limits.ciphertext_bytes = std::num::NonZeroUsize::new(100 * page).unwrap();
                    limits.metadata_entries = std::num::NonZeroUsize::new(1000).unwrap();
                    let admission =
                        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
                    let (low, high) = (id("low"), id("high"));
                    let definition =
                        |id: &CacheId, priority| racer_wire::control::CacheDefinition {
                            id: id.clone(),
                            name: id.0.clone(),
                            client_socket: "/c".into(),
                            origin_socket: "/o".into(),
                            priority,
                            mode: racer_wire::control::CacheMode::Cached,
                            origin: None,
                        };
                    let weigh = |low_priority, high_priority| {
                        admission.policy().weights().update(&[
                            definition(&low, low_priority),
                            definition(&high, high_priority),
                        ]);
                        admission.reweigh();
                    };
                    weigh(900, 100);
                    let cache = MemoryCache::new(
                        BufferPool::new(admission.clone()),
                        crate::test_support::availability_for(
                            &env,
                            vec![low.clone(), high.clone()],
                        ),
                    );
                    // High activates first, alone, with 20 pages of accounted bytes.
                    let high_pages: Vec<_> = (0..4)
                        .map(|i| verified_in(&cache, "high", &format!("h{i}")))
                        .collect();
                    let _high_extra = admission
                        .reserve(Some(&high), class, 20 * page - 4 * CIPHER)
                        .unwrap();
                    // Low's 80 pages fit its 90-page share once it activates.
                    let low_pages: Vec<_> = (0..8)
                        .map(|i| verified_in(&cache, "low", &format!("l{i}")))
                        .collect();
                    let low_extra = admission
                        .reserve(Some(&low), class, 80 * page - 8 * CIPHER)
                        .unwrap();
                    assert_eq!(admission.used(class), 100 * page);
                    assert_eq!(admission.excess(&low, class), 0);
                    assert!(admission.excess(&high, class) > 0);
                    assert_eq!(admission.excess(&id("missing"), class), 0);
                    let reclaim = |requester: &CacheId| {
                        let (owner, deficit) =
                            admission.reclamation(requester, class, CIPHER).unwrap();
                        assert_eq!((owner, deficit), (None, CIPHER));
                        let used = admission.used(class);
                        let released = cache.reclaim(
                            class,
                            None,
                            deficit,
                            |cache| admission.excess(cache, class),
                            |_, _| 0,
                            |_, _| 0,
                        );
                        assert_eq!(released, CIPHER);
                        assert_eq!(admission.used(class), used - released);
                    };
                    // An under-share low request takes over-share high bytes.
                    reclaim(&low);
                    assert_eq!((resident(&cache, "low"), resident(&cache, "high")), (8, 3));
                    // Live reweigh: now low is far over share.
                    weigh(100, 900);
                    assert_eq!(admission.excess(&high, class), 0);
                    assert!(admission.excess(&low, class) > 0);
                    for i in 0..3 {
                        // Refill to full aggregate capacity with a tiny high page.
                        verified_in(&cache, "high", &format!("refill{i}"));
                        assert_eq!(admission.used(class), 100 * page);
                        for page in &low_pages {
                            let _ = cache.ciphertext(page).unwrap();
                        }
                        reclaim(&high);
                        assert_eq!(resident(&cache, "high"), 4 + i);
                        assert_eq!(resident(&cache, "low"), 7 - i);
                    }
                    assert!(cache.ciphertext(&high_pages[3]).unwrap().is_some());
                    // Retirement: once low's last charge leaves, it has no excess.
                    drop(low_extra);
                    cache.remove_cache(&low).unwrap();
                    assert_eq!(admission.excess(&low, class), 0);
                    assert_lanes(&cache);
                }
            }

            #[test]
            fn capacity_and_eviction_resume_after_examined_victim() {
                let env = racer_io::Env::host();
                for capacity in [false, true] {
                    let admission = admission(if capacity { 3 } else { 8 });
                    let cache = MemoryCache::new(
                        BufferPool::new(admission.clone()),
                        crate::test_support::availability(&env),
                    );
                    let a = publish(&cache, &admission, "a");
                    let b = publish(&cache, &admission, "b");
                    let c = publish(&cache, &admission, "c");
                    if !capacity {
                        assert_eq!(cache.evict_idle(1), Ok(page_alloc::buf::GRANULE));
                    }
                    let d = publish(&cache, &admission, "d");
                    assert!(!cache.entries.borrow().pages.contains_key(&a));
                    assert_eq!(cache.entries.borrow().reclaim_after, 1);
                    if capacity {
                        publish(&cache, &admission, "e");
                    } else {
                        assert_eq!(cache.evict_idle(1), Ok(page_alloc::buf::GRANULE));
                    }
                    let entries = cache.entries.borrow();
                    assert!(!entries.pages.contains_key(&b));
                    assert!(entries.pages.contains_key(&c));
                    assert!(entries.pages.contains_key(&d));
                    assert_eq!(entries.reclaim_after, 2);
                }
            }

            #[test]
            fn zero_targets_and_ciphertext_sufficiency_leave_paired_cursor_untouched() {
                let env = racer_io::Env::host();
                for evict in [false, true] {
                    let admission = admission(8);
                    let cache = MemoryCache::new(
                        BufferPool::new(admission.clone()),
                        crate::test_support::availability(&env),
                    );
                    let paired = publish(&cache, &admission, "paired");
                    let cipher = publish_ciphertext(&cache, &admission, "cipher");
                    assert_eq!(cache.evict_idle(0), Ok(0));
                    for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
                        assert_eq!(
                            cache.reclaim_idle(class, None, 0, |_, _| panic!(
                                "zero target callback"
                            )),
                            0
                        );
                    }
                    assert_eq!(cache.entries.borrow().reclaim_after, 0);
                    assert_eq!(*cache.ciphertext_cursor.borrow(), None);
                    if evict {
                        assert_eq!(cache.evict_idle(1), Ok(page_alloc::buf::GRANULE));
                    } else {
                        assert_eq!(
                            cache.reclaim_idle(ResourceClass::Ciphertext, None, 1, |_, _| panic!(
                                "target already met"
                            )),
                            page_alloc::buf::GRANULE
                        );
                    }
                    assert_eq!(cache.entries.borrow().reclaim_after, 0);
                    assert!(cache.entries.borrow().pages.contains_key(&paired));
                    assert_eq!(*cache.ciphertext_cursor.borrow(), Some(cipher));
                }
            }

            #[test]
            fn examined_filters_and_staging_callback_advance_cursor() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let a = publish(&cache, &admission, "a");
                let b = publish(&cache, &admission, "b");
                let held = cache.entries.borrow().pages[&a].1.ciphertext.clone();
                // The cache holds no plaintext, so plaintext pressure is a no-op.
                assert_eq!(
                    cache.reclaim_idle(ResourceClass::Plaintext, None, 1, |_, _| panic!(
                        "plaintext callback"
                    )),
                    0
                );
                assert_eq!(cache.entries.borrow().reclaim_after, 0);
                assert_eq!(
                    cache.reclaim_idle(ResourceClass::Ciphertext, None, 1, |_, _| 0),
                    page_alloc::buf::GRANULE
                );
                assert_eq!(cache.entries.borrow().reclaim_after, 2);
                assert!(cache.entries.borrow().pages.contains_key(&a));
                assert!(!cache.entries.borrow().pages.contains_key(&b));
                let c = publish(&cache, &admission, "c");
                let wrong = CacheId("wrong".into());
                assert_eq!(
                    cache.reclaim_idle(ResourceClass::Ciphertext, Some(&wrong), 1, |_, _| panic!(
                        "wrong cache callback"
                    )),
                    0
                );
                assert_eq!(cache.entries.borrow().reclaim_after, 1);
                let staging = admission
                    .reserve(Some(&c.version.object.cache), ResourceClass::Ciphertext, 1)
                    .unwrap();
                let mut staging = Some(staging);
                assert_eq!(
                    cache.reclaim_idle(ResourceClass::Ciphertext, None, 1, |page, _| {
                        assert_eq!(page, &c);
                        drop(staging.take().unwrap());
                        1
                    }),
                    1
                );
                assert_eq!(cache.entries.borrow().reclaim_after, 3);
                assert!(cache.entries.borrow().pages.contains_key(&c));
                drop(held);
            }

            /// Run `expire_idle` once per checkpoint interval up to `until`.
            fn expire_through(
                cache: &MemoryCache,
                start: std::time::Instant,
                until: std::time::Duration,
            ) -> usize {
                let mut released = 0;
                let mut at = std::time::Duration::ZERO;
                while at <= until {
                    released += cache.expire_idle(start + at);
                    at += racer_core::cache::EXPIRY_CHECKPOINT;
                }
                released
            }

            #[test]
            fn expire_idle_releases_untouched_entries_and_keeps_recent_hits() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let start = std::time::Instant::now();
                let cold = publish(&cache, &admission, "cold");
                let hot = publish(&cache, &admission, "hot");
                let cipher = publish_ciphertext(&cache, &admission, "cipher");
                let touched = publish_ciphertext(&cache, &admission, "touched");
                // Nothing is old enough yet.
                assert_eq!(
                    expire_through(&cache, start, racer_core::cache::IDLE_TTL / 2),
                    0
                );
                // Hits halfway through renew both classes.
                assert!(cache.ciphertext(&hot).unwrap().is_some());
                assert!(cache.unverified(&touched).unwrap().is_some());
                let half = start + racer_core::cache::IDLE_TTL / 2;
                assert_eq!(
                    expire_through(&cache, half, racer_core::cache::IDLE_TTL / 2),
                    2
                );
                assert!(!cache.entries.borrow().pages.contains_key(&cold));
                assert!(cache.entries.borrow().pages.contains_key(&hot));
                assert!(!cache.ciphertext_entries.borrow().contains_key(&cipher));
                assert!(cache.ciphertext_entries.borrow().contains_key(&touched));
                // Without further hits the renewed entries expire one TTL later.
                assert_eq!(
                    expire_through(
                        &cache,
                        start + racer_core::cache::IDLE_TTL,
                        racer_core::cache::IDLE_TTL
                    ),
                    2
                );
                assert_eq!(cache.entries.borrow().len(), 0);
                assert_eq!(cache.ciphertext_entries.borrow().len(), 0);
                let entries = cache.ciphertext_entries.borrow();
                assert!(entries.order.is_empty() && entries.ticks.is_empty());
                drop(entries);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            }

            #[test]
            fn pinned_readers_keep_entries_busy_and_unpinned_observers_do_not() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let start = std::time::Instant::now();
                let pinned = publish(&cache, &admission, "pinned");
                let observed = publish(&cache, &admission, "observed");
                let cipher = publish_ciphertext(&cache, &admission, "cipher");
                let page = |id: &PageId| cache.entries.borrow().pages[id].1.ciphertext.clone();
                let reader = page(&pinned);
                assert_eq!(reader.pins(), 2);
                let clone = reader.clone();
                assert_eq!(reader.pins(), 3);
                drop(clone);
                assert_eq!(reader.pins(), 2);
                let observer = page(&observed).unpinned();
                assert_eq!(observer.pins(), 1);
                let observer_clone = observer.clone();
                assert_eq!(observer.pins(), 1);
                drop(observer_clone);
                let cipher_observer = cache.ciphertext_entries.borrow()[&cipher]
                    .copy
                    .ciphertext
                    .unpinned();
                assert_eq!(
                    expire_through(
                        &cache,
                        start,
                        racer_core::cache::IDLE_TTL + racer_core::cache::EXPIRY_CHECKPOINT
                    ),
                    2
                );
                // The pinned reader keeps its entry; observers do not.
                assert!(cache.entries.borrow().pages.contains_key(&pinned));
                assert!(!cache.entries.borrow().pages.contains_key(&observed));
                assert!(!cache.ciphertext_entries.borrow().contains_key(&cipher));
                // Observers still keep the bytes readable after eviction.
                assert_eq!(observer.pins(), 0);
                assert!(!observer.bytes().is_empty());
                assert!(!cipher_observer.bytes().is_empty());
                drop((observer, cipher_observer));
                drop(reader);
                assert_eq!(page(&pinned).pins(), 2);
            }

            #[test]
            fn republishing_verified_ciphertext_does_not_pin_it() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let start = std::time::Instant::now();
                let page = publish(&cache, &admission, "page");
                // A ciphertext acquisition after the plaintext was released
                // returns the verified entry, which a fill republishes.
                let copy = cache.unverified(&page).unwrap().unwrap();
                cache.publish_ciphertext(copy.clone()).unwrap();
                assert!(cache.ciphertext_entries.borrow().is_empty());
                assert_eq!(cache.entries.borrow().len(), 1);
                // Still held externally, so not idle yet.
                assert_eq!(
                    expire_through(&cache, start, racer_core::cache::IDLE_TTL * 2),
                    0
                );
                drop(copy);
                let later =
                    start + racer_core::cache::IDLE_TTL * 2 + racer_core::cache::EXPIRY_CHECKPOINT;
                assert_eq!(cache.expire_idle(later), 1);
                assert_eq!(cache.entries.borrow().len(), 0);
                assert!(cache.ciphertext_entries.borrow().is_empty());
            }

            #[test]
            fn publish_ciphertext_still_caches_pages_without_verified_entry() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let verified = publish(&cache, &admission, "verified");
                let other = publish_ciphertext(&cache, &admission, "other");
                assert!(cache.entries.borrow().pages.contains_key(&verified));
                assert!(!cache.entries.borrow().pages.contains_key(&other));
                assert!(cache.ciphertext_entries.borrow().contains_key(&other));
            }

            #[test]
            fn expire_idle_skips_entries_in_use_until_released() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let start = std::time::Instant::now();
                let page = publish(&cache, &admission, "page");
                let cipher = publish_ciphertext(&cache, &admission, "cipher");
                let held = cache.entries.borrow().pages[&page].1.ciphertext.clone();
                let held_cipher = cache.ciphertext_entries.borrow()[&cipher]
                    .copy
                    .ciphertext
                    .clone();
                assert_eq!(
                    expire_through(&cache, start, racer_core::cache::IDLE_TTL * 2),
                    0
                );
                assert_eq!(cache.entries.borrow().len(), 1);
                assert_eq!(cache.ciphertext_entries.borrow().len(), 1);
                drop((held, held_cipher));
                // The next checkpoint pass revisits and releases them.
                let later =
                    start + racer_core::cache::IDLE_TTL * 2 + racer_core::cache::EXPIRY_CHECKPOINT;
                assert_eq!(cache.expire_idle(later), 2);
                assert_eq!(cache.entries.borrow().len(), 0);
                assert_eq!(cache.ciphertext_entries.borrow().len(), 0);
            }

            #[test]
            fn expire_idle_bounds_work_per_call() {
                let env = racer_io::Env::host();
                let count = racer_core::cache::EXPIRY_BATCH * 2 + 3;
                let admission = admission(count * 2 + 8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                for n in 0..count {
                    publish(&cache, &admission, &format!("p{n:03}"));
                    publish_ciphertext(&cache, &admission, &format!("c{n:03}"));
                }
                let start = std::time::Instant::now();
                assert_eq!(cache.expire_idle(start), 0);
                let expired = start + racer_core::cache::IDLE_TTL;
                assert_eq!(
                    cache.expire_idle(expired),
                    racer_core::cache::EXPIRY_BATCH * 2
                );
                assert_eq!(
                    cache.expire_idle(expired),
                    racer_core::cache::EXPIRY_BATCH * 2
                );
                assert_eq!(cache.expire_idle(expired), 6);
                // The pass is finished: further calls in this interval do no work.
                assert_eq!(cache.expire_idle(expired), 0);
                assert_eq!(cache.expiry.borrow().verified, None);
                assert_eq!(cache.expiry.borrow().ciphertext, None);
                assert_eq!(cache.entries.borrow().len(), 0);
                assert_eq!(cache.ciphertext_entries.borrow().len(), 0);
            }

            #[test]
            fn expire_idle_resumes_past_busy_prefix_across_checkpoints() {
                let env = racer_io::Env::host();
                let busy = racer_core::cache::EXPIRY_BATCH + 1;
                let idle = 3;
                let admission = admission((busy + idle) * 2 + 8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let mut held = Vec::new();
                for n in 0..busy {
                    let page = publish(&cache, &admission, &format!("p{n:03}"));
                    let cipher = publish_ciphertext(&cache, &admission, &format!("c{n:03}"));
                    held.push(cache.entries.borrow().pages[&page].1.ciphertext.clone());
                    held.push(
                        cache.ciphertext_entries.borrow()[&cipher]
                            .copy
                            .ciphertext
                            .clone(),
                    );
                }
                for n in busy..busy + idle {
                    publish(&cache, &admission, &format!("p{n:03}"));
                    publish_ciphertext(&cache, &admission, &format!("c{n:03}"));
                }
                let start = std::time::Instant::now();
                assert_eq!(cache.expire_idle(start), 0);
                // One call per checkpoint: each call starts a new interval.
                let mut released = 0;
                for step in 0..4u32 {
                    released += cache.expire_idle(
                        start
                            + racer_core::cache::IDLE_TTL
                            + racer_core::cache::EXPIRY_CHECKPOINT * step,
                    );
                }
                assert_eq!(released, idle * 2);
                assert_eq!(cache.entries.borrow().len(), busy);
                assert_eq!(cache.ciphertext_entries.borrow().len(), busy);
                drop(held);
            }

            #[test]
            fn expiry_checkpoints_stay_bounded() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let start = std::time::Instant::now();
                expire_through(&cache, start, racer_core::cache::IDLE_TTL * 10);
                // Calls within one interval do not add checkpoints.
                for _ in 0..10 {
                    cache.expire_idle(start + racer_core::cache::IDLE_TTL * 10);
                }
                let limit = (racer_core::cache::IDLE_TTL.as_secs()
                    / racer_core::cache::EXPIRY_CHECKPOINT.as_secs())
                    as usize
                    + 2;
                assert!(cache.expiry.borrow().checkpoints.len() <= limit);
            }

            #[test]
            fn ciphertext_partial_scan_preserves_unexamined_keys_after_insertion() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                let a = publish_ciphertext(&cache, &admission, "a");
                let b = publish_ciphertext(&cache, &admission, "b");
                let c = publish_ciphertext(&cache, &admission, "c");
                assert!(a < b && b < c);
                assert_eq!(cache.reclaim_ciphertext(None, 1), page_alloc::buf::GRANULE);
                assert_eq!(*cache.ciphertext_cursor.borrow(), Some(a));
                let d = publish_ciphertext(&cache, &admission, "d");
                assert!(c < d);
                assert_eq!(cache.reclaim_ciphertext(None, 1), page_alloc::buf::GRANULE);
                assert_eq!(*cache.ciphertext_cursor.borrow(), Some(b.clone()));
                let entries = cache.ciphertext_entries.borrow();
                assert!(!entries.contains_key(&b));
                assert!(entries.contains_key(&c));
                assert!(entries.contains_key(&d));
            }

            #[test]
            fn ciphertext_busy_prefix_and_wrong_cache_advance_bounded_scan() {
                let env = racer_io::Env::host();
                let admission = admission(300);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                for n in 0..257 {
                    publish_ciphertext(&cache, &admission, &format!("v{n:03}"));
                }
                let ids: Vec<_> = cache.ciphertext_entries.borrow().keys().cloned().collect();
                let held: Vec<_> = ids[..256]
                    .iter()
                    .map(|id| {
                        cache.ciphertext_entries.borrow()[id]
                            .copy
                            .ciphertext
                            .clone()
                    })
                    .collect();
                assert_eq!(cache.reclaim_ciphertext(None, 1), 0);
                assert_eq!(*cache.ciphertext_cursor.borrow(), Some(ids[255].clone()));
                assert_eq!(cache.reclaim_ciphertext(None, 1), page_alloc::buf::GRANULE);
                assert_eq!(*cache.ciphertext_cursor.borrow(), Some(ids[256].clone()));
                drop(held);
                assert_eq!(
                    cache.reclaim_ciphertext(Some(&CacheId("wrong".into())), 1),
                    0
                );
                assert_eq!(*cache.ciphertext_cursor.borrow(), Some(ids[255].clone()));
                assert_eq!(cache.ciphertext_entries.borrow().len(), 256);
                assert_eq!(cache.reclaim_ciphertext(None, 1), page_alloc::buf::GRANULE);
                assert_eq!(*cache.ciphertext_cursor.borrow(), Some(ids[0].clone()));
            }

            #[test]
            fn empty_and_maximum_clock_selection_is_nonmutating_and_wraps() {
                let env = racer_io::Env::host();
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(&env),
                );
                cache.entries.borrow_mut().reclaim_after = u64::MAX;
                assert!(cache.entries.borrow().candidates().is_empty());
                assert_eq!(cache.evict_idle(1), Ok(0));
                assert_eq!(cache.entries.borrow().reclaim_after, u64::MAX);
                cache.entries.borrow_mut().clock = u64::MAX - 2;
                let a = publish(&cache, &admission, "a");
                let b = publish(&cache, &admission, "b");
                assert_eq!(
                    cache.entries.borrow().candidates(),
                    vec![(u64::MAX - 1, a.clone()), (u64::MAX, b.clone())]
                );
                assert_eq!(cache.entries.borrow().reclaim_after, u64::MAX);
                assert_eq!(
                    cache.reclaim_idle(ResourceClass::Ciphertext, None, 1, |_, _| 0),
                    page_alloc::buf::GRANULE
                );
                assert_eq!(cache.entries.borrow().reclaim_after, u64::MAX - 1);
                assert_eq!(
                    cache.reclaim_idle(ResourceClass::Ciphertext, None, 1, |_, _| 0),
                    page_alloc::buf::GRANULE
                );
                assert_eq!(cache.entries.borrow().reclaim_after, u64::MAX);
                assert_eq!(
                    cache.reclaim_idle(ResourceClass::Ciphertext, None, 1, |_, _| 0),
                    0
                );
                assert_eq!(cache.entries.borrow().reclaim_after, u64::MAX);
            }

            enum Change {
                None,
                Publish,
                Touch,
            }

            fn check(env: &racer_io::Env, class: ResourceClass, change: Change) {
                let admission = admission(8);
                let cache = MemoryCache::new(
                    BufferPool::new(admission.clone()),
                    crate::test_support::availability(env),
                );
                let mut ids = Vec::new();
                for name in ["a", "b", "c"] {
                    let page = bundle(&admission, name);
                    ids.push(page.plaintext.page().clone());
                    cache.publish(page).unwrap();
                }
                // Published plaintext is not retained, so plaintext pressure
                // finds nothing to release and leaves every entry in place.
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                if matches!(class, ResourceClass::Plaintext) {
                    assert_eq!(cache.reclaim_idle(class, None, usize::MAX, |_, _| 0), 0);
                    assert_eq!(cache.entries.borrow().len(), 3);
                    assert_eq!(cache.entries.borrow().reclaim_after, 0);
                    return;
                }
                let charge = page_alloc::buf::GRANULE;
                assert!(
                    cache
                        .entries
                        .borrow()
                        .pages
                        .values()
                        .all(|(_, page)| idle(page))
                );
                assert_eq!(cache.reclaim_idle(class, None, 1, |_, _| 0), charge);
                {
                    let entries = cache.entries.borrow();
                    assert!(!entries.pages.contains_key(&ids[0]), "first victim is A");
                    assert!(entries.pages.contains_key(&ids[1]), "B remains idle");
                    assert!(entries.pages.contains_key(&ids[2]), "C remains idle");
                }
                assert_eq!(
                    admission.used(ResourceClass::Ciphertext),
                    2 * page_alloc::buf::GRANULE
                );

                let expected = match change {
                    Change::None => vec![false, false, true],
                    Change::Publish => {
                        let page = bundle(&admission, "d");
                        ids.push(page.plaintext.page().clone());
                        cache.publish(page).unwrap();
                        vec![false, false, true, true]
                    }
                    Change::Touch => {
                        drop(cache.ciphertext(&ids[1]).unwrap().unwrap());
                        vec![false, true, false]
                    }
                };
                assert!(
                    cache
                        .entries
                        .borrow()
                        .pages
                        .values()
                        .all(|(_, page)| idle(page))
                );
                assert_eq!(cache.reclaim_idle(class, None, 1, |_, _| 0), charge);
                let entries = cache.entries.borrow();
                let present: Vec<_> = ids
                    .iter()
                    .map(|id| entries.pages.contains_key(id))
                    .collect();
                assert_eq!(
                    present, expected,
                    "{class:?}: resident [A, B, C, optional D]; reclaim older unprocessed idle pages before recent pages"
                );
            }

            #[test]
            fn plaintext_without_new_ticks_reclaims_older_idle_page() {
                let env = racer_io::Env::host();
                check(&env, ResourceClass::Plaintext, Change::None);
            }

            #[test]
            fn ciphertext_without_new_ticks_reclaims_older_idle_page() {
                let env = racer_io::Env::host();
                check(&env, ResourceClass::Ciphertext, Change::None);
            }

            #[test]
            fn plaintext_preserves_newly_published_page() {
                let env = racer_io::Env::host();
                check(&env, ResourceClass::Plaintext, Change::Publish);
            }

            #[test]
            fn ciphertext_preserves_newly_published_page() {
                let env = racer_io::Env::host();
                check(&env, ResourceClass::Ciphertext, Change::Publish);
            }

            #[test]
            fn plaintext_preserves_recently_touched_page() {
                let env = racer_io::Env::host();
                check(&env, ResourceClass::Plaintext, Change::Touch);
            }

            #[test]
            fn ciphertext_preserves_recently_touched_page() {
                let env = racer_io::Env::host();
                check(&env, ResourceClass::Ciphertext, Change::Touch);
            }
        }

        #[test]
        fn ciphertext_residency_is_distinct_bounded_and_conditionally_invalidated() {
            let env = racer_io::Env::host();
            let admission = admission(2);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let first = bundle(&admission, "cipher");
            let id = first.plaintext.page().clone();
            let unverified = UnverifiedPage {
                copy: first.copy(),
                disk_token: None,
            };
            cache.publish_ciphertext(unverified.clone()).unwrap();
            assert!(cache.get(&id).unwrap().is_none());
            assert_eq!(
                cache.ciphertext(&id).unwrap().unwrap().ciphertext.bytes(),
                first.ciphertext.bytes()
            );
            let replacement = bundle(&admission, "cipher");
            cache.invalidate_ciphertext(&UnverifiedPage {
                copy: replacement.copy(),
                disk_token: None,
            });
            assert!(
                cache.unverified(&id).unwrap().is_some(),
                "different allocation must not invalidate"
            );
            cache.invalidate_ciphertext(&unverified);
            assert!(cache.unverified(&id).unwrap().is_none());
            cache.publish_ciphertext(unverified).unwrap();
            let plaintext = first.plaintext.clone();
            cache.publish(first).unwrap();
            assert!(cache.ciphertext_entries.borrow().get(&id).is_none());
            assert!(cache.get(&id).unwrap().is_some());
            // Without a live plaintext the verified ciphertext is still
            // available for a local decrypt.
            drop(plaintext);
            assert!(cache.get(&id).unwrap().is_none());
            assert!(cache.unverified(&id).unwrap().is_some());
        }
        #[test]
        fn publication_retains_ciphertext_and_only_a_weak_plaintext_handle() {
            let env = racer_io::Env::host();
            let admission = admission(8);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let page = bundle(&admission, "v1");
            let id = page.plaintext.page().clone();
            let ciphertext = page.ciphertext.clone();
            let held = page.plaintext.clone();
            cache.publish(page).unwrap();
            // A live reader's plaintext is shared without another copy.
            let shared = cache.get(&id).unwrap().unwrap();
            assert!(Arc::ptr_eq(&shared.plaintext.inner, &held.inner));
            assert!(Arc::ptr_eq(&shared.ciphertext.inner, &ciphertext.inner));
            drop((shared, held));
            // The cache alone keeps no plaintext or plaintext charge.
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                page_alloc::buf::GRANULE
            );
            assert!(cache.get(&id).unwrap().is_none());
            // Local readers decrypt the verified ciphertext; peers copy it.
            let local = cache.unverified(&id).unwrap().unwrap();
            assert!(local.disk_token.is_none());
            assert!(Arc::ptr_eq(&local.copy.ciphertext.inner, &ciphertext.inner));
            let peer = cache.ciphertext(&id).unwrap().unwrap();
            assert!(Arc::ptr_eq(&peer.ciphertext.inner, &ciphertext.inner));
            assert!(cache.metadata(&id.version).unwrap().is_some());
            drop((local, peer));
            // A fresh decrypt relinks the weak handle but keeps the original
            // ciphertext and its single charge.
            let decrypted = bundle(&admission, "v1");
            let relinked = decrypted.plaintext.clone();
            cache.publish(decrypted).unwrap();
            let shared = cache.get(&id).unwrap().unwrap();
            assert!(Arc::ptr_eq(&shared.plaintext.inner, &relinked.inner));
            assert!(Arc::ptr_eq(&shared.ciphertext.inner, &ciphertext.inner));
            drop((shared, relinked, ciphertext));
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                page_alloc::buf::GRANULE
            );
            assert_eq!(cache.entries.borrow().len(), 1);
        }
        #[test]
        fn failed_decrypt_of_verified_ciphertext_invalidates_the_entry() {
            let env = racer_io::Env::host();
            let admission = admission(8);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let page = bundle(&admission, "v1");
            let id = page.plaintext.page().clone();
            cache.publish(page).unwrap();
            let local = cache.unverified(&id).unwrap().unwrap();
            // A different allocation with equal bytes is not this entry.
            cache.invalidate_ciphertext(&UnverifiedPage {
                copy: bundle(&admission, "v1").copy(),
                disk_token: None,
            });
            assert!(cache.ciphertext(&id).unwrap().is_some());
            cache.invalidate_ciphertext(&local);
            assert!(cache.ciphertext(&id).unwrap().is_none());
            assert!(cache.unverified(&id).unwrap().is_none());
            drop(local);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
        #[test]
        fn expiry_releases_verified_ciphertext_while_a_reader_holds_plaintext() {
            let env = racer_io::Env::host();
            let admission = admission(8);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let page = bundle(&admission, "v1");
            let id = page.plaintext.page().clone();
            let held = page.plaintext.clone();
            cache.publish(page).unwrap();
            let start = std::time::Instant::now();
            assert_eq!(cache.expire_idle(start), 0);
            // Held plaintext is the reader's; it does not pin the entry.
            assert_eq!(cache.expire_idle(start + racer_core::cache::IDLE_TTL), 1);
            assert!(cache.ciphertext(&id).unwrap().is_none());
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            assert_eq!(held.bytes(), &[1; 3]);
            drop(held);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        }
        #[test]
        fn bounded_reclamation_advances_past_a_busy_prefix() {
            let env = racer_io::Env::host();
            let admission = admission(300);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let mut busy = Vec::new();
            let mut ids = Vec::new();
            for n in 0..300 {
                let page = bundle(&admission, &format!("v{n}"));
                ids.push(page.plaintext.page().clone());
                if n < 256 {
                    busy.push(page.clone());
                }
                cache.publish(page).unwrap();
            }
            assert_eq!(cache.evict_idle(1), Ok(0));
            assert_eq!(cache.evict_idle(1), Ok(page_alloc::buf::GRANULE));
            assert!(cache.ciphertext(&ids[256]).unwrap().is_none());
            assert!(cache.ciphertext(&ids[257]).unwrap().is_some());
            assert!(
                cache.get(&ids[257]).unwrap().is_none(),
                "plaintext released"
            );
            for reader in &busy {
                assert_eq!(reader.plaintext.bytes(), &[1; 3]);
                assert!(cache.get(reader.plaintext.page()).unwrap().is_some());
            }
            // Only the readers' live plaintext stays charged.
            assert_eq!(admission.used(ResourceClass::Plaintext), 256 * 4096);
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                299 * page_alloc::buf::GRANULE
            );
            drop(busy);
            for _ in 0..3 {
                cache.evict_idle(usize::MAX).unwrap();
            }
            for id in ids {
                assert!(cache.ciphertext(&id).unwrap().is_none());
            }
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
        #[test]
        fn busy_leases_protect_both_allocations_and_eviction_releases_idle_bytes() {
            let env = racer_io::Env::host();
            let admission = admission(8);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let page = bundle(&admission, "v1");
            let id = page.plaintext.page().clone();
            let plaintext = page.plaintext.clone();
            cache.publish(page).unwrap();
            let copy = cache.ciphertext(&id).unwrap().unwrap();
            assert_eq!(cache.evict_idle(usize::MAX), Ok(0));
            drop(copy);
            // Live plaintext is shared but does not pin the entry, and its
            // charge is never counted as cache bytes.
            assert!(cache.get(&id).unwrap().is_some());
            assert_eq!(cache.evict_idle(0), Ok(0));
            assert_eq!(cache.evict_idle(1), Ok(page_alloc::buf::GRANULE));
            assert_eq!(admission.used(ResourceClass::Plaintext), 4096);
            drop(plaintext);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            assert!(cache.get(&id).unwrap().is_none());
            assert!(cache.metadata(&id.version).unwrap().is_none());
        }
        #[test]
        fn duplicate_preserves_original_ciphertext_metadata_and_expired_deadline() {
            let env = racer_io::Env::host();
            let admission = admission(8);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let page = bundle(&admission, "v1");
            let id = page.plaintext.page().clone();
            let original = page.ciphertext.envelope().clone();
            cache.publish(page).unwrap();
            let mut duplicate = bundle(&admission, "v1");
            duplicate.metadata.expires_at = ExpiresAt::from_unix_millis(123456).unwrap();
            Arc::get_mut(&mut duplicate.ciphertext.inner)
                .unwrap()
                .envelope
                .nonce
                .0[0] ^= 1;
            cache.publish(duplicate).unwrap();
            let copy = cache.ciphertext(&id).unwrap().unwrap();
            assert_eq!(copy.ciphertext.envelope(), &original);
            assert_eq!(copy.ciphertext.bytes(), &[2; 19]);
            assert_eq!(
                copy.metadata.expires_at,
                ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap()
            );
            assert_eq!(
                cache.metadata(&id.version).unwrap(),
                Some(copy.metadata.immutable())
            );
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                page_alloc::buf::GRANULE
            );
        }
        /// A duplicate publish is not a use, so the oldest
        /// insert is still evicted first (the simulator's `memory_insert`
        /// keeps the same order).
        #[test]
        fn duplicate_publish_does_not_touch() {
            let env = racer_io::Env::host();
            let admission = admission(2);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let id = |v: &str| bundle(&admission, v).plaintext.page().clone();
            for version in ["v0", "v1", "v0", "v2"] {
                cache.publish(bundle(&admission, version)).unwrap();
            }
            let resident = cache.resident();
            assert!(!resident.contains(&id("v0")));
            assert!(resident.contains(&id("v1")) && resident.contains(&id("v2")));
        }
        #[test]
        fn capacity_evicts_least_recent_idle_entry_and_rejects_all_busy() {
            let env = racer_io::Env::host();
            let admission = admission(2);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            let first = bundle(&admission, "v1");
            let first_id = first.plaintext.page().clone();
            let second = bundle(&admission, "v2");
            let second_id = second.plaintext.page().clone();
            cache.publish(first).unwrap();
            cache.publish(second).unwrap();
            // Readers of cached ciphertext keep entries busy.
            let busy = cache.ciphertext(&first_id).unwrap().unwrap();
            let also_busy = cache.ciphertext(&second_id).unwrap().unwrap();
            assert_eq!(
                cache.publish(bundle(&admission, "v3")),
                Err(Error::Overloaded)
            );
            drop(also_busy);
            let third = bundle(&admission, "v3");
            let third_id = third.plaintext.page().clone();
            cache.publish(third).unwrap();
            assert!(cache.ciphertext(&second_id).unwrap().is_none());
            assert!(cache.ciphertext(&first_id).unwrap().is_some());
            drop(busy);
            let fourth = bundle(&admission, "v4");
            let fourth_id = fourth.plaintext.page().clone();
            cache.publish(fourth).unwrap();
            assert!(cache.ciphertext(&first_id).unwrap().is_some());
            assert!(cache.ciphertext(&third_id).unwrap().is_none());
            assert!(cache.ciphertext(&fourth_id).unwrap().is_some());
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                2 * page_alloc::buf::GRANULE
            );
        }
        #[test]
        fn publication_rejects_foreign_undercharged_and_conflicting_bundles() {
            let env = racer_io::Env::host();
            let admission = admission(8);
            let cache = MemoryCache::new(
                BufferPool::new(admission.clone()),
                crate::test_support::availability(&env),
            );
            assert_eq!(
                cache.publish(bundle(&self::admission(8), "v1")),
                Err(Error::InvalidConfiguration)
            );
            let mut malformed = bundle(&admission, "v1");
            Arc::get_mut(&mut malformed.ciphertext.inner)
                .unwrap()
                .fixture_pop();
            assert_eq!(cache.publish(malformed), Err(Error::CorruptRecord));
            let mut undercharged = bundle(&admission, "v1");
            // Plaintext storage cannot carry a non-plaintext charge at all;
            // a charge for another cache must still be refused at publish.
            let wrong = admission
                .reserve(
                    Some(&CacheId("other".into())),
                    ResourceClass::Plaintext,
                    page_alloc::buf::GRANULE,
                )
                .unwrap();
            let storage = &mut Arc::get_mut(&mut undercharged.plaintext.inner)
                .unwrap()
                .storage;
            let ciphertext = admission
                .reserve(None, ResourceClass::Ciphertext, page_alloc::buf::GRANULE)
                .unwrap();
            assert!(storage.replace_charge(PlaintextCharge(ciphertext)).is_err());
            assert!(storage.replace_charge(PlaintextCharge(wrong)).is_ok());
            assert_eq!(
                cache.publish(undercharged),
                Err(Error::InvalidConfiguration)
            );
            let original = bundle(&admission, "v1");
            let id = original.plaintext.page().clone();
            // Conflicts are detectable only against live plaintext.
            let held = original.plaintext.clone();
            cache.publish(original).unwrap();
            let mut conflicting = bundle(&admission, "v1");
            Arc::get_mut(&mut conflicting.plaintext.inner)
                .unwrap()
                .storage[0] ^= 1;
            assert_eq!(cache.publish(conflicting), Err(Error::CorruptRecord));
            let mut inconsistent = bundle(&admission, "v2");
            let rejected_id = inconsistent.plaintext.page().clone();
            inconsistent.metadata.version.etag = crate::model::StrongEtag::test_value("v1");
            assert_eq!(cache.publish(inconsistent), Err(Error::CorruptRecord));
            assert_eq!(cache.get(&id).unwrap().unwrap().plaintext.bytes(), &[1; 3]);
            assert!(cache.get(&rejected_id).unwrap().is_none());
            assert_eq!(admission.used(ResourceClass::Plaintext), 4096);
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                page_alloc::buf::GRANULE
            );
            drop(held);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        }
        #[test]
        fn eviction_is_cache_scoped_and_preserves_live_leases() {
            let env = racer_io::Env::host();
            let admission = admission(8);
            let other_cache = CacheId("44444444-4444-4444-8444-444444444444".into());
            let availability = crate::test_support::availability_for(
                &env,
                vec![
                    CacheId(crate::test_support::security::CACHE.into()),
                    other_cache.clone(),
                ],
            );
            let cache = MemoryCache::new(BufferPool::new(admission.clone()), availability);
            let page = bundle(&admission, "v1");
            let id = page.plaintext.page().clone();
            let mut other_descriptor = page.metadata.immutable();
            other_descriptor.version.object.cache = other_cache;
            let other = bundle_for(&admission, other_descriptor.clone());
            let other_id = other.plaintext.page().clone();
            cache.publish(page.clone()).unwrap();
            cache.publish(other).unwrap();
            cache.remove_cache(&id.version.object.cache).unwrap();
            assert!(cache.ciphertext(&id).unwrap().is_none());
            assert!(cache.ciphertext(&other_id).unwrap().is_some());
            assert_eq!(page.plaintext.bytes(), &[1; 3]);
            assert_eq!(admission.used(ResourceClass::Plaintext), 4096);
            drop(page);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            let lease = cache.ciphertext(&other_id).unwrap().unwrap();
            cache.remove_cache(&other_id.version.object.cache).unwrap();
            assert!(cache.ciphertext(&other_id).unwrap().is_none());
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                page_alloc::buf::GRANULE
            );
            drop(lease);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
        #[test]
        fn eviction_is_idempotent_and_churn_needs_no_tombstones() {
            let env = racer_io::Env::host();
            let admission = admission(1);
            let cache = MemoryCache::new(
                BufferPool::new(admission),
                crate::test_support::availability(&env),
            );
            let id = CacheId("cache".into());
            assert_eq!(cache.remove_cache(&id), Ok(()));
            assert_eq!(cache.remove_cache(&id), Ok(()));
            assert_eq!(cache.remove_cache(&CacheId("other".into())), Ok(()));
            for n in 0..10_000 {
                cache.remove_cache(&CacheId(n.to_string())).unwrap();
            }
        }
        #[test]
        fn empty_stable_catalog_rotates_without_consuming_page_metadata_capacity() {
            use crate::control::for_caches;
            use crate::test_support::security::rotation_bundle;
            let env = racer_io::Env::host();
            let admission = admission(1024);
            let keys = Rc::new(crate::test_support::security::keys(&env));
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            let caches: Vec<_> = (0..356)
                .map(|cache| CacheId(format!("{cache:08x}-0000-4000-8000-000000000000")))
                .collect();
            let memory = MemoryCache::new(
                BufferPool::new(admission.clone()),
                for_caches(&env, keys.clone(), caches.clone()),
            );
            use racer_wire::control::{CacheEncryptionKey, CacheKeyPurpose, CacheKeyState};
            // The authority default: the active page key plus RETAINED - 1
            // decrypt keys stay readable. Pages of a key stay readable for
            // RETAINED - 1 rotations and become unreadable after the next.
            const RETAINED: usize = 7;
            let mut history: Vec<Vec<CacheEncryptionKey>> = Vec::new();
            for generation in 2u64..=(RETAINED as u64 + 3) {
                let mut next = rotation_bundle(generation, roots.clone());
                let templates = std::mem::take(&mut next.cache_keys);
                for cache in 0u64..356 {
                    for template in &templates {
                        let mut key = template.clone();
                        key.key.cache = CacheId(format!("{cache:08x}-0000-4000-8000-000000000000"));
                        let (reference, state, mut material) = key.clone().into_installation();
                        material[8..16].copy_from_slice(&cache.to_be_bytes());
                        key = racer_wire::control::CacheEncryptionKey::new(
                            reference, state, material,
                        );
                        next.cache_keys.push(key);
                    }
                }
                let pages: Vec<_> = next
                    .cache_keys
                    .iter()
                    .filter(|key| key.key.purpose == CacheKeyPurpose::Page)
                    .cloned()
                    .collect();
                for old in history.iter().rev().take(RETAINED - 1) {
                    for key in old {
                        let mut key = key.clone();
                        key.state = CacheKeyState::Decrypt;
                        next.cache_keys.push(key);
                    }
                }
                keys.install(next.clone()).unwrap();
                for (age, old) in history.iter().rev().enumerate() {
                    for key in old {
                        assert_eq!(
                            memory.availability.page(&key.key.cache, key.key.id),
                            age < RETAINED - 1,
                            "generation {generation} age {age}"
                        );
                    }
                }
                history.push(pages);
            }
            assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }

    pub(crate) fn admission(entries: usize) -> Rc<flow_control::Quotas<AdmissionPolicy>> {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.metadata_entries = std::num::NonZeroUsize::new(entries).unwrap();
        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)))
    }
    pub(crate) fn bundle(
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        version: &str,
    ) -> PageResult {
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::ObjectVersion;
        use crate::model::StrongEtag;
        bundle_for(
            admission,
            VersionMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(crate::test_support::security::CACHE.into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value(version),
                },
                length: 3,
            },
        )
    }
    pub(crate) fn bundle_for(
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        metadata: VersionMetadata,
    ) -> PageResult {
        let page = PageId {
            version: metadata.version.clone(),
            number: PageNumber(0),
        };
        let cache = &page.version.object.cache;
        let plaintext = VerifiedPage {
            inner: Arc::new(VerifiedBytes {
                page: page.clone(),
                storage: crate::memory::fixture_plaintext(admission, Some(cache), &[1; 3]),
            }),
        };
        // Disk-aligned like production AEAD output, so persistence writes the
        // body in place and stages only the header block.
        let pool = BufferPool::new(admission.clone());
        let mut buffer = pool
            .encryption_output(
                admission
                    .reserve(Some(cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
                19,
            )
            .unwrap();
        buffer.as_mut_slice().fill(2);
        let ciphertext = pool
            .aligned_ciphertext_page(
                buffer,
                PageEnvelope {
                    page_size: racer_wire::object::PageSize::DEFAULT,
                    cipher: crate::model::PageCipher::Xaes256Gcm,
                    page,
                    key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                    nonce: Nonce([2; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
            )
            .unwrap();
        PageResult {
            metadata: metadata.for_pin(),
            plaintext,
            ciphertext,
        }
    }
    #[test]
    fn slab_ciphertext_keeps_pointer_capacity_and_rehomes() {
        for shared in [false, true] {
            let source = admission(8);
            let target = admission(8);
            let fixture = bundle(&source, "slab");
            let envelope = fixture.ciphertext.envelope().clone();
            let cache = envelope.page.version.object.cache.clone();
            drop(fixture);
            let mut buffer = page_alloc::buf::Buf::unpooled(
                4096,
                4096,
                source
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 4096)
                    .unwrap(),
                false,
            )
            .unwrap();
            buffer[137..156].fill(7);
            let pointer = buffer[137..].as_ptr();
            let page = BufferPool::new(source.clone())
                .slab_ciphertext(buffer, envelope, 137..156)
                .unwrap();
            assert_eq!(page.bytes().as_ptr(), pointer);
            assert_eq!(page.bytes(), &[7; 19]);
            assert_eq!(page.inner.capacity(), 4096);
            assert_eq!(source.used(ResourceClass::Ciphertext), 4096);
            let retained = shared.then(|| page.clone());
            let moved = page
                .rehome(
                    target
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 4096)
                        .unwrap(),
                )
                .unwrap();
            assert_eq!(moved.bytes().as_ptr() == pointer, !shared);
            assert!(target.owns(moved.inner.reservation()));
            assert_eq!(
                source.used(ResourceClass::Ciphertext),
                if shared { 4096 } else { 0 }
            );
            std::thread::spawn(move || {
                assert_eq!(moved.bytes(), &[7; 19]);
                drop(moved);
            })
            .join()
            .unwrap();
            drop(retained);
            assert_eq!(source.used(ResourceClass::Ciphertext), 0);
            assert_eq!(target.used(ResourceClass::Ciphertext), 0);
        }
    }

    #[test]
    fn slab_ciphertext_rejects_bad_ranges_and_authority() {
        let source = admission(8);
        let other = admission(8);
        let fixture = bundle(&source, "slab");
        let envelope = fixture.ciphertext.envelope().clone();
        let cache = envelope.page.version.object.cache.clone();
        drop(fixture);
        for range in [
            0..0,
            1..19,
            4090..4109,
            std::ops::Range { start: 20, end: 1 },
        ] {
            let buffer = page_alloc::buf::Buf::unpooled(
                4096,
                4096,
                source
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 4096)
                    .unwrap(),
                false,
            )
            .unwrap();
            assert!(matches!(
                BufferPool::new(source.clone()).slab_ciphertext(buffer, envelope.clone(), range),
                Err(Error::CorruptRecord)
            ));
            assert_eq!(source.used(ResourceClass::Ciphertext), 0);
        }
        for class in [ResourceClass::Ciphertext, ResourceClass::Plaintext] {
            let buffer = page_alloc::buf::Buf::unpooled(
                4096,
                4096,
                source.reserve(Some(&cache), class, 4096).unwrap(),
                false,
            );
            if matches!(class, ResourceClass::Plaintext) {
                assert!(matches!(
                    buffer,
                    Err(page_alloc::Error::InvalidConfiguration)
                ));
                continue;
            }
            assert!(matches!(
                BufferPool::new(other.clone()).slab_ciphertext(
                    buffer.unwrap(),
                    envelope.clone(),
                    0..19
                ),
                Err(Error::InvalidConfiguration)
            ));
            assert_eq!(source.used(class), 0);
        }
        let buffer = page_alloc::buf::Buf::unpooled(
            4096,
            4096,
            source
                .reserve(Some(&cache), ResourceClass::Ciphertext, 4096)
                .unwrap(),
            false,
        )
        .unwrap();
        let page = BufferPool::new(source.clone())
            .slab_ciphertext(buffer, envelope, 4077..4096)
            .unwrap();
        assert!(matches!(
            page.rehome(
                other
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                    .unwrap()
            ),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(source.used(ResourceClass::Ciphertext), 0);
        assert_eq!(other.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn rehome_moves_exclusive_bytes_but_preserves_shared_transport_owners() {
        for shared in [false, true] {
            let source = admission(8);
            let target = admission(8);
            let mut page = bundle(&source, "rehome");
            let provenance = crate::telemetry::PeerProvenance {
                request: crate::model::RequestId([3; 16]),
                attempt: crate::model::AttemptId([4; 16]),
                supplier: [b'a'; 36],
                remote: [b'b'; 36],
            };
            page.ciphertext.provenance = Some(provenance);
            let pointer = page.ciphertext.bytes().as_ptr();
            let original = page.ciphertext.bytes().to_vec();
            let retained = shared.then(|| page.ciphertext.clone());
            let reservation = target
                .reserve(
                    Some(&page.metadata.version.object.cache),
                    ResourceClass::Ciphertext,
                    page_alloc::buf::GRANULE,
                )
                .unwrap();
            let moved = page.ciphertext.rehome(reservation).unwrap();
            assert_eq!(moved.bytes().as_ptr() == pointer, !shared);
            assert_eq!(moved.bytes(), &original[..]);
            assert_eq!(moved.provenance, Some(provenance));
            if let Some(retained) = &retained {
                assert_eq!(retained.provenance, Some(provenance));
            }
            assert!(target.owns(moved.inner.reservation()));
            assert_eq!(
                source.used(ResourceClass::Ciphertext),
                if shared { page_alloc::buf::GRANULE } else { 0 }
            );
            assert_eq!(
                target.used(ResourceClass::Ciphertext),
                page_alloc::buf::GRANULE
            );
            drop(retained);
            assert_eq!(source.used(ResourceClass::Ciphertext), 0);
            drop(moved);
            assert_eq!(target.used(ResourceClass::Ciphertext), 0);
        }
    }

    #[test]
    fn aligned_pages_are_disk_aligned_validated_and_rehome_into_aligned_copies() {
        let source = admission(8);
        let target = admission(8);
        let pool = BufferPool::new(source.clone());
        let base = bundle(&source, "aligned");
        let envelope = base.ciphertext.envelope().clone();
        let cache = envelope.page.version.object.cache.clone();
        let length = envelope.ciphertext_length as usize;
        let capacity = pool.aligned_capacity(length).unwrap();
        let alignment = default_disk_alignment();
        let reserve = |quotas: &Rc<flow_control::Quotas<AdmissionPolicy>>, cache, amount| {
            quotas
                .reserve(Some(cache), ResourceClass::Ciphertext, amount)
                .unwrap()
        };
        let fill = |amount| {
            let mut buffer = pool
                .aligned_ciphertext(reserve(&source, &cache, capacity), amount)
                .unwrap();
            buffer.as_mut_slice().fill(0xa5);
            buffer
        };
        // The charge must cover the pooled size class, not just the length.
        assert!(
            pool.aligned_ciphertext(reserve(&source, &cache, length), length)
                .is_err()
        );
        // Length and cache mismatches are rejected and storage returns idle.
        assert!(matches!(
            pool.aligned_ciphertext_page(fill(length - 1), envelope.clone()),
            Err(Error::CorruptRecord)
        ));
        let wrong = CacheId("wrong".into());
        let mut buffer = pool
            .aligned_ciphertext(reserve(&source, &wrong, capacity), length)
            .unwrap();
        buffer.as_mut_slice().fill(1);
        assert!(matches!(
            pool.aligned_ciphertext_page(buffer, envelope.clone()),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(pool.disk_idle_bytes(), capacity);
        assert_eq!(
            source.used(ResourceClass::Ciphertext),
            page_alloc::buf::GRANULE
        );

        let mut buffer = pool
            .aligned_ciphertext(reserve(&source, &cache, capacity), length)
            .unwrap();
        assert_eq!(pool.disk_idle_bytes(), 0, "storage is recycled");
        buffer
            .as_mut_slice()
            .copy_from_slice(base.ciphertext.bytes());
        let page = pool
            .aligned_ciphertext_page(buffer, envelope.clone())
            .unwrap();
        assert_eq!(page.bytes(), base.ciphertext.bytes());
        let body = page.disk_body(alignment).unwrap();
        assert_eq!(body.as_ptr(), page.bytes().as_ptr());
        assert_eq!(body.len(), 4096);
        assert!(body[length..].iter().all(|byte| *byte == 0));
        // A device needing a larger unit gets no zero-copy body.
        let stricter = page_alloc::Alignment::new(4096, 8192, 8192).unwrap();
        assert!(!page.is_disk_aligned(stricter));
        // Copied pages are not zero-padded and always need a copy.
        let copied = BufferPool::new(source.clone())
            .ciphertext(
                reserve(&source, &cache, length),
                envelope.clone(),
                base.ciphertext.bytes(),
            )
            .unwrap();
        assert!(!copied.is_disk_aligned(alignment));
        drop(copied);

        // A shared page is copied into fresh aligned storage.
        let retained = page.clone();
        let moved = page.rehome(reserve(&target, &cache, capacity)).unwrap();
        assert_ne!(moved.bytes().as_ptr(), retained.bytes().as_ptr());
        assert_eq!(moved.bytes(), retained.bytes());
        assert!(moved.is_disk_aligned(alignment));
        assert!(target.owns(moved.inner.reservation()));
        drop(retained);
        assert_eq!(pool.disk_idle_bytes(), capacity);
        assert_eq!(
            source.used(ResourceClass::Ciphertext),
            page_alloc::buf::GRANULE
        );
        drop(moved);
        assert_eq!(target.used(ResourceClass::Ciphertext), 0);
        // Releasing idle storage keeps admission untouched.
        pool.reclaim_idle_buffers();
        assert_eq!(pool.disk_idle_bytes(), 0);
    }

    #[test]
    fn aligned_pool_empties_when_idle_and_is_retained_while_reused() {
        let admission = admission(8);
        let pool = BufferPool::new(admission.clone());
        let length = PAGE_BYTES as usize + 16;
        let capacity = pool.aligned_capacity(length).unwrap();
        let take = || {
            let reservation = admission
                .reserve(None, ResourceClass::Ciphertext, capacity)
                .unwrap();
            pool.aligned_ciphertext(reservation, length).unwrap()
        };
        let burst = |count: usize| {
            let buffers: Vec<_> = (0..count).map(|_| take()).collect();
            drop(buffers);
        };
        let full = POOL_IDLE_PER_CLASS * capacity;
        burst(POOL_IDLE_PER_CLASS);
        assert_eq!(pool.disk_idle_bytes(), full);
        let window = page_alloc::DEFAULT_WINDOW;
        let start = std::time::Instant::now();
        assert_eq!(pool.maintain_idle_buffers(start), 0);
        // Active reuse every window keeps the pool full.
        for turn in 1..=20 {
            burst(POOL_IDLE_PER_CLASS);
            assert_eq!(pool.maintain_idle_buffers(start + window * turn), 0);
            assert_eq!(pool.disk_idle_bytes(), full);
        }
        // Once idle, each window releases half; a few windows release all.
        let mut now = start + window * 21;
        assert_eq!(pool.maintain_idle_buffers(now), full / 2);
        for _ in 0..POOL_IDLE_PER_CLASS {
            now += window;
            pool.maintain_idle_buffers(now);
        }
        assert_eq!(pool.disk_idle_bytes(), 0);
        assert!(pool.pool_stats().trimmed >= 2);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(admission.used(ResourceClass::Pool), 0);

        // A constrained pool sheds at once and at every maintenance.
        burst(1);
        pool.constrain_idle_buffers(true);
        assert!(pool.pool_stats().constrained);
        assert_eq!(pool.disk_idle_bytes(), 0);
        burst(1);
        assert_eq!(pool.maintain_idle_buffers(now), capacity);
        assert_eq!(pool.disk_idle_bytes(), 0);
        pool.constrain_idle_buffers(false);
        burst(1);
        assert_eq!(pool.disk_idle_bytes(), capacity);
    }

    #[test]
    fn encryption_output_upgrades_exact_charges_and_rejects_foreign_ones() {
        let source = admission(8);
        let pool = BufferPool::new(source.clone());
        let cache = CacheId("cache".into());
        let length = PAGE_BYTES as usize + 16;
        let capacity = pool.aligned_capacity(length).unwrap();
        assert!(capacity > length);
        let exact = source
            .reserve(Some(&cache), ResourceClass::Ciphertext, length)
            .unwrap();
        let buffer = pool.encryption_output(exact, length).unwrap();
        assert_eq!(buffer.len(), length);
        assert_eq!(buffer.charge().amount(), capacity);
        assert_eq!(source.used(ResourceClass::Ciphertext), capacity);
        drop(buffer);
        assert_eq!(source.used(ResourceClass::Ciphertext), 0);
        // A charge from another worker's admission is rejected and released.
        let other = admission(8);
        let foreign = other
            .reserve(Some(&cache), ResourceClass::Ciphertext, length)
            .unwrap();
        assert!(matches!(
            pool.encryption_output(foreign, length),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(other.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn rehome_rejects_wrong_cache_class_and_insufficient_charge() {
        let source = admission(8);
        let target = admission(8);
        let page = bundle(&source, "rehome");
        let cache = &page.metadata.version.object.cache;
        let wrong = CacheId("wrong".into());
        for (cache, class, amount) in [
            (&wrong, ResourceClass::Ciphertext, 19),
            (cache, ResourceClass::Plaintext, 19),
            (cache, ResourceClass::Ciphertext, 18),
        ] {
            assert!(matches!(
                page.ciphertext
                    .clone()
                    .rehome(target.reserve(Some(cache), class, amount).unwrap(),),
                Err(Error::InvalidConfiguration)
            ));
        }
        assert_eq!(
            source.used(ResourceClass::Ciphertext),
            page_alloc::buf::GRANULE
        );
        assert_eq!(target.used(ResourceClass::Ciphertext), 0);
        assert_eq!(target.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn aead_fingerprints_record_stored_tag_and_separate_body_from_identity() {
        use crate::model::RequestId;
        use crate::security::capture_aead_failure;
        let admission = admission(8);
        let mut page = bundle(&admission, "sensitive-etag");
        let request = RequestId([9; 16]);
        let capture = |p: &CiphertextPage| {
            // Use a small stand-in AAD to isolate diagnostic fingerprinting.
            // Production passes the already validated canonical page_aad bytes.
            let mut aad = p.envelope().nonce.0.to_vec();
            aad.extend_from_slice(p.envelope().page.version.etag.as_bytes());
            capture_aead_failure(p, &aad, request, 0)
        };
        assert_eq!(capture(&page.ciphertext).peer, None);
        let provenance = crate::telemetry::PeerProvenance {
            request: RequestId([3; 16]),
            attempt: crate::model::AttemptId([4; 16]),
            supplier: [b'a'; 36],
            remote: [b'b'; 36],
        };
        page.ciphertext.provenance = Some(provenance);
        let first = capture(&page.ciphertext);
        let repeat = capture(&page.ciphertext);
        // Frozen SHA-256 vectors include the domain, u64 field lengths, exact
        // quoted ETag and page number. The AAD fingerprint hashes exact bytes.
        let hex = |bytes: &[u8]| {
            bytes
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>()
        };
        assert_eq!(
            hex(&first.page),
            "c862fae4132569c58e99bb681cc7e9907ea20a693ea6760bf1ea5fcf7224243e"
        );
        assert_eq!(
            hex(&first.aad),
            "611dfd441fbad13fcb7e6e98a61a2704e30651ca1f2353acdc32906dd8ae3122"
        );
        assert_eq!(first.request, request);
        assert_eq!(first.peer, Some(provenance));
        assert_eq!(first.number, 0);
        assert_eq!(first.key, page.ciphertext.envelope().key_id.0);
        assert_eq!(first.nonce, [2; 24]);
        assert_eq!((first.plaintext, first.ciphertext), (3, 19));
        let stored_tag = |p: &CiphertextPage| {
            let bytes = p.bytes();
            <[u8; 16]>::try_from(&bytes[bytes.len() - 16..]).unwrap()
        };
        assert_eq!(first.tag, stored_tag(&page.ciphertext));
        assert_eq!(first.page, repeat.page);
        assert_eq!(first.aad, repeat.aad);
        assert_eq!(first.tag, repeat.tag);
        let inner = Arc::get_mut(&mut page.ciphertext.inner).unwrap();
        // A body change before the tag leaves the stored tag fingerprint alone.
        inner.fixture_bytes_mut()[0] ^= 1;
        assert!(page.validate_metadata().is_ok());
        assert_eq!(capture(&page.ciphertext).tag, first.tag);
        let inner = Arc::get_mut(&mut page.ciphertext.inner).unwrap();
        let last = inner.fixture_bytes_mut().len() - 1;
        inner.fixture_bytes_mut()[last] ^= 1;
        let changed = capture(&page.ciphertext);
        assert_ne!(first.tag, changed.tag);
        assert_eq!(first.page, changed.page);
        assert_eq!(first.aad, changed.aad);
        Arc::get_mut(&mut page.ciphertext.inner)
            .unwrap()
            .envelope
            .nonce
            .0[0] ^= 1;
        let changed_aad = capture(&page.ciphertext);
        assert_eq!(changed.tag, changed_aad.tag);
        assert_eq!(changed.page, changed_aad.page);
        assert_ne!(changed.aad, changed_aad.aad);
        let failures = crate::telemetry::Failures::default();
        failures.observer(crate::model::WorkerId(0)).record_aead(
            crate::security::CryptoId {
                worker: crate::model::WorkerId(0),
                generation: 0,
                sequence: 1,
            },
            first,
        );
        let mut text = String::new();
        failures.write_aead(&mut text).unwrap();
        assert!(!text.contains("sensitive-etag"));
        assert!(!text.contains(&page.ciphertext.envelope().page.version.object.key.to_hex()));
        drop(page);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        let mut retained = String::new();
        failures.write_aead(&mut retained).unwrap();
        assert_eq!(retained, text, "diagnostic facts do not retain page owners");
    }
    #[test]
    fn plaintext_is_stable_pooled_and_reservation_backed() {
        let admission = admission(8);
        let pool = BufferPool::new(admission.clone());
        let cache = CacheId("cache".into());
        let granule = page_alloc::buf::GRANULE;
        let reservation = admission
            .reserve(Some(&cache), ResourceClass::Plaintext, 3)
            .unwrap();
        let mut buffer = pool.plaintext(reservation, 3).unwrap();
        let pointer = buffer.bytes().unwrap().as_ptr();
        buffer.bytes_mut().unwrap().copy_from_slice(b"abc");
        // The charge is topped up to cover the pooled capacity.
        assert_eq!(buffer.reservation().amount(), granule);
        let storage = buffer.into_buf();
        assert_eq!(storage.as_ptr(), pointer);
        assert_eq!(&*storage, b"abc");
        assert!(storage.wipes());
        assert_eq!(admission.used(ResourceClass::Plaintext), granule);
        drop(storage);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        // The idle buffer is reused, charged to the pool while idle.
        assert_eq!(admission.used(ResourceClass::Pool), granule);
        let reused = pool
            .plaintext_storage(
                admission
                    .reserve(Some(&cache), ResourceClass::Plaintext, granule)
                    .unwrap(),
                3,
            )
            .unwrap();
        assert_eq!(reused.as_ptr(), pointer);
        assert_eq!(&*reused, &[0; 3], "pooled plaintext is wiped on return");
        assert_eq!(admission.used(ResourceClass::Pool), 0);
    }
    #[test]
    fn allocations_reject_wrong_provenance_class_cache_and_bounds() {
        let admission = admission(8);
        let pool = BufferPool::new(admission.clone());
        let other = self::admission(8);
        let cache = CacheId("cache".into());
        for (owner, class, cache, amount, length) in [
            (&other, ResourceClass::Plaintext, Some(&cache), 3, 3),
            (&admission, ResourceClass::Ciphertext, Some(&cache), 3, 3),
            (&admission, ResourceClass::Plaintext, None, 3, 3),
            (&admission, ResourceClass::Plaintext, Some(&cache), 2, 3),
            (&admission, ResourceClass::Plaintext, Some(&cache), 3, 0),
            (
                &admission,
                ResourceClass::Plaintext,
                Some(&cache),
                3,
                PAGE_BYTES as usize + 1,
            ),
        ] {
            assert!(matches!(
                pool.plaintext(owner.reserve(cache, class, amount).unwrap(), length),
                Err(Error::InvalidConfiguration)
            ));
        }
        let page = bundle(&admission, "v1");
        let envelope = page.ciphertext.envelope().clone();
        let wrong = CacheId("other".into());
        for (owner, class, cache, amount) in [
            (&other, ResourceClass::Ciphertext, &cache, 19),
            (&admission, ResourceClass::Plaintext, &cache, 19),
            (&admission, ResourceClass::Ciphertext, &wrong, 19),
            (&admission, ResourceClass::Ciphertext, &cache, 18),
        ] {
            assert!(matches!(
                pool.ciphertext(
                    owner.reserve(Some(cache), class, amount).unwrap(),
                    envelope.clone(),
                    vec![0; 19]
                ),
                Err(Error::InvalidConfiguration)
            ));
        }
        for length in [0, 18, 20] {
            assert!(matches!(
                pool.ciphertext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 20)
                        .unwrap(),
                    envelope.clone(),
                    vec![0; length]
                ),
                Err(Error::CorruptRecord)
            ));
        }
        let mut excess_capacity = Vec::with_capacity(100);
        excess_capacity.resize(19, 0);
        assert!(matches!(
            pool.ciphertext(
                admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
                envelope.clone(),
                excess_capacity
            ),
            Err(Error::InvalidConfiguration)
        ));
        let mut invalid = envelope;
        invalid.plaintext_length = 0;
        assert!(matches!(
            pool.ciphertext(
                admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
                invalid,
                vec![0; 19]
            ),
            Err(Error::CorruptRecord)
        ));
    }
    #[test]
    fn immutable_clones_charge_once_until_last_cross_thread_owner() {
        let admission = admission(8);
        let page = bundle(&admission, "v1");
        let plain = page.plaintext.clone();
        let cipher = page.ciphertext.clone();
        let pointer = cipher.bytes().as_ptr();
        assert_eq!(page.ciphertext.bytes().as_ptr(), pointer);
        drop(page);
        assert_eq!(admission.used(ResourceClass::Plaintext), 4096);
        assert_eq!(
            admission.used(ResourceClass::Ciphertext),
            page_alloc::buf::GRANULE
        );
        std::thread::spawn(move || drop((plain, cipher)))
            .join()
            .unwrap();
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
    mod page {
        use super::*;
        use crate::error::Error;

        /// Both acquisition forms enforce identity and structure without retaining owners.
        #[test]
        fn acquired_validation_preserves_identity_metadata_and_ownership() {
            let admission = super::admission(8);
            for plaintext in [false, true] {
                for malformed in [false, true] {
                    let mut page = bundle(&admission, "v1");
                    let id = page.plaintext.page().clone();
                    if malformed {
                        page.metadata.length += 1;
                    }
                    let acquired = if plaintext {
                        AcquiredPage::Plaintext(page)
                    } else {
                        AcquiredPage::Ciphertext(UnverifiedPage {
                            copy: page.copy(),
                            disk_token: None,
                        })
                    };
                    let owner = match &acquired {
                        AcquiredPage::Plaintext(page) => &page.ciphertext.inner,
                        AcquiredPage::Ciphertext(page) => &page.copy.ciphertext.inner,
                    };
                    let owners = Arc::strong_count(owner);
                    let expected = if malformed {
                        Err(Error::CorruptRecord)
                    } else {
                        Ok(())
                    };
                    assert_eq!(acquired.validate_for(&id), expected);
                    let mut wrong = id.clone();
                    wrong.number.0 += 1;
                    assert_eq!(acquired.validate_for(&wrong), Err(Error::CorruptRecord));
                    // This checks net retention, not temporary clones during validation.
                    assert_eq!(Arc::strong_count(owner), owners);
                    assert_eq!(acquired.validate_for(&id), expected);
                }
            }
        }

        #[test]
        fn shared_results_reject_truncated_or_padded_ciphertext() {
            let admission = crate::memory::tests::admission(8);
            for length in [0, 18, 19, 20] {
                let mut page = crate::memory::tests::bundle(&admission, "v1");
                std::sync::Arc::get_mut(&mut page.ciphertext.inner)
                    .unwrap()
                    .fixture_resize(length, 0);
                let expected = if length == 19 {
                    Ok(())
                } else {
                    Err(Error::CorruptRecord)
                };
                assert_eq!(page.validate_metadata(), expected);
                assert_eq!(page.copy().validate_metadata(), expected);
            }
        }
        #[test]
        fn shared_result_rejects_mixed_metadata_plaintext_and_ciphertext() {
            let admission = crate::memory::tests::admission(8);
            let bundle = crate::memory::tests::bundle(&admission, "v1");
            let metadata = &bundle.metadata;
            let page = bundle.plaintext.page();
            let envelope = bundle.ciphertext.envelope();
            assert_eq!(validate_association(metadata, page, 3, envelope), Ok(()));
            for case in 0..4 {
                let mut metadata = metadata.clone();
                let mut page = page.clone();
                let mut length = 3;
                match case {
                    0 => length = 2,
                    1 => page.version.etag = crate::model::StrongEtag::test_value("v2"),
                    2 => metadata.version.object.key.0[0] ^= 1,
                    _ => metadata.length = 4,
                }
                assert_eq!(
                    validate_association(&metadata, &page, length, envelope),
                    Err(Error::CorruptRecord)
                );
            }
        }
    }
    pub(in crate::memory) mod pipe_tests {
        use super::*;
        use crate::config::Limits;
        use crate::error::Error;
        use crate::http::acquire_wait;
        use crate::http::new_pipe_pool;
        use crate::runtime::RequestScope;
        use flow_control::Quotas;
        use std::task::Waker;

        pub(crate) fn admission(pipes: usize) -> Rc<Quotas<AdmissionPolicy>> {
            let small = std::num::NonZeroUsize::new(8).unwrap();
            let bytes = std::num::NonZeroUsize::new(32 * 1024 * 1024).unwrap();
            Rc::new(Quotas::new(AdmissionPolicy::new(Limits {
                plaintext_bytes: bytes,
                ciphertext_bytes: bytes,
                dirty_bytes: bytes,
                registered_bytes: bytes,
                request_context_bytes: bytes,
                flights: small,
                waiters_per_flight: small,
                queue_entries: small,
                connections_per_neighbor: small,
                client_connections: small,
                peer_connections: small,
                outbound_connections: small,
                pipes: std::num::NonZeroUsize::new(pipes).unwrap(),
                range_window_pages: small,
                header_bytes: small,
                placement_cache_bytes: small,
                path_cache_bytes: small,
                active_path_searches: small,
                cached_paths: small,
                retained_snapshots: small,
                metadata_entries: small,
                relay_transfers: small,
            })))
        }

        #[test]
        fn immediate_acquisition_does_not_subscribe_to_cancellation() {
            use crate::model::RequestId;
            use std::task::Context;
            use std::task::Poll;
            use std::time::Duration;
            use std::time::Instant;
            let env = racer_io::Env::host();
            let admission = admission(1);
            let pool = new_pipe_pool(admission.clone(), env.sim_os().cloned());
            let scope =
                RequestScope::host(RequestId([0; 16]), Instant::now() + Duration::from_secs(5))
                    .unwrap();
            let mut cx = Context::from_waker(Waker::noop());
            let Poll::Ready(Ok(held)) = acquire_wait(&pool, &scope).as_mut().poll(&mut cx) else {
                panic!("immediate acquisition failed")
            };
            assert_eq!(scope.cancellation.waiters(), 0);
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            assert_eq!(admission.used(ResourceClass::Pipe), 1);
            let mut wait = acquire_wait(&pool, &scope);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            drop(held);
            assert!(matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        }

        #[test]
        fn scheduled_wait_cancellation_deadline_stop_and_abandonment_release_admission() {
            use crate::model::RequestId;
            use crate::test_support::WakeCounter;
            use std::sync::Arc;
            use std::task::Context;
            use std::task::Poll;
            use std::time::Duration;
            use std::time::Instant;
            let env = racer_io::Env::host();
            for failure in [
                Some(Error::Canceled),
                Some(Error::DeadlineExceeded),
                Some(Error::Unavailable),
                None,
            ] {
                let admission = admission(1);
                let pool = new_pipe_pool(admission.clone(), env.sim_os().cloned());
                let held = pool.acquire().unwrap();
                let scope = RequestScope::host(
                    RequestId([0; 16]),
                    Instant::now()
                        + if failure == Some(Error::DeadlineExceeded) {
                            Duration::from_millis(10)
                        } else {
                            Duration::from_secs(5)
                        },
                )
                .unwrap();
                let count = Arc::new(WakeCounter::default());
                let waker = Waker::from(count.clone());
                let mut cx = Context::from_waker(&waker);
                let mut wait = acquire_wait(&pool, &scope);
                assert!(wait.as_mut().poll(&mut cx).is_pending());
                assert_eq!(
                    admission.used(ResourceClass::RequestContext),
                    pool.waiter_bytes()
                );
                match failure {
                    Some(Error::Canceled) => {
                        scope.cancel().unwrap();
                        assert!(count.count() > 0);
                    }
                    Some(Error::DeadlineExceeded) => std::thread::sleep(Duration::from_millis(20)),
                    Some(Error::Unavailable) => admission.stop(),
                    _ => {}
                }
                if let Some(expected) = failure {
                    assert!(
                        matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
                    );
                }
                drop(wait);
                assert_eq!(admission.used(ResourceClass::RequestContext), 0);
                assert_eq!(admission.used(ResourceClass::Pipe), 1);
                drop(held);
                if failure != Some(Error::Unavailable) {
                    // No stale FIFO entry may prevent the next caller's progress.
                    let fresh = RequestScope::host(
                        RequestId([1; 16]),
                        Instant::now() + Duration::from_secs(5),
                    )
                    .unwrap();
                    assert!(matches!(
                        acquire_wait(&pool, &fresh).as_mut().poll(&mut cx),
                        Poll::Ready(Ok(_))
                    ));
                }
                drop(pool);
                assert_eq!(admission.used(ResourceClass::Pipe), 0);
            }
        }
    }
}
