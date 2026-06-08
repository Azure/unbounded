// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! S3 serving frontend: the HTTP serving engine speaking native S3.
//!
//! This is the structural twin of [`crate::frontend::http_serve`]: the
//! factory ([`S3Frontend`]) and the per-shard engine ([`S3Driver`])
//! mirror [`HttpFrontend`](crate::frontend::HttpFrontend) /
//! [`HttpDriver`](crate::frontend::HttpDriver) exactly, down to the
//! `SO_REUSEPORT` bind, the accept loop, and the zero-copy stripe
//! streaming. The only divergence is the per-request serve function:
//! every error is rendered as an S3 `<Error>` XML document (see
//! [`crate::frontend::s3_xml`]) with the matching status, and a length
//! read that fails with
//! [`Error::OriginNotFound`](crate::bufferpool::Error::OriginNotFound)
//! becomes a `404 NoSuchKey` instead of a dropped connection.
//!
//! HEAD requests never carry a response body, even on error: those
//! paths send only the status line and headers (`Content-Length: 0`,
//! plus `Content-Range` for an unsatisfiable range), never the XML.
//!
//! Linux-gated because serving depends on the io_uring
//! [`NetHandle`](crate::ring::NetHandle).

use std::future::Future;
use std::net::SocketAddr;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::task::{Context, Poll, Waker};

use ::http::header::{ACCEPT_RANGES, CONNECTION, CONTENT_LENGTH, CONTENT_RANGE, CONTENT_TYPE};
use ::http::{Response, StatusCode};

use crate::bufferpool::{BufferPool, Error};
use crate::config::{FrontendKind, FrontendSpec};
use crate::frontend::FrontendError;
use crate::frontend::range::{ByteRange, RangeError, ResolvedRange, full_object, stripe_set};
use crate::frontend::s3_xml::{S3ErrorCode, error_xml};
use crate::http::{
    FdGuard, HttpRequest, MAX_HEADER_BYTES, Method, ParseError, RECV_CHUNK, bind_listener,
    send_all, serialize_response_head, split_query,
};
use crate::ring::NetHandle;
use crate::runtime::noop_waker;
use crate::storage::{OriginRef, StripeReq};

/// The `x-amz-request-id` response header name. Present on every
/// response (success and error) so clients and proxies can correlate.
const X_AMZ_REQUEST_ID: &str = "x-amz-request-id";

/// Monotonic source for per-request `x-amz-request-id` values. A plain
/// process-global counter is enough: the ids only need to be present
/// and non-empty, and a counter keeps them deterministic for tests.
static REQUEST_ID_SEQ: AtomicU64 = AtomicU64::new(1);

/// S3 serving frontend factory. Built once per [`FrontendSpec`]; holds
/// only the immutable configuration distilled from the spec.
///
/// Like [`HttpFrontend`](crate::frontend::HttpFrontend) it does not
/// implement the `Frontend` trait: the per-shard [`S3Driver`] is
/// generic over the concrete bufferpool type, which is only nameable in
/// the binary, so the binary binds the listener via
/// [`S3Frontend::bind_listener`] and constructs the driver directly.
pub struct S3Frontend {
    id: String,
    bind: SocketAddr,
    backend_id: String,
}

impl S3Frontend {
    /// Construct from a [`FrontendSpec`], validating the kind and
    /// parsing the bind address.
    ///
    /// Mirrors [`HttpFrontend::from_spec`](crate::frontend::HttpFrontend):
    /// a spec whose [`FrontendKind`] is not `S3` is rejected with
    /// [`FrontendError::UnsupportedKind`], so a misrouted spec fails
    /// loudly rather than being served by the wrong engine.
    pub fn from_spec(spec: &FrontendSpec) -> Result<Self, FrontendError> {
        if spec.kind != FrontendKind::S3 {
            return Err(FrontendError::UnsupportedKind("non-s3 frontend kind"));
        }
        let bind = spec
            .bind
            .parse::<SocketAddr>()
            .map_err(|_| FrontendError::BadBind(spec.bind.clone()))?;
        Ok(Self {
            id: spec.id.clone(),
            bind,
            backend_id: spec.backend.clone(),
        })
    }

    /// Stable identifier, matching [`FrontendSpec::id`].
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The backend id this frontend resolves origin metadata and stripe
    /// fetches against.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// The configured listen address.
    pub fn bind(&self) -> SocketAddr {
        self.bind
    }

