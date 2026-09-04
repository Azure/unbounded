// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::cell::RefCell;
use std::collections::HashMap;
use std::future::Future;
use std::pin::{Pin, pin};
use std::rc::Rc;
use std::task::{Context, Poll};

use crate::bufferpool::{
    BlockStore, BufferPool, BulkRef, Error, PageRef, PageStream, Pool, PoolConfig, Req, StripeKey,
    Transport,
};
use crate::memory::Backing;
use crate::runtime::noop_waker;

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

#[derive(Clone, Debug)]
struct TestReq {
    key: StripeKey,
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        self.key
    }
}

struct TestTransport {
    base: *mut u8,
    page_size: usize,
    stripes: RefCell<HashMap<StripeKey, Vec<u8>>>,
}

impl TestTransport {
    fn new(base: *mut u8, page_size: usize) -> Self {
        Self {
            base,
            page_size,
            stripes: RefCell::new(HashMap::new()),
        }
    }

    fn put_stripe(&self, key: StripeKey, bytes: Vec<u8>) {
        self.stripes.borrow_mut().insert(key, bytes);
    }
}

struct OneShotStream {
    page: PageRef,
    done: bool,
}

impl PageStream for OneShotStream {
    fn poll_next(
        mut self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        if self.done {
            return Poll::Ready(None);
        }
        self.done = true;
        Poll::Ready(Some(Ok(self.page)))
    }
}

impl Transport<TestReq> for Rc<TestTransport> {
    type Stream<'a> = OneShotStream;

    fn bulk_get<'a>(
        &'a self,
        _req: &'a TestReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        assert_eq!(dsts.len(), 1);
        let dst = dsts[0];
        let stripes = self.stripes.borrow();
        let bytes = stripes.get(&src.stripe).expect("stripe not configured");
        let start = src.offset as usize;
        let end = start + src.len as usize;
        assert!(end <= bytes.len());
        unsafe {
            let dst_ptr = self
                .base
                .add(dst.page_idx as usize * self.page_size + dst.offset as usize);
            std::ptr::copy_nonoverlapping(bytes.as_ptr().add(start), dst_ptr, src.len as usize);
        }
        OneShotStream {
            page: dst,
            done: false,
        }
    }
}

struct MissBlockStore;

impl BlockStore for MissBlockStore {
    fn register_pages(&self, _backing: &Backing) -> Result<(), Error> {
        Ok(())
    }

    async fn read_page<R: Req + ?Sized>(
        &self,
        _req: &R,
        _stripe_off: u64,
        _dst: PageRef,
    ) -> Result<bool, Error> {
        Ok(false)
    }

    async fn write_page<R: Req + ?Sized>(
        &self,
        _req: &R,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), Error> {
        Ok(())
    }
}

struct HeapOwner {
    ptr: *mut u8,
    layout: std::alloc::Layout,
}

unsafe impl Send for HeapOwner {}
unsafe impl Sync for HeapOwner {}

impl Drop for HeapOwner {
    fn drop(&mut self) {
        unsafe {
            std::alloc::dealloc(self.ptr, self.layout);
        }
    }
}

fn heap_backing(page_size: usize, page_count: usize) -> Backing {
    let layout = std::alloc::Layout::from_size_align(page_size * page_count, page_size).unwrap();
    let ptr = unsafe { std::alloc::alloc_zeroed(layout) };
    assert!(!ptr.is_null(), "heap_backing alloc failed");
    let owner = HeapOwner { ptr, layout };
    Backing {
        base: owner.ptr,
        page_size,
        page_count,
        keepalive: std::sync::Arc::new(owner),
    }
}

fn key(byte: u8) -> StripeKey {
    StripeKey([byte; 32])
}

#[test]
fn owned_page_future_remains_attached_if_polled_after_stream_drop() {
    const P: usize = 64;
    let backing = heap_backing(P, 1);
    let transport = Rc::new(TestTransport::new(backing.base, backing.page_size));
    let k0 = key(0x91);
    let k1 = key(0x92);
    transport.put_stripe(k0, vec![0xA1; P]);
    transport.put_stripe(k1, vec![0xB2; P]);
    let pool = Pool::new(
        PoolConfig {
            max_concurrent_streams: 4,
            max_inflight_pages: 1,
        },
        backing,
        transport.clone(),
        MissBlockStore,
    )
    .expect("pool");

    let req0 = TestReq { key: k0 };
    let stream = pool.read_owned(&req0, 0, P as u64).expect("owned stream");
    let future = stream
        .page_owned_future_at(0)
        .expect("page should be in owned stream");
    drop(stream);
    let page = block_on(future).expect("future should still resolve after stream drop");
    assert_eq!(page.as_slice(), &[0xA1; P]);
    drop(page);

    let req1 = TestReq { key: k1 };
    let second = block_on(async {
        let mut stream = pool.read(&req1, 0, P as u64).await.unwrap();
        stream
            .next_page()
            .await
            .unwrap()
            .unwrap()
            .as_slice()
            .to_vec()
    });
    assert_eq!(second, vec![0xB2; P]);
    assert_eq!(pool.free_pages() + pool.cached_pages(), 1);
    assert_eq!(pool.active_inflight_entries(), 0);
}
