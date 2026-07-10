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

use crate::bufferpool::{BulkRef, Error, PageRef};
use crate::http::StatusCode;
use crate::ring::{NetHandle, SockAddr};
use crate::storage::{ObjectMetadata, StripeReq};
use crate::tls::TlsContext;

use super::Backend;
use super::conn::{
    OriginConnPool, body_response_reusable, head_response_reusable, send_request_read_head,
};
use super::http_family::{
    HTTP_PAGE_ERRORS, HTTP_RESPONSE_ERRORS, HttpFlavor, absolute_range, check_origin_status,
    copy_body_into_pages, expected_body_len, format_get_request, format_head_request,
    locate_in_pages, pages_capacity, write_slice_into_pages, zero_fill_pages_from,
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
            return HttpFetchStream::immediate_error("http backend: request missing origin");
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
                    fetch_metadata(
                        handle,
                        origin_addr,
                        host,
                        sni_host,
                        tls,
                        path,
                        dsts_owned.clone(),
                        backing_base,
                        page_size,
                        self.limiter.clone(),
                        conns,
                    ),
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
                fetch(
                    handle,
                    conns,
                    origin_addr,
                    host,
                    sni_host,
                    tls,
                    path,
                    start,
                    len,
                    dsts_owned.clone(),
                    backing_base,
                    page_size,
                    self.limiter.clone(),
                ),
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

/// Perform one whole origin fetch: dial the origin, send a ranged GET,
/// receive and validate the response head, then stream the body
/// directly into the destination bufferpool pages via `recv_fixed`
/// (zero-copy: no heap accumulation of the body). The destination
/// pages are captured in `delivered` so the stream can yield them after
/// this resolves.
#[allow(clippy::too_many_arguments)]
async fn fetch(
    handle: NetHandle,
    conns: OriginConnPool,
    origin: SockAddr,
    host: String,
    sni_host: String,
    tls: Option<Rc<TlsContext>>,
    path: String,
    start: u64,
    len: u64,
    dsts: Vec<PageRef>,
    backing_base: *mut u8,
    page_size: usize,
    limiter: FetchLimiter,
) -> Result<(), Error> {
    let total: u64 = dsts.iter().map(|p| p.len as u64).sum();
    if total != len {
        return Err(Error::from(
            "http backend: destination page lengths do not match requested range",
        ));
    }
    if len == 0 {
        // Guards the inclusive `start + len - 1` Range bound below
        // against underflow. The Pool always requests a full page, so
        // this is defensive.
        return Err(Error::from("http backend: zero-length fetch requested"));
    }

    // Bound concurrent origin work to `http_concurrency`. The permit is
    // held for the whole fetch and returned to the pool on drop.
    let _permit = limiter.acquire().await;
    let request = format_get_request(HttpFlavor::Http, &path, &host, start, start + len - 1)?;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        None,
        "http backend: malformed origin response head",
        "http backend: connection closed before response headers complete",
        "http backend: response head exceeds limit",
    )
    .await?;
    let fd = conn.fd();
    let is_tls = conn.is_tls();
    let status = head.status;
    let version_minor = head.version_minor;
    let header_end = head.header_end;
    let content_length = head.content_length;
    let content_range_start = head.content_range_start;
    let connection = head.connection;
    let buf = head.buf;

    check_origin_status(status, start, HTTP_RESPONSE_ERRORS)?;

    // An origin that answers a different slice than we asked for would
    // silently corrupt the stripe, so reject a mismatched Content-Range.
    if let Some(cr_start) = content_range_start {
        if cr_start != start {
            return Err(Error::from(
                "http backend: origin Content-Range start does not match request",
            ));
        }
    }

    // The origin tells us how many body bytes this response carries. A
    // ranged request whose end runs past the object's EOF gets a short
    // 206 (Content-Length < the page we asked for) covering only the
    // bytes that exist; we read exactly that many and zero-fill the rest
    // of the page. The frontend clamps every served read to the object
    // length, so the padding is never handed to a client. A connection
    // that closes before the advertised length is a genuine truncation.
    let body_start = header_end;
    let body_len_mode = expected_body_len(
        status,
        version_minor,
        connection.as_deref(),
        content_length,
        len,
        HTTP_RESPONSE_ERRORS,
    )?;

    // `body_cap` is the most body bytes we will accept into the pages.
    // For a known Content-Length it is that length (already validated by
    // `expected_body_len` to be <= the requested range, hence <=
    // capacity); for the close-delimited case it is the requested range,
    // which equals the page capacity. Either way it never exceeds
    // `capacity`, so the destination pages always have room.
    let capacity = pages_capacity(&dsts);
    debug_assert_eq!(capacity as u64, len, "capacity must equal requested range");
    let body_cap: usize = match body_len_mode {
        Some(n) => n as usize,
        None => len as usize,
    };
    if body_cap > capacity {
        return Err(Error::from("http backend: over read from origin"));
    }

    // recv_fixed targets the registered fixed backing at buf index 0,
    // whose base is `backing_base` (Phase 2 invariant). The page math
    // below produces registered byte offsets relative to that same base.

    // Body bytes that shared the header TCP segment are already in `buf`
    // past `header_end`; place them first with a page-aware memcpy. Any
    // bytes beyond `body_cap` are surplus the origin overstuffed and are
    // dropped (the old path truncated them identically).
    let leading = &buf[body_start..];
    let lead_take = leading.len().min(body_cap);
    if lead_take > 0 {
        write_slice_into_pages(
            &leading[..lead_take],
            &dsts,
            0,
            backing_base,
            page_size,
            HTTP_PAGE_ERRORS,
        )?;
    }
    let mut filled = lead_take;

    // Stream the remaining body straight into the destination pages. A
    // single recv never crosses a page boundary (pages are contiguous
    // within themselves but not with each other in the backing), so the
    // length is capped at the room left in the current page and at the
    // bytes still wanted.
    while filled < body_cap {
        let Some((page_byte_off, room)) =
            locate_in_pages(&dsts, filled, page_size, HTTP_PAGE_ERRORS)?
        else {
            break;
        };
        let recv_len = room.min(body_cap - filled);
        // SAFETY: `page_byte_off` addresses a page inside the registered
        // fixed backing (buf index 0, base == backing_base) that the
        // Pool reserves for this fetch across every await here; the
        // backend is shard-pinned so no other thread touches it. The
        // destination stays reserved until this future resolves.
        let n_recv = crate::tls::recv_fixed(&handle, fd, is_tls, page_byte_off, recv_len).await?;
        if n_recv == 0 {
            // EOF. With a known length this is a truncation; for the
            // close-delimited case it is the normal end of the body.
            match body_len_mode {
                Some(_) => return Err(Error::from("http backend: short read from origin")),
                None => break,
            }
        }
        filled += n_recv;
    }

    // Zero-fill the tail the body did not cover (short 206 / short 200 /
    // close-delimited stream that ended early). The frontend clamps
    // served reads to the object length, so this padding is never
    // returned to a client.
    zero_fill_pages_from(&dsts, filled, backing_base, page_size, HTTP_PAGE_ERRORS)?;
    if body_response_reusable(
        version_minor,
        connection.as_deref(),
        content_length,
        body_cap,
        leading.len(),
        filled,
    ) {
        conns.put(conn);
    }
    Ok(())
}