    /// Create, bind, and listen the per-shard accept socket with
    /// `SO_REUSEPORT` (and `SO_REUSEADDR`) so every shard accepts on the
    /// same port, returning the listening [`RawFd`]. Linux-only.
    ///
    /// The caller owns the returned fd and is responsible for closing
    /// it; [`S3Driver`] only reads it for `accept`.
    pub fn bind_listener(&self) -> Result<RawFd, FrontendError> {
        bind_listener(self.bind).map_err(|_| FrontendError::BadBind(self.bind.to_string()))
    }
}

/// Per-shard S3 serving engine, generic over the bufferpool `P` so the
/// concrete `ShardPool` type (nameable only in the binary) can be
/// plugged in by the binary.
///
/// Structurally identical to
/// [`HttpDriver`](crate::frontend::HttpDriver): one persistent `accept`
/// future plus one serve future per live connection, advanced by
/// [`Self::progress`] with a noop waker and registered as a shard-loop
/// tick hook.
pub struct S3Driver<P: BufferPool<Req = StripeReq> + 'static> {
    pool: Rc<P>,
    handle: NetHandle,
    listen_fd: RawFd,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    accept_fut: Pin<Box<dyn Future<Output = std::io::Result<RawFd>>>>,
    conns: Vec<Pin<Box<dyn Future<Output = ()>>>>,
    waker: Waker,
}

impl<P: BufferPool<Req = StripeReq> + 'static> S3Driver<P> {
    /// Build a serving engine over a bound `listen_fd`.
    ///
    /// `stripe_size` and `page_size` come from the shard's pool
    /// geometry.
    pub fn new(
        pool: Rc<P>,
        handle: NetHandle,
        listen_fd: RawFd,
        backend_id: String,
        stripe_size: u64,
        page_size: usize,
    ) -> Self {
        let accept_fut = Box::pin(handle.accept(listen_fd));
        Self {
            pool,
            handle,
            listen_fd,
            backend_id,
            stripe_size,
            page_size,
            accept_fut,
            conns: Vec::new(),
            waker: noop_waker(),
        }
    }

    /// Number of live connection serve futures.
    pub fn conn_count(&self) -> usize {
        self.conns.len()
    }

    /// Whether the engine has no in-flight connection futures. The
    /// accept future is always live, so idleness only tracks
    /// connections.
    pub fn is_idle(&self) -> bool {
        self.conns.is_empty()
    }

    /// Advance the engine by one cooperative step: drain ready accepts
    /// (spawning a serve future per new connection and rearming a fresh
    /// accept), then poll every live connection future once, dropping
    /// the completed ones. Returns whether any future made progress so
    /// the shard loop busy-polls under load.
    pub fn progress(&mut self) -> bool {
        let mut busy = false;
        let waker = self.waker.clone();
        let mut cx = Context::from_waker(&waker);

        // Drain the accept future as long as it resolves; each
        // resolution spawns a serve future and rearms a new accept.
        loop {
            match self.accept_fut.as_mut().poll(&mut cx) {
                Poll::Ready(res) => {
                    busy = true;
                    if let Ok(fd) = res {
                        let serve = serve_connection_s3(
                            Rc::clone(&self.pool),
                            self.handle.clone(),
                            fd,
                            self.backend_id.clone(),
                            self.stripe_size,
                            self.page_size,
                        );
                        self.conns.push(Box::pin(serve));
                    }
                    // Rearm regardless of accept success so a transient
                    // accept error does not stop the listener.
                    self.accept_fut = Box::pin(self.handle.accept(self.listen_fd));
                }
                Poll::Pending => break,
            }
        }

        let mut i = 0;
        while i < self.conns.len() {
            match self.conns[i].as_mut().poll(&mut cx) {
                Poll::Ready(()) => {
                    let _ = self.conns.swap_remove(i);
                    busy = true;
                }
                Poll::Pending => i += 1,
            }
        }
        busy
    }
}

/// Serve one accepted connection end-to-end, then close its fd.
///
/// Owns everything it needs so the future is `'static` and can live in
/// the driver's future set across ticks. All paths close `conn_fd` via
/// the [`FdGuard`]; serve errors just drop the connection.
async fn serve_connection_s3<P: BufferPool<Req = StripeReq>>(
    pool: Rc<P>,
    handle: NetHandle,
    conn_fd: RawFd,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
) {
    let _fd = FdGuard(conn_fd);
    let _ = serve_request_s3(&pool, &handle, conn_fd, &backend_id, stripe_size, page_size).await;
}

