// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! S3-compatible origin backend. [`S3Backend`] is the S3 sibling of
//! [`HttpBackend`](super::HttpBackend): it fetches a stripe's byte range
//! (or an object's length) from an S3-compatible origin and fills the
//! destination bufferpool pages.
//!
//! The origin scheme selects the transport: `http://` dials the origin
//! in plaintext HTTP/1.1; `https://` performs a TLS 1.3 handshake via
//! OpenSSL with kernel TLS (kTLS) so body bytes are decrypted straight
//! into the registered backing (zero copy preserved). Record-aware recv
//! lives in [`crate::tls`]. The `Host:` header and SNI/cert hostname are
//! carried as owned strings parsed from the configured endpoint URL.
//!
//! It mirrors the HTTP backend's fetch/length structure and connection
//! reuse rules. It diverges in two ways:
//!
//! - It carries the origin **hostname** (not just a resolved IPv4) so the
//!   `Host:` header uses the real virtual-host name the bucket policy
//!   expects, while the TCP connect still dials the resolved IPv4
//!   [`SockAddr`].
//! - A `404 Not Found` from the origin maps to
//!   [`Error::OriginNotFound`](crate::bufferpool::Error::OriginNotFound)
//!   rather than the generic non-2xx error, so the pool can distinguish a
//!   missing object from a transport failure.

use std::rc::Rc;

use crate::bufferpool::{BulkRef, PageRef};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::StripeReq;
use crate::tls::TlsContext;

use super::Backend;
use super::conn::OriginConnPool;
use super::http_family::{
    GetFetchInputs, OriginFetchInputs, S3_ENGINE_POLICY, absolute_range, fetch_metadata,
    fetch_range,
};
use super::limiter::FetchLimiter;

/// Stream produced by [`S3Backend::bulk_get`].
pub type S3FetchStream<'a> = super::http_family::FetchStream<'a>;

/// Origin backend that fetches stripe byte ranges from an
/// S3-compatible origin into bufferpool pages. The endpoint scheme
/// selects plaintext HTTP/1.1 (`http://`) or TLS 1.3 with kTLS
/// (`https://`); see the module docs.
///
/// Shard-pinned: the [`NetHandle`] and raw `backing_base` pointer are
/// only ever touched on the owning shard thread that built this backend.
pub struct S3Backend {
    handle: NetHandle,
    origin: SockAddr,
    /// The origin hostname used for the `Host:` header. The TCP connect
    /// uses `origin` (the resolved IPv4), but the bucket's virtual-host
    /// name must travel in `Host:`.
    host: String,
    /// Hostname (no port) used for SNI and certificate verification on
    /// TLS connections. Empty for plaintext origins.
    sni_host: String,
    /// TLS context shared across fetches when the endpoint is `https://`;
    /// `None` for plaintext origins.
    tls: Option<Rc<TlsContext>>,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    backing_base: *mut u8,
    limiter: FetchLimiter,
    conns: OriginConnPool,
}

impl S3Backend {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        handle: NetHandle,
        origin: SockAddr,
        host: String,
        sni_host: String,
        tls: Option<Rc<TlsContext>>,
        backend_id: String,
        stripe_size: u64,
        page_size: usize,
        backing_base: *mut u8,
        http_concurrency: usize,
    ) -> Self {
        Self {
            handle,
            origin,
            host,
            sni_host,
            tls,
            backend_id,
            stripe_size,
            page_size,
            backing_base,
            limiter: FetchLimiter::new(http_concurrency),
            conns: OriginConnPool::new(http_concurrency),
        }
    }

    /// The configured `backend_id` this backend serves, i.e. the
    /// `OriginRef::backend_id` whose stripes route here.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// Resolve a `host:port` URL value to a single IPv4 [`SockAddr`].
    /// Delegates to [`HttpBackend::resolve_origin`](super::HttpBackend::resolve_origin),
    /// which takes the first IPv4 `ToSocketAddrs` yields and errors on
    /// IPv6-only origins (v1 dials IPv4 only). The hostname for the
    /// `Host:` header is passed separately to [`S3Backend::new`].
    pub fn resolve_origin(url: &str) -> std::io::Result<SockAddr> {
        super::HttpBackend::resolve_origin(url)
    }
}

