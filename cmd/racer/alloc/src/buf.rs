//! One pooled, aligned, charge-bound buffer type and its per-worker pool.
//!
//! # Model
//!
//! A [`Buf`] owns initialized, stable, aligned bytes plus one live accounting
//! guard ([`Charge`]). Its `wipe` flag marks plaintext: wiped storage is erased
//! with one `explicit_bzero` helper ([`wipe_bytes`]) before it is pooled or
//! freed. A `Buf` holds no `Rc`, so it is `Send` whenever its charge is `Send`
//! and `Sync` whenever its charge is `Sync`. No `unsafe impl Send`/`Sync` is
//! used: the storage pointer lives in an `AtomicPtr`, which carries the
//! thread-safety bounds structurally. `Arc<Buf<C>>` is the shared immutable
//! form (it replaces `FrozenBuffer`).
//!
//! A [`Pool`] is owned by one worker (`take` needs `&mut`), but buffers return
//! to it from any thread, including crypto threads and other workers: each
//! size class has a lock-free multi-producer return stack that only the owner
//! drains (it takes the whole stack at once, so the stack has no ABA hazard).
//! Idle pooled bytes are charged to a dedicated accounting class through
//! [`PoolAccount`] so admission sees them; the live charge of a returning
//! buffer is dropped only after its idle charge is taken (a brief overcount,
//! never an undercount). A failed idle charge frees the storage instead.
//!
//! Non-wiped storage is pooled dirty: returns do not zero or discard it.
//! `MADV_DONTNEED` is applied only by [`Pool::shed`], which the owner calls
//! under memory pressure; discarded mappings stay pooled "cold" and uncharged.
//! Storage of at least 2 MiB still comes from the huge-page mmap path.
//! Disk reads should land in pooled buffers taken from the worker's pool so a
//! hit reuses storage instead of mapping and unmapping a fresh page.
//!
//! # Replaced types
//!
//! Every type in the left column except `AlignedBuffer` and the slab pool,
//! which still back disk write staging, has been removed.
//!
//! | Old | New |
//! |---|---|
//! | `AlignedBuffer<C>`, slab `BufferPool`, `Slab::allocate` free lists | [`Buf<C>`] from [`Pool::take`] |
//! | `FrozenBuffer<C>` | `Arc<Buf<C>>` |
//! | `WorkerBufferPool`, `SharedPool`, `DetachedBuffer`, `give_back` | [`Pool`] with its cross-thread return stacks; a `Buf` is already `Send` |
//! | flow `Recycler`/`Buffers`, `Charge::buffer`, `Charge::recycle`, `PoolDemand` sizing | [`Pool::take`] with the request's charge; [`Pool::shed`] for pressure |
//! | `PoolCharge`, `AlignedBody`, `ChargedBytes`, `ChargedBuffer` (flow and `src`) | [`Buf<Charge<P>>`]; the charge rides in the buffer |
//! | per-allocation `Charge::requires_wipe` | the `wipe` flag at [`Pool::take`] |
//! | three idle charge rules (retained primary charge, recycler bytes, shared budget) | one [`PoolAccount`] class for idle bytes |
//!
//! Wiring changes expected in `src`: `memory.rs` (`rehome` takes a pooled
//! `Buf` at device alignment with padded capacity instead of copying twice),
//! `store.rs` (disk reads `take` from the worker pool; encrypt writes aligned
//! pooled output), and the admission policy gains a `Pool` resource class whose
//! [`PoolAccount`] adapter is `flow_control::QuotaPoolAccount`.
//!
//! [`CiphertextPage`] is the pooled ciphertext shape: `Aligned { buf, offset,
//! len }` or `Registered(R)`, where `R` is the RDMA registered page type from
//! the root crate. `Owned(Vec)` and slab variants fold into `Aligned`.

use crate::sync::{Arc, AtomicBool, AtomicPtr, AtomicUsize, Ordering};
use crate::{Charge, Error, Release, Result, allocate_zeroed};
use std::alloc::{Layout, dealloc};
use std::fmt;
use std::ops::{Deref, DerefMut};

/// Number of distinct capacities a pool can keep; later sizes are unpooled.
pub const MAX_CLASSES: usize = 16;

/// Capacity granule. Capacities round up to this or the alignment, whichever
/// is larger, so near-equal requests share a class.
pub const GRANULE: usize = 4096;

const NO_CLASS: u8 = u8::MAX;

/// Accounting for idle pooled bytes, shared by every thread that returns
/// buffers. Implementations must be cheap and must not block.
pub trait PoolAccount: Send + Sync + 'static {
    /// Charge `bytes` of idle storage. `false` frees the storage instead.
    fn charge_idle(&self, bytes: usize) -> bool;

    /// Release `bytes` previously charged by [`Self::charge_idle`].
    fn release_idle(&self, bytes: usize);
}

/// Opt out of idle accounting.
impl PoolAccount for () {
    fn charge_idle(&self, _bytes: usize) -> bool {
        true
    }

    fn release_idle(&self, _bytes: usize) {}
}

