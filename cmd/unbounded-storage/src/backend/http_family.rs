// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared GET/HEAD engines and page mechanics for HTTP-family origin backends.

use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use ::http::header::{HOST, RANGE};

use crate::bufferpool::{Error, PageRef, PageStream};
use crate::http::{Method, StatusCode, response_closes_after_body, serialize_request};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::ObjectMetadata;
use crate::tls::TlsContext;

use super::conn::{
    OriginConnPool, body_response_reusable, head_response_reusable, send_request_read_head,
};
use super::limiter::FetchLimiter;

const AZURE_MS_VERSION: &str = "2021-08-06";

#[derive(Clone, Copy)]
pub(super) enum HttpFlavor {
    Http,
    S3,
    Azure,
}

impl HttpFlavor {
    const fn is_azure(self) -> bool {
        matches!(self, Self::Azure)
    }
}

#[derive(Clone, Copy)]
pub(super) struct ResponseErrorMessages {
    missing_content_length: &'static str,
    content_length_exceeds_range: &'static str,
    non_success_status: &'static str,
    ignored_range: &'static str,
}

impl ResponseErrorMessages {
    pub(super) const fn new(
        missing_content_length: &'static str,
        content_length_exceeds_range: &'static str,
        non_success_status: &'static str,
        ignored_range: &'static str,
    ) -> Self {
        Self {
            missing_content_length,
            content_length_exceeds_range,
            non_success_status,
            ignored_range,
        }
    }
}

#[derive(Clone, Copy)]
pub(super) struct PageErrorMessages {
    over_read: &'static str,
    offset_overflow: &'static str,
}

impl PageErrorMessages {
    pub(super) const fn new(over_read: &'static str, offset_overflow: &'static str) -> Self {
        Self {
            over_read,
            offset_overflow,
        }
    }
}

pub(super) const HTTP_PAGE_ERRORS: PageErrorMessages = PageErrorMessages::new(
    "http backend: over read from origin",
    "http backend: page byte offset overflow",
);

#[derive(Clone, Copy)]
pub(super) struct HttpEnginePolicy {
    flavor: HttpFlavor,
    response_errors: ResponseErrorMessages,
    page_errors: PageErrorMessages,
    pub(super) missing_origin: &'static str,
    get_build: &'static str,
    get_destination_mismatch: &'static str,
    get_zero_length: &'static str,
    get_malformed_head: &'static str,
    get_closed_head: &'static str,
    get_head_too_large: &'static str,
    get_content_range_mismatch: &'static str,
    get_short_read: &'static str,
    head_build: &'static str,
    head_destination_too_small: &'static str,
    head_malformed: &'static str,
    head_closed: &'static str,
    head_too_large: &'static str,
    head_non_200: &'static str,
    head_missing_content_length: &'static str,
}

pub(super) static HTTP_ENGINE_POLICY: HttpEnginePolicy = HttpEnginePolicy {
    flavor: HttpFlavor::Http,
    response_errors: ResponseErrorMessages::new(
        "http backend: origin response missing Content-Length on keep-alive connection",
        "http backend: origin Content-Length exceeds requested range",
        "http backend: origin returned non-2xx status",
        "http backend: origin ignored Range (200) for a non-zero offset",
    ),
    page_errors: HTTP_PAGE_ERRORS,
    missing_origin: "http backend: request missing origin",
    get_build: "http backend: failed to build origin GET request",
    get_destination_mismatch: "http backend: destination page lengths do not match requested range",
    get_zero_length: "http backend: zero-length fetch requested",
    get_malformed_head: "http backend: malformed origin response head",
    get_closed_head: "http backend: connection closed before response headers complete",
    get_head_too_large: "http backend: response head exceeds limit",
    get_content_range_mismatch: "http backend: origin Content-Range start does not match request",
    get_short_read: "http backend: short read from origin",
    head_build: "http backend: failed to build origin HEAD request",
    head_destination_too_small: "http backend: length entry destination smaller than 8 bytes",
    head_malformed: "http backend: malformed origin response head",
    head_closed: "http backend: connection closed before metadata HEAD headers complete",
    head_too_large: "http backend: metadata HEAD response head exceeds 64 KiB",
    head_non_200: "http backend: metadata HEAD returned non-200 status",
    head_missing_content_length: "http backend: metadata HEAD missing Content-Length",
};

