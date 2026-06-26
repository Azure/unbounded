// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-shard [`BlockStore`] view over a hot-swappable
//! [`DiskChannelDirectory`].
//!
//! Each shard holds a [`LiveShardLocalStore`] and registers its
//! NUMA-local backing through [`Self::register_backing`]. The view
//! resolves the `PageRef`s emitted by its `Pool` to absolute byte
//! ranges in that backing, picks the owning disk with
//! [`crate::storage::local::disk_for`], and ships the raw `(ptr, len)`
//! to that disk's storage core over its [`PageChannel`]. The engine
//! and its ring run entirely on the storage core; the shard never
//! touches the device directly.
//!
//! On every `BlockStore` call the view loads the directory's published
//! `(channels, generation)` pair atomically and compares the
//! generation against the one it last replayed against; on a mismatch
//! it re-registers every recorded backing against every channel in
//! the newly-published set before delegating. Because the snapshot
//! and generation come from a single `ArcSwap` load, "last_seen ==
//! N" always means the recorded set was registered against the
//! snapshot published as gen N.
//!
//! When the directory has no channels published (no projected cache disks)
//! reads and writes return [`Error::Transport`] - the data path is
//! offline by definition.

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::Mutex;

use crate::bufferpool::{BlockStore, Error, PageCachePolicy, PageRef, Req};
use crate::storage::PageChannel;
use crate::storage::disk_for;

use super::channels::DiskChannelDirectory;

/// `BlockStore` that forwards to whichever per-disk [`PageChannel`]
/// set is currently published by the directory, replaying buffer
/// registrations whenever the directory generation advances.
pub struct LiveShardLocalStore {
    directory: Arc<DiskChannelDirectory>,
    state: Mutex<ReplayState>,
}

/// Hot-swappable set of cache directories keyed by cache id.
pub struct CacheDirectorySet {
    directories: Mutex<HashMap<String, CacheDirectoryEntry>>,
}

#[derive(Clone)]
struct CacheDirectoryEntry {
    directory: Arc<DiskChannelDirectory>,
    priority: i32,
}

/// Chain-aware `BlockStore` dispatcher. Requests without a cache id
/// always miss and drop writeback; requests with a cache id use that
/// cache's own [`DiskChannelDirectory`] and replay state.
pub struct ChainLocalStore {
    directories: Arc<CacheDirectorySet>,
    stores: Mutex<HashMap<String, StoreEntry>>,
    registered: Mutex<Vec<RegisteredBacking>>,
}

struct StoreEntry {
    directory: Arc<DiskChannelDirectory>,
    store: Arc<LiveShardLocalStore>,
}

struct RegisteredBacking {
    base: *mut u8,
    page_size: usize,
    page_count: usize,
    keepalive: Arc<dyn Send + Sync>,
}

/// Registered backings plus the directory generation we last replayed
/// against. Kept together behind one mutex so a concurrent
/// `apply_channels` cannot wedge an updated generation between an
/// in-flight registration loop and the bookkeeping that records it.
struct ReplayState {
    registered: Vec<ShardBacking>,
    last_seen_generation: Option<u64>,
}

#[derive(Copy, Clone)]
struct ShardBacking {
    base: *mut u8,
    page_size: usize,
    page_count: usize,
}

// SAFETY: `ShardBacking::base` mirrors the contract on
// [`crate::storage::local::ShardLocalStore`]: it points into a
// pinned, shard-owned region whose lifetime outlives this store and
// is only ever dereferenced (to compute page offsets) from the shard
// that registered it.
unsafe impl Send for LiveShardLocalStore {}
unsafe impl Sync for LiveShardLocalStore {}

unsafe impl Send for ChainLocalStore {}
unsafe impl Sync for ChainLocalStore {}