/// A standalone idle-byte budget, for tests and callers without admission.
#[derive(Debug)]
pub struct IdleLimit {
    limit: usize,

    used: AtomicUsize,
}

impl IdleLimit {
    /// Allow at most `limit` idle bytes.
    pub fn new(limit: usize) -> Self {
        Self {
            limit,
            used: AtomicUsize::new(0),
        }
    }

    /// Currently charged idle bytes.
    pub fn used(&self) -> usize {
        self.used.load(Ordering::Acquire)
    }
}

impl PoolAccount for IdleLimit {
    fn charge_idle(&self, bytes: usize) -> bool {
        let mut used = self.used.load(Ordering::Relaxed);
        loop {
            let Some(next) = used.checked_add(bytes).filter(|n| *n <= self.limit) else {
                return false;
            };
            match self
                .used
                .compare_exchange_weak(used, next, Ordering::AcqRel, Ordering::Relaxed)
            {
                Ok(_) => return true,
                Err(actual) => used = actual,
            }
        }
    }

    fn release_idle(&self, bytes: usize) {
        let previous = self.used.fetch_sub(bytes, Ordering::AcqRel);
        debug_assert!(previous >= bytes, "idle release exceeds charge");
    }
}

impl<A: PoolAccount> PoolAccount for std::sync::Arc<A> {
    fn charge_idle(&self, bytes: usize) -> bool {
        (**self).charge_idle(bytes)
    }

    fn release_idle(&self, bytes: usize) {
        (**self).release_idle(bytes)
    }
}

/// Erase secrets so the stores cannot be optimized away.
pub fn wipe_bytes(bytes: &mut [u8]) {
    #[cfg(all(
        target_os = "linux",
        any(target_env = "gnu", target_env = "musl"),
        not(miri)
    ))]
    // SAFETY: `bytes` is a live exclusive slice of initialized memory.
    unsafe {
        libc::explicit_bzero(bytes.as_mut_ptr().cast(), bytes.len())
    };
    #[cfg(not(all(
        target_os = "linux",
        any(target_env = "gnu", target_env = "musl"),
        not(miri)
    )))]
    {
        use zeroize::Zeroize;
        bytes.zeroize();
    }
}

/// Raw aligned storage plus its pool bookkeeping. Boxed once per allocation
/// and reused with it, so pooling never allocates.
struct Region {
    // Exclusive owner of the bytes. `AtomicPtr` (always std, never loom: it is
    // only accessed by the exclusive owner or through `&self` reads) makes the
    // region structurally `Send + Sync` like `Box<[u8]>`.
    pointer: std::sync::atomic::AtomicPtr<u8>,

    capacity: usize,

    align: usize,

    release: Release,

    // All bytes are zero.
    clean: bool,

    // Holds an idle charge of `capacity` bytes.
    charged: bool,

    class: u8,

    next: AtomicPtr<Region>,
}

impl Region {
    fn allocate(capacity: usize, align: usize, class: u8) -> Result<Box<Self>> {
        let layout =
            Layout::from_size_align(capacity, align).map_err(|_| Error::InvalidConfiguration)?;
        let (pointer, release) = allocate_zeroed(layout)?;
        Ok(Box::new(Self {
            pointer: std::sync::atomic::AtomicPtr::new(pointer.as_ptr()),
            capacity,
            align,
            release,
            clean: true,
            charged: false,
            class,
            next: AtomicPtr::new(std::ptr::null_mut()),
        }))
    }

    fn bytes(&self) -> &[u8] {
        // SAFETY: `capacity` initialized bytes live until Drop; shared borrow.
        unsafe { std::slice::from_raw_parts(self.pointer.load(Ordering::Relaxed), self.capacity) }
    }

    fn bytes_mut(&mut self) -> &mut [u8] {
        self.clean = false;
        // SAFETY: exclusive borrow of `capacity` initialized live bytes.
        unsafe { std::slice::from_raw_parts_mut(*self.pointer.get_mut(), self.capacity) }
    }

    fn wipe(&mut self) {
        if !self.clean {
            wipe_bytes(self.bytes_mut());
            self.clean = true;
        }
    }

    fn zero(&mut self) {
        if !self.clean {
            self.bytes_mut().fill(0);
            self.clean = true;
        }
    }

    /// Return physical pages to the kernel, keeping the mapping. Only mmap
    /// storage supports this; heap storage reports `false`.
    fn discard(&mut self) -> bool {
        #[cfg(all(target_os = "linux", not(miri)))]
        if let Release::Mapping(length) = self.release {
            // SAFETY: exclusive anonymous private mapping with no I/O in flight;
            // DONTNEED replaces contents with demand-zero pages.
            let pointer = *self.pointer.get_mut();
            if unsafe { libc::madvise(pointer.cast(), length, libc::MADV_DONTNEED) } == 0 {
                self.clean = true;
                return true;
            }
        }
        false
    }
}

