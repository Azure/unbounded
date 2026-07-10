// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Azure Blob Storage origin backend. [`AzureBackend`] is the Azure
//! sibling of [`S3Backend`](super::S3Backend): it fetches a stripe's
//! byte range (or an object's length) from an Azure Blob origin and
//! fills the destination bufferpool pages. An `http://` endpoint uses
//! plaintext HTTP/1.1; an `https://` endpoint uses TLS 1.3 driven by
//! OpenSSL with kernel TLS (kTLS) so the body still lands zero-copy in
//! the registered backing, with record-aware recv in [`crate::tls`].
//!
//! It mirrors the S3 backend's fetch/length structure and connection
//! reuse rules. Like the S3 backend it carries the origin **host** for
//! the `Host:` header and a separate **SNI** hostname for TLS (the TCP
//! connect still dials the resolved IPv4 [`SockAddr`]) and maps a `404
//! Not Found` to
//! [`Error::OriginNotFound`](crate::bufferpool::Error::OriginNotFound).
//!
//! It diverges from the S3 backend in one wire-level way: every GET/HEAD
//! carries the `2021-08-06` `x-ms-version` header so the request
//! is conformant with the Azure Blob REST API. v1 is anonymous: it
//! targets public-read containers (or the Azurite emulator) and sends no
//! authorization or shared-key signature.

use std::rc::Rc;

use crate::bufferpool::{BulkRef, PageRef};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::StripeReq;
use crate::tls::TlsContext;

use super::Backend;
use super::conn::OriginConnPool;
use super::http_family::{
    AZURE_ENGINE_POLICY, GetFetchInputs, OriginFetchInputs, absolute_range, fetch_metadata,
    fetch_range,
};
use super::limiter::FetchLimiter;

/// Stream produced by [`AzureBackend::bulk_get`].
pub type AzureFetchStream<'a> = super::http_family::FetchStream<'a>;

/// Origin backend that fetches stripe byte ranges from an Azure Blob
/// origin into bufferpool pages over plaintext HTTP/1.1.
///
/// Shard-pinned: the [`NetHandle`] and raw `backing_base` pointer are
/// only ever touched on the owning shard thread that built this backend.
pub struct AzureBackend {
    handle: NetHandle,
    origin: SockAddr,
    /// The origin host used for the `Host:` header. The TCP connect
    /// uses `origin` (the resolved IPv4), but the storage account's
    /// virtual-host name must travel in `Host:`.
    host: String,
    /// The hostname (no port) used for TLS SNI and certificate
    /// verification. Empty for plaintext (`http://`) origins.
    sni_host: String,
    /// TLS context for `https://` origins; `None` for plaintext.
    tls: Option<Rc<TlsContext>>,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    backing_base: *mut u8,
    limiter: FetchLimiter,
    conns: OriginConnPool,
}

impl AzureBackend {
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
    /// `Host:` header is passed separately to [`AzureBackend::new`].
    pub fn resolve_origin(url: &str) -> std::io::Result<SockAddr> {
        super::HttpBackend::resolve_origin(url)
    }
}

impl AzureBackend {
    /// Owned-stream variant of [`Backend::bulk_get`]. Mirrors
    /// [`super::S3Backend::fetch_stream`]: the returned stream borrows
    /// nothing from `self`, so it is `'static` and can be handed out by
    /// a [`super::registry::BackendRegistry`] owning the backend through
    /// an `Rc`.
    pub fn fetch_stream(
        &self,
        req: &StripeReq,
        src: BulkRef,
        dsts: &[PageRef],
    ) -> AzureFetchStream<'static> {
        let Some(origin) = req.origin() else {
            return AzureFetchStream::immediate_error(AZURE_ENGINE_POLICY.missing_origin);
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
            let fut = Box::pin(crate::metrics::instrument_backend(
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
                    policy: &AZURE_ENGINE_POLICY,
                }),
            ));
            return AzureFetchStream::pending(fut, dsts_owned);
        }

        debug_assert!(!origin.is_metadata_entry());
        let (start, len) = absolute_range(origin.stripe_idx, self.stripe_size, src.offset, src.len);

        let fut = Box::pin(crate::metrics::instrument_backend(
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
                    policy: &AZURE_ENGINE_POLICY,
                },
                start,
                len,
            }),
        ));
        AzureFetchStream::pending(fut, dsts_owned)
    }
}

impl Backend for AzureBackend {
    type Req = StripeReq;
    type Stream<'a> = AzureFetchStream<'a>;

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
        let addr = AzureBackend::resolve_origin("127.0.0.1:10000").expect("resolves");
        assert_eq!(
            addr.as_ipv4(),
            Some((std::net::Ipv4Addr::new(127, 0, 0, 1), 10000))
        );
    }
}