impl CacheDirectorySet {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            directories: Mutex::new(HashMap::new()),
        })
    }

    pub fn reconcile<I, S>(&self, caches: I)
    where
        I: IntoIterator,
        I::Item: Into<(S, i32)>,
        S: Into<String>,
    {
        let desired: HashMap<String, i32> = caches
            .into_iter()
            .map(|cache| {
                let (id, priority) = cache.into();
                (id.into(), priority)
            })
            .collect();
        let mut guard = self.directories.lock().unwrap();
        for entry in guard
            .iter()
            .filter_map(|(id, entry)| (!desired.contains_key(id)).then_some(entry))
        {
            entry.directory.apply_channels(Vec::new());
        }
        guard.retain(|id, _| desired.contains_key(id));
        for (id, priority) in desired {
            guard
                .entry(id)
                .and_modify(|entry| entry.priority = priority)
                .or_insert_with(|| CacheDirectoryEntry {
                    directory: DiskChannelDirectory::new(),
                    priority,
                });
        }
    }

    pub fn get(&self, cache_id: &str) -> Option<Arc<DiskChannelDirectory>> {
        self.directories
            .lock()
            .unwrap()
            .get(cache_id)
            .map(|entry| entry.directory.clone())
    }

    pub fn priority(&self, cache_id: &str) -> Option<i32> {
        self.directories
            .lock()
            .unwrap()
            .get(cache_id)
            .map(|entry| entry.priority)
    }

    pub fn get_or_create(&self, cache_id: &str) -> Arc<DiskChannelDirectory> {
        self.directories
            .lock()
            .unwrap()
            .entry(cache_id.to_string())
            .or_insert_with(|| CacheDirectoryEntry {
                directory: DiskChannelDirectory::new(),
                priority: 0,
            })
            .directory
            .clone()
    }

    pub fn apply_channels(
        &self,
        cache_id: &str,
        channels: Vec<(
            std::path::PathBuf,
            crate::storage::PageChannel,
            Option<u16>,
            bool,
        )>,
    ) {
        self.get_or_create(cache_id).apply_channels(channels);
    }

    pub fn drive_numa(&self, cache_id: Option<&str>) -> Arc<Vec<Option<u16>>> {
        cache_id
            .and_then(|id| self.get(id))
            .map(|directory| directory.drive_numa())
            .unwrap_or_else(|| Arc::new(Vec::new()))
    }
}

impl ChainLocalStore {
    pub fn new(directories: Arc<CacheDirectorySet>) -> Arc<Self> {
        Arc::new(Self {
            directories,
            stores: Mutex::new(HashMap::new()),
            registered: Mutex::new(Vec::new()),
        })
    }

    fn store_for(&self, cache_id: &str) -> Arc<LiveShardLocalStore> {
        let directory = self.directories.get_or_create(cache_id);
        let mut guard = self.stores.lock().unwrap();
        if let Some(entry) = guard.get(cache_id) {
            if Arc::ptr_eq(&entry.directory, &directory) {
                return entry.store.clone();
            }
        }
        let store = Arc::new(LiveShardLocalStore::new(directory.clone()));
        for backing in self.registered.lock().unwrap().iter() {
            let backing = crate::memory::Backing {
                base: backing.base,
                page_size: backing.page_size,
                page_count: backing.page_count,
                keepalive: backing.keepalive.clone(),
            };
            let _ = store.register_pages(&backing);
        }
        guard.insert(
            cache_id.to_string(),
            StoreEntry {
                directory,
                store: store.clone(),
            },
        );
        store
    }
}

impl LiveShardLocalStore {
    /// Build a per-shard view over `directory`. No backings are
    /// registered until [`Self::register_backing`] is called.
    pub fn new(directory: Arc<DiskChannelDirectory>) -> Self {
        Self {
            directory,
            state: Mutex::new(ReplayState {
                registered: Vec::new(),
                last_seen_generation: None,
            }),
        }
    }