impl Drop for Region {
    fn drop(&mut self) {
        let pointer = *self.pointer.get_mut();
        match self.release {
            // SAFETY: allocated by `allocate_zeroed` with this exact layout.
            Release::Heap => unsafe {
                dealloc(
                    pointer,
                    Layout::from_size_align_unchecked(self.capacity, self.align),
                )
            },
            #[cfg(all(target_os = "linux", not(miri)))]
            // SAFETY: one munmap of the exact trimmed mapping.
            Release::Mapping(length) => unsafe {
                libc::munmap(pointer.cast(), length);
            },
        }
    }
}

/// One capacity class: owner-assigned capacity plus a cross-thread return stack.
struct Class {
    capacity: AtomicUsize,

    head: AtomicPtr<Region>,

    // Regions idle in this class, in the owner's list or the return stack.
    idle: AtomicUsize,
}

impl Class {
    fn new() -> Self {
        Self {
            capacity: AtomicUsize::new(0),
            head: AtomicPtr::new(std::ptr::null_mut()),
            idle: AtomicUsize::new(0),
        }
    }

    fn push(&self, region: Box<Region>) {
        let raw = Box::into_raw(region);
        let mut head = self.head.load(Ordering::Relaxed);
        loop {
            // SAFETY: `raw` is exclusively ours until the CAS publishes it.
            unsafe { (*raw).next.store(head, Ordering::Relaxed) };
            match self
                .head
                .compare_exchange_weak(head, raw, Ordering::Release, Ordering::Relaxed)
            {
                Ok(_) => return,
                Err(actual) => head = actual,
            }
        }
    }

    /// Take the whole stack. Acquire pairs with each push's Release.
    fn take_all(&self, mut out: impl FnMut(Box<Region>)) {
        let mut cursor = self.head.swap(std::ptr::null_mut(), Ordering::Acquire);
        while !cursor.is_null() {
            // SAFETY: every node was leaked by `push`; swapping the head out
            // gives this thread exclusive ownership of the whole chain.
            let region = unsafe { Box::from_raw(cursor) };
            cursor = region.next.load(Ordering::Relaxed);
            out(region);
        }
    }
}

/// Pool state reachable from returning buffers on any thread.
struct Shared {
    account: Box<dyn PoolAccount>,

    closed: AtomicBool,

    // Charged idle bytes.
    idle_bytes: AtomicUsize,

    max_idle: usize,

    classes: [Class; MAX_CLASSES],
}

impl Shared {
    fn uncharge(&self, region: &mut Region) {
        if region.charged {
            region.charged = false;
            self.idle_bytes
                .fetch_sub(region.capacity, Ordering::Relaxed);
            self.account.release_idle(region.capacity);
        }
    }

    /// Pool a returning region, or hand it back to be freed.
    fn give_back(&self, mut region: Box<Region>) -> Option<Box<Region>> {
        let class = self.classes.get(usize::from(region.class))?;
        if self.closed.load(Ordering::Acquire) {
            return Some(region);
        }
        if class.idle.fetch_add(1, Ordering::AcqRel) >= self.max_idle {
            class.idle.fetch_sub(1, Ordering::AcqRel);
            return Some(region);
        }
        if !self.account.charge_idle(region.capacity) {
            class.idle.fetch_sub(1, Ordering::AcqRel);
            return Some(region);
        }
        region.charged = true;
        self.idle_bytes
            .fetch_add(region.capacity, Ordering::Relaxed);
        class.push(region);
        None
    }
}

impl Drop for Shared {
    /// Free regions returned after the owner closed the pool.
    fn drop(&mut self) {
        for class in &self.classes {
            class.take_all(|mut region| self.uncharge(&mut region));
        }
    }
}

/// Construction parameters for a [`Pool`].
#[derive(Clone, Copy, Debug)]
pub struct PoolConfig {
    /// Memory alignment of every buffer; a power of two.
    pub alignment: usize,

    /// Idle buffers kept per capacity class.
    pub max_idle_per_class: usize,
}

/// A worker's buffer pool. See the module docs.
pub struct Pool {
    shared: Arc<Shared>,

    // Boxed: regions are intrusive stack nodes whose address must not change.
    #[allow(clippy::vec_box)]
    local: [Vec<Box<Region>>; MAX_CLASSES],

    alignment: usize,

    granule: usize,

    // Buffers checked out since creation, for demand-driven idle trimming.
    takes: u64,
}

impl fmt::Debug for Pool {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Pool")
            .field("alignment", &self.alignment)
            .field("idle_bytes", &self.idle_bytes())
            .finish_non_exhaustive()
    }
}

