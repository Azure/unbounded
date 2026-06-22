// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Synthetic origin backend for benchmarking. [`FakeBackend`] serves
//! zero-filled objects of a single fixed size without any network I/O,
//! so the cache and frontend read path can be load-tested in production
//! without standing up a real origin.
//!
//! It mirrors [`HttpBackend`](super::http::HttpBackend)'s
//! `Backend`/`PageStream` shape but collapses the whole fetch to a
//! synchronous page fill: there is no ring, no socket, and no future to
//! drive. A data request zero-fills the destination pages directly; a
//! metadata request writes the object's [`ObjectMetadata`] (carrying the
//! fixed `object_size`) so the frontend learns the object's length and
//! clamps served reads to it. Because the fill is synchronous and
//! borrows nothing, the produced [`FakeFetchStream`] is fully owned and
//! the backend can be served from behind an `Arc`/`ArcSwap` just like
//! the real origin backends.

use std::pin::Pin;
use std::task::{Context, Poll};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::storage::{ObjectMetadata, StripeReq};

use super::Backend;
use super::http::copy_body_into_pages;

/// Origin backend that synthesizes zero-filled objects of a fixed size.
///
/// Shard-pinned for the same reason as
/// [`HttpBackend`](super::http::HttpBackend): the raw `backing_base`
/// pointer is only ever written on the single thread that drives the
/// backend. Unlike the HTTP backend it holds no ring, so the only
/// cross-thread concern is the backing pointer.
pub struct FakeBackend {
    backend_id: String,
    object_size: u64,
    page_size: usize,
    backing_base: *mut u8,
}

// SAFETY: mirrors `HttpBackend`. `FakeBackend` is shard-pinned: the
// embedder constructs it on, and only ever drives it from, a single
// pinned shard thread, so the raw `backing_base` pointer is never
// touched concurrently. The `Send + Sync` marker exists solely to
// satisfy the `Backend: Send + Sync` bound the cross-shard registry
// requires; it is not an invitation to touch the backend off its shard.
unsafe impl Send for FakeBackend {}
unsafe impl Sync for FakeBackend {}

impl FakeBackend {
    pub fn new(
        backend_id: String,
        object_size: u64,
        page_size: usize,
        backing_base: *mut u8,
    ) -> Self {
        Self {
            backend_id,
            object_size,
            page_size,
            backing_base,
        }
    }

    /// The configured `backend_id` this backend serves, i.e. the
    /// `OriginRef::backend_id` whose stripes route here.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// Owned-stream variant of [`Backend::bulk_get`].
    ///
    /// Fills the destination pages synchronously, then returns a stream
    /// that yields each filled [`PageRef`] in order. The returned stream
    /// borrows nothing from `self`, so it is `'static` (see the module
    /// docs and [`super::registry::BackendRegistry`]).
    pub fn fetch_stream(
        &self,
        req: &StripeReq,
        _src: BulkRef,
        dsts: &[PageRef],
    ) -> FakeFetchStream {
        let Some(origin) = req.origin() else {
            return FakeFetchStream::immediate_error("fake backend: request missing origin");
        };

        let dsts_owned = dsts.to_vec();
        // A metadata entry reports the object's length; a data request is
        // a byte range whose synthetic content is all zeros. Both reduce
        // to a page fill via `copy_body_into_pages` (an empty body
        // zero-fills every destination byte).
        let fill = if origin.is_metadata_entry() {
            self.fill_metadata(&dsts_owned)
        } else {
            copy_body_into_pages(&[], &dsts_owned, self.backing_base, self.page_size)
        };

        match fill {
            Ok(()) => FakeFetchStream::ready(dsts_owned),
            Err(e) => FakeFetchStream::immediate_err(e),
        }
    }

    /// Encode the object's [`ObjectMetadata`] and write it into the
    /// metadata entry's destination page(s).
    fn fill_metadata(&self, dsts: &[PageRef]) -> Result<(), Error> {
        let body = ObjectMetadata::new(self.object_size).encode()?;
        let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
        if body.len() > capacity {
            return Err(Error::from(
                "fake backend: metadata entry destination too small",
            ));
        }
        copy_body_into_pages(&body, dsts, self.backing_base, self.page_size)
    }
}

impl Backend for FakeBackend {
    type Req = StripeReq;
    type Stream<'a> = FakeFetchStream;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        self.fetch_stream(req, src, dsts)
    }
}

/// Stream produced by [`FakeBackend::bulk_get`]. The pages are already
/// filled by the time it is constructed, so it simply yields each
/// destination [`PageRef`] in order followed by `None`. On a fill error
/// it yields that error once then `None`.
pub struct FakeFetchStream {
    state: FillState,
    /// The destination pages to yield, in order.
    delivered: Vec<PageRef>,
    next: usize,
}

enum FillState {
    /// Pages are filled; emit each delivered page in order.
    Delivering,
    /// A single error to emit before ending the stream.
    Failed(Option<Error>),
    /// Stream exhausted.
    Done,
}

impl FakeFetchStream {
    fn ready(delivered: Vec<PageRef>) -> Self {
        Self {
            state: FillState::Delivering,
            delivered,
            next: 0,
        }
    }