    /// Record this shard's single backing and register it against every
    /// channel in the currently-published set (if any). Subsequent
    /// directory swaps replay the same backing against the new channels.
    pub fn register_backing(&self, backing: &crate::memory::Backing) -> Result<(), Error> {
        let entry = ShardBacking {
            base: backing.base,
            page_size: backing.page_size,
            page_count: backing.page_count,
        };
        {
            let mut guard = self.state.lock().unwrap();
            // A PageRef carries no backing identity, so the live view can
            // only resolve pages safely against one registered backing.
            if !guard.registered.is_empty() {
                return Err(Error::BadConfig(
                    "register_backing: shard view already has a registered backing",
                ));
            }
            guard.registered.push(entry);
            // Force a replay even if the generation matches what we
            // last saw: the new entry has never been registered
            // against the current channel set.
            guard.last_seen_generation = None;
        }
        let _ = self.current_or_replay();
        Ok(())
    }

    /// Resolve the currently-published channel set, replaying the
    /// registered backing against every channel if the directory
    /// generation has advanced since we last replayed. The snapshot
    /// and its publishing generation come from a single
    /// [`DiskChannelDirectory::snapshot`] load, so the pair is
    /// consistent. Returns `None` when the directory has no channels.
    fn current_or_replay(&self) -> Option<Arc<super::channels::ChannelSet>> {
        let (set, gen_n) = self.directory.snapshot();
        let set = set?;
        let mut guard = self.state.lock().unwrap();
        if guard.last_seen_generation != Some(gen_n) {
            Self::replay_locked(&set.channels, &guard.registered);
            guard.last_seen_generation = Some(gen_n);
        }
        Some(set)
    }

    /// Re-register the recorded backing against every channel.
    /// Errors are logged and swallowed: a single bad registration
    /// must not prevent the rest of the set from being seated.
    fn replay_locked(channels: &[PageChannel], backings: &[ShardBacking]) {
        for b in backings {
            for ch in channels {
                if let Err(e) = ch.register_buffer(b.base, b.page_size * b.page_count) {
                    eprintln!("disks: replay register_buffer failed: {e:?}");
                }
            }
        }
    }

    /// Resolve a `PageRef` to an absolute `(ptr, len)` into the shard
    /// backing.
    ///
    /// INVARIANT: a shard registers exactly one backing. A `PageRef`
    /// carries no backing identity, so this pointer math is only
    /// correct against a single backing.
    ///
    /// - Zero backings registered (channels were published before
    ///   [`Self::register_backing`] ran) is a recoverable runtime race:
    ///   fail the I/O with `ENXIO` rather than panicking the storage
    ///   core.
    fn resolve(&self, page: PageRef) -> Result<(*mut u8, usize), Error> {
        let guard = self.state.lock().unwrap();
        let b = match guard.registered.as_slice() {
            [] => return Err(Error::Io(libc::ENXIO)),
            [only] => *only,
            _ => return Err(Error::BadConfig("multiple registered backings")),
        };
        debug_assert!((page.page_idx as usize) < b.page_count);
        // SAFETY: `page_idx < page_count` (debug-asserted) and the
        // backing region is `page_count * page_size` bytes, so the
        // pointer is in-bounds. The pool guarantees
        // `offset + len <= page_size`.
        let p = unsafe {
            b.base
                .add(page.page_idx as usize * b.page_size + page.offset as usize)
        };
        Ok((p, page.len as usize))
    }