impl Pool {
    /// Create an empty pool whose idle bytes are charged to `account`.
    pub fn new(config: PoolConfig, account: impl PoolAccount) -> Result<Self> {
        if !config.alignment.is_power_of_two() || config.alignment > crate::HUGE_PAGE_BYTES {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            shared: Arc::new(Shared {
                account: Box::new(account),
                closed: AtomicBool::new(false),
                idle_bytes: AtomicUsize::new(0),
                max_idle: config.max_idle_per_class,
                classes: std::array::from_fn(|_| Class::new()),
            }),
            local: std::array::from_fn(|_| Vec::new()),
            alignment: config.alignment,
            granule: config.alignment.max(GRANULE),
            takes: 0,
        })
    }

    /// Buffers checked out since creation. Owners compare samples across a
    /// maintenance window to tell an idle pool from one in use.
    pub fn takes(&self) -> u64 {
        self.takes
    }

    /// Memory alignment of every buffer from this pool.
    pub fn alignment(&self) -> usize {
        self.alignment
    }

    /// Charged idle bytes, including buffers returned by other threads.
    pub fn idle_bytes(&self) -> usize {
        self.shared.idle_bytes.load(Ordering::Relaxed)
    }

    /// Capacity a buffer of `len` bytes occupies.
    pub fn capacity_for(&self, len: usize) -> Result<usize> {
        len.max(1)
            .checked_next_multiple_of(self.granule)
            .filter(|n| *n <= crate::Alignment::MAX_TRANSFER_LENGTH)
            .ok_or(Error::InvalidConfiguration)
    }

    fn class_for(&self, capacity: usize) -> u8 {
        let mut free = None;
        for (index, class) in self.shared.classes.iter().enumerate() {
            match class.capacity.load(Ordering::Relaxed) {
                c if c == capacity => return index as u8,
                0 if free.is_none() => free = Some(index),
                _ => {}
            }
        }
        match free {
            Some(index) => {
                self.shared.classes[index]
                    .capacity
                    .store(capacity, Ordering::Relaxed);
                index as u8
            }
            None => NO_CLASS,
        }
    }

    fn pop(&mut self, class: u8) -> Option<Box<Region>> {
        let index = usize::from(class);
        if self.local[index].is_empty() {
            let local = &mut self.local[index];
            self.shared.classes[index].take_all(|region| local.push(region));
        }
        let mut region = self.local[index].pop()?;
        self.shared.classes[index]
            .idle
            .fetch_sub(1, Ordering::AcqRel);
        self.shared.uncharge(&mut region);
        Some(region)
    }

    /// Take a buffer of `len` bytes bound to `charge`, which must cover the
    /// full capacity. Contents are unspecified (never another buffer's wiped
    /// plaintext); use [`Self::take_zeroed`] when zeros are required.
    pub fn take<C: Charge>(&mut self, len: usize, charge: C, wipe: bool) -> Result<Buf<C>> {
        let capacity = self.capacity_for(len)?;
        if !charge.covers(capacity) {
            return Err(Error::InvalidConfiguration);
        }
        let class = self.class_for(capacity);
        self.takes = self.takes.wrapping_add(1);
        let region = match (class != NO_CLASS).then(|| self.pop(class)).flatten() {
            Some(region) => region,
            None => Region::allocate(capacity, self.alignment, class)?,
        };
        Ok(Buf {
            region: Some(region),
            len,
            wipe,
            pool: (class != NO_CLASS).then(|| self.shared.clone()),
            charge,
        })
    }

    /// [`Self::take`], with every byte zero.
    pub fn take_zeroed<C: Charge>(&mut self, len: usize, charge: C, wipe: bool) -> Result<Buf<C>> {
        let mut buf = self.take(len, charge, wipe)?;
        buf.region_mut().zero();
        Ok(buf)
    }

    fn drain_returns(&mut self) {
        for (index, class) in self.shared.classes.iter().enumerate() {
            let local = &mut self.local[index];
            class.take_all(|region| local.push(region));
        }
    }

    /// Under memory pressure, release at least `bytes` of charged idle memory
    /// where possible. Mappings are discarded with `MADV_DONTNEED` and kept
    /// cold (uncharged); heap storage is freed. Returns bytes released.
    pub fn shed(&mut self, bytes: usize) -> usize {
        self.drain_returns();
        let mut shed = 0;
        for index in 0..MAX_CLASSES {
            let mut position = 0;
            while shed < bytes && position < self.local[index].len() {
                let region = &mut self.local[index][position];
                if !region.charged {
                    position += 1;
                    continue;
                }
                shed += region.capacity;
                if region.discard() {
                    self.shared.uncharge(region);
                    position += 1;
                } else {
                    let mut region = self.local[index].swap_remove(position);
                    self.shared.classes[index]
                        .idle
                        .fetch_sub(1, Ordering::AcqRel);
                    self.shared.uncharge(&mut region);
                }
            }
        }
        shed
    }

    /// Free every idle buffer.
    pub fn clear(&mut self) {
        self.drain_returns();
        for (index, local) in self.local.iter_mut().enumerate() {
            for mut region in local.drain(..) {
                self.shared.classes[index]
                    .idle
                    .fetch_sub(1, Ordering::AcqRel);
                self.shared.uncharge(&mut region);
            }
        }
    }
}

impl Drop for Pool {
    /// Close to new returns, then free idle storage. Returns that raced the
    /// close are freed when the last buffer releases the shared state.
    fn drop(&mut self) {
        self.shared.closed.store(true, Ordering::Release);
        self.clear();
    }
}

/// Aligned, initialized, stable storage bound to a live charge. See the
/// module docs.
#[must_use]
pub struct Buf<C: Charge = ()> {
    // Some until Drop or a consuming conversion.
    region: Option<Box<Region>>,

