// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Enum dispatch over the concrete origin backends.
//!
//! The node configures exactly one origin tier per `backend_id`, but
//! that tier may be a plaintext HTTP origin ([`HttpBackend`]) or an
//! S3-compatible origin ([`S3Backend`]). Rather than box the backend
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

use super::{Backend, HttpBackend, S3Backend};

/// One configured origin tier: either a plaintext HTTP origin or an
/// S3-compatible origin. Implements [`Backend`] by delegating to the
/// inner backend and wrapping its stream in [`OriginStream`].
pub enum OriginBackend {
    Http(HttpBackend),
    S3(S3Backend),
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
        match self {
            OriginBackend::Http(b) => OriginStream::Http(b.bulk_get(req, src, dsts)),
            OriginBackend::S3(b) => OriginStream::S3(b.bulk_get(req, src, dsts)),
        }
    }
}

/// Stream produced by [`OriginBackend::bulk_get`]: whichever concrete
/// page stream the selected inner backend yields. [`PageStream`] is
/// delegated to the active variant.
pub enum OriginStream<'a> {
    Http(<HttpBackend as Backend>::Stream<'a>),
    S3(<S3Backend as Backend>::Stream<'a>),
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
        }
    }
}