/// The fallible S3 serve body. Returns `Err(())` on any I/O or pool
/// error the client cannot be told about; the caller closes the fd
/// regardless. Error responses (S3 XML for GET, bodyless heads for
/// HEAD) are best-effort sends followed by `Ok(())`.
async fn serve_request_s3<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    handle: &NetHandle,
    conn_fd: RawFd,
    backend_id: &str,
    stripe_size: u64,
    page_size: usize,
) -> Result<(), ()> {
    let request_id = next_request_id();

    // 1. Read until the request head is complete (or the cap is hit).
    let mut buf: Vec<u8> = Vec::new();
    loop {
        match HttpRequest::parse(&buf) {
            Ok(_) => break,
            Err(ParseError::Incomplete) => {
                if buf.len() > MAX_HEADER_BYTES {
                    return Err(());
                }
                let chunk = handle.recv(conn_fd, RECV_CHUNK).await.map_err(|_| ())?;
                if chunk.is_empty() {
                    return Err(());
                }
                buf.extend_from_slice(&chunk);
            }
            Err(_) => {
                // Malformed request line: we cannot know the method, so
                // answer with a GET-style XML 400 against the root.
                let bytes = error_bytes(S3ErrorCode::InvalidRequest, "/", &request_id, false, None);
                let _ = send_all(handle, conn_fd, bytes).await;
                return Ok(());
            }
        }
    }
    let req = HttpRequest::parse(&buf).map_err(|_| ())?;

    // 2. Path is the request target with any query stripped; it is the
    // origin object id and the XML `<Resource>`.
    let path = split_query(req.target).0.to_string();

    // 3. Only GET and HEAD are served; anything else is 405. The method
    // here is neither GET nor HEAD on the error arm, so the body is
    // allowed (GET-style XML).
    let is_head = match req.method {
        Method::GET => false,
        Method::HEAD => true,
        _ => {
            let bytes = error_bytes(
                S3ErrorCode::MethodNotAllowed,
                &path,
                &request_id,
                false,
                None,
            );
            let _ = send_all(handle, conn_fd, bytes).await;
            return Ok(());
        }
    };

    // 4. Optional Range header. A malformed Range is a 400; an
    // unsatisfiable one is handled at resolve time as a 416.
    let range = match req.header("range") {
        Some(v) => match ByteRange::parse(v) {
            Ok(r) => Some(r),
            Err(_) => {
                let bytes = error_bytes(
                    S3ErrorCode::InvalidRequest,
                    &path,
                    &request_id,
                    is_head,
                    None,
                );
                let _ = send_all(handle, conn_fd, bytes).await;
                return Ok(());
            }
        },
        None => None,
    };

    // 5. Resolve object length, distinguishing a missing origin object
    // (404 NoSuchKey) from any other failure (500 InternalError). Unlike
    // the plain HTTP frontend, a length-read failure is never a silently
    // dropped connection.
    let len = match read_object_length_s3(pool, backend_id, &path).await {
        LenResult::Len(l) => l,
        LenResult::NotFound => {
            let bytes = error_bytes(S3ErrorCode::NoSuchKey, &path, &request_id, is_head, None);
            let _ = send_all(handle, conn_fd, bytes).await;
            return Ok(());
        }
        LenResult::Other => {
            let bytes = error_bytes(
                S3ErrorCode::InternalError,
                &path,
                &request_id,
                is_head,
                None,
            );
            let _ = send_all(handle, conn_fd, bytes).await;
            return Ok(());
        }
    };

    // 6. Resolve the requested range against the length.
    let resolved = match range {
        None => full_object(len),
        Some(br) => match br.resolve(len) {
            Ok(r) => r,
            Err(RangeError::Unsatisfiable { object_len }) => {
                // 416 InvalidRange: include `Content-Range: bytes */LEN`
                // for both GET and HEAD; GET additionally carries the
                // XML body, HEAD carries only the head.
                let bytes = error_bytes(
                    S3ErrorCode::InvalidRange,
                    &path,
                    &request_id,
                    is_head,
                    Some(object_len),
                );
                let _ = send_all(handle, conn_fd, bytes).await;
                return Ok(());
            }
            Err(_) => {
                let bytes = error_bytes(
                    S3ErrorCode::InvalidRequest,
                    &path,
                    &request_id,
                    is_head,
                    None,
                );
                let _ = send_all(handle, conn_fd, bytes).await;
                return Ok(());
            }
        },
    };

    // 7. Response head: 206 if the client sent a Range, else 200.
    let head = if range.is_some() {
        partial_head(resolved, len, &request_id)
    } else {
        full_head(len, &request_id)
    };
    send_all(handle, conn_fd, head).await?;

    // 7b. HEAD carries no body: the head (with Content-Length /
    // Content-Range) is the entire response.
    if is_head {
        return Ok(());
    }

    // 8. Stream the body stripe-by-stripe out of the bufferpool.
    //
    // Within each stripe we use the windowed read path so the pool
    // keeps many page fetches in flight ahead of the byte we are
    // currently sending. The client send (`send_zc_fixed`) is
    // strictly in order on the single TCP stream, but the fabric
    // fetches of pages ahead of the cursor overlap with it, which is
    // what lets a single large object saturate the RDMA fabric NIC.
    // `usize::MAX` requests the full window: `read_windowed` clamps
    // it to the pool's configured `max_inflight_pages` budget, so the
    // prefetch depth is governed by that single knob.
    for slice in stripe_set(resolved, stripe_size) {
        let origin_ref = OriginRef {
            backend_id: backend_id.to_string(),
            origin_object_id: path.clone(),
            stripe_idx: slice.stripe_idx,
        };
        let pool_req = StripeReq::new(origin_ref.stripe_key()).with_origin(origin_ref);
        let mut rs = pool
            .read_windowed(&pool_req, slice.intra_offset, slice.intra_len, usize::MAX)
            .map_err(|_| ())?;
        while let Some(page) = rs.next_page().await {
            let page = page.map_err(|_| ())?;
            let pr = page.page_ref();
            let page_byte_offset = pr.page_idx as usize * page_size + pr.offset as usize;
            let n = pr.len as usize;
            // The PageGuard must stay alive until the SEND_ZC
            // notification (when the kernel is done with the source
            // page); awaiting here holds it across every partial send,
            // then we drop it before the next `next_page` as the stream
            // contract requires.
            //
            // SEND_ZC's CQE `res` is the count the kernel accepted,
            // which can be short under socket-send-buffer pressure, so
            // we loop advancing the byte offset until the whole page is
            // on the wire. A zero count means the peer closed.
            let mut sent_offset = page_byte_offset;
            let mut remaining = n;
            while remaining > 0 {
                let sent = handle
                    .send_zc_fixed(conn_fd, 0, sent_offset, remaining)
                    .await
                    .map_err(|_| ())?;
                if sent == 0 {
                    return Err(());
                }
                sent_offset += sent;
                remaining -= sent;
            }
            drop(page);
        }
    }
    Ok(())
}

