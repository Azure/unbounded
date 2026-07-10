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

use ::http::header::{HOST, RANGE};

use crate::bufferpool::{BulkRef, Error, PageRef};
use crate::http::{Method, StatusCode, serialize_request};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::{ObjectMetadata, StripeReq};
use crate::tls::TlsContext;

use super::Backend;
use super::conn::{
    OriginConnPool, body_response_reusable, head_response_reusable, send_request_read_head,
};
use super::http_family::{
    S3_PAGE_ERRORS, S3_RESPONSE_ERRORS, absolute_range, check_origin_status, copy_body_into_pages,
    expected_body_len, locate_in_pages, pages_capacity, write_slice_into_pages,
    zero_fill_pages_from,
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
            return S3FetchStream::immediate_error("s3 backend: request missing origin");
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

/// Perform one whole origin fetch: dial the origin, send a ranged GET,
/// accumulate the response, validate it, and memcpy the body into the
/// destination pages.
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
            "s3 backend: destination page lengths do not match requested range",
        ));
    }
    if len == 0 {
        // Guards the inclusive `start + len - 1` Range bound below
        // against underflow. The Pool always requests a full page, so
        // this is defensive.
        return Err(Error::from("s3 backend: zero-length fetch requested"));
    }

    // Bound concurrent origin work to `http_concurrency`. The permit is
    // held for the whole fetch and returned to the pool on drop.
    let _permit = limiter.acquire().await;
    let request = format_get_request(&path, &host, start, start + len - 1)?;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        None,
        "s3 backend: malformed origin response head",
        "s3 backend: connection closed before response headers complete",
        "s3 backend: response head exceeds limit",
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

    check_origin_status(status, start, S3_RESPONSE_ERRORS)?;

    // An origin that answers a different slice than we asked for would
    // silently corrupt the stripe, so reject a mismatched Content-Range.
    if let Some(cr_start) = content_range_start {
        if cr_start != start {
            return Err(Error::from(
                "s3 backend: origin Content-Range start does not match request",
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
        S3_RESPONSE_ERRORS,
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
        return Err(Error::from("s3 backend: over read from origin"));
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
            S3_PAGE_ERRORS,
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
            locate_in_pages(&dsts, filled, page_size, S3_PAGE_ERRORS)?
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
                Some(_) => return Err(Error::from("s3 backend: short read from origin")),
                None => break,
            }
        }
        filled += n_recv;
    }

    // Zero-fill the tail the body did not cover (short 206 / short 200 /
    // close-delimited stream that ended early). The frontend clamps
    // served reads to the object length, so this padding is never
    // returned to a client.
    zero_fill_pages_from(&dsts, filled, backing_base, page_size, S3_PAGE_ERRORS)?;
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

/// Fill a metadata entry: HEAD the origin object, take its
/// `Content-Length` as the object's byte length, and write the encoded
/// [`ObjectMetadata`] into the (single) destination page.
#[allow(clippy::too_many_arguments)]
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
            "s3 backend: length entry destination smaller than 8 bytes",
        ));
    }

    // Bound concurrent origin work to `http_concurrency` (see `fetch`).
    let _permit = limiter.acquire().await;
    let request = format_head_request(&path, &host)?;
    const MAX_HEAD: usize = 64 * 1024;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        Some(MAX_HEAD),
        "s3 backend: malformed origin response head",
        "s3 backend: connection closed before metadata HEAD headers complete",
        "s3 backend: metadata HEAD response head exceeds 64 KiB",
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
            "s3 backend: metadata HEAD returned non-200 status",
        ));
    }
    let length = content_length
        .ok_or_else(|| Error::from("s3 backend: metadata HEAD missing Content-Length"))?;

    let body = ObjectMetadata::new(length).encode()?;
    copy_body_into_pages(&body, &dsts, backing_base, page_size, S3_PAGE_ERRORS)?;
    if head_response_reusable(version_minor, connection.as_deref(), header_end, buf.len()) {
        conns.put(conn);
    }
    Ok(())
}

/// Format a ranged HTTP/1.1 GET request against the S3 origin.
/// `start`/`end` are inclusive byte offsets for the `Range` header.
fn format_get_request(path: &str, host: &str, start: u64, end: u64) -> Result<Vec<u8>, Error> {
    let req = ::http::Request::builder()
        .method(Method::GET)
        .uri(path)
        .header(HOST, host)
        .header(RANGE, format!("bytes={start}-{end}"))
        .body(())
        .map_err(|_| Error::from("s3 backend: failed to build origin GET request"))?;
    Ok(serialize_request(&req))
}

/// Format an HTTP/1.1 HEAD request against the S3 origin, used by the
/// length-entry fill path.
fn format_head_request(path: &str, host: &str) -> Result<Vec<u8>, Error> {
    let req = ::http::Request::builder()
        .method(Method::HEAD)
        .uri(path)
        .header(HOST, host)
        .body(())
        .map_err(|_| Error::from("s3 backend: failed to build origin HEAD request"))?;
    Ok(serialize_request(&req))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn get_request_has_expected_headers() {
        let req = format_get_request("/bucket/key", "s3.example.com", 0, 4095).unwrap();
        let s = std::str::from_utf8(&req).unwrap();
        assert!(s.starts_with("GET /bucket/key HTTP/1.1\r\n"), "got: {s}");
        assert!(s.contains("host: s3.example.com\r\n"), "got: {s}");
        assert!(s.contains("range: bytes=0-4095\r\n"), "got: {s}");
        assert!(!s.contains("connection:"), "got: {s}");
        assert!(!s.contains("x-amz-date"), "got: {s}");
        assert!(!s.contains("authorization"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn head_request_omits_range() {
        let req = format_head_request("/bucket/key", "s3.example.com").unwrap();
        let s = std::str::from_utf8(&req).unwrap();
        assert!(s.starts_with("HEAD /bucket/key HTTP/1.1\r\n"), "got: {s}");
        assert!(s.contains("host: s3.example.com\r\n"), "got: {s}");
        assert!(!s.contains("range:"), "got: {s}");
        assert!(!s.contains("x-amz-date"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn resolve_origin_parses_ipv4() {
        let addr = S3Backend::resolve_origin("127.0.0.1:9000").expect("resolves");
        assert_eq!(
            addr.as_ipv4(),
            Some((std::net::Ipv4Addr::new(127, 0, 0, 1), 9000))
        );
    }
}