    fn immediate_error(msg: &'static str) -> Self {
        Self {
            state: FillState::Failed(Some(Error::from(msg))),
            delivered: Vec::new(),
            next: 0,
        }
    }

    fn immediate_err(err: Error) -> Self {
        Self {
            state: FillState::Failed(Some(err)),
            delivered: Vec::new(),
            next: 0,
        }
    }
}

impl PageStream for FakeFetchStream {
    fn poll_next(
        self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        let this = self.get_mut();
        loop {
            match &mut this.state {
                FillState::Delivering => {
                    if this.next >= this.delivered.len() {
                        this.state = FillState::Done;
                        return Poll::Ready(None);
                    }
                    let page = this.delivered[this.next];
                    this.next += 1;
                    return Poll::Ready(Some(Ok(page)));
                }
                FillState::Failed(slot) => {
                    let e = slot.take();
                    this.state = FillState::Done;
                    return Poll::Ready(e.map(Err));
                }
                FillState::Done => return Poll::Ready(None),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::ptr;
    use std::task::{RawWaker, RawWakerVTable, Waker};

    use crate::bufferpool::StripeKey;
    use crate::storage::{ObjectMetadata, OriginRef};

    use super::*;

    const PAGE_SIZE: usize = 4096;

    fn noop_waker() -> Waker {
        fn no_op(_: *const ()) {}
        fn clone(_: *const ()) -> RawWaker {
            RawWaker::new(ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, no_op, no_op, no_op);
        // SAFETY: the vtable's fns are all no-ops over a null data
        // pointer, so the waker upholds the `RawWaker` contract.
        unsafe { Waker::from_raw(RawWaker::new(ptr::null(), &VTABLE)) }
    }

    /// A backing buffer of `pages` contiguous pages plus the `PageRef`s
    /// that address them. The buffer is pre-filled with `0xAA` so a test
    /// can prove the backend actually wrote zeros (rather than the page
    /// having been zero to begin with).
    fn backing(pages: usize) -> (Vec<u8>, Vec<PageRef>) {
        let buf = vec![0xAAu8; pages * PAGE_SIZE];
        let dsts = (0..pages)
            .map(|i| PageRef {
                page_idx: i as u32,
                offset: 0,
                len: PAGE_SIZE as u32,
            })
            .collect();
        (buf, dsts)
    }

    /// Drive a stream to completion, returning the yielded pages.
    fn drain(mut stream: FakeFetchStream) -> Result<Vec<PageRef>, Error> {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut out = Vec::new();
        loop {
            match Pin::new(&mut stream).poll_next(&mut cx) {
                Poll::Ready(Some(Ok(p))) => out.push(p),
                Poll::Ready(Some(Err(e))) => return Err(e),
                Poll::Ready(None) => return Ok(out),
                Poll::Pending => panic!("fake stream must never be pending"),
            }
        }
    }

    #[test]
    fn data_request_zero_fills_every_page() {
        let (mut buf, dsts) = backing(2);
        let backend = FakeBackend::new("fake".into(), 8192, PAGE_SIZE, buf.as_mut_ptr());

        let origin = OriginRef::new("fake", "obj", 0);
        let req = StripeReq::new(origin_key()).with_origin(origin);
        let src = BulkRef {
            stripe: origin_key(),
            offset: 0,
            len: (2 * PAGE_SIZE) as u32,
        };

        let pages = drain(backend.fetch_stream(&req, src, &dsts)).expect("fill");
        assert_eq!(pages.len(), 2, "every destination page must be yielded");
        assert!(
            buf.iter().all(|&b| b == 0),
            "data pages must be fully zero-filled",
        );
    }

    #[test]
    fn metadata_request_reports_configured_object_size() {
        let object_size = 7 * 1024 * 1024;
        let (mut buf, dsts) = backing(1);
        let backend = FakeBackend::new("fake".into(), object_size, PAGE_SIZE, buf.as_mut_ptr());

        let origin = OriginRef::metadata_entry("fake", "obj");
        let req = StripeReq::new(origin_key()).with_origin(origin);
        let src = BulkRef {
            stripe: origin_key(),
            offset: 0,
            len: PAGE_SIZE as u32,
        };

        let pages = drain(backend.fetch_stream(&req, src, &dsts)).expect("fill");
        assert_eq!(pages.len(), 1);
        let meta = ObjectMetadata::decode(&buf).expect("decode metadata");
        assert_eq!(meta.length, object_size);
        assert!(meta.is_empty(), "fake metadata carries no key/value pairs");
    }

    #[test]
    fn missing_origin_yields_one_error_then_ends() {
        let backend = FakeBackend::new("fake".into(), 4096, PAGE_SIZE, ptr::null_mut());
        let req = StripeReq::new(origin_key());
        let dsts: Vec<PageRef> = Vec::new();
        let src = BulkRef {
            stripe: origin_key(),
            offset: 0,
            len: 0,
        };

        let err = drain(backend.fetch_stream(&req, src, &dsts))
            .expect_err("a request without an origin must error");
        let _ = err;
    }

    fn origin_key() -> StripeKey {
        crate::storage::stripe_key("fake-keyspace", "/obj", 0)
    }
}