/// Outcome of reading an object's length entry, preserving the one
/// distinction the S3 frontend acts on: a missing origin object maps to
/// a 404, every other failure to a 500.
enum LenResult {
    /// The length entry resolved to this byte length.
    Len(u64),
    /// The origin reported the object does not exist
    /// ([`Error::OriginNotFound`]).
    NotFound,
    /// Any other I/O, transport, or pool error.
    Other,
}

/// Resolve an object's length by reading its dedicated content-addressed
/// length entry through the pool, preserving an
/// [`Error::OriginNotFound`] as [`LenResult::NotFound`].
///
/// This is the S3 frontend's own variant of
/// `http_serve::read_object_length`, which collapses every error into
/// `Err(())`. Here the error is inspected at both fallible points (the
/// `read` call and the first `next_page`) so a 404 can be told apart
/// from a 500.
async fn read_object_length_s3<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    backend_id: &str,
    path: &str,
) -> LenResult {
    let origin_ref = OriginRef::length_entry(backend_id, path);
    let req = StripeReq::new(origin_ref.stripe_key()).with_origin(origin_ref);
    let mut rs = match pool.read(&req, 0, 8).await {
        Ok(rs) => rs,
        Err(Error::OriginNotFound) => return LenResult::NotFound,
        Err(_) => return LenResult::Other,
    };
    let page = match rs.next_page().await {
        Some(Ok(p)) => p,
        Some(Err(Error::OriginNotFound)) => return LenResult::NotFound,
        Some(Err(_)) => return LenResult::Other,
        None => return LenResult::Other,
    };
    let bytes = page.as_slice();
    if bytes.len() < 8 {
        return LenResult::Other;
    }
    let mut le = [0u8; 8];
    le.copy_from_slice(&bytes[..8]);
    LenResult::Len(u64::from_le_bytes(le))
}

