// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! End-to-end tests for the bufferpool. The executor here is a
//! deliberately tiny single-thread block-on built on a noop waker;
//! mocks are pollable directly so we can drive concurrent reads
//! without an external runtime.

use std::cell::RefCell;
use std::collections::HashMap;
use std::future::Future;
use std::pin::{Pin, pin};
use std::rc::Rc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

use crate::bufferpool::{
    BlockStore, BufferPool, BulkRef, Error, PageRef, PageStream, Pool, PoolConfig, Req, StripeKey,
    Transport,
};
use crate::memory::Backing;

// ---------------------------------------------------------------------------
// Tiny single-thread executor.
// ---------------------------------------------------------------------------

fn noop_waker() -> Waker {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: the vtable functions never dereference the data pointer.
    unsafe { Waker::from_raw(raw()) }
}

fn block_on<F: Future>(future: F) -> F::Output {
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = pin!(future);
    let mut spins: u64 = 0;
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                assert!(spins < 1_000_000, "block_on stuck (no progress)");
            }
        }
    }
}

fn block_on_two<F1, F2>(f1: F1, f2: F2) -> (F1::Output, F2::Output)
where
    F1: Future,
    F2: Future,
{
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut f1 = pin!(f1);
    let mut f2 = pin!(f2);
    let mut o1: Option<F1::Output> = None;
    let mut o2: Option<F2::Output> = None;
    let mut spins: u64 = 0;
    loop {
        if o1.is_none() {
            if let Poll::Ready(v) = f1.as_mut().poll(&mut cx) {
                o1 = Some(v);
            }
        }
        if o2.is_none() {
            if let Poll::Ready(v) = f2.as_mut().poll(&mut cx) {
                o2 = Some(v);
            }
        }
        if o1.is_some() && o2.is_some() {
            return (o1.unwrap(), o2.unwrap());
        }
        spins += 1;
        assert!(spins < 1_000_000, "block_on_two stuck (no progress)");
    }
}

// ---------------------------------------------------------------------------
// Heap-backed Backing for tests.
// ---------------------------------------------------------------------------

struct HeapOwner {
    ptr: *mut u8,
    layout: std::alloc::Layout,
}

// SAFETY: tests are single-threaded; the allocation lives for the
// owner's lifetime.
unsafe impl Send for HeapOwner {}
unsafe impl Sync for HeapOwner {}

impl Drop for HeapOwner {
    fn drop(&mut self) {
        // SAFETY: matches the alloc.
        unsafe {
            std::alloc::dealloc(self.ptr, self.layout);
        }
    }
}

fn heap_backing(page_size: usize, page_count: usize) -> Backing {
    let layout = std::alloc::Layout::from_size_align(page_size * page_count, page_size).unwrap();
    // SAFETY: layout is valid (size > 0, align is power of two).
    let ptr = unsafe { std::alloc::alloc_zeroed(layout) };
    assert!(!ptr.is_null(), "heap_backing alloc failed");
    let owner = HeapOwner { ptr, layout };
    Backing {
        base: owner.ptr,
        page_size,
        page_count,
        _own: Box::new(owner),
    }
}

// ---------------------------------------------------------------------------
// Test request type.
// ---------------------------------------------------------------------------

#[derive(Clone, Debug)]
struct TestReq {
    key: StripeKey,
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        self.key
    }
}

// ---------------------------------------------------------------------------
// Mock transport.
// ---------------------------------------------------------------------------

struct MockTransport {
    /// `key -> stripe bytes`. Written into `dst` on `bulk_get`.
    stripes: RefCell<HashMap<StripeKey, Vec<u8>>>,
    /// Number of `bulk_get` calls completed.
    calls: RefCell<u32>,
    /// Pending `bulk_get`s pend this many polls before completing.
    pend_polls: RefCell<usize>,
    /// Force `bulk_get` to return an error instead of completing.
    error_mode: RefCell<bool>,
    /// Bound at construction (the embedder pre-registers the
    /// backing; `Transport` no longer carries a registration hook).
    base: *mut u8,
    page_size: usize,
}

impl MockTransport {
    fn new(base: *mut u8, page_size: usize) -> Self {
        Self {
            stripes: RefCell::new(HashMap::new()),
            calls: RefCell::new(0),
            pend_polls: RefCell::new(0),
            error_mode: RefCell::new(false),
            base,
            page_size,
        }
    }