    async fn read_page_with_priority<R: Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        dst: PageRef,
        priority: i32,
    ) -> Result<bool, Error> {
        let key = req.key();
        let Some(channels) = self.current_or_replay() else {
            return Err(Error::from("no disks open"));
        };
        let (p, len) = self.resolve(dst)?;
        // NOTE: `disk_for` routes by `channels.len()`. Changing the set
        // of open disks (add/remove via reconcile or hot-swap) changes
        // that divisor, so a stripe persisted under a different open-disk
        // count hashes to a different disk and becomes UNREACHABLE. The
        // directory generation bump only invalidates the buffer cache,
        // not on-disk placement: changing the open-disk set is NOT a
        // data-preserving operation.
        let idx = disk_for(&key, stripe_off, channels.len());
        let slice = std::ptr::slice_from_raw_parts_mut(p, len);
        // SAFETY: `resolve` produced an in-bounds pointer into the
        // shard's pinned backing; the pool guarantees the page is not
        // aliased for the duration of this future.
        channels[idx]
            .read_page_with_priority(key, stripe_off, slice, priority)
            .await
    }

    async fn write_page_with_priority<R: Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        page: PageRef,
        priority: i32,
    ) -> Result<(), Error> {
        let key = req.key();
        let Some(channels) = self.current_or_replay() else {
            return Err(Error::from("no disks open"));
        };
        let (p, len) = self.resolve(page)?;
        // NOTE: see `read_page`. `disk_for`'s `channels.len()` divisor
        // means changing the set of open disks repartitions placement;
        // stripes written under a different open-disk count become
        // unreachable. Changing the open-disk set is NOT data-preserving.
        let idx = disk_for(&key, stripe_off, channels.len());
        let slice = std::ptr::slice_from_raw_parts(p.cast_const(), len);
        // SAFETY: see `read_page` above.
        channels[idx]
            .write_page_with_priority(key, stripe_off, slice, priority)
            .await
    }
}

impl BlockStore for LiveShardLocalStore {
    fn register_pages(&self, backing: &crate::memory::Backing) -> Result<(), Error> {
        self.register_backing(backing)
    }

    async fn read_page<R: Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        self.read_page_with_priority(req, stripe_off, dst, 0).await
    }

    async fn write_page<R: Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        self.write_page_with_priority(req, stripe_off, page, 0)
            .await
    }
}

impl BlockStore for ChainLocalStore {
    fn register_pages(&self, backing: &crate::memory::Backing) -> Result<(), Error> {
        let mut registered = self.registered.lock().unwrap();
        if !registered.is_empty() {
            return Err(Error::BadConfig(
                "register_pages: chain store already has a registered backing",
            ));
        }
        registered.push(RegisteredBacking {
            base: backing.base,
            page_size: backing.page_size,
            page_count: backing.page_count,
            keepalive: backing.keepalive.clone(),
        });
        drop(registered);
        for entry in self.stores.lock().unwrap().values() {
            entry.store.register_pages(backing)?;
        }
        Ok(())
    }

    fn page_cache_enabled<R: Req + ?Sized>(&self, req: &R, stripe_off: u64) -> bool {
        self.page_cache_policy(req).enabled(req.key(), stripe_off)
    }

    fn page_cache_policy<R: Req + ?Sized>(&self, req: &R) -> PageCachePolicy {
        let Some(cache_id) = req.cache_id() else {
            return PageCachePolicy::Disabled;
        };
        let Some(directory) = self.directories.get(cache_id) else {
            return PageCachePolicy::Disabled;
        };
        let Some(channels) = directory.current() else {
            return PageCachePolicy::Disabled;
        };
        PageCachePolicy::Custom(Arc::new(move |key, stripe_off| {
            let idx = disk_for(&key, stripe_off, channels.channels.len());
            channels
                .page_cache_enabled
                .get(idx)
                .copied()
                .unwrap_or(true)
        }))
    }

