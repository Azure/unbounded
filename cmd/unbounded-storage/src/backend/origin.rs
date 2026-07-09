// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Enum dispatch over the concrete origin backends.
//!
//! The node configures exactly one origin tier per `backend_id`, but
//! that tier may be a plaintext HTTP origin ([`HttpBackend`]), an
//! S3-compatible origin ([`S3Backend`]), an Azure Blob origin
//! ([`AzureBackend`]), or a synthetic [`FakeBackend`] for benchmarking.
//! Rather than box the backend behind `dyn Backend` (which the
//! [`Backend`] trait's generic associated `Stream<'a>` type forbids
//! without erasing the stream), [`OriginBackend`] is a static enum the
//! embedder selects at construction time.

use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::storage::StripeReq;

use super::{AzureBackend, Backend, FakeBackend, HttpBackend, S3Backend};

/// One configured origin tier: a plaintext HTTP origin, an
/// S3-compatible origin, an Azure Blob origin, or a synthetic
/// [`FakeBackend`] for benchmarking.
pub enum OriginBackend {
    Http(HttpBackend),
    S3(S3Backend),
    Azure(AzureBackend),
    Fake(FakeBackend),
}

impl OriginBackend {
    /// Owned-stream variant used by [`super::registry::BackendRegistry`].
    /// The registry gives the stream an `Rc` guard so a backend can be
    /// borrowed safely without forcing every concrete backend to copy its
    /// destination-page slice.
    pub fn fetch_stream<'a>(
        backend: Rc<Self>,
        req: &StripeReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> OriginStream<'a> {
        match backend.as_ref() {
            OriginBackend::Http(b) => OriginStream::Http(b.fetch_stream(req, src, dsts)),
            OriginBackend::S3(b) => OriginStream::S3(b.fetch_stream(req, src, dsts)),
            OriginBackend::Azure(b) => OriginStream::Azure(b.fetch_stream(req, src, dsts)),
            OriginBackend::Fake(b) => match b.fill(req, src, dsts) {
                Ok(()) => OriginStream::Fake {
                    backend,
                    delivered: dsts,
                    state: FakeOwnedState::Delivering,
                    next: 0,
                },
                Err(e) => OriginStream::Fake {
                    backend,
                    delivered: &[],
                    state: FakeOwnedState::Failed(Some(e)),
                    next: 0,
                },
            },
        }
    }
}

impl Backend for OriginBackend {
    type Req = StripeReq;
    type Stream<'a> = OriginBorrowedStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        match self {
            OriginBackend::Http(b) => OriginBorrowedStream::Http(b.bulk_get(req, src, dsts)),
            OriginBackend::S3(b) => OriginBorrowedStream::S3(b.bulk_get(req, src, dsts)),
            OriginBackend::Azure(b) => OriginBorrowedStream::Azure(b.bulk_get(req, src, dsts)),
            OriginBackend::Fake(b) => OriginBorrowedStream::Fake(b.bulk_get(req, src, dsts)),
        }
    }
}

/// Stream produced by [`OriginBackend::fetch_stream`] for registry use.
pub enum OriginStream<'a> {
    Http(<HttpBackend as Backend>::Stream<'static>),
    S3(<S3Backend as Backend>::Stream<'static>),
    Azure(<AzureBackend as Backend>::Stream<'static>),
    Fake {
        backend: Rc<OriginBackend>,
        delivered: &'a [PageRef],
        state: FakeOwnedState,
        next: usize,
    },
}

enum FakeOwnedState {
    Delivering,
    Failed(Option<Error>),
    Done,
}

impl PageStream for OriginStream<'_> {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        match self.get_mut() {
            OriginStream::Http(s) => Pin::new(s).poll_next(cx),
            OriginStream::S3(s) => Pin::new(s).poll_next(cx),
            OriginStream::Azure(s) => Pin::new(s).poll_next(cx),
            OriginStream::Fake {
                backend,
                delivered,
                state,
                next,
            } => {
                let _keepalive = backend;
                loop {
                    match state {
                        FakeOwnedState::Delivering => {
                            if *next >= delivered.len() {
                                *state = FakeOwnedState::Done;
                                return Poll::Ready(None);
                            }

                            let page = delivered[*next];
                            *next += 1;
                            return Poll::Ready(Some(Ok(page)));
                        }
                        FakeOwnedState::Failed(slot) => {
                            let e = slot.take();
                            *state = FakeOwnedState::Done;
                            return Poll::Ready(e.map(Err));
                        }
                        FakeOwnedState::Done => return Poll::Ready(None),
                    }
                }
            }
        }
    }
}

/// Stream produced by direct [`OriginBackend::bulk_get`] calls.
pub enum OriginBorrowedStream<'a> {
    Http(<HttpBackend as Backend>::Stream<'a>),
    S3(<S3Backend as Backend>::Stream<'a>),
    Azure(<AzureBackend as Backend>::Stream<'a>),
    Fake(<FakeBackend as Backend>::Stream<'a>),
}

impl PageStream for OriginBorrowedStream<'_> {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        match self.get_mut() {
            OriginBorrowedStream::Http(s) => Pin::new(s).poll_next(cx),
            OriginBorrowedStream::S3(s) => Pin::new(s).poll_next(cx),
            OriginBorrowedStream::Azure(s) => Pin::new(s).poll_next(cx),
            OriginBorrowedStream::Fake(s) => Pin::new(s).poll_next(cx),
        }
    }
}