    fn put_stripe(&self, key: StripeKey, bytes: Vec<u8>) {
        self.stripes.borrow_mut().insert(key, bytes);
    }

    fn calls(&self) -> u32 {
        *self.calls.borrow()
    }

    fn set_pend_polls(&self, n: usize) {
        *self.pend_polls.borrow_mut() = n;
    }

    fn set_error_mode(&self, on: bool) {
        *self.error_mode.borrow_mut() = on;
    }

    async fn do_bulk_get(&self, _req: &TestReq, src: BulkRef, dst: PageRef) -> Result<(), Error> {
        // Pend the configured number of polls (one polling round
        // per pend, decremented on each call).
        for _ in 0..*self.pend_polls.borrow() {
            PendOnce { fired: false }.await;
        }
        *self.pend_polls.borrow_mut() = 0;

        if *self.error_mode.borrow() {
            return Err(Error::from("forced error"));
        }

        let stripes = self.stripes.borrow();
        let bytes = stripes.get(&src.stripe).expect("stripe not configured");
        let start = src.offset as usize;
        let end = start + src.len as usize;
        assert!(end <= bytes.len(), "src out of range");

        let page_size = self.page_size;
        let dst_ptr = unsafe {
            self.base
                .add(dst.page_idx as usize * page_size + dst.offset as usize)
        };
        // SAFETY: dst is a pool page, src is a Vec<u8>, both valid.
        unsafe {
            std::ptr::copy_nonoverlapping(bytes.as_ptr().add(start), dst_ptr, src.len as usize);
        }
        *self.calls.borrow_mut() += 1;
        Ok(())
    }
}

/// Single-page stream adapter wrapping a boxed future. Yields the
/// one configured `PageRef` on success, or the future's error.
struct OneShotStream<'a> {
    fut: Option<Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>>,
    page: PageRef,
}

impl<'a> PageStream for OneShotStream<'a> {
    fn poll_next(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        // SAFETY: `OneShotStream` is `Unpin`-safe in practice: we
        // never move `fut` once Some, only poll it.
        let this = unsafe { self.as_mut().get_unchecked_mut() };
        let Some(fut) = this.fut.as_mut() else {
            return Poll::Ready(None);
        };
        match fut.as_mut().poll(cx) {
            Poll::Pending => Poll::Pending,
            Poll::Ready(Ok(())) => {
                this.fut = None;
                Poll::Ready(Some(Ok(this.page)))
            }
            Poll::Ready(Err(e)) => {
                this.fut = None;
                Poll::Ready(Some(Err(e)))
            }
        }
    }
}

impl Transport<TestReq> for MockTransport {
    type Stream<'a> = OneShotStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a TestReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        assert_eq!(dsts.len(), 1, "Pool always issues single-page bulk_get");
        let page = dsts[0];
        OneShotStream {
            fut: Some(Box::pin(self.do_bulk_get(req, src, page))),
            page,
        }
    }
}

// ---------------------------------------------------------------------------
// Mock blockstore.
// ---------------------------------------------------------------------------

struct MockBlockStore {
    cache: RefCell<HashMap<(StripeKey, u64), Vec<u8>>>,
    reads: RefCell<u32>,
    writes: RefCell<u32>,
    /// `write_page` pends this many polls before completing.
    write_pend_polls: RefCell<usize>,
    base: RefCell<Option<*mut u8>>,
    page_size: RefCell<usize>,
}

impl MockBlockStore {
    fn new() -> Self {
        Self {
            cache: RefCell::new(HashMap::new()),
            reads: RefCell::new(0),
            writes: RefCell::new(0),
            write_pend_polls: RefCell::new(0),
            base: RefCell::new(None),
            page_size: RefCell::new(0),
        }
    }

    fn preload(&self, key: StripeKey, off: u64, bytes: Vec<u8>) {
        self.cache.borrow_mut().insert((key, off), bytes);
    }

    fn writes(&self) -> u32 {
        *self.writes.borrow()
    }

    fn reads(&self) -> u32 {
        *self.reads.borrow()
    }

    fn set_write_pend_polls(&self, n: usize) {
        *self.write_pend_polls.borrow_mut() = n;
    }
}