/// Format the `200 OK` head for serving a whole object.
fn full_head(len: u64, request_id: &str) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_LENGTH, len.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(X_AMZ_REQUEST_ID, request_id)
        .header(CONNECTION, "close")
        .body(())
        .expect("valid full-object response head");
    serialize_response_head(&resp)
}

/// Format the `206 Partial Content` head for a resolved byte range.
/// `END` in `Content-Range` is inclusive (`resolved.end - 1`).
fn partial_head(resolved: ResolvedRange, total: u64, request_id: &str) -> Vec<u8> {
    let start = resolved.start;
    let end_incl = resolved.end - 1;
    let clen = resolved.len();
    let resp = Response::builder()
        .status(StatusCode::PARTIAL_CONTENT)
        .header(CONTENT_RANGE, format!("bytes {start}-{end_incl}/{total}"))
        .header(CONTENT_LENGTH, clen.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(X_AMZ_REQUEST_ID, request_id)
        .header(CONNECTION, "close")
        .body(())
        .expect("valid partial-content response head");
    serialize_response_head(&resp)
}

/// Build the full byte response for an S3 error.
///
/// For GET (`is_head == false`) the head carries the XML body with a
/// matching `Content-Length` and `Content-Type: application/xml`, and
/// the body is appended. For HEAD (`is_head == true`) only the head is
/// returned, with `Content-Length: 0` and no body, as both S3 and HTTP
/// require. `content_range`, when set, adds `Content-Range: bytes */LEN`
/// (used for the 416 unsatisfiable case).
fn error_bytes(
    code: S3ErrorCode,
    resource: &str,
    request_id: &str,
    is_head: bool,
    content_range: Option<u64>,
) -> Vec<u8> {
    let status =
        StatusCode::from_u16(code.http_status_u16()).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);

    if is_head {
        let mut builder = Response::builder()
            .status(status)
            .header(CONTENT_LENGTH, "0")
            .header(X_AMZ_REQUEST_ID, request_id)
            .header(CONNECTION, "close");
        if let Some(total) = content_range {
            builder = builder.header(CONTENT_RANGE, format!("bytes */{total}"));
        }
        let resp = builder.body(()).expect("valid bodyless error head");
        return serialize_response_head(&resp);
    }

    let body = error_xml(code, resource, request_id);
    let mut builder = Response::builder()
        .status(status)
        .header(CONTENT_LENGTH, body.len().to_string())
        .header(CONTENT_TYPE, "application/xml")
        .header(X_AMZ_REQUEST_ID, request_id)
        .header(CONNECTION, "close");
    if let Some(total) = content_range {
        builder = builder.header(CONTENT_RANGE, format!("bytes */{total}"));
    }
    let resp = builder.body(()).expect("valid error response head");
    let mut head = serialize_response_head(&resp);
    head.extend_from_slice(body.as_bytes());
    head
}