    async fn read_page<R: Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        let Some(cache_id) = req.cache_id() else {
            return Ok(false);
        };
        let Some(directory) = self.directories.get(cache_id) else {
            return Ok(false);
        };
        if directory.current().is_none() {
            return Ok(false);
        }
        let priority = self.directories.priority(cache_id).unwrap_or(0);
        self.store_for(cache_id)
            .read_page_with_priority(req, stripe_off, dst, priority)
            .await
    }

    async fn write_page<R: Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        let Some(cache_id) = req.cache_id() else {
            return Ok(());
        };
        let Some(directory) = self.directories.get(cache_id) else {
            return Ok(());
        };
        if directory.current().is_none() {
            return Ok(());
        }
        let priority = self.directories.priority(cache_id).unwrap_or(0);
        self.store_for(cache_id)
            .write_page_with_priority(req, stripe_off, page, priority)
            .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use crate::bufferpool::StripeKey;
    use crate::runtime::noop_waker;
    use crate::storage::PageService;
    use crate::storage::blockdev::{BlockDevice, MockDevice, MockDeviceConfig};
    use crate::storage::types::{Error as DevError, Lba};
    use crate::storage::{EngineConfig, StorageEngine, StripeReq};
    use std::future::Future;
    use std::path::PathBuf;
    use std::pin::{Pin, pin};
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use std::sync::mpsc::channel as std_channel;
    use std::task::{Context, Poll};
    use std::thread::JoinHandle;
    use std::time::Duration;

    fn block_on<F: Future>(f: F) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut f = pin!(f);
        let mut spins = 0u64;
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

    /// MockDevice wrapper that counts `register_buffers` calls into a
    /// shared atomic so replay tests can observe registrations even
    /// though the device itself lives on the storage-core thread and
    /// is `!Send`.
    struct CountingDevice {
        inner: MockDevice,
        registers: Arc<AtomicUsize>,
    }

    impl BlockDevice for CountingDevice {
        fn page_size(&self) -> usize {
            self.inner.page_size()
        }
        fn capacity_pages(&self) -> u64 {
            self.inner.capacity_pages()
        }
        fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), DevError> {
            self.registers.fetch_add(1, Ordering::Relaxed);
            self.inner.register_buffers(base, len)
        }
        fn write_queue_depth(&self) -> u32 {
            self.inner.write_queue_depth()
        }
        async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), DevError> {
            self.inner.read(lba, dst).await
        }
        async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), DevError> {
            self.inner.write(lba, src).await
        }
    }

    /// A live storage core driving a `CountingDevice` engine behind a
    /// [`PageChannel`]. Holds the join handle and stop flag so the
    /// test can shut it down deterministically.
    struct Core {
        channel: PageChannel,
        registers: Arc<AtomicUsize>,
        stop: Arc<AtomicBool>,
        join: Option<JoinHandle<()>>,
    }

    impl Core {
        fn spawn() -> Core {
            let registers = Arc::new(AtomicUsize::new(0));
            let registers_thr = registers.clone();
            let stop = Arc::new(AtomicBool::new(false));
            let stop_thr = stop.clone();
            let (tx, rx) = std_channel::<PageChannel>();
            let join = std::thread::spawn(move || {
                let device = Arc::new(CountingDevice {
                    inner: MockDevice::new(MockDeviceConfig {
                        page_size: 4096,
                        capacity_pages: 256,
                        ..Default::default()
                    }),
                    registers: registers_thr,
                });
                let mut cfg = EngineConfig::default();
                cfg.page_size_bytes = 4096;
                cfg.btree_page_bytes = 4096;
                let engine = Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap());
                let (channel, prx) = PageChannel::new();
                tx.send(channel).expect("send channel back");
                let mut service = PageService::new(engine.clone(), prx);

                let waker = noop_waker();
                let mut cx = Context::from_waker(&waker);
                let mut mutator: Pin<Box<dyn Future<Output = ()>>> =
                    Box::pin(engine.clone().run_mutator());
                let mut close_signaled = false;
                let mut mutator_done = false;
                loop {
                    if !mutator_done {
                        if let Poll::Ready(()) = mutator.as_mut().poll(&mut cx) {
                            mutator_done = true;
                        }
                    }
                    service.poll_once(&mut cx);
                    let shutdown =
                        stop_thr.load(Ordering::Acquire) || service.channel_disconnected();
                    if shutdown && !close_signaled {
                        engine.close_mutator();
                        close_signaled = true;
                    }
                    if close_signaled && mutator_done && !service.has_inflight() {
                        service.fail_all(Error::Io(libc::EIO));
                        service.drain_pending(Error::Io(libc::EIO));
                        service.mark_dead();
                        return;
                    }
                    std::thread::sleep(Duration::from_micros(50));
                }
            });
            let channel = rx.recv().expect("receive channel");
            Core {
                channel,
                registers,
                stop,
                join: Some(join),
            }
        }

        fn shutdown(mut self) {
            self.stop.store(true, Ordering::Release);
            if let Some(j) = self.join.take() {
                let _ = j.join();
            }
        }
    }

    fn make_backing(pages: usize) -> (Box<[u8]>, crate::memory::Backing) {
        let mut buf = vec![0u8; 4096 * pages].into_boxed_slice();
        let base = buf.as_mut_ptr();
        let backing = crate::memory::Backing {
            base,
            page_size: 4096,
            page_count: pages,
            keepalive: std::sync::Arc::new(()),
        };
        (buf, backing)
    }

    #[test]
    fn replays_registration_after_directory_swap() {
        let t = DiskChannelDirectory::new();
        let core1 = Core::spawn();
        t.apply_channels(vec![(
            PathBuf::from("/a"),
            core1.channel.clone(),
            None,
            true,
        )]);

        let view = LiveShardLocalStore::new(t.clone());
        let (_buf, backing) = make_backing(8);
        view.register_backing(&backing).unwrap();
        let gen1 = t.generation();
        assert_eq!(view.state.lock().unwrap().last_seen_generation, Some(gen1));

        let core2 = Core::spawn();
        t.apply_channels(vec![(
            PathBuf::from("/b"),
            core2.channel.clone(),
            None,
            true,
        )]);
        let gen2 = t.generation();
        assert_ne!(gen1, gen2);
        let _ = view.current_or_replay();
        assert_eq!(view.state.lock().unwrap().last_seen_generation, Some(gen2));

        core1.shutdown();
        core2.shutdown();
    }

    #[test]
    fn second_backing_is_rejected() {
        let t = DiskChannelDirectory::new();
        let core1 = Core::spawn();
        t.apply_channels(vec![(
            PathBuf::from("/a"),
            core1.channel.clone(),
            None,
            true,
        )]);

        let view = LiveShardLocalStore::new(t.clone());
        let (_buf_a, backing_a) = make_backing(4);
        view.register_backing(&backing_a).unwrap();

        let (_buf_b, backing_b) = make_backing(4);
        assert!(matches!(
            view.register_backing(&backing_b),
            Err(Error::BadConfig(_))
        ));
        assert_eq!(view.state.lock().unwrap().registered.len(), 1);

        core1.shutdown();
    }

    #[test]
    fn empty_directory_returns_io_error() {
        let t = DiskChannelDirectory::new();
        let view = LiveShardLocalStore::new(t);
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        let err = block_on(view.read_page(&StripeKey([0; 32]), 0, dst));
        assert!(matches!(err, Err(Error::Transport(_))));
    }

    #[test]
    fn write_then_read_round_trips_through_channels() {
        // Two disks exercise `disk_for` routing. Write a pattern from
        // page 0, then read it back into page 1 of the same backing.
        let t = DiskChannelDirectory::new();
        let core0 = Core::spawn();
        let core1 = Core::spawn();
        t.apply_channels(vec![
            (PathBuf::from("/a"), core0.channel.clone(), None, true),
            (PathBuf::from("/b"), core1.channel.clone(), None, true),
        ]);

        let view = LiveShardLocalStore::new(t.clone());
        let (mut buf, backing) = make_backing(2);
        view.register_pages(&backing).unwrap();

        for i in 0..4096usize {
            buf[i] = ((i * 13) & 0xff) as u8;
        }
        let key = StripeKey([0x42; 32]);
        let src = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        // First write rejected by admission; second admits.
        block_on(view.write_page(&key, 0, src)).unwrap();
        block_on(view.write_page(&key, 0, src)).unwrap();

        let dst = PageRef {
            page_idx: 1,
            offset: 0,
            len: 4096,
        };
        let hit = block_on(view.read_page(&key, 0, dst)).unwrap();
        assert!(hit, "expected cache hit through the channel view");
        for i in 0..4096usize {
            assert_eq!(buf[4096 + i], ((i * 13) & 0xff) as u8, "byte {i} mismatch");
        }

        core0.shutdown();
        core1.shutdown();
    }

    #[test]
    fn read_write_before_backing_registered_errors_not_panics() {
        // Channels are published but no backing has been registered for
        // this shard yet (a real race: `apply_channels` can land before
        // `register_backing`). The live read/write path must fail
        // gracefully with `ENXIO` rather than panic the storage core.
        let t = DiskChannelDirectory::new();
        let core = Core::spawn();
        t.apply_channels(vec![(
            PathBuf::from("/a"),
            core.channel.clone(),
            None,
            true,
        )]);

        let view = LiveShardLocalStore::new(t.clone());
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        let rerr = block_on(view.read_page(&StripeKey([0; 32]), 0, dst));
        assert!(matches!(rerr, Err(Error::Io(e)) if e == libc::ENXIO));

        let src = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        let werr = block_on(view.write_page(&StripeKey([0; 32]), 0, src));
        assert!(matches!(werr, Err(Error::Io(e)) if e == libc::ENXIO));

        core.shutdown();
    }

    #[test]
    fn cache_id_readd_rebinds_chain_store_to_new_directory() {
        let dirs = CacheDirectorySet::new();
        dirs.reconcile(["cache-a"]);
        let core1 = Core::spawn();
        dirs.apply_channels(
            "cache-a",
            vec![(PathBuf::from("/a"), core1.channel.clone(), None, true)],
        );

        let view = ChainLocalStore::new(dirs.clone());
        let (_buf, backing) = make_backing(2);
        view.register_pages(&backing).unwrap();
        let first = view.store_for("cache-a");

        dirs.reconcile(std::iter::empty::<&str>());
        dirs.reconcile(["cache-a"]);
        let core2 = Core::spawn();
        dirs.apply_channels(
            "cache-a",
            vec![(PathBuf::from("/b"), core2.channel.clone(), None, true)],
        );

        let second = view.store_for("cache-a");
        assert!(
            !Arc::ptr_eq(&first, &second),
            "re-added cache id must get a store bound to the new directory",
        );
        assert_eq!(
            second.state.lock().unwrap().registered.len(),
            1,
            "registered backing must be replayed into replacement store",
        );
        assert!(second.current_or_replay().is_some());

        core1.shutdown();
        core2.shutdown();
    }

    #[test]
    fn page_cache_policy_lookup_does_not_create_live_store() {
        let dirs = CacheDirectorySet::new();
        dirs.reconcile(["cache-a"]);
        let (channel, _rx) = PageChannel::new();
        dirs.apply_channels("cache-a", vec![(PathBuf::from("/a"), channel, None, false)]);

        let view = ChainLocalStore::new(dirs);
        let req = StripeReq::new(StripeKey([0x99; 32])).with_cache_id(Some("cache-a".to_string()));

        assert!(!view.page_cache_enabled(&req, 0));
        assert!(
            view.stores.lock().unwrap().is_empty(),
            "policy lookup must not instantiate or replay a live store",
        );
    }

    #[test]
    fn no_cache_id_disables_page_cache_policy() {
        let dirs = CacheDirectorySet::new();
        dirs.reconcile(["cache-a"]);
        let (channel, _rx) = PageChannel::new();
        dirs.apply_channels("cache-a", vec![(PathBuf::from("/a"), channel, None, true)]);

        let view = ChainLocalStore::new(dirs);
        let req = StripeReq::new(StripeKey([0x9a; 32]));

        assert!(!view.page_cache_enabled(&req, 0));
        assert!(
            view.stores.lock().unwrap().is_empty(),
            "no-cache policy lookup must not instantiate a live store",
        );
    }

    #[test]
    fn chain_store_rejects_second_backing() {
        let t = DiskChannelDirectory::new();
        let view = ChainLocalStore::new(Arc::new(CacheDirectorySet {
            directories: Mutex::new(HashMap::from([("cache-a".to_string(), t)])),
        }));
        let (_b1, backing1) = make_backing(4);
        view.register_pages(&backing1).unwrap();

        let (_b2, backing2) = make_backing(4);
        let err = view.register_pages(&backing2);
        assert!(matches!(err, Err(Error::BadConfig(_))));
        assert_eq!(view.registered.lock().unwrap().len(), 1);
    }
}
