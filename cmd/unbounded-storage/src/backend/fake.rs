// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Synthetic origin backend for benchmarking. [`FakeBackend`] serves
//! deterministic loadgen objects of a single fixed size without any
//! network I/O, so the cache and frontend read path can be load-tested
//! in production without standing up a real origin.
//!
//! It mirrors [`HttpBackend`](super::http::HttpBackend)'s
//! `Backend`/`PageStream` shape but collapses the whole fetch to a
//! synchronous page fill: there is no ring, no socket, and no future to
//! drive. A data request fills destination pages from the shared
//! deterministic synthetic object contract; a metadata request writes
//! the object's [`ObjectMetadata`] (carrying the fixed `object_size`) so
//! the frontend learns the object's length and clamps served reads to it.
//! Because the fill is synchronous, the produced [`FakeFetchStream`]
//! only needs to borrow the destination-page slice long enough to yield
//! those copied page refs back to the pool.

use std::pin::Pin;
use std::task::{Context, Poll};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::storage::{ObjectMetadata, StripeReq, fill_synthetic_pages, parse_synthetic_object_id};

use super::Backend;
use super::http::copy_body_into_pages;

/// Origin backend that synthesizes deterministic objects of a fixed size.
///
/// Shard-pinned for the same reason as
/// [`HttpBackend`](super::http::HttpBackend): the raw `backing_base`
/// pointer is only ever written on the single thread that drives the
/// backend. Unlike the HTTP backend it holds no ring, so the only
/// only unsafe concern is the backing pointer.
pub struct FakeBackend {
    backend_id: String,
    stripe_size: u64,
    metadata_body: Box<[u8]>,
    page_size: usize,
    backing_base: *mut u8,
}

impl FakeBackend {
    pub fn new(
        backend_id: String,
        stripe_size: u64,
        object_size: u64,
        page_size: usize,
        backing_base: *mut u8,
    ) -> Self {
        let metadata_body = ObjectMetadata::new(object_size)
            .encode()
            .expect("fake backend metadata encoding should not fail")
            .into_boxed_slice();

        Self {
            backend_id,
            stripe_size,
            metadata_body,
            page_size,
            backing_base,
        }
    }

    /// The configured `backend_id` this backend serves, i.e. the
    /// `OriginRef::backend_id` whose stripes route here.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// Fills the destination pages synchronously, then returns a stream
    /// that yields each filled [`PageRef`] in order.
    pub fn fetch_stream<'a>(
        &self,
        req: &StripeReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> FakeFetchStream<'a> {
        match self.fill(req, src, dsts) {
            Ok(()) => FakeFetchStream::ready(dsts),
            Err(e) => FakeFetchStream::immediate_err(e),
        }
    }

    pub(super) fn fill(
        &self,
        req: &StripeReq,
        src: BulkRef,
        dsts: &[PageRef],
    ) -> Result<(), Error> {
        let Some(origin) = req.origin() else {
            return Err(Error::from("fake backend: request missing origin"));
        };
        let Some(object) = parse_synthetic_object_id(&origin.origin_object_id) else {
            return Err(Error::from("fake backend: invalid synthetic object id"));
        };

        if origin.is_metadata_entry() {
            self.fill_metadata(dsts)
        } else {
            let Some(start_offset) = origin
                .stripe_idx
                .checked_mul(self.stripe_size)
                .and_then(|base| base.checked_add(src.offset))
            else {
                return Err(Error::from("fake backend: synthetic offset overflow"));
            };
            fill_synthetic_pages(
                object,
                start_offset,
                dsts,
                self.backing_base,
                self.page_size,
            )
        }
    }

    /// Encode the object's [`ObjectMetadata`] and write it into the
    /// metadata entry's destination page(s).
    fn fill_metadata(&self, dsts: &[PageRef]) -> Result<(), Error> {
        let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
        if self.metadata_body.len() > capacity {
            return Err(Error::from(
                "fake backend: metadata entry destination too small",
            ));
        }
        copy_body_into_pages(&self.metadata_body, dsts, self.backing_base, self.page_size)
    }
}

impl Backend for FakeBackend {
    type Req = StripeReq;
    type Stream<'a> = FakeFetchStream<'a>;

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
pub struct FakeFetchStream<'a> {
    delivered: &'a [PageRef],
    state: FillState,
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

impl<'a> FakeFetchStream<'a> {
    fn ready(delivered: &'a [PageRef]) -> Self {
        Self {
            delivered,
            state: FillState::Delivering,
            next: 0,
        }
    }

    fn immediate_err(err: Error) -> Self {
        Self {
            delivered: &[],
            state: FillState::Failed(Some(err)),
            next: 0,
        }
    }
}

impl PageStream for FakeFetchStream<'_> {
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
    /// can prove the backend actually wrote synthetic content.
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

    fn fake_backend<'a>(buf: &'a mut [u8], object_size: u64) -> FakeBackend {
        FakeBackend::new(
            "fake".into(),
            PAGE_SIZE as u64,
            object_size,
            PAGE_SIZE,
            buf.as_mut_ptr(),
        )
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
    fn data_request_fills_deterministic_bytes() {
        let (mut buf, dsts) = backing(2);
        let backend = fake_backend(&mut buf, 8192);

        let object_id = crate::storage::synthetic_object_id(7, 11);
        let object = crate::storage::parse_synthetic_object_id(&object_id).unwrap();
        let origin = OriginRef::new("fake", object_id, 0);
        let req = StripeReq::new(origin.stripe_key()).with_origin(origin);
        let src = BulkRef {
            stripe: origin_key(),
            offset: 0,
            len: (2 * PAGE_SIZE) as u32,
        };

        let pages = drain(backend.fetch_stream(&req, src, &dsts)).expect("fill");
        assert_eq!(pages.len(), 2, "every destination page must be yielded");
        assert!(crate::storage::synthetic_matches_bytes(object, 0, &buf));
    }

    #[test]
    fn invalid_synthetic_object_id_errors() {
        let (mut buf, dsts) = backing(1);
        let backend = fake_backend(&mut buf, 4096);

        let origin = OriginRef::new("fake", "obj", 0);
        let req = StripeReq::new(origin.stripe_key()).with_origin(origin);
        let src = BulkRef {
            stripe: origin_key(),
            offset: 0,
            len: PAGE_SIZE as u32,
        };

        drain(backend.fetch_stream(&req, src, &dsts))
            .expect_err("invalid synthetic object id must error");
    }

    #[test]
    fn metadata_request_reports_configured_object_size() {
        let object_size = 7 * 1024 * 1024;
        let (mut buf, dsts) = backing(1);
        let backend = fake_backend(&mut buf, object_size);

        let origin = OriginRef::metadata_entry("fake", crate::storage::synthetic_object_id(7, 11));
        let req = StripeReq::new(origin.stripe_key()).with_origin(origin);
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
        let backend = FakeBackend::new(
            "fake".into(),
            PAGE_SIZE as u64,
            4096,
            PAGE_SIZE,
            ptr::null_mut(),
        );
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
        OriginRef::new("fake", crate::storage::synthetic_object_id(7, 11), 0).stripe_key()
    }
}