impl S3Backend {
    /// Owned-stream variant of [`Backend::bulk_get`]. Mirrors
    /// [`super::HttpBackend::fetch_stream`]: the returned stream borrows
    /// nothing from `self`, so it is `'static` and can be handed out by
    /// a [`super::registry::BackendRegistry`] owning the backend through
    /// an `Rc`.
    pub fn fetch_stream(
        &self,
        req: &StripeReq,
        src: BulkRef,
        dsts: &[PageRef],
    ) -> S3FetchStream<'static> {
        let Some(origin) = req.origin() else {
            return S3FetchStream::immediate_error(S3_ENGINE_POLICY.missing_origin);
        };
        let path = origin.origin_object_id.clone();

        let dsts_owned = dsts.to_vec();
        let handle = self.handle.clone();
        let origin_addr = self.origin;
        let backing_base = self.backing_base;
        let page_size = self.page_size;
        let host = self.host.clone();
        let sni_host = self.sni_host.clone();
        let tls = self.tls.clone();
        let conns = self.conns.clone();

        // A metadata entry is not a byte range of the object; it is a
        // synthetic one-page cache entry whose payload is the object's
        // metadata. The sentinel `stripe_idx` would overflow
        // `absolute_range`, so this must branch before that is computed.
        if origin.is_metadata_entry() {
            let mut log = crate::obs::ReqLog::new("backend.s3");
            log.str_field("op", "HEAD")
                .str_field("backend", self.backend_id())
                .str_field("path", &path)
                .field("pages", dsts_owned.len());
            let fut = Box::pin(crate::obs::instrument(
                log,
                crate::metrics::instrument_backend(
                    self.backend_id().to_string(),
                    page_size as u64,
                    fetch_metadata(OriginFetchInputs {
                        handle,
                        conns,
                        origin: origin_addr,
                        host,
                        sni_host,
                        tls,
                        path,
                        dsts: dsts_owned.clone(),
                        backing_base,
                        page_size,
                        limiter: self.limiter.clone(),
                        policy: &S3_ENGINE_POLICY,
                    }),
                ),
            ));
            return S3FetchStream::pending(fut, dsts_owned);
        }

        debug_assert!(!origin.is_metadata_entry());
        let (start, len) = absolute_range(origin.stripe_idx, self.stripe_size, src.offset, src.len);

        let mut log = crate::obs::ReqLog::new("backend.s3");
        log.str_field("op", "GET")
            .str_field("backend", self.backend_id())
            .str_field("path", &path)
            .field("off", start)
            .field("len", len)
            .field("pages", dsts_owned.len());
        let fut = Box::pin(crate::obs::instrument(
            log,
            crate::metrics::instrument_backend(
                self.backend_id().to_string(),
                len,
                fetch_range(GetFetchInputs {
                    origin: OriginFetchInputs {
                        handle,
                        conns,
                        origin: origin_addr,
                        host,
                        sni_host,
                        tls,
                        path,
                        dsts: dsts_owned.clone(),
                        backing_base,
                        page_size,
                        limiter: self.limiter.clone(),
                        policy: &S3_ENGINE_POLICY,
                    },
                    start,
                    len,
                }),
            ),
        ));
        S3FetchStream::pending(fut, dsts_owned)
    }
}

impl Backend for S3Backend {
    type Req = StripeReq;
    type Stream<'a> = S3FetchStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        self.fetch_stream(req, src, dsts)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolve_origin_parses_ipv4() {
        let addr = S3Backend::resolve_origin("127.0.0.1:9000").expect("resolves");
        assert_eq!(
            addr.as_ipv4(),
            Some((std::net::Ipv4Addr::new(127, 0, 0, 1), 9000))
        );
    }
}