pub(super) static S3_ENGINE_POLICY: HttpEnginePolicy = HttpEnginePolicy {
    flavor: HttpFlavor::S3,
    response_errors: ResponseErrorMessages::new(
        "s3 backend: origin response missing Content-Length on keep-alive connection",
        "s3 backend: origin Content-Length exceeds requested range",
        "s3 backend: origin returned non-2xx status",
        "s3 backend: origin ignored Range (200) for a non-zero offset",
    ),
    page_errors: PageErrorMessages::new(
        "s3 backend: over read from origin",
        "s3 backend: page byte offset overflow",
    ),
    missing_origin: "s3 backend: request missing origin",
    get_build: "s3 backend: failed to build origin GET request",
    get_destination_mismatch: "s3 backend: destination page lengths do not match requested range",
    get_zero_length: "s3 backend: zero-length fetch requested",
    get_malformed_head: "s3 backend: malformed origin response head",
    get_closed_head: "s3 backend: connection closed before response headers complete",
    get_head_too_large: "s3 backend: response head exceeds limit",
    get_content_range_mismatch: "s3 backend: origin Content-Range start does not match request",
    get_short_read: "s3 backend: short read from origin",
    head_build: "s3 backend: failed to build origin HEAD request",
    head_destination_too_small: "s3 backend: length entry destination smaller than 8 bytes",
    head_malformed: "s3 backend: malformed origin response head",
    head_closed: "s3 backend: connection closed before metadata HEAD headers complete",
    head_too_large: "s3 backend: metadata HEAD response head exceeds 64 KiB",
    head_non_200: "s3 backend: metadata HEAD returned non-200 status",
    head_missing_content_length: "s3 backend: metadata HEAD missing Content-Length",
};

pub(super) static AZURE_ENGINE_POLICY: HttpEnginePolicy = HttpEnginePolicy {
    flavor: HttpFlavor::Azure,
    response_errors: ResponseErrorMessages::new(
        "azure backend: origin response missing Content-Length on keep-alive connection",
        "azure backend: origin Content-Length exceeds requested range",
        "azure backend: origin returned non-2xx status",
        "azure backend: origin ignored Range (200) for a non-zero offset",
    ),
    page_errors: PageErrorMessages::new(
        "azure backend: over read from origin",
        "azure backend: page byte offset overflow",
    ),
    missing_origin: "azure backend: request missing origin",
    get_build: "azure backend: failed to build origin GET request",
    get_destination_mismatch: "azure backend: destination page lengths do not match requested range",
    get_zero_length: "azure backend: zero-length fetch requested",
    get_malformed_head: "azure backend: malformed origin response head",
    get_closed_head: "azure backend: connection closed before response headers complete",
    get_head_too_large: "azure backend: response head exceeds limit",
    get_content_range_mismatch: "azure backend: origin Content-Range start does not match request",
    get_short_read: "azure backend: short read from origin",
    head_build: "azure backend: failed to build origin HEAD request",
    head_destination_too_small: "azure backend: length entry destination smaller than 8 bytes",
    head_malformed: "azure backend: malformed origin response head",
    head_closed: "azure backend: connection closed before metadata HEAD headers complete",
    head_too_large: "azure backend: metadata HEAD response head exceeds 64 KiB",
    head_non_200: "azure backend: metadata HEAD returned non-200 status",
    head_missing_content_length: "azure backend: metadata HEAD missing Content-Length",
};

pub(super) struct OriginFetchInputs {
    pub(super) handle: NetHandle,
    pub(super) conns: OriginConnPool,
    pub(super) origin: SockAddr,
    pub(super) host: String,
    pub(super) sni_host: String,
    pub(super) tls: Option<Rc<TlsContext>>,
    pub(super) path: String,
    pub(super) dsts: Vec<PageRef>,
    pub(super) backing_base: *mut u8,
    pub(super) page_size: usize,
    pub(super) limiter: FetchLimiter,
    pub(super) policy: &'static HttpEnginePolicy,
}