    len: usize,

    wipe: bool,

    pool: Option<Arc<Shared>>,

    // Declared last so it drops after the region was pooled or freed.
    charge: C,
}

impl<C: Charge> fmt::Debug for Buf<C> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Buf")
            .field("len", &self.len)
            .field("capacity", &self.capacity())
            .field("wipe", &self.wipe)
            .field("pooled", &self.pool.is_some())
            .finish_non_exhaustive()
    }
}

impl<C: Charge> Buf<C> {
    /// Allocate unpooled zeroed storage, for callers without a worker pool.
    pub fn unpooled(len: usize, alignment: usize, charge: C, wipe: bool) -> Result<Self> {
        if !alignment.is_power_of_two() || alignment > crate::HUGE_PAGE_BYTES {
            return Err(Error::InvalidConfiguration);
        }
        let capacity = len
            .max(1)
            .checked_next_multiple_of(alignment.max(GRANULE))
            .ok_or(Error::InvalidConfiguration)?;
        if !charge.covers(capacity) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            region: Some(Region::allocate(capacity, alignment, NO_CLASS)?),
            len,
            wipe,
            pool: None,
            charge,
        })
    }

    fn region(&self) -> &Region {
        self.region.as_deref().expect("live buffer")
    }

    fn region_mut(&mut self) -> &mut Region {
        self.region.as_deref_mut().expect("live buffer")
    }

    /// Logical length.
    pub fn len(&self) -> usize {
        self.len
    }

    /// Whether the logical length is zero.
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// Allocated bytes, at least `len`.
    pub fn capacity(&self) -> usize {
        self.region().capacity
    }

    /// Memory alignment of the first byte.
    pub fn alignment(&self) -> usize {
        self.region().align
    }

    /// Whether storage is wiped before pooling or freeing.
    pub fn wipes(&self) -> bool {
        self.wipe
    }

    /// Whether this buffer returns to a pool on drop.
    pub fn is_pooled(&self) -> bool {
        self.pool.is_some()
    }

    /// The live accounting guard.
    pub fn charge(&self) -> &C {
        &self.charge
    }

    /// Change the logical length within capacity. Bytes stay initialized.
    pub fn set_len(&mut self, len: usize) -> Result<()> {
        if len > self.capacity() {
            return Err(Error::InvalidConfiguration);
        }
        self.len = len;
        Ok(())
    }

    /// The whole initialized capacity, for I/O that fills past `len`.
    pub fn spare_mut(&mut self) -> &mut [u8] {
        self.region_mut().bytes_mut()
    }

    /// Rebind to a new charge that covers the capacity, returning the old one.
    pub fn replace_charge(&mut self, charge: C) -> std::result::Result<C, C> {
        if !charge.covers(self.capacity()) {
            return Err(charge);
        }
        Ok(std::mem::replace(&mut self.charge, charge))
    }

    /// Move the buffer to a new charge of another type, which must cover the
    /// full capacity. On refusal both are handed back unchanged.
    pub fn rebind<D: Charge>(mut self, charge: D) -> std::result::Result<(Buf<D>, C), (Self, D)> {
        if !charge.covers(self.capacity()) {
            return Err((self, charge));
        }
        let region = self.region.take();
        let pool = self.pool.take();
        let (len, wipe) = (self.len, self.wipe);
        let shell = std::mem::ManuallyDrop::new(self);
        // SAFETY: the shell is never dropped and its other fields are now
        // empty, so moving the charge out leaves nothing to release twice.
        let old = unsafe { std::ptr::read(&shell.charge) };
        Ok((
            Buf {
                region,
                len,
                wipe,
                pool,
                charge,
            },
            old,
        ))
    }
}

impl<C: Charge> Deref for Buf<C> {
    type Target = [u8];

    fn deref(&self) -> &[u8] {
        &self.region().bytes()[..self.len]
    }
}

impl<C: Charge> DerefMut for Buf<C> {
    fn deref_mut(&mut self) -> &mut [u8] {
        let len = self.len;
        &mut self.region_mut().bytes_mut()[..len]
    }
}

impl<C: Charge> Drop for Buf<C> {
    fn drop(&mut self) {
        let Some(mut region) = self.region.take() else {
            return;
        };
        if self.wipe {
            region.wipe();
        }
        // A region the pool refuses is freed here, before the charge drops.
        if let Some(pool) = self.pool.take() {
            drop(pool.give_back(region));
        }
    }
}

// SAFETY: the bytes are initialized, stable, and live until Drop, and the
// mutable view requires exclusive access.
unsafe impl<C: Charge> racer_io::ring::IoBuffer for Buf<C> {
    fn bytes(&self) -> racer_io::Result<&[u8]> {
        Ok(self)
    }

    fn bytes_mut(&mut self) -> racer_io::Result<&mut [u8]> {
        Ok(self)
    }
}

/// Bytes of a registered (RDMA) page. Implemented by the root crate.
pub trait RegisteredPage {
    /// The page's ciphertext.
    fn bytes(&self) -> &[u8];
}

/// Placeholder for callers without registered pages.
#[derive(Debug)]
pub enum NoRegistration {}