impl BlockStore for MockBlockStore {
    fn register_pages(&self, backing: &Backing) -> Result<(), Error> {
        *self.base.borrow_mut() = Some(backing.base);
        *self.page_size.borrow_mut() = backing.page_size;
        Ok(())
    }

    async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        *self.reads.borrow_mut() += 1;
        let cache = self.cache.borrow();
        let Some(bytes) = cache.get(&(key, stripe_off)) else {
            return Ok(false);
        };
        let base = self.base.borrow().expect("registered");
        let page_size = *self.page_size.borrow();
        let dst_ptr = unsafe { base.add(dst.page_idx as usize * page_size + dst.offset as usize) };
        // SAFETY: dst is a pool page.
        unsafe {
            std::ptr::copy_nonoverlapping(bytes.as_ptr(), dst_ptr, bytes.len());
        }
        Ok(true)
    }

    async fn write_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        let pend = *self.write_pend_polls.borrow();
        for _ in 0..pend {
            PendOnce { fired: false }.await;
        }
        *self.write_pend_polls.borrow_mut() = 0;
        *self.writes.borrow_mut() += 1;
        let base = self.base.borrow().expect("registered");
        let page_size = *self.page_size.borrow();
        let src_ptr =
            unsafe { base.add(page.page_idx as usize * page_size + page.offset as usize) };
        let mut buf = vec![0u8; page.len as usize];
        // SAFETY: src is a pool page.
        unsafe {
            std::ptr::copy_nonoverlapping(src_ptr, buf.as_mut_ptr(), page.len as usize);
        }
        self.cache.borrow_mut().insert((key, stripe_off), buf);
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// PendOnce: yields once before completing. Used by mocks to model
// asynchronous progress without an external runtime.
// ---------------------------------------------------------------------------

struct PendOnce {
    fired: bool,
}

impl Future for PendOnce {
    type Output = ();
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let me = self.get_mut();
        if me.fired {
            Poll::Ready(())
        } else {
            me.fired = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    }
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

fn key(b: u8) -> StripeKey {
    StripeKey([b; 32])
}

/// Pool owns the mocks via these adapters; tests retain a clone of
/// the inner `Rc` so they can poke the mocks directly.
struct TransportRc(Rc<MockTransport>);
struct BlockStoreRc(Rc<MockBlockStore>);

impl Transport<TestReq> for TransportRc {
    type Stream<'a> = OneShotStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a TestReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        self.0.bulk_get(req, src, dsts)
    }
}

impl BlockStore for BlockStoreRc {
    fn register_pages(&self, backing: &Backing) -> Result<(), Error> {
        self.0.register_pages(backing)
    }
    async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        self.0.read_page(key, stripe_off, dst).await
    }
    async fn write_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        self.0.write_page(key, stripe_off, page).await
    }
}