pub(super) struct GetFetchInputs {
    pub(super) origin: OriginFetchInputs,
    pub(super) start: u64,
    pub(super) len: u64,
}

/// Drives one origin fetch to completion, then yields its destination pages in
/// order. A failed fetch yields exactly one error before terminating.
pub struct FetchStream<'a> {
    state: FetchState<'a>,
    delivered: Vec<PageRef>,
    next: usize,
}

enum FetchState<'a> {
    Running(Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>),
    Delivering,
    Failed(Option<Error>),
    Done,
}

impl<'a> FetchStream<'a> {
    pub(super) fn pending(
        fut: Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>,
        delivered: Vec<PageRef>,
    ) -> Self {
        Self {
            state: FetchState::Running(fut),
            delivered,
            next: 0,
        }
    }

    pub(super) fn immediate_error(msg: &'static str) -> Self {
        Self {
            state: FetchState::Failed(Some(Error::from(msg))),
            delivered: Vec::new(),
            next: 0,
        }
    }
}

impl PageStream for FetchStream<'_> {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        let this = self.get_mut();
        loop {
            match &mut this.state {
                FetchState::Running(fut) => match fut.as_mut().poll(cx) {
                    Poll::Pending => return Poll::Pending,
                    Poll::Ready(Ok(())) => this.state = FetchState::Delivering,
                    Poll::Ready(Err(e)) => this.state = FetchState::Failed(Some(e)),
                },
                FetchState::Delivering => {
                    if this.next >= this.delivered.len() {
                        this.state = FetchState::Done;
                        return Poll::Ready(None);
                    }
                    let page = this.delivered[this.next];
                    this.next += 1;
                    return Poll::Ready(Some(Ok(page)));
                }
                FetchState::Failed(slot) => {
                    let error = slot.take();
                    this.state = FetchState::Done;
                    return Poll::Ready(error.map(Err));
                }
                FetchState::Done => return Poll::Ready(None),
            }
        }
    }
}

