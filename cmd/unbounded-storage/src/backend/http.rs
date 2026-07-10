// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP origin backend. [`HttpBackend`] is the cache-miss origin tier:
//! when a read misses all the way through the P2P cache, it fetches the
//! stripe's byte range from an HTTP/1.1 origin server and fills the
//! destination bufferpool pages. The origin endpoint URL selects the
//! transport: `http://` is plaintext, `https://` runs a TLS 1.3
//! handshake (OpenSSL) and enables kernel TLS so the body still lands
//! zero-copy in the destination pages (the kernel decrypts in place).
//! The record-aware recv path lives in [`crate::tls`].
//!
//! Origin connections are kept alive when the response has a known,
//! fully-consumed body and the peer did not ask to close the stream. Idle
//! sockets remain in the backend's shard-local pool.
//!
//! ## Address resolution and the `Host` header
//!
//! [`HttpBackend::resolve_origin`] resolves the endpoint URL's authority
//! to a single IPv4 [`SockAddr`] at startup (DNS at bring-up is fine).
//! IPv6-only origins are unsupported in v1 and surface as an error. The
//! `Host:` header and the TLS SNI/certificate hostname are carried as
//! owned strings derived from the configured URL, not re-rendered from
//! the resolved address.
//!
//! ## Future optimizations (not in v1)
//!
//! - Conditional revalidation via ETag. Per the content-addressed
//!   design the ETag should fold into the stripe key's identity (a new
//!   ETag yields a new key), rather than being tracked as separate
//!   per-stripe metadata.

use std::rc::Rc;

use crate::bufferpool::{BulkRef, PageRef};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::{ObjectMetadata, StripeReq};
use crate::tls::TlsContext;

use super::Backend;
use super::conn::OriginConnPool;
use super::http_family::{
    GetFetchInputs, HTTP_ENGINE_POLICY, HTTP_PAGE_ERRORS, OriginFetchInputs, absolute_range,
    copy_body_into_pages, fetch_metadata, fetch_range,
};
use super::limiter::FetchLimiter;

/// Stream produced by [`HttpBackend::bulk_get`].
pub type HttpFetchStream<'a> = super::http_family::FetchStream<'a>;

/// Origin backend that fetches stripe byte ranges from an HTTP/1.1
/// origin server (plaintext `http://` or kernel-TLS `https://`) into
/// bufferpool pages.
///
/// Shard-pinned: the [`NetHandle`] and raw `backing_base` pointer are
/// only ever touched on the owning shard thread that built this backend.
pub struct HttpBackend {
    handle: NetHandle,
    origin: SockAddr,
    /// Authority sent in the `Host:` header (`host` or `host:port`),
    /// derived from the configured endpoint URL.
    host: String,
    /// Hostname (no port) used for TLS SNI and certificate verification.
    /// Empty on a plaintext (`http://`) backend.
    sni_host: String,
    /// TLS context when the endpoint is `https://`; `None` for plaintext.
    /// Shared (`Rc`) across the fetch futures this backend spawns.
    tls: Option<Rc<TlsContext>>,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    backing_base: *mut u8,
    limiter: FetchLimiter,
    conns: OriginConnPool,
}

impl HttpBackend {
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
    ///
    /// Takes the first IPv4 address `ToSocketAddrs` yields. DNS at
    /// startup is acceptable for the origin tier. If only IPv6
    /// addresses resolve, this returns an error: v1 dials IPv4 only.
    pub fn resolve_origin(url: &str) -> std::io::Result<SockAddr> {
        use std::net::{SocketAddr, ToSocketAddrs};

        let mut last_v6 = false;
        for addr in url.to_socket_addrs()? {
            match addr {
                SocketAddr::V4(v4) => {
                    let sin = libc::sockaddr_in {
                        sin_family: libc::AF_INET as libc::sa_family_t,
                        sin_port: v4.port().to_be(),
                        sin_addr: libc::in_addr {
                            s_addr: u32::from(*v4.ip()).to_be(),
                        },
                        sin_zero: [0; 8],
                    };
                    return Ok(SockAddr::from_sockaddr_in(sin));
                }
                SocketAddr::V6(_) => last_v6 = true,
            }
        }
        let msg = if last_v6 {
            "http backend: origin resolved to IPv6 only; v1 dials IPv4 only"
        } else {
            "http backend: origin endpoint did not resolve to any address"
        };
        Err(std::io::Error::new(std::io::ErrorKind::Other, msg))
    }
}