impl RegisteredPage for NoRegistration {
    fn bytes(&self) -> &[u8] {
        match *self {}
    }
}

enum PageRepr<C: Charge, R> {
    Aligned { buf: Buf<C>, offset: u32, len: u32 },
    Registered(R),
}

/// Immutable page ciphertext in pooled aligned storage or registered memory.
pub struct CiphertextPage<C: Charge = (), R = NoRegistration> {
    repr: PageRepr<C, R>,
}

impl<C: Charge, R: RegisteredPage> CiphertextPage<C, R> {
    /// Ciphertext at `offset..offset + len` of `buf`, which must be in bounds.
    pub fn aligned(buf: Buf<C>, offset: u32, len: u32) -> Result<Self> {
        let end = (offset as usize)
            .checked_add(len as usize)
            .ok_or(Error::InvalidConfiguration)?;
        if end > buf.len() {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            repr: PageRepr::Aligned { buf, offset, len },
        })
    }

    /// Ciphertext in a registered page.
    pub fn registered(page: R) -> Self {
        Self {
            repr: PageRepr::Registered(page),
        }
    }

    /// The ciphertext bytes.
    pub fn bytes(&self) -> &[u8] {
        match &self.repr {
            PageRepr::Aligned { buf, offset, len } => {
                &buf[*offset as usize..*offset as usize + *len as usize]
            }
            PageRepr::Registered(page) => page.bytes(),
        }
    }

    /// The backing pooled buffer, when aligned.
    pub fn buf(&self) -> Option<&Buf<C>> {
        match &self.repr {
            PageRepr::Aligned { buf, .. } => Some(buf),
            PageRepr::Registered(_) => None,
        }
    }

    /// The registered page, when registered.
    pub fn registered_page(&self) -> Option<&R> {
        match &self.repr {
            PageRepr::Aligned { .. } => None,
            PageRepr::Registered(page) => Some(page),
        }
    }
}

#[cfg(all(test, not(loom)))]
mod tests {
    use super::*;
    use std::sync::Mutex;

    /// Charge that logs its drop, to check ordering against idle charging.
    struct Logged {
        bytes: usize,
        log: std::sync::Arc<Mutex<Vec<&'static str>>>,
    }

    impl Charge for Logged {
        fn covers(&self, bytes: usize) -> bool {
            self.bytes >= bytes
        }
    }

    impl Drop for Logged {
        fn drop(&mut self) {
            self.log.lock().unwrap().push("live released");
        }
    }