/// Copy `body` into the destination pages in order and zero-fill any
/// destination bytes the body does not cover.
pub(super) fn copy_body_into_pages(
    body: &[u8],
    dsts: &[PageRef],
    backing_base: *mut u8,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<(), Error> {
    let capacity = pages_capacity(dsts);
    if body.len() > capacity {
        return Err(Error::from(errors.over_read));
    }
    let mut consumed = 0usize;
    for page in dsts {
        let n = page.len as usize;
        let avail = body.len().saturating_sub(consumed).min(n);
        let page_offset = page_byte_offset(page, page_size, errors)?;
        // SAFETY: the destination addresses a page inside the registered
        // backing the embedder keeps alive for the shard's lifetime. The
        // caller guarantees the page geometry and shard-local access.
        unsafe {
            let dst = backing_base.add(page_offset);
            if avail > 0 {
                std::ptr::copy_nonoverlapping(body.as_ptr().add(consumed), dst, avail);
            }
            if avail < n {
                std::ptr::write_bytes(dst.add(avail), 0, n - avail);
            }
        }
        consumed += avail;
    }
    Ok(())
}

/// Total destination byte capacity across `dsts`.
pub(super) fn pages_capacity(dsts: &[PageRef]) -> usize {
    dsts.iter().map(|p| p.len as usize).sum()
}

/// Registered byte offset of a page within the fixed backing.
pub(super) fn page_byte_offset(
    page: &PageRef,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<usize, Error> {
    (page.page_idx as usize)
        .checked_mul(page_size)
        .and_then(|base| base.checked_add(page.offset as usize))
        .ok_or_else(|| Error::from(errors.offset_overflow))
}

/// Locate the destination page covering logical body offset `at`.
pub(super) fn locate_in_pages(
    dsts: &[PageRef],
    at: usize,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<Option<(usize, usize)>, Error> {
    let mut page_start = 0usize;
    for page in dsts {
        let n = page.len as usize;
        if at < page_start + n {
            let within = at - page_start;
            let off = page_byte_offset(page, page_size, errors)?
                .checked_add(within)
                .ok_or_else(|| Error::from(errors.offset_overflow))?;
            return Ok(Some((off, n - within)));
        }
        page_start += n;
    }
    Ok(None)
}

/// Copy `src` into the destination pages starting at logical offset `start`.
pub(super) fn write_slice_into_pages(
    src: &[u8],
    dsts: &[PageRef],
    start: usize,
    backing_base: *mut u8,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<(), Error> {
    let end = start
        .checked_add(src.len())
        .ok_or_else(|| Error::from(errors.offset_overflow))?;
    if end > pages_capacity(dsts) {
        return Err(Error::from(errors.over_read));
    }
    let mut written = 0usize;
    while written < src.len() {
        let (off, room) = locate_in_pages(dsts, start + written, page_size, errors)?
            .ok_or_else(|| Error::from(errors.over_read))?;
        let n = room.min(src.len() - written);
        // SAFETY: `off` and `n` describe bytes within one destination page;
        // the caller guarantees the backing remains valid and shard-local.
        unsafe {
            std::ptr::copy_nonoverlapping(src.as_ptr().add(written), backing_base.add(off), n);
        }
        written += n;
    }
    Ok(())
}

/// Zero-fill destination bytes from logical offset `from` to capacity.
pub(super) fn zero_fill_pages_from(
    dsts: &[PageRef],
    from: usize,
    backing_base: *mut u8,
    page_size: usize,
    errors: PageErrorMessages,
) -> Result<(), Error> {
    let mut page_start = 0usize;
    for page in dsts {
        let n = page.len as usize;
        let page_end = page_start + n;
        if from < page_end {
            let within = from.saturating_sub(page_start);
            let off = page_byte_offset(page, page_size, errors)?
                .checked_add(within)
                .ok_or_else(|| Error::from(errors.offset_overflow))?;
            // SAFETY: `off` addresses the current destination page and
            // `n - within` cannot escape that page.
            unsafe {
                std::ptr::write_bytes(backing_base.add(off), 0, n - within);
            }
        }
        page_start = page_end;
    }
    Ok(())
}

/// Compute the absolute origin byte range for a stripe sub-range.
pub(super) fn absolute_range(
    stripe_idx: u64,
    stripe_size: u64,
    src_offset: u64,
    src_len: u32,
) -> (u64, u64) {
    let start = stripe_idx
        .saturating_mul(stripe_size)
        .saturating_add(src_offset);
    (start, src_len as u64)
}

/// Validate the response status against the requested absolute offset.
pub(super) fn check_origin_status(
    status: StatusCode,
    start: u64,
    errors: ResponseErrorMessages,
) -> Result<(), Error> {
    if status == StatusCode::NOT_FOUND {
        return Err(Error::OriginNotFound);
    }
    if status != StatusCode::OK && status != StatusCode::PARTIAL_CONTENT {
        return Err(Error::from(errors.non_success_status));
    }
    if status == StatusCode::OK && start != 0 {
        return Err(Error::from(errors.ignored_range));
    }
    Ok(())
}

/// Determine the expected body length, or `None` for a close-delimited body.
pub(super) fn expected_body_len(
    status: StatusCode,
    version_minor: u8,
    connection: Option<&str>,
    content_length: Option<u64>,
    requested_len: u64,
    errors: ResponseErrorMessages,
) -> Result<Option<u64>, Error> {
    let Some(content_length) = content_length else {
        if !response_closes_after_body(version_minor, connection) {
            return Err(Error::from(errors.missing_content_length));
        }
        return Ok(None);
    };
    if status == StatusCode::PARTIAL_CONTENT && content_length > requested_len {
        return Err(Error::from(errors.content_length_exceeds_range));
    }
    Ok(Some(content_length.min(requested_len)))
}

fn format_get_request(
    policy: &'static HttpEnginePolicy,
    path: &str,
    host: &str,
    start: u64,
    end: u64,
) -> Result<Vec<u8>, Error> {
    let mut builder = ::http::Request::builder()
        .method(Method::GET)
        .uri(path)
        .header(HOST, host)
        .header(RANGE, format!("bytes={start}-{end}"));
    if policy.flavor.is_azure() {
        builder = builder.header("x-ms-version", AZURE_MS_VERSION);
    }
    let request = builder
        .body(())
        .map_err(|_| Error::from(policy.get_build))?;
    Ok(serialize_request(&request))
}

fn format_head_request(
    policy: &'static HttpEnginePolicy,
    path: &str,
    host: &str,
) -> Result<Vec<u8>, Error> {
    let mut builder = ::http::Request::builder()
        .method(Method::HEAD)
        .uri(path)
        .header(HOST, host);
    if policy.flavor.is_azure() {
        builder = builder.header("x-ms-version", AZURE_MS_VERSION);
    }
    let request = builder
        .body(())
        .map_err(|_| Error::from(policy.head_build))?;
    Ok(serialize_request(&request))
}

pub(super) async fn fetch_range(inputs: GetFetchInputs) -> Result<(), Error> {
    let GetFetchInputs {
        origin:
            OriginFetchInputs {
                handle,
                conns,
                origin,
                host,
                sni_host,
                tls,
                path,
                dsts,
                backing_base,
                page_size,
                limiter,
                policy,
            },
        start,
        len,
    } = inputs;

    let total: u64 = dsts.iter().map(|p| p.len as u64).sum();
    if total != len {
        return Err(Error::from(policy.get_destination_mismatch));
    }
    if len == 0 {
        return Err(Error::from(policy.get_zero_length));
    }

    let _permit = limiter.acquire().await;
    let request = format_get_request(policy, &path, &host, start, start + len - 1)?;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        None,
        policy.get_malformed_head,
        policy.get_closed_head,
        policy.get_head_too_large,
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

    check_origin_status(status, start, policy.response_errors)?;
    if let Some(cr_start) = content_range_start {
        if cr_start != start {
            return Err(Error::from(policy.get_content_range_mismatch));
        }
    }

    let body_start = header_end;
    let body_len_mode = expected_body_len(
        status,
        version_minor,
        connection.as_deref(),
        content_length,
        len,
        policy.response_errors,
    )?;
    let capacity = pages_capacity(&dsts);
    debug_assert_eq!(capacity as u64, len, "capacity must equal requested range");
    let body_cap: usize = match body_len_mode {
        Some(n) => n as usize,
        None => len as usize,
    };
    if body_cap > capacity {
        return Err(Error::from(policy.page_errors.over_read));
    }

    let leading = &buf[body_start..];
    let lead_take = leading.len().min(body_cap);
    if lead_take > 0 {
        write_slice_into_pages(
            &leading[..lead_take],
            &dsts,
            0,
            backing_base,
            page_size,
            policy.page_errors,
        )?;
    }
    let mut filled = lead_take;

    while filled < body_cap {
        let Some((page_byte_off, room)) =
            locate_in_pages(&dsts, filled, page_size, policy.page_errors)?
        else {
            break;
        };
        let recv_len = room.min(body_cap - filled);
        let n_recv = crate::tls::recv_fixed(&handle, fd, is_tls, page_byte_off, recv_len).await?;
        if n_recv == 0 {
            match body_len_mode {
                Some(_) => return Err(Error::from(policy.get_short_read)),
                None => break,
            }
        }
        filled += n_recv;
    }

    zero_fill_pages_from(&dsts, filled, backing_base, page_size, policy.page_errors)?;
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

pub(super) async fn fetch_metadata(inputs: OriginFetchInputs) -> Result<(), Error> {
    let OriginFetchInputs {
        handle,
        conns,
        origin,
        host,
        sni_host,
        tls,
        path,
        dsts,
        backing_base,
        page_size,
        limiter,
        policy,
    } = inputs;

    let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
    if capacity < 8 {
        return Err(Error::from(policy.head_destination_too_small));
    }

    let _permit = limiter.acquire().await;
    let request = format_head_request(policy, &path, &host)?;
    const MAX_HEAD: usize = 64 * 1024;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        Some(MAX_HEAD),
        policy.head_malformed,
        policy.head_closed,
        policy.head_too_large,
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
        return Err(Error::from(policy.head_non_200));
    }
    let length = content_length.ok_or_else(|| Error::from(policy.head_missing_content_length))?;

    let body = ObjectMetadata::new(length).encode()?;
    copy_body_into_pages(&body, &dsts, backing_base, page_size, policy.page_errors)?;
    if head_response_reusable(version_minor, connection.as_deref(), header_end, buf.len()) {
        conns.put(conn);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;

    const ERRORS: PageErrorMessages = PageErrorMessages::new(
        "test backend: over read from origin",
        "test backend: page byte offset overflow",
    );

    const POLICIES: [&HttpEnginePolicy; 3] =
        [&HTTP_ENGINE_POLICY, &S3_ENGINE_POLICY, &AZURE_ENGINE_POLICY];

    fn page(page_idx: u32) -> PageRef {
        PageRef {
            page_idx,
            offset: 0,
            len: 4096,
        }
    }

    fn poll(stream: &mut FetchStream<'_>) -> Poll<Option<Result<PageRef, Error>>> {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        Pin::new(stream).poll_next(&mut cx)
    }

    fn assert_page(stream: &mut FetchStream<'_>, expected: PageRef) {
        match poll(stream) {
            Poll::Ready(Some(Ok(page))) => assert_eq!(page, expected),
            other => panic!("expected page {expected:?}, got {other:?}"),
        }
    }

    fn assert_done(stream: &mut FetchStream<'_>) {
        assert!(matches!(poll(stream), Poll::Ready(None)));
    }

    #[test]
    fn successful_fetch_yields_pages_in_order_then_stays_done() {
        let pages = vec![page(3), page(7)];
        let mut stream = FetchStream::pending(Box::pin(async { Ok(()) }), pages.clone());

        assert_page(&mut stream, pages[0]);
        assert_page(&mut stream, pages[1]);
        assert_done(&mut stream);
        assert_done(&mut stream);
    }

    #[test]
    fn failed_fetch_yields_one_error_then_stays_done() {
        let mut stream = FetchStream::pending(
            Box::pin(async { Err(Error::from("fetch failed")) }),
            vec![page(1)],
        );

        assert!(matches!(poll(&mut stream), Poll::Ready(Some(Err(_)))));
        assert_done(&mut stream);
        assert_done(&mut stream);
    }

    #[test]
    fn successful_empty_delivery_ends_cleanly() {
        let mut stream = FetchStream::pending(Box::pin(async { Ok(()) }), Vec::new());

        assert_done(&mut stream);
        assert_done(&mut stream);
    }

    #[test]
    fn copy_body_into_pages_fills_and_zeroes_pages() {
        let page_size = 4096usize;
        let mut backing = vec![0xAAu8; page_size * 3];
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 8,
                len: 3,
            },
        ];

        copy_body_into_pages(
            &[1, 2, 3, 4, 5],
            &dsts,
            backing.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();

        assert_eq!(&backing[page_size..page_size + 4], &[1, 2, 3, 4]);
        assert_eq!(&backing[2 * page_size + 8..2 * page_size + 11], &[5, 0, 0]);
    }

    #[test]
    fn copy_body_into_pages_rejects_over_read_with_supplied_message() {
        let mut backing = [0u8; 4];
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4,
        }];

        let err =
            copy_body_into_pages(&[0; 5], &dsts, backing.as_mut_ptr(), 4, ERRORS).unwrap_err();

        assert_eq!(format_error(err), "test backend: over read from origin");
    }

    #[test]
    fn page_geometry_helpers_locate_capacity_and_report_overflow() {
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 8,
                len: 3,
            },
        ];

        assert_eq!(pages_capacity(&dsts), 7);
        assert_eq!(pages_capacity(&[]), 0);
        assert_eq!(
            locate_in_pages(&dsts, 3, 4096, ERRORS).unwrap(),
            Some((4099, 1))
        );
        assert_eq!(
            locate_in_pages(&dsts, 4, 4096, ERRORS).unwrap(),
            Some((8200, 3))
        );
        assert_eq!(locate_in_pages(&dsts, 7, 4096, ERRORS).unwrap(), None);

        let overflow = PageRef {
            page_idx: u32::MAX,
            offset: u32::MAX,
            len: 1,
        };
        let err = page_byte_offset(&overflow, usize::MAX, ERRORS).unwrap_err();
        assert_eq!(format_error(err), "test backend: page byte offset overflow");
    }

    #[test]
    fn write_slice_into_pages_assembles_across_page_boundary() {
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size * 3];
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 8,
                len: 3,
            },
        ];

        write_slice_into_pages(
            &[10, 20, 30],
            &dsts,
            0,
            backing.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();
        write_slice_into_pages(
            &[40, 50, 60, 70],
            &dsts,
            3,
            backing.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();

        assert_eq!(&backing[page_size..page_size + 4], &[10, 20, 30, 40]);
        assert_eq!(
            &backing[2 * page_size + 8..2 * page_size + 11],
            &[50, 60, 70]
        );
    }

    #[test]
    fn write_slice_into_pages_rejects_overflow() {
        let mut backing = [0u8; 4];
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4,
        }];

        let err =
            write_slice_into_pages(&[0; 3], &dsts, 2, backing.as_mut_ptr(), 4, ERRORS).unwrap_err();

        assert_eq!(format_error(err), "test backend: over read from origin");
    }

    #[test]
    fn zero_fill_pages_from_spans_remaining_pages() {
        let page_size = 4096usize;
        let mut backing = vec![0xAAu8; page_size * 3];
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 0,
                len: 4,
            },
        ];

        zero_fill_pages_from(&dsts, 2, backing.as_mut_ptr(), page_size, ERRORS).unwrap();

        assert_eq!(&backing[page_size..page_size + 2], &[0xAA, 0xAA]);
        assert_eq!(&backing[page_size + 2..page_size + 4], &[0, 0]);
        assert_eq!(&backing[2 * page_size..2 * page_size + 4], &[0, 0, 0, 0]);
    }

    #[test]
    fn streaming_write_and_zero_fill_match_copy() {
        let page_size = 4096usize;
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 0,
                len: 4,
            },
        ];
        let mut via_copy = vec![0xAAu8; page_size * 3];
        let mut via_stream = via_copy.clone();

        copy_body_into_pages(&[9, 8], &dsts, via_copy.as_mut_ptr(), page_size, ERRORS).unwrap();
        write_slice_into_pages(
            &[9, 8],
            &dsts,
            0,
            via_stream.as_mut_ptr(),
            page_size,
            ERRORS,
        )
        .unwrap();
        zero_fill_pages_from(&dsts, 2, via_stream.as_mut_ptr(), page_size, ERRORS).unwrap();

        assert_eq!(via_copy, via_stream);
    }

    #[test]
    fn absolute_range_offsets_into_stripe() {
        assert_eq!(absolute_range(0, 4 * 1024 * 1024, 100, 200), (100, 200));
        let stripe = 4 * 1024 * 1024u64;
        assert_eq!(
            absolute_range(3, stripe, 8192, 4096),
            (3 * stripe + 8192, 4096)
        );
    }

    #[test]
    fn response_status_policy_is_shared_by_all_flavors() {
        for policy in POLICIES {
            assert!(matches!(
                check_origin_status(StatusCode::NOT_FOUND, 4096, policy.response_errors),
                Err(Error::OriginNotFound)
            ));
            assert!(
                check_origin_status(StatusCode::PARTIAL_CONTENT, 4096, policy.response_errors)
                    .is_ok()
            );
            assert!(check_origin_status(StatusCode::OK, 0, policy.response_errors).is_ok());
            assert!(check_origin_status(StatusCode::OK, 1, policy.response_errors).is_err());
            assert!(check_origin_status(StatusCode::FORBIDDEN, 0, policy.response_errors).is_err());
        }
    }

    #[test]
    fn response_length_policy_is_shared_by_all_flavors() {
        for policy in POLICIES {
            let errors = policy.response_errors;
            assert_eq!(
                expected_body_len(
                    StatusCode::PARTIAL_CONTENT,
                    1,
                    None,
                    Some(1000),
                    4096,
                    errors,
                )
                .unwrap(),
                Some(1000)
            );
            assert!(
                expected_body_len(
                    StatusCode::PARTIAL_CONTENT,
                    1,
                    None,
                    Some(5000),
                    4096,
                    errors,
                )
                .is_err()
            );
            assert_eq!(
                expected_body_len(StatusCode::OK, 1, None, Some(1_000_000), 4096, errors).unwrap(),
                Some(4096)
            );
            assert_eq!(
                expected_body_len(StatusCode::OK, 1, Some("close"), None, 4096, errors).unwrap(),
                None
            );
            assert_eq!(
                expected_body_len(StatusCode::OK, 0, None, None, 4096, errors).unwrap(),
                None
            );
            assert!(expected_body_len(StatusCode::OK, 1, None, None, 4096, errors).is_err());
            assert!(
                expected_body_len(StatusCode::OK, 0, Some("keep-alive"), None, 4096, errors)
                    .is_err()
            );
        }
    }

    #[test]
    fn http_and_s3_requests_have_common_anonymous_headers() {
        for policy in [&HTTP_ENGINE_POLICY, &S3_ENGINE_POLICY] {
            let request =
                format_get_request(policy, "/bucket/key", "origin.example", 0, 4095).unwrap();
            let text = std::str::from_utf8(&request).unwrap();
            assert!(text.starts_with("GET /bucket/key HTTP/1.1\r\n"));
            assert!(text.contains("host: origin.example\r\n"));
            assert!(text.contains("range: bytes=0-4095\r\n"));
            assert!(!text.contains("connection:"));
            assert!(!text.contains("authorization"));
            assert!(!text.contains("x-amz-date"));
            assert!(!text.contains("x-ms-version"));

            let request = format_head_request(policy, "/bucket/key", "origin.example").unwrap();
            let text = std::str::from_utf8(&request).unwrap();
            assert!(text.starts_with("HEAD /bucket/key HTTP/1.1\r\n"));
            assert!(!text.contains("range:"));
        }
    }

    #[test]
    fn azure_requests_include_version_header() {
        let request = format_get_request(
            &AZURE_ENGINE_POLICY,
            "/container/key",
            "acct.blob.core.windows.net",
            4096,
            8191,
        )
        .unwrap();
        let text = std::str::from_utf8(&request).unwrap();
        assert!(text.contains("range: bytes=4096-8191\r\n"));
        assert!(text.contains("x-ms-version: 2021-08-06\r\n"));
        assert!(!text.contains("authorization"));

        let request = format_head_request(
            &AZURE_ENGINE_POLICY,
            "/container/key",
            "acct.blob.core.windows.net",
        )
        .unwrap();
        let text = std::str::from_utf8(&request).unwrap();
        assert!(text.contains("x-ms-version: 2021-08-06\r\n"));
        assert!(!text.contains("range:"));
    }

    #[test]
    fn engine_policies_preserve_backend_literals_and_wire_variation() {
        let cases = [
            (
                &HTTP_ENGINE_POLICY,
                HttpFlavor::Http,
                "http backend: request missing origin",
                "http backend: destination page lengths do not match requested range",
                "http backend: metadata HEAD response head exceeds 64 KiB",
            ),
            (
                &S3_ENGINE_POLICY,
                HttpFlavor::S3,
                "s3 backend: request missing origin",
                "s3 backend: destination page lengths do not match requested range",
                "s3 backend: metadata HEAD response head exceeds 64 KiB",
            ),
            (
                &AZURE_ENGINE_POLICY,
                HttpFlavor::Azure,
                "azure backend: request missing origin",
                "azure backend: destination page lengths do not match requested range",
                "azure backend: metadata HEAD response head exceeds 64 KiB",
            ),
        ];

        for (policy, flavor, missing_origin, destination_mismatch, head_too_large) in cases {
            assert!(matches!(
                (policy.flavor, flavor),
                (HttpFlavor::Http, HttpFlavor::Http)
                    | (HttpFlavor::S3, HttpFlavor::S3)
                    | (HttpFlavor::Azure, HttpFlavor::Azure)
            ));
            assert_eq!(policy.missing_origin, missing_origin);
            assert_eq!(policy.get_destination_mismatch, destination_mismatch);
            assert_eq!(policy.head_too_large, head_too_large);
        }
        assert!(!HTTP_ENGINE_POLICY.flavor.is_azure());
        assert!(!S3_ENGINE_POLICY.flavor.is_azure());
        assert!(AZURE_ENGINE_POLICY.flavor.is_azure());
    }

    fn format_error(error: Error) -> String {
        match error {
            Error::Transport(inner) => inner.to_string(),
            other => panic!("expected transport error, got {other:?}"),
        }
    }
}