fn make_pool_v2(
    page_size: usize,
    page_count: usize,
) -> (
    Pool<TransportRc, BlockStoreRc, TestReq>,
    Rc<MockTransport>,
    Rc<MockBlockStore>,
) {
    let backing = heap_backing(page_size, page_count);
    // `Transport` is now constructed already aware of the backing's
    // base/page_size (embedder pre-registration model); `BlockStore`
    // still receives the backing via `Pool::new -> register_pages`.
    let t = Rc::new(MockTransport::new(backing.base, backing.page_size));
    let s = Rc::new(MockBlockStore::new());
    let pool = Pool::new(
        PoolConfig::default(),
        backing,
        TransportRc(t.clone()),
        BlockStoreRc(s.clone()),
    )
    .unwrap();
    (pool, t, s)
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

#[test]
fn disk_hit_returns_bytes_no_transport_call() {
    const P: usize = 4096;
    let (pool, transport, store) = make_pool_v2(P, 4);
    let k = key(0xAA);
    let mut data = vec![0u8; P];
    for (i, b) in data.iter_mut().enumerate() {
        *b = (i & 0xff) as u8;
    }
    store.preload(k, 0, data.clone());

    let req = TestReq { key: k };
    let bytes = block_on(async {
        let mut s = pool.read(&req, 0, P as u64).await.unwrap();
        let g = s.next_page().await.unwrap().unwrap();
        let v = g.as_slice().to_vec();
        drop(g);
        assert!(s.next_page().await.is_none(), "EOF");
        v
    });
    assert_eq!(bytes, data);
    assert_eq!(transport.calls(), 0, "no transport call on disk hit");
    assert_eq!(store.reads(), 1);
    assert_eq!(store.writes(), 0);
    assert_eq!(pool.free_pages(), 4, "all pages returned");
    assert_eq!(pool.inflight_entries(), 0, "inflight cleaned up");
}

#[test]
fn disk_miss_peer_fetch_with_tee_writes_blockstore() {
    const P: usize = 4096;
    let (pool, transport, store) = make_pool_v2(P, 4);
    let k = key(0xBB);
    let mut stripe = vec![0u8; P * 2];
    for (i, b) in stripe.iter_mut().enumerate() {
        *b = ((i + 7) & 0xff) as u8;
    }
    transport.put_stripe(k, stripe.clone());

    let req = TestReq { key: k };
    let bytes = block_on(async {
        let mut s = pool.read(&req, 0, P as u64).await.unwrap();
        let g = s.next_page().await.unwrap().unwrap();
        g.as_slice().to_vec()
    });
    assert_eq!(bytes, stripe[..P]);
    assert_eq!(transport.calls(), 1);
    assert_eq!(store.writes(), 1, "tee landed");
    assert_eq!(pool.free_pages(), 4);
    assert_eq!(pool.inflight_entries(), 0);
}

#[test]
fn multi_page_window_with_intra_page_offsets() {
    const P: usize = 1024;
    let (pool, transport, _store) = make_pool_v2(P, 8);
    let k = key(0xCC);
    let mut stripe = vec![0u8; P * 4];
    for (i, b) in stripe.iter_mut().enumerate() {
        *b = (i & 0xff) as u8;
    }
    transport.put_stripe(k, stripe.clone());

    // Read [P/2, 2*P + P/2): straddles three pages, intra offsets
    // on first and last.
    let off = (P / 2) as u64;
    let len = (P + P) as u64; // total 2*P bytes; spans 3 pages
    let req = TestReq { key: k };
    let bytes = block_on(async {
        let mut out = Vec::new();
        let mut s = pool.read(&req, off, len).await.unwrap();
        while let Some(r) = s.next_page().await {
            out.extend_from_slice(r.unwrap().as_slice());
        }
        out
    });
    assert_eq!(bytes.len(), len as usize);
    assert_eq!(bytes, stripe[off as usize..(off + len) as usize]);
    assert_eq!(transport.calls(), 3);
    assert_eq!(pool.free_pages(), 8);
}

#[test]
fn single_flight_coalesces_concurrent_reads() {
    const P: usize = 4096;
    let (pool, transport, _store) = make_pool_v2(P, 4);
    let k = key(0xDD);
    let mut stripe = vec![0u8; P];
    for (i, b) in stripe.iter_mut().enumerate() {
        *b = (i & 0xff) as u8;
    }
    transport.put_stripe(k, stripe.clone());
    transport.set_pend_polls(2);

    let req1 = TestReq { key: k };
    let req2 = TestReq { key: k };
    let f1 = async {
        let mut s = pool.read(&req1, 0, P as u64).await.unwrap();
        s.next_page().await.unwrap().unwrap().as_slice().to_vec()
    };
    let f2 = async {
        let mut s = pool.read(&req2, 0, P as u64).await.unwrap();
        s.next_page().await.unwrap().unwrap().as_slice().to_vec()
    };
    let (b1, b2) = block_on_two(f1, f2);
    assert_eq!(b1, stripe);
    assert_eq!(b2, stripe);
    assert_eq!(transport.calls(), 1, "single-flight coalesced");
    assert_eq!(pool.free_pages(), 4);
    assert_eq!(pool.inflight_entries(), 0);
}

#[test]
fn eof_terminates_with_none() {
    const P: usize = 4096;
    let (pool, _transport, store) = make_pool_v2(P, 4);
    let k = key(1);
    store.preload(k, 0, vec![9u8; P]);
    let req = TestReq { key: k };
    block_on(async {
        let mut s = pool.read(&req, 0, P as u64).await.unwrap();
        let g = s.next_page().await.unwrap().unwrap();
        assert_eq!(g.len(), P);
        drop(g);
        assert!(s.next_page().await.is_none());
    });
}

#[test]
fn transport_error_propagates_and_recycles_pages() {
    const P: usize = 4096;
    let (pool, transport, _store) = make_pool_v2(P, 4);
    let k = key(2);
    transport.put_stripe(k, vec![0u8; P]);
    transport.set_error_mode(true);
    let req = TestReq { key: k };
    let r: Result<(), Error> = block_on(async {
        let mut s = pool.read(&req, 0, P as u64).await.unwrap();
        match s.next_page().await {
            Some(Err(e)) => Err(e),
            Some(Ok(_)) => panic!("expected error"),
            None => panic!("unexpected EOF"),
        }
    });
    match r {
        Err(Error::Transport(_)) => {}
        other => panic!("expected transport error, got {other:?}"),
    }
    assert_eq!(pool.free_pages(), 4, "page returned after error");
    assert_eq!(pool.inflight_entries(), 0);
}

#[test]
fn dropped_leader_promotes_new_leader() {
    // First read becomes leader, drops mid-fetch (we just drop the
    // future before completion). Second read starts fresh and
    // completes. We assert the second read still gets the bytes.
    const P: usize = 4096;
    let (pool, transport, _store) = make_pool_v2(P, 4);
    let k = key(3);
    let mut stripe = vec![0u8; P];
    for (i, b) in stripe.iter_mut().enumerate() {
        *b = (i & 0xff) as u8;
    }
    transport.put_stripe(k, stripe.clone());
    transport.set_pend_polls(2);

    let req = TestReq { key: k };

    // Start reader 1; poll once so it becomes leader and parks
    // on bulk_get (pend_polls=2 -> pends twice).
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    let fut1 = pool.read(&req, 0, P as u64);
    let mut fut1 = pin!(fut1);
    let mut s1 = match fut1.as_mut().poll(&mut cx) {
        Poll::Ready(Ok(s)) => s,
        other => panic!("read should be ready synchronously: {other:?}"),
    };
    // Begin polling next_page; this becomes leader and pends.
    {
        let np = s1.next_page();
        let mut np = pin!(np);
        for _ in 0..2 {
            match np.as_mut().poll(&mut cx) {
                Poll::Pending => {}
                Poll::Ready(_) => panic!("should be pending"),
            }
        }
        // Drop the next_page future mid-fetch; LeaderGuard runs.
    }
    drop(s1);

    // Now a fresh reader picks up. pend_polls is back to 2 (set on
    // the mock), so transport will pend another two rounds. The
    // pool will retry the I/O cleanly.
    transport.set_pend_polls(2);
    let bytes = block_on(async {
        let mut s = pool.read(&req, 0, P as u64).await.unwrap();
        s.next_page().await.unwrap().unwrap().as_slice().to_vec()
    });
    assert_eq!(bytes, stripe);
    assert_eq!(pool.free_pages(), 4);
    assert_eq!(pool.inflight_entries(), 0);
}

#[test]
fn free_list_parking_unblocks_on_release() {
    // 1-page pool. First read holds the page (does not drop the
    // guard); second read parks waiting for a free page; on first
    // guard drop, second read proceeds.
    const P: usize = 4096;
    let (pool, _transport, store) = make_pool_v2(P, 1);
    let k1 = key(0x10);
    let k2 = key(0x20);
    store.preload(k1, 0, vec![1u8; P]);
    store.preload(k2, 0, vec![2u8; P]);

    let req1 = TestReq { key: k1 };
    let req2 = TestReq { key: k2 };

    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    // Reader 1: get a guard and HOLD it.
    let fut1 = async {
        let mut s = pool.read(&req1, 0, P as u64).await.unwrap();
        let g = s.next_page().await.unwrap().unwrap();
        g.as_slice().to_vec()
    };
    let mut fut1 = pin!(fut1);
    // Reader 2: should park waiting for a free page.
    let fut2 = async {
        let mut s = pool.read(&req2, 0, P as u64).await.unwrap();
        s.next_page().await.unwrap().unwrap().as_slice().to_vec()
    };
    let mut fut2 = pin!(fut2);

    // Drive fut1 to ready while fut2 pends.
    let v1 = loop {
        if let Poll::Ready(v) = fut1.as_mut().poll(&mut cx) {
            break v;
        }
        // Also poll fut2 so any side-effects happen, but it should
        // not complete until fut1's guard drops (and fut1 here
        // returns the bytes after dropping its guard at end of
        // async block).
        let _ = fut2.as_mut().poll(&mut cx);
    };
    assert_eq!(v1, vec![1u8; P]);

    // Now drive fut2 to completion.
    let v2 = loop {
        if let Poll::Ready(v) = fut2.as_mut().poll(&mut cx) {
            break v;
        }
    };
    assert_eq!(v2, vec![2u8; P]);
    assert_eq!(pool.free_pages(), 1);
}

#[test]
fn stream_limit_enforced() {
    const P: usize = 4096;
    let backing = heap_backing(P, 2);
    let t = Rc::new(MockTransport::new(backing.base, backing.page_size));
    let s = Rc::new(MockBlockStore::new());
    s.preload(key(0), 0, vec![0u8; P]);
    let cfg = PoolConfig {
        max_concurrent_streams: 1,
        ..PoolConfig::default()
    };
    let pool = Pool::new(
        cfg,
        backing,
        TransportRc(t.clone()),
        BlockStoreRc(s.clone()),
    )
    .unwrap();
    let req = TestReq { key: key(0) };
    block_on(async {
        let s1 = pool.read(&req, 0, P as u64).await.unwrap();
        let r2 = pool.read(&req, 0, P as u64).await;
        match r2 {
            Err(Error::StreamLimit) => {}
            other => panic!("expected StreamLimit, got {other:?}"),
        }
        drop(s1);
        // After dropping s1, slot is released and a new stream is admissible.
        let _s3 = pool.read(&req, 0, P as u64).await.unwrap();
    });
}

#[test]
fn rejects_bad_backing() {
    let backing = Backing {
        base: 0x1000 as *mut u8,
        page_size: 0,
        page_count: 1,
        _own: Box::new(()),
    };
    let t = TransportRc(Rc::new(MockTransport::new(
        backing.base,
        // `backing.page_size` is 0 here on purpose; the test only
        // exercises Pool::new's BadConfig early-return, which fires
        // before any `bulk_get` runs.
        1,
    )));
    let s = BlockStoreRc(Rc::new(MockBlockStore::new()));
    let r = Pool::<_, _, TestReq>::new(PoolConfig::default(), backing, t, s);
    assert!(matches!(r, Err(Error::BadConfig(_))));
}

#[test]
fn non_leader_consumes_concurrently_with_tee() {
    // Reader 1 is the leader: bulk_get completes instantly, slot
    // transitions to Ready, then leader awaits a slow write_page.
    // Reader 2 (subscribed on the same key) must be able to obtain
    // and consume its PageGuard while the tee is still in flight.
    const P: usize = 4096;
    let (pool, transport, store) = make_pool_v2(P, 4);
    let k = key(0xEE);
    let mut stripe = vec![0u8; P];
    for (i, b) in stripe.iter_mut().enumerate() {
        *b = (i & 0xff) as u8;
    }
    transport.put_stripe(k, stripe.clone());
    store.set_write_pend_polls(8);

    let req1 = TestReq { key: k };
    let req2 = TestReq { key: k };

    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    let f1 = async {
        let mut s = pool.read(&req1, 0, P as u64).await.unwrap();
        s.next_page().await.unwrap().unwrap().as_slice().to_vec()
    };
    let f2 = async {
        let mut s = pool.read(&req2, 0, P as u64).await.unwrap();
        s.next_page().await.unwrap().unwrap().as_slice().to_vec()
    };
    let mut f1 = pin!(f1);
    let mut f2 = pin!(f2);

    // Drive both. Reader 2 must complete BEFORE reader 1 (leader is
    // still awaiting write_page).
    let mut o2: Option<Vec<u8>> = None;
    let mut spins = 0u64;
    loop {
        let _ = f1.as_mut().poll(&mut cx);
        if o2.is_none()
            && let Poll::Ready(v) = f2.as_mut().poll(&mut cx)
        {
            o2 = Some(v);
            break;
        }
        spins += 1;
        assert!(spins < 1_000_000, "reader 2 stuck behind tee");
    }
    assert_eq!(o2.unwrap(), stripe);
    // Tee should still be pending.
    assert_eq!(store.writes(), 0, "tee should not have completed yet");

    // Now drive reader 1 to completion (tee finishes too).
    let v1 = loop {
        if let Poll::Ready(v) = f1.as_mut().poll(&mut cx) {
            break v;
        }
    };
    assert_eq!(v1, stripe);
    assert_eq!(store.writes(), 1);
    assert_eq!(transport.calls(), 1);
    assert_eq!(pool.free_pages(), 4);
    assert_eq!(pool.inflight_entries(), 0);
}

#[test]
fn page_pinned_across_tee_until_subscriber_drops() {
    // 1-page pool. Non-leader consumes its bytes during the tee
    // and drops the PageGuard while tee_pending is still true; the
    // page must NOT be recycled until the tee completes.
    const P: usize = 4096;
    let (pool, transport, store) = make_pool_v2(P, 1);
    let k = key(0xAB);
    transport.put_stripe(k, vec![0xAAu8; P]);
    store.set_write_pend_polls(4);

    let req1 = TestReq { key: k };
    let req2 = TestReq { key: k };

    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    let f1 = async {
        let mut s = pool.read(&req1, 0, P as u64).await.unwrap();
        // Consume the leader's bytes (this only completes after the
        // tee since the leader awaits write_page).
        let g = s.next_page().await.unwrap().unwrap();
        g.as_slice().to_vec()
    };
    let f2 = async {
        let mut s = pool.read(&req2, 0, P as u64).await.unwrap();
        let g = s.next_page().await.unwrap().unwrap();
        let v = g.as_slice().to_vec();
        // Drop the guard immediately; tee is still in flight.
        drop(g);
        v
    };
    let mut f1 = pin!(f1);
    let mut f2 = pin!(f2);

    // Drive both until reader 2 finishes (its guard drops while tee
    // is pending).
    let mut o2: Option<Vec<u8>> = None;
    let mut spins = 0u64;
    loop {
        let _ = f1.as_mut().poll(&mut cx);
        if o2.is_none()
            && let Poll::Ready(v) = f2.as_mut().poll(&mut cx)
        {
            o2 = Some(v);
            break;
        }
        spins += 1;
        assert!(spins < 1_000_000, "reader 2 stuck");
    }
    assert_eq!(o2.unwrap(), vec![0xAAu8; P]);
    // The single backing page must still be held by the leader's
    // tee even though reader 2 dropped its guard.
    assert_eq!(pool.free_pages(), 0, "page must stay pinned across tee");

    // Drain reader 1; tee completes, page recycles.
    let v1 = loop {
        if let Poll::Ready(v) = f1.as_mut().poll(&mut cx) {
            break v;
        }
    };
    assert_eq!(v1, vec![0xAAu8; P]);
    assert_eq!(store.writes(), 1);
    assert_eq!(pool.free_pages(), 1);
    assert_eq!(pool.inflight_entries(), 0);
}

#[test]
fn leader_drop_during_tee_releases_page() {
    // Leader drops its future while the tee is in flight. The
    // TeeGuard must clear `tee_pending` so the page returns to the
    // free list once consumer holds drain.
    const P: usize = 4096;
    let (pool, transport, store) = make_pool_v2(P, 1);
    let k = key(0xCD);
    transport.put_stripe(k, vec![0xCDu8; P]);
    store.set_write_pend_polls(8);

    let req = TestReq { key: k };
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);

    {
        let f = async {
            let mut s = pool.read(&req, 0, P as u64).await.unwrap();
            let _g = s.next_page().await.unwrap().unwrap();
            // Hold guard, then drop everything mid-tee.
        };
        let mut f = pin!(f);
        // Poll a bounded number of times: enough for the leader to
        // mark Ready and start the tee, but not enough to finish
        // the 8-poll write_page.
        for _ in 0..3 {
            let _ = f.as_mut().poll(&mut cx);
        }
        // Drop f mid-tee.
    }

    // Tee was abandoned; page must be back in the free list.
    assert_eq!(pool.free_pages(), 1, "page recycled after leader drop");
    assert_eq!(pool.inflight_entries(), 0);
    // bulk_get completed before the drop.
    assert_eq!(transport.calls(), 1);
    // write_page never completed (best-effort tee).
    assert_eq!(store.writes(), 0);
}
