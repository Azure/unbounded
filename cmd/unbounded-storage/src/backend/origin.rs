// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Enum dispatch over the concrete origin backends.
//!
//! The node configures exactly one origin tier per `backend_id`, but
//! that tier may be a plaintext HTTP origin ([`HttpBackend`]), an
//! S3-compatible origin ([`S3Backend`]), or an Azure Blob origin
//! ([`AzureBackend`]). Rather than box the backend
//! behind `dyn Backend` (which the [`Backend`] trait's generic
//! associated `Stream<'a>` type forbids without erasing the stream),
//! [`OriginBackend`] is a static enum the embedder selects at
//! construction time. Its [`Backend::Stream`] is [`OriginStream`], an
//! enum that wraps whichever concrete stream the chosen backend
//! produces and delegates [`PageStream::poll_next`] to it.

use std::pin::Pin;
use std::task::{Context, Poll};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::storage::StripeReq;

use super::{AzureBackend, Backend, FakeBackend, HttpBackend, S3Backend};

/// One configured origin tier: a plaintext HTTP origin, an
/// S3-compatible origin, an Azure Blob origin, or a synthetic
/// [`FakeBackend`] for benchmarking. Implements [`Backend`] by
/// delegating to the inner backend and wrapping its stream in
/// [`OriginStream`].
pub enum OriginBackend {
    Http(HttpBackend),
    S3(S3Backend),
    Azure(AzureBackend),
    Fake(FakeBackend),
}

impl OriginBackend {
    /// Owned-stream variant of [`Backend::bulk_get`]. Delegates to the
    /// inner backend's `fetch_stream`, which borrows nothing from the
    /// backend, so the returned [`OriginStream`] is `'static`. This is
    /// what lets a [`super::registry::BackendRegistry`] serve a fetch
    /// from a backend it holds behind an `Arc`/`ArcSwap` without the
    /// temporary `Arc` guard having to outlive the stream.
    pub fn fetch_stream(
        &self,
        req: &StripeReq,
        src: BulkRef,
        dsts: &[PageRef],
    ) -> OriginStream<'static> {
        match self {
            OriginBackend::Http(b) => OriginStream::Http(b.fetch_stream(req, src, dsts)),
            OriginBackend::S3(b) => OriginStream::S3(b.fetch_stream(req, src, dsts)),
            OriginBackend::Azure(b) => OriginStream::Azure(b.fetch_stream(req, src, dsts)),
            OriginBackend::Fake(b) => OriginStream::Fake(b.fetch_stream(req, src, dsts)),
        }
    }
}

impl Backend for OriginBackend {
    type Req = StripeReq;
    type Stream<'a> = OriginStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        // NB: this cannot delegate to the inherent `fetch_stream`, which
        // returns `OriginStream<'static>`: `OriginStream<'a>` projects
        // each inner backend's associated `Stream<'a>` and so is
        // invariant in `'a`, which blocks the `'static -> 'a` coercion.
        // The inner `bulk_get`s build the correctly-lifetimed stream.
        match self {
            OriginBackend::Http(b) => OriginStream::Http(b.bulk_get(req, src, dsts)),
            OriginBackend::S3(b) => OriginStream::S3(b.bulk_get(req, src, dsts)),
            OriginBackend::Azure(b) => OriginStream::Azure(b.bulk_get(req, src, dsts)),
            OriginBackend::Fake(b) => OriginStream::Fake(b.bulk_get(req, src, dsts)),
        }
    }
}

/// Stream produced by [`OriginBackend::bulk_get`]: whichever concrete
/// page stream the selected inner backend yields. [`PageStream`] is
/// delegated to the active variant.
pub enum OriginStream<'a> {
    Http(<HttpBackend as Backend>::Stream<'a>),
    S3(<S3Backend as Backend>::Stream<'a>),
    Azure(<AzureBackend as Backend>::Stream<'a>),
    Fake(<FakeBackend as Backend>::Stream<'a>),
}

impl PageStream for OriginStream<'_> {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        // Both inner streams are `Unpin` (their only pinned state is a
        // `Pin<Box<dyn Future>>`, which is itself `Unpin`), so the enum
        // is `Unpin` and the inner streams can be re-pinned in place.
        match self.get_mut() {
            OriginStream::Http(s) => Pin::new(s).poll_next(cx),
            OriginStream::S3(s) => Pin::new(s).poll_next(cx),
            OriginStream::Azure(s) => Pin::new(s).poll_next(cx),
            OriginStream::Fake(s) => Pin::new(s).poll_next(cx),
        }
    }
}