/// Mint the next per-request `x-amz-request-id`. A zero-padded hex of a
/// monotonic counter: always non-empty and deterministic for tests.
fn next_request_id() -> String {
    let n = REQUEST_ID_SEQ.fetch_add(1, Ordering::Relaxed);
    format!("{n:016x}")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::{ReadStream, WindowedRead};
    use crate::config::FrontendKind;
    use std::cell::RefCell;

    fn spec(id: &str, bind: &str) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: FrontendKind::S3,
            bind: bind.to_string(),
            backend: "primary".to_string(),
        }
    }

    #[test]
    fn from_spec_validates_kind_and_bind() {
        let f = S3Frontend::from_spec(&spec("workload", "0.0.0.0:9000")).unwrap();
        assert_eq!(f.id(), "workload");
        assert_eq!(f.backend_id(), "primary");
        assert_eq!(f.bind(), "0.0.0.0:9000".parse().unwrap());

        let bad = S3Frontend::from_spec(&spec("f", "not-an-addr"));
        assert!(matches!(bad, Err(FrontendError::BadBind(_))));
    }

    #[test]
    fn from_spec_rejects_non_s3_kind() {
        let mut s = spec("f", "127.0.0.1:9000");
        s.kind = FrontendKind::Http;
        assert!(matches!(
            S3Frontend::from_spec(&s),
            Err(FrontendError::UnsupportedKind(_))
        ));
    }

    #[test]
    fn full_head_has_request_id_and_accept_ranges() {
        let head = full_head(4096, "deadbeef");
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-length: 4096\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("x-amz-request-id: deadbeef\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_inclusive_end_and_request_id() {
        let head = partial_head(ResolvedRange { start: 0, end: 100 }, 1000, "id1");
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 206 Partial Content\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes 0-99/1000\r\n"), "got: {s}");
        assert!(s.contains("content-length: 100\r\n"), "got: {s}");
        assert!(s.contains("x-amz-request-id: id1\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn get_error_carries_xml_body_with_matching_length() {
        let bytes = error_bytes(S3ErrorCode::NoSuchKey, "/k", "rid", false, None);
        let s = std::str::from_utf8(&bytes).unwrap();
        assert!(s.starts_with("HTTP/1.1 404 Not Found\r\n"), "got: {s}");
        assert!(s.contains("content-type: application/xml\r\n"), "got: {s}");
        assert!(s.contains("x-amz-request-id: rid\r\n"), "got: {s}");
        let (head, body) = s.split_once("\r\n\r\n").unwrap();
        let expected = error_xml(S3ErrorCode::NoSuchKey, "/k", "rid");
        assert_eq!(body, expected);
        assert!(
            head.contains(&format!("content-length: {}\r\n", expected.len())),
            "got: {head}"
        );
    }

    #[test]
    fn head_error_has_no_body_and_zero_length() {
        let bytes = error_bytes(S3ErrorCode::NoSuchKey, "/k", "rid", true, None);
        let s = std::str::from_utf8(&bytes).unwrap();
        assert!(s.starts_with("HTTP/1.1 404 Not Found\r\n"), "got: {s}");
        assert!(s.contains("content-length: 0\r\n"), "got: {s}");
        assert!(!s.contains("<Error>"), "HEAD must not carry a body: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn unsatisfiable_get_has_content_range_and_xml_body() {
        let bytes = error_bytes(S3ErrorCode::InvalidRange, "/k", "rid", false, Some(100));
        let s = std::str::from_utf8(&bytes).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes */100\r\n"), "got: {s}");
        let (_head, body) = s.split_once("\r\n\r\n").unwrap();
        assert!(body.contains("<Code>InvalidRange</Code>"), "got: {body}");
    }

    #[test]
    fn unsatisfiable_head_has_content_range_no_body() {
        let bytes = error_bytes(S3ErrorCode::InvalidRange, "/k", "rid", true, Some(100));
        let s = std::str::from_utf8(&bytes).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes */100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 0\r\n"), "got: {s}");
        assert!(!s.contains("<Error>"), "HEAD must not carry a body: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn request_ids_are_non_empty_and_advance() {
        let a = next_request_id();
        let b = next_request_id();
        assert!(!a.is_empty());
        assert!(!b.is_empty());
        assert_ne!(a, b);
    }

    /// A mock pool whose `read` never constructs a `ReadStream` (that
    /// constructor is crate-internal to bufferpool): it always errors.
    /// Sufficient to wire an [`S3Driver`] for accept-loop tests, where
    /// the serve path is not exercised against a real pool.
    struct MockPool;

    impl BufferPool for MockPool {
        type Req = StripeReq;

        async fn read<'p>(
            &'p self,
            _req: &'p StripeReq,
            _offset: u64,
            _len: u64,
        ) -> Result<ReadStream<'p>, Error> {
            Err(Error::from("mock pool has no data"))
        }

        fn read_windowed<'p>(
            &'p self,
            _req: &'p StripeReq,
            _offset: u64,
            _len: u64,
            _window: usize,
        ) -> Result<WindowedRead<'p>, Error> {
            Err(Error::from("mock pool has no data"))
        }
    }

    #[test]
    fn driver_idle_progress_returns_false_without_clients() {
        // Needs a real socket ring; skip gracefully when unavailable.
        let ring = match crate::ring::NetworkRing::new(16) {
            Ok(r) => Rc::new(RefCell::new(r)),
            Err(e) => {
                eprintln!("driver_idle_progress: ring unavailable: {e}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(ring);
        let listen_fd = match bind_listener("127.0.0.1:0".parse().unwrap()) {
            Ok(fd) => fd,
            Err(e) => {
                eprintln!("driver_idle_progress: bind failed: {e}; skipping");
                return;
            }
        };
        let mut driver = S3Driver::new(
            Rc::new(MockPool),
            handle,
            listen_fd,
            "primary".to_string(),
            4 * 1024 * 1024,
            2 * 1024 * 1024,
        );
        // No client has connected: accept is pending, no conns, so the
        // engine reports no work and stays idle.
        assert!(!driver.progress());
        assert_eq!(driver.conn_count(), 0);
        assert!(driver.is_idle());

        unsafe {
            libc::close(listen_fd);
        }
    }
}