    struct LoggedAccount(std::sync::Arc<Mutex<Vec<&'static str>>>);

    impl PoolAccount for LoggedAccount {
        fn charge_idle(&self, _bytes: usize) -> bool {
            self.0.lock().unwrap().push("idle charged");
            true
        }

        fn release_idle(&self, _bytes: usize) {
            self.0.lock().unwrap().push("idle released");
        }
    }

    /// A charge with a fixed cover.
    #[derive(Debug)]
    struct Bytes(usize);

    impl Charge for Bytes {
        fn covers(&self, bytes: usize) -> bool {
            self.0 >= bytes
        }
    }

    fn pool(limit: &std::sync::Arc<IdleLimit>) -> Pool {
        Pool::new(
            PoolConfig {
                alignment: 512,
                max_idle_per_class: 4,
            },
            limit.clone(),
        )
        .unwrap()
    }

    #[test]
    fn take_reports_geometry() {
        let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
        let mut pool = pool(&limit);
        let buf = pool.take(5000, (), false).unwrap();
        assert_eq!(buf.len(), 5000);
        assert_eq!(buf.capacity(), 8192);
        assert_eq!(buf.alignment(), 512);
        assert_eq!(buf.as_ptr() as usize % 512, 0);
        assert!(buf.is_pooled());
        let empty = pool.take(0, (), false).unwrap();
        assert!(empty.is_empty());
        assert_eq!(empty.capacity(), GRANULE);
    }

    #[test]
    fn takes_counts_checkouts() {
        let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
        let mut pool = pool(&limit);
        assert_eq!(pool.takes(), 0);
        drop(pool.take(10, (), false).unwrap());
        drop(pool.take(10, (), false).unwrap());
        assert_eq!(pool.takes(), 2);
        // A refused charge is not a checkout.
        assert!(pool.take(4097, Bytes(4096), false).is_err());
        assert_eq!(pool.takes(), 2);
    }

    #[test]
    fn rejects_bad_configuration() {
        assert!(
            Pool::new(
                PoolConfig {
                    alignment: 3,
                    max_idle_per_class: 1
                },
                ()
            )
            .is_err()
        );
        assert!(Buf::unpooled(1, 3, (), false).is_err());
        let mut pool = Pool::new(
            PoolConfig {
                alignment: 64,
                max_idle_per_class: 1,
            },
            (),
        )
        .unwrap();
        assert_eq!(
            pool.take(4097, Bytes(4096), false).unwrap_err(),
            Error::InvalidConfiguration
        );
        assert!(pool.take(4096, Bytes(4096), false).is_ok());
        assert!(pool.take(usize::MAX, (), false).is_err());
    }

    #[test]
    fn reuses_storage_and_accounts_idle() {
        let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
        let mut pool = pool(&limit);
        let buf = pool.take(4096, (), false).unwrap();
        let address = buf.as_ptr();
        drop(buf);
        assert_eq!(limit.used(), 4096);
        assert_eq!(pool.idle_bytes(), 4096);
        let buf = pool.take(100, (), false).unwrap();
        assert_eq!(buf.as_ptr(), address);
        assert_eq!(limit.used(), 0);
        drop(buf);
        pool.clear();
        assert_eq!(limit.used(), 0);
        assert_eq!(pool.idle_bytes(), 0);
    }

    #[test]
    fn idle_charge_precedes_live_release() {
        let log = std::sync::Arc::new(Mutex::new(Vec::new()));
        let mut pool = Pool::new(
            PoolConfig {
                alignment: 64,
                max_idle_per_class: 1,
            },
            LoggedAccount(log.clone()),
        )
        .unwrap();
        let buf = pool
            .take(
                10,
                Logged {
                    bytes: 4096,
                    log: log.clone(),
                },
                false,
            )
            .unwrap();
        drop(buf);
        assert_eq!(*log.lock().unwrap(), ["idle charged", "live released"]);
        drop(pool);
        assert_eq!(log.lock().unwrap().last(), Some(&"idle released"));
    }

    #[test]
    fn wipes_plaintext_before_reuse() {
        let mut pool = Pool::new(
            PoolConfig {
                alignment: 64,
                max_idle_per_class: 1,
            },
            (),
        )
        .unwrap();
        let mut buf = pool.take(4096, (), true).unwrap();
        buf.fill(0xAB);
        let address = buf.as_ptr();
        drop(buf);
        let buf = pool.take(4096, (), false).unwrap();
        assert_eq!(buf.as_ptr(), address);
        assert!(buf.iter().all(|b| *b == 0));
    }

    #[test]
    fn ciphertext_returns_dirty_and_take_zeroed_clears() {
        let mut pool = Pool::new(
            PoolConfig {
                alignment: 64,
                max_idle_per_class: 1,
            },
            (),
        )
        .unwrap();
        let mut buf = pool.take(4096, (), false).unwrap();
        buf.fill(7);
        drop(buf);
        let buf = pool.take(4096, (), false).unwrap();
        assert!(buf.iter().all(|b| *b == 7), "returns do not zero");
        drop(buf);
        let buf = pool.take_zeroed(4096, (), false).unwrap();
        assert!(buf.iter().all(|b| *b == 0));
    }

    #[test]
    fn frees_when_idle_budget_or_depth_is_exhausted() {
        let limit = std::sync::Arc::new(IdleLimit::new(4096));
        let mut pool = pool(&limit);
        let a = pool.take(4096, (), false).unwrap();
        let b = pool.take(4096, (), false).unwrap();
        drop(a);
        drop(b);
        assert_eq!(limit.used(), 4096, "second return exceeds the budget");
        assert_eq!(pool.idle_bytes(), 4096);

        let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
        let mut pool = self::pool(&limit);
        let bufs: Vec<_> = (0..6)
            .map(|_| pool.take(4096, (), false).unwrap())
            .collect();
        drop(bufs);
        assert_eq!(limit.used(), 4 * 4096, "depth caps idle buffers");
    }

    #[test]
    fn class_table_overflow_is_unpooled() {
        let mut pool = Pool::new(
            PoolConfig {
                alignment: 64,
                max_idle_per_class: 1,
            },
            (),
        )
        .unwrap();
        for class in 0..MAX_CLASSES {
            assert!(
                pool.take((class + 1) * GRANULE, (), false)
                    .unwrap()
                    .is_pooled()
            );
        }
        let extra = pool.take((MAX_CLASSES + 1) * GRANULE, (), false).unwrap();
        assert!(!extra.is_pooled());
    }

    #[test]
    fn cross_thread_return() {
        let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
        let mut pool = pool(&limit);
        let buf = pool.take(4096, (), true).unwrap();
        let address = buf.as_ptr() as usize;
        std::thread::spawn(move || drop(buf)).join().unwrap();
        assert_eq!(limit.used(), 4096);
        let buf = pool.take(4096, (), false).unwrap();
        assert_eq!(buf.as_ptr() as usize, address);
    }

    #[test]
    fn buffer_outlives_pool() {
        let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
        let mut pool = pool(&limit);
        let buf = pool.take(4096, (), false).unwrap();
        let idle = pool.take(4096, (), false).unwrap();
        drop(idle);
        drop(pool);
        assert_eq!(limit.used(), 0);
        drop(buf);
        assert_eq!(limit.used(), 0);
    }

    #[test]
    fn shed_releases_charged_idle() {
        let limit = std::sync::Arc::new(IdleLimit::new(1 << 30));
        let mut pool = pool(&limit);
        let bufs: Vec<_> = (0..3)
            .map(|_| pool.take(4096, (), false).unwrap())
            .collect();
        drop(bufs);
        assert_eq!(pool.shed(5000), 8192);
        assert_eq!(limit.used(), 4096);
        assert_eq!(pool.shed(usize::MAX), 4096);
        assert_eq!(limit.used(), 0);
        assert_eq!(pool.shed(1), 0);
    }

    #[cfg(not(miri))]
    #[test]
    fn huge_buffers_are_mapped_and_discarded_cold() {
        let limit = std::sync::Arc::new(IdleLimit::new(1 << 30));
        let mut pool = pool(&limit);
        let mut buf = pool.take(crate::HUGE_PAGE_BYTES, (), false).unwrap();
        assert_eq!(buf.as_ptr() as usize % crate::HUGE_PAGE_BYTES, 0);
        buf.fill(9);
        let address = buf.as_ptr();
        drop(buf);
        assert_eq!(pool.shed(1), crate::HUGE_PAGE_BYTES);
        assert_eq!(limit.used(), 0);
        let buf = pool.take(crate::HUGE_PAGE_BYTES, (), false).unwrap();
        assert_eq!(buf.as_ptr(), address, "cold mapping stays pooled");
        assert!(buf.iter().all(|b| *b == 0));
        assert_eq!(limit.used(), 0);
    }

    #[test]
    fn set_len_and_rebind() {
        let mut buf = Buf::unpooled(10, 64, Bytes(4096), false).unwrap();
        assert!(!buf.is_pooled());
        assert!(buf.set_len(4096).is_ok());
        assert_eq!(buf.set_len(4097), Err(Error::InvalidConfiguration));
        buf.spare_mut()[4095] = 1;
        assert_eq!(buf[4095], 1);
        let buf = buf.rebind(Bytes(10)).unwrap_err().0;
        let (buf, old) = buf.rebind(Bytes(8192)).ok().unwrap();
        assert_eq!(old.0, 4096);
        assert_eq!(buf.charge().0, 8192);
        assert_eq!(buf[4095], 1);
    }

    #[test]
    fn ciphertext_page_bounds() {
        let mut buf = Buf::unpooled(16, 64, (), false).unwrap();
        buf.copy_from_slice(&[1; 16]);
        assert!(
            CiphertextPage::<(), NoRegistration>::aligned(
                Buf::unpooled(16, 64, (), false).unwrap(),
                8,
                9
            )
            .is_err()
        );
        let page = CiphertextPage::<(), NoRegistration>::aligned(buf, 4, 8).unwrap();
        assert_eq!(page.bytes(), &[1; 8]);
        assert!(page.buf().is_some() && page.registered_page().is_none());

        struct Reg(Vec<u8>);
        impl RegisteredPage for Reg {
            fn bytes(&self) -> &[u8] {
                &self.0
            }
        }
        let page = CiphertextPage::<(), Reg>::registered(Reg(vec![2; 3]));
        assert_eq!(page.bytes(), &[2; 3]);
        assert!(page.buf().is_none());
    }

    #[test]
    fn send_and_sync_follow_the_charge() {
        fn send<T: Send>() {}
        fn sync<T: Sync>() {}
        send::<Buf<()>>();
        sync::<Buf<()>>();
        send::<Pool>();
        send::<CiphertextPage<()>>();
    }
}

/// Loom models of cross-thread returns. Run with `--cfg loom`.
#[cfg(all(test, loom))]
mod loom_tests {
    use super::*;