impl HttpBackend {
    /// Owned-stream variant of [`Backend::bulk_get`].
    ///
    /// The returned [`HttpFetchStream`] borrows nothing from `self`: the
    /// origin address is `Copy`, the path/host are cloned, the
    /// destination pages are copied into an owned `Vec`, and the ring
    /// handle is owned. That makes the stream `'static`, which is what
    /// lets a [`super::registry::BackendRegistry`] hand out streams from
    /// a backend it owns through an `Rc` without borrowing the registry.
    pub fn fetch_stream(
        &self,
        req: &StripeReq,
        src: BulkRef,
        dsts: &[PageRef],
    ) -> HttpFetchStream<'static> {
        let Some(origin) = req.origin() else {
            return HttpFetchStream::immediate_error(HTTP_ENGINE_POLICY.missing_origin);
        };
        let path = origin.origin_object_id.clone();
        let host = self.host.clone();
        let sni_host = self.sni_host.clone();
        let tls = self.tls.clone();

        let dsts_owned = dsts.to_vec();
        let handle = self.handle.clone();
        let origin_addr = self.origin;
        let backing_base = self.backing_base;
        let page_size = self.page_size;
        let conns = self.conns.clone();

        // A metadata entry is not a byte range of the object; it is a
        // synthetic one-page cache entry whose payload is the object's
        // metadata. The sentinel `stripe_idx` would overflow
        // `absolute_range`, so this must branch before that is computed.
        if origin.is_metadata_entry() {
            let mut log = crate::obs::ReqLog::new("backend.http");
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
                        policy: &HTTP_ENGINE_POLICY,
                    }),
                ),
            ));
            return HttpFetchStream::pending(fut, dsts_owned);
        }

        debug_assert!(!origin.is_metadata_entry());
        let (start, len) = absolute_range(origin.stripe_idx, self.stripe_size, src.offset, src.len);

        let mut log = crate::obs::ReqLog::new("backend.http");
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
                        policy: &HTTP_ENGINE_POLICY,
                    },
                    start,
                    len,
                }),
            ),
        ));
        HttpFetchStream::pending(fut, dsts_owned)
    }
}

impl Backend for HttpBackend {
    type Req = StripeReq;
    type Stream<'a> = HttpFetchStream<'a>;

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
        let addr = HttpBackend::resolve_origin("127.0.0.1:8080").expect("resolves");
        assert_eq!(
            addr.as_ipv4(),
            Some((std::net::Ipv4Addr::new(127, 0, 0, 1), 8080))
        );
    }

    #[test]
    fn resolve_origin_rejects_unparseable() {
        assert!(HttpBackend::resolve_origin("not a host:port at all").is_err());
    }

    #[test]
    fn metadata_written_into_page() {
        // The metadata-fill path encodes the object's `ObjectMetadata`
        // through `copy_body_into_pages`, which must land the payload at
        // the page start and zero-fill the tail. The page then decodes
        // back to the same metadata.
        let page_size = 4096usize;
        let mut backing = vec![0xFFu8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: page_size as u32,
        }];
        let body = ObjectMetadata::new(12345).encode().unwrap();
        let body_len = body.len();
        copy_body_into_pages(&body, &dsts, base, page_size, HTTP_PAGE_ERRORS).unwrap();

        let decoded = ObjectMetadata::decode(&backing).unwrap();
        assert_eq!(decoded, ObjectMetadata::new(12345));
        assert_eq!(decoded.length, 12345);
        assert!(decoded.is_empty());
        assert!(
            backing[body_len..].iter().all(|&b| b == 0),
            "tail not zeroed"
        );
    }
}