/// Fill a length entry: HEAD the origin object, take its
/// `Content-Length` as the object's byte length, and write that length
/// as a little-endian `u64` into the (single) destination page. HEAD
/// has no body, so only the header block is read.
///
/// The 8 length bytes are written through the same
/// [`copy_body_into_pages`] path as the data fetch, which copies them
/// into the first page bytes and zero-fills the remainder of the
/// destination pages.
async fn fetch_metadata(
    handle: NetHandle,
    origin: SockAddr,
    host: String,
    sni_host: String,
    tls: Option<Rc<TlsContext>>,
    path: String,
    dsts: Vec<PageRef>,
    backing_base: *mut u8,
    page_size: usize,
    limiter: FetchLimiter,
    conns: OriginConnPool,
) -> Result<(), Error> {
    let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
    if capacity < 8 {
        return Err(Error::from(
            "http backend: length entry destination smaller than 8 bytes",
        ));
    }

    // Bound concurrent origin work to `http_concurrency` (see `fetch`).
    let _permit = limiter.acquire().await;
    let request = format_head_request(HttpFlavor::Http, &path, &host)?;
    const MAX_HEAD: usize = 64 * 1024;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        Some(MAX_HEAD),
        "http backend: malformed origin response head",
        "http backend: connection closed before metadata HEAD headers complete",
        "http backend: metadata HEAD response head exceeds 64 KiB",
    )
    .await?;
    let status = head.status;
    let version_minor = head.version_minor;
    let header_end = head.header_end;
    let content_length = head.content_length;
    let connection = head.connection;
    let buf = head.buf;

    if status == StatusCode::NOT_FOUND {
        return Err(Error::OriginNotFound);
    }
    if status != StatusCode::OK {
        return Err(Error::from(
            "http backend: metadata HEAD returned non-200 status",
        ));
    }
    let length = content_length
        .ok_or_else(|| Error::from("http backend: metadata HEAD missing Content-Length"))?;

    let body = ObjectMetadata::new(length).encode()?;
    copy_body_into_pages(&body, &dsts, backing_base, page_size, HTTP_PAGE_ERRORS)?;
    if head_response_reusable(version_minor, connection.as_deref(), header_end, buf.len()) {
        conns.put(conn);
    }
    Ok(())
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