    fn pool(limit: &std::sync::Arc<IdleLimit>, depth: usize) -> Pool {
        Pool::new(
            PoolConfig {
                alignment: 64,
                max_idle_per_class: depth,
            },
            limit.clone(),
        )
        .unwrap()
    }

    #[test]
    fn concurrent_return_and_take() {
        loom::model(|| {
            let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
            let mut pool = pool(&limit, 4);
            let a = pool.take(64, (), true).unwrap();
            let b = pool.take(64, (), false).unwrap();
            let thread = loom::thread::spawn(move || drop(a));
            drop(b);
            let c = pool.take(64, (), false).unwrap();
            thread.join().unwrap();
            drop(c);
            pool.clear();
            assert_eq!(limit.used(), 0);
            assert_eq!(pool.idle_bytes(), 0);
        });
    }

    #[test]
    fn two_returners_respect_depth() {
        loom::model(|| {
            let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
            let mut pool = pool(&limit, 1);
            let a = pool.take(64, (), false).unwrap();
            let b = pool.take(64, (), false).unwrap();
            let t = loom::thread::spawn(move || drop(a));
            drop(b);
            t.join().unwrap();
            assert_eq!(limit.used(), GRANULE);
            drop(pool);
            assert_eq!(limit.used(), 0);
        });
    }

    #[test]
    fn return_races_pool_drop() {
        loom::model(|| {
            let limit = std::sync::Arc::new(IdleLimit::new(1 << 20));
            let mut pool = pool(&limit, 2);
            let a = pool.take(64, (), false).unwrap();
            let t = loom::thread::spawn(move || drop(a));
            drop(pool);
            t.join().unwrap();
            assert_eq!(limit.used(), 0);
        });
    }
}
