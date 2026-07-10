// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP serving frontend: the working replacement for the old S3 stub.
//!
//! This file owns two concerns:
//!
//! 1. **The factory** ([`HttpFrontend`]): validates a
//!    [`FrontendSpec`](crate::config::FrontendSpec) and binds a
//!    `SO_REUSEPORT` listening socket via `libc` so every shard can
//!    accept on the same port. It exposes plain inherent methods
//!    (`from_spec`, `bind_listener`) rather than a trait: the concrete
//!    bufferpool type is only nameable in the binary, so the binary
//!    builds and drives it directly.
//! 2. **The serving engine** ([`HttpDriver`]): a per-shard,
//!    cooperatively-driven engine generic over the bufferpool `P`. It
//!    owns an internal future set (one persistent accept future plus
//!    one future per live connection) advanced by [`HttpDriver::progress`],
//!    which the shard loop registers as a tick hook.
//!
//! The hot serve path is: parse the request, resolve the object length
//! by reading the object's dedicated length entry through the pool,
//! resolve the byte range, write the
//! response head, then stream the body stripe-by-stripe out of the
//! bufferpool with zero-copy `SEND_ZC`, holding each [`PageGuard`]
//! across the send (the SEND_ZC notification is when the kernel is done
//! with the page) before advancing the stream.
//!
//! Linux-gated because serving depends on the io_uring
//! [`NetHandle`](crate::ring::NetHandle).

use std::collections::VecDeque;
use std::future::{Future, poll_fn};
use std::net::SocketAddr;
use std::os::fd::{AsRawFd, RawFd};
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};
use std::time::{Duration, Instant};

use ::http::header::{ACCEPT_RANGES, CONNECTION, CONTENT_LENGTH, CONTENT_RANGE};
use ::http::{Response, StatusCode};

use crate::bufferpool::{BufferPool, Error as PoolError, ReadStream, StripeKey, StripePlan};
use crate::config::schema::DEFAULT_HTTP_FRONTEND_MAX_REQUESTS_PER_CONNECTION;
use crate::config::{FrontendSpec, frontend_spec};
use crate::fanout::{FanoutTable, FetchEvent, FetchPage, FetchStream, Owner};
use crate::frontend::FrontendError;
use crate::frontend::range::{
    ByteRange, RangeError, ResolvedRange, StripeSlice, full_object, stripe_set,
};
use crate::frontend::serve_metrics::{ConnGuard, ReqOutcome};
use crate::http::{
    BoundListener, FdGuard, HttpRequest, ListeningSocket, MAX_HEADER_BYTES, Method, ParseError,
    RECV_CHUNK, bind_socket, connection_header_value, request_allows_keep_alive, send_all,
    serialize_response_head, split_query,
};
use crate::ring::NetHandle;
use crate::runtime::noop_waker;
use crate::storage::{ObjectMetadata, OriginRef, StripeReq};

/// HTTP serving frontend factory. Built once per [`FrontendSpec`];
/// holds only the immutable configuration distilled from the spec.
///
/// The per-shard [`HttpDriver`] is generic over the concrete bufferpool
/// type, which is only nameable in the binary, so the binary binds the
/// listener via [`HttpFrontend::bind_listener`] and constructs the
/// driver directly.
pub struct HttpFrontend {
    id: String,
    addr: SocketAddr,
    max_requests_per_connection: usize,
}

const MAX_FRONTEND_CONNS: usize = 4096;
const KEEP_ALIVE_IDLE_TIMEOUT: Duration = Duration::from_secs(30);

impl HttpFrontend {
    /// Construct from a [`FrontendSpec`], validating the config type and
    /// parsing the listen address.
    pub fn from_spec(spec: &FrontendSpec) -> Result<Self, FrontendError> {
        let cfg = match spec.config.as_ref() {
            Some(frontend_spec::Config::Http(cfg)) => cfg,
            _ => return Err(FrontendError::UnsupportedKind("non-http frontend config")),
        };
        let addr = cfg
            .addr
            .parse::<SocketAddr>()
            .map_err(|_| FrontendError::BadBind(cfg.addr.clone()))?;
        let max_requests_per_connection = cfg
            .max_requests_per_connection
            .unwrap_or(DEFAULT_HTTP_FRONTEND_MAX_REQUESTS_PER_CONNECTION);
        if max_requests_per_connection == 0 {
            return Err(FrontendError::BadConfig(
                "max_requests_per_connection must be greater than zero",
            ));
        }
        Ok(Self {
            id: spec.name.clone(),
            addr,
            max_requests_per_connection: max_requests_per_connection as usize,
        })
    }

    /// Stable identifier, matching [`FrontendSpec::name`].
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The configured listen address.
    pub fn bind(&self) -> SocketAddr {
        self.addr
    }

    /// Maximum requests served before the frontend closes a keep-alive
    /// connection and lets the client reconnect through `SO_REUSEPORT`.
    pub fn max_requests_per_connection(&self) -> usize {
        self.max_requests_per_connection
    }

    /// Bind a dormant per-shard `SO_REUSEPORT` socket.
    pub fn bind_socket(&self) -> Result<BoundListener, FrontendError> {
        bind_socket(self.addr).map_err(|_| FrontendError::BadBind(self.addr.to_string()))
    }

    /// Bind and activate the per-shard listener.
    pub fn bind_listener(&self) -> Result<ListeningSocket, FrontendError> {
        self.bind_socket()?
            .listen()
            .map_err(|_| FrontendError::BadBind(self.addr.to_string()))
    }
}

/// Per-shard HTTP serving engine, generic over the bufferpool `P` so
/// the concrete `ShardPool` type (nameable only in the binary) can be
/// plugged in by Phase E.
///
/// Owns its own internal future set: one persistent `accept` future and
/// one serve future per live connection. [`Self::progress`] advances
/// them with a noop waker, mirroring `ShardLoop::drive`; the shard loop
/// registers `progress` as a tick hook. The socket ring's own
/// `progress` is a separate tick hook (added by the shard bring-up), so
/// the serve/accept futures' slots get filled even though this engine
/// only polls them.
pub struct HttpDriver<P: BufferPool<Req = StripeReq> + 'static> {
    pool: Rc<P>,
    handle: NetHandle,
    frontend_id: Rc<str>,
    backend_id: String,
    cache_id: Option<String>,
    stripe_size: u64,
    page_size: usize,
    fanout: Rc<FanoutTable>,
    bypass: bool,
    max_requests_per_connection: usize,
    accept_fut: Pin<Box<dyn Future<Output = std::io::Result<RawFd>>>>,
    // Declared after the future so the pending accept is cancelled first.
    listener: ListeningSocket,
    conns: Vec<Pin<Box<dyn Future<Output = ()>>>>,
    waker: Waker,
}

impl<P: BufferPool<Req = StripeReq> + 'static> HttpDriver<P> {
    /// Build a serving engine that owns an active listener.
    ///
    /// `stripe_size` and `page_size` come from the shard's pool
    /// geometry. `fanout` is this shard's view of the stripe-ownership
    /// ring; for single-shard deployments it routes every stripe to the
    /// local pool. When `bypass` is set, the frontend bridges straight
    /// to its backend: cache, peer routing, and fanout are all skipped.
    pub fn new(
        pool: Rc<P>,
        handle: NetHandle,
        listener: ListeningSocket,
        frontend_id: Rc<str>,
        backend_id: String,
        cache_id: Option<String>,
        stripe_size: u64,
        page_size: usize,
        fanout: Rc<FanoutTable>,
        bypass: bool,
        max_requests_per_connection: usize,
    ) -> Self {
        let accept_fut = Box::pin(handle.accept(listener.as_raw_fd()));
        Self {
            pool,
            handle,
            frontend_id,
            backend_id,
            cache_id,
            stripe_size,
            page_size,
            fanout,
            bypass,
            max_requests_per_connection,
            accept_fut,
            listener,
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
                        if self.conns.len() >= MAX_FRONTEND_CONNS {
                            // SAFETY: this driver owns newly accepted fds.
                            unsafe {
                                libc::close(fd);
                            }
                            self.accept_fut =
                                Box::pin(self.handle.accept(self.listener.as_raw_fd()));
                            continue;
                        }
                        let serve = serve_connection(
                            Rc::clone(&self.pool),
                            self.handle.clone(),
                            fd,
                            Rc::clone(&self.frontend_id),
                            self.backend_id.clone(),
                            self.cache_id.clone(),
                            self.stripe_size,
                            self.page_size,
                            Rc::clone(&self.fanout),
                            self.bypass,
                            self.max_requests_per_connection,
                        );
                        self.conns.push(Box::pin(serve));
                    }
                    // Rearm regardless of accept success so a transient
                    // accept error does not stop the listener.
                    self.accept_fut = Box::pin(self.handle.accept(self.listener.as_raw_fd()));
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

///
/// Owns everything it needs so the future is `'static` and can live in
/// the driver's future set across ticks. All paths close `conn_fd` via
/// the [`FdGuard`]; serve errors just drop the connection.
async fn serve_connection<P: BufferPool<Req = StripeReq>>(
    pool: Rc<P>,
    handle: NetHandle,
    conn_fd: RawFd,
    frontend_id: Rc<str>,
    backend_id: String,
    cache_id: Option<String>,
    stripe_size: u64,
    page_size: usize,
    fanout: Rc<FanoutTable>,
    bypass: bool,
    max_requests_per_connection: usize,
) {
    let _fd = FdGuard(conn_fd);
    let _conn = ConnGuard::new();
    let mut buf: Vec<u8> = Vec::new();
    let mut deadline = None;
    let mut requests_served = 0usize;
    loop {
        let mut log = crate::obs::ReqLog::new("frontend.http");
        let start = std::time::Instant::now();
        let mut outcome = ReqOutcome::default();
        let result = serve_request(
            &pool,
            &handle,
            conn_fd,
            &backend_id,
            cache_id.as_deref(),
            stripe_size,
            page_size,
            &fanout,
            bypass,
            &mut buf,
            deadline,
            requests_served.saturating_add(1) < max_requests_per_connection,
            &mut log,
            &mut outcome,
        )
        .await;
        match result {
            Ok(ServeStep::Closed) => break,
            Ok(ServeStep::Close) => {
                outcome.record(&frontend_id, start.elapsed().as_secs_f64());
                log.finish_ok();
                break;
            }
            Ok(ServeStep::KeepAlive) => {
                requests_served = requests_served.saturating_add(1);
                outcome.record(&frontend_id, start.elapsed().as_secs_f64());
                log.finish_ok();
                deadline = Some(Instant::now() + KEEP_ALIVE_IDLE_TIMEOUT);
            }
            Err(()) => {
                outcome.record(&frontend_id, start.elapsed().as_secs_f64());
                log.finish_err("connection error");
                break;
            }
        }
    }
}

enum ServeStep {
    KeepAlive,
    Close,
    Closed,
}

/// The fallible serve body. Returns `Err(())` on any I/O, parse, or
/// pool error; the caller closes the fd regardless. Error responses
/// (400/405/416) are best-effort sends followed by a close/keep-alive
/// step based on the parsed request.
async fn serve_request<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    handle: &NetHandle,
    conn_fd: RawFd,
    backend_id: &str,
    cache_id: Option<&str>,
    stripe_size: u64,
    page_size: usize,
    fanout: &Rc<FanoutTable>,
    bypass: bool,
    buf: &mut Vec<u8>,
    idle_deadline: Option<Instant>,
    allow_keep_alive_after_response: bool,
    log: &mut crate::obs::ReqLog,
    outcome: &mut ReqOutcome,
) -> Result<ServeStep, ()> {
    // 1. Read until the request head is complete (or the cap is hit).
    let req = loop {
        match HttpRequest::parse(&buf) {
            Ok(req) => break req,
            Err(ParseError::Incomplete) => {
                if buf.len() >= MAX_HEADER_BYTES {
                    return Err(());
                }
                let chunk = recv_with_deadline(handle, conn_fd, RECV_CHUNK, idle_deadline)
                    .await
                    .map_err(|_| ())?;
                if chunk.is_empty() {
                    return if buf.is_empty() {
                        Ok(ServeStep::Closed)
                    } else {
                        Err(())
                    };
                }
                buf.extend_from_slice(&chunk);
            }
            Err(_) => {
                send_all(handle, conn_fd, status_line_response(400, false)).await?;
                log.field("status", 400);
                outcome.status = 400;
                buf.clear();
                return Ok(ServeStep::Close);
            }
        }
    };
    if req.header_end > MAX_HEADER_BYTES {
        return Err(());
    }
    let keep_alive = request_allows_keep_alive(&req) && allow_keep_alive_after_response;
    let header_end = req.header_end;

    // 2. Only GET and HEAD are served. HEAD resolves length and builds
    // the same head as GET but never streams a body.
    let is_head = match req.method {
        Method::GET => false,
        Method::HEAD => true,
        _ => {
            send_all(handle, conn_fd, status_line_response(405, keep_alive)).await?;
            log.field("status", 405);
            outcome.status = 405;
            finish_request(buf, header_end);
            return Ok(next_step(keep_alive));
        }
    };
    outcome.method = if is_head { "HEAD" } else { "GET" };
    log.str_field("method", if is_head { "HEAD" } else { "GET" });

    // 3. Path is the request target with any query stripped.
    let path = split_query(req.target).0.to_string();
    log.str_field("path", &path);

    // 4. Optional Range header.
    let range = match req.header("range") {
        Some(v) => match ByteRange::parse(v) {
            Ok(r) => Some(r),
            Err(_) => {
                send_all(handle, conn_fd, status_line_response(400, keep_alive)).await?;
                log.field("status", 400);
                outcome.status = 400;
                finish_request(buf, header_end);
                return Ok(next_step(keep_alive));
            }
        },
        None => None,
    };

    // 5. Resolve object length by reading the object's dedicated
    // metadata entry through the pool. The HTTP backend fills it from
    // an origin HEAD on a miss; local-disk and peer hits skip the
    // origin entirely.
    let len = match read_object_length(
        pool,
        backend_id,
        cache_id,
        &path,
        stripe_size,
        page_size,
        bypass,
    )
    .await
    {
        LenResult::Len(len) => len,
        LenResult::NotFound => {
            let _ = send_all(handle, conn_fd, status_line_response(404, keep_alive)).await;
            log.field("status", 404);
            outcome.status = 404;
            finish_request(buf, header_end);
            return Ok(next_step(keep_alive));
        }
        LenResult::Other => return Err(()),
    };

    // 6. Resolve the requested range against the length.
    let resolved = match range {
        None => full_object(len),
        Some(br) => match br.resolve(len) {
            Ok(r) => r,
            Err(RangeError::Unsatisfiable { object_len }) => {
                send_all(
                    handle,
                    conn_fd,
                    unsatisfiable_response(object_len, keep_alive),
                )
                .await?;
                log.field("status", 416);
                outcome.status = 416;
                finish_request(buf, header_end);
                return Ok(next_step(keep_alive));
            }
            Err(_) => {
                send_all(handle, conn_fd, status_line_response(400, keep_alive)).await?;
                log.field("status", 400);
                outcome.status = 400;
                finish_request(buf, header_end);
                return Ok(next_step(keep_alive));
            }
        },
    };

    // 7. Response head: 206 if the client sent a Range, else 200.
    let head = if range.is_some() {
        partial_head(resolved, len, keep_alive)
    } else {
        full_head(len, keep_alive)
    };
    outcome.status = if range.is_some() { 206 } else { 200 };
    // HEAD carries no body, so no body bytes are streamed to the client;
    // only count the body length for requests that actually send one.
    outcome.bytes = if is_head { 0 } else { resolved.len() };
    log.field("status", if range.is_some() { 206 } else { 200 })
        .field("bytes", resolved.len());
    send_all(handle, conn_fd, head).await?;

    // 7b. HEAD carries no body: the head (with Content-Length /
    // Content-Range) is the entire response.
    if is_head {
        finish_request(buf, header_end);
        return Ok(next_step(keep_alive));
    }

    // 8. Stream the body, fanning each stripe out to its content-address
    // owner shard (or the local pool when this shard owns it).
    stream_body(
        pool,
        handle,
        fanout,
        conn_fd,
        backend_id,
        cache_id,
        &path,
        page_size,
        resolved,
        stripe_size,
        bypass,
    )
    .await?;
    finish_request(buf, header_end);
    Ok(next_step(keep_alive))
}

/// One stripe's place in the in-order send cursor.
enum Ticket {
    /// This shard owns the stripe; stream it from the local pool at the
    /// head of the window (deferred so local NVMe reads do not block the
    /// remote kicks behind them).
    Local { slice: StripeSlice },
    /// A peer shard owns the stripe. The owner may work ahead of the send
    /// cursor, but the owner-side service caps emitted pins per fetch.
    Remote {
        stream: FetchStream,
        pending: Vec<Option<FetchPage>>,
        buf_index: u16,
    },
    /// The owner channel was already disconnected when dispatch tried to
    /// enqueue the fetch. Preserve the stripe's position in the window;
    /// draining the ticket fails the connection in object order.
    Failed,
}

/// Build the content-addressed key and pool request for one stripe slice.
fn stripe_request(
    backend_id: &str,
    cache_id: Option<&str>,
    path: &str,
    stripe_size: u64,
    slice: StripeSlice,
    bypass: bool,
) -> (StripeKey, StripeReq) {
    let origin_ref = OriginRef {
        backend_id: backend_id.to_string(),
        origin_object_id: path.to_string(),
        stripe_idx: slice.stripe_idx,
    };
    let key = cache_id
        .map(|cache_id| origin_ref.stripe_key_for_cache(cache_id, stripe_size))
        .unwrap_or_else(|| origin_ref.stripe_key());
    let req = StripeReq::new(key)
        .with_origin(origin_ref)
        .with_cache_id(cache_id.map(ToOwned::to_owned))
        .with_bypass(bypass);
    (key, req)
}

/// Send `len` bytes from registered fixed buffer `buf_index` starting at
/// `offset`, looping over short `SEND_ZC` completions. `Err(())` on a
/// zero-length completion (peer closed) or ring error.
async fn send_region(
    handle: &NetHandle,
    conn_fd: RawFd,
    buf_index: u16,
    offset: usize,
    len: usize,
) -> Result<(), ()> {
    let mut sent_offset = offset;
    let mut remaining = len;
    while remaining > 0 {
        let sent = handle
            .send_zc_fixed(conn_fd, buf_index, sent_offset, remaining)
            .await
            .map_err(|_| ())?;
        if sent == 0 {
            return Err(());
        }
        sent_offset += sent;
        remaining -= sent;
    }
    Ok(())
}

async fn send_remote_region(
    handle: &NetHandle,
    conn_fd: RawFd,
    buf_index: u16,
    offset: usize,
    len: usize,
    stream: &FetchStream,
    pin_token: u64,
) -> Result<(), ()> {
    let release = stream.pin_release_hold(pin_token);
    let mut sent_offset = offset;
    let mut remaining = len;
    while remaining > 0 {
        let sent = handle
            .send_zc_fixed_with_completion(
                conn_fd,
                buf_index,
                sent_offset,
                remaining,
                Box::new(release.token()),
            )
            .await
            .map_err(|_| ())?;
        if sent == 0 {
            return Err(());
        }
        sent_offset += sent;
        remaining -= sent;
    }
    release.close();
    Ok(())
}

async fn send_remote_stream(
    handle: &NetHandle,
    conn_fd: RawFd,
    stream: &mut FetchStream,
    pending: &mut Vec<Option<FetchPage>>,
    buf_index: u16,
) -> Result<(), ()> {
    let mut next_ordinal = 0usize;
    let mut done = false;
    loop {
        while next_ordinal < pending.len() {
            let Some(page) = pending[next_ordinal].take() else {
                break;
            };
            send_remote_region(
                handle,
                conn_fd,
                buf_index,
                page.loc.page_byte_offset as usize,
                page.loc.len as usize,
                stream,
                page.pin_token,
            )
            .await?;
            next_ordinal += 1;
        }

        if done {
            return if next_ordinal == pending.len() {
                Ok(())
            } else {
                Err(())
            };
        }

        match stream.next_event().await.map_err(|_| ())? {
            FetchEvent::Page(page) => {
                let ordinal = page.ordinal;
                if ordinal >= pending.len() {
                    pending.resize_with(ordinal + 1, || None);
                }
                if pending[ordinal].is_some() || ordinal < next_ordinal {
                    stream.release(page.pin_token);
                    return Err(());
                }
                pending[ordinal] = Some(page);
            }
            FetchEvent::Done => done = true,
        }
    }
}

/// Route one stripe to its owner and, for remote owners, kick the fetch
/// so its command reaches the owner shard before we start sending the
/// stripes ahead of it. The owner can emit ready pages before this ticket
/// reaches the HTTP send cursor; `FetchService` bounds that by actual
/// emitted pins per fetch.
async fn dispatch_ticket(
    fanout: &Rc<FanoutTable>,
    backend_id: &str,
    cache_id: Option<&str>,
    path: &str,
    stripe_size: u64,
    slice: StripeSlice,
    bypass: bool,
) -> Ticket {
    let (key, req) = stripe_request(backend_id, cache_id, path, stripe_size, slice, bypass);
    match fanout.owner_of_cache(&key, cache_id, slice.intra_offset) {
        Owner::Local => Ticket::Local { slice },
        Owner::Peer(peer) => {
            let buf_index = peer.buf_index;
            match peer.channel.fetch(req, slice.intra_offset, slice.intra_len) {
                Ok(stream) => Ticket::Remote {
                    stream,
                    pending: Vec::new(),
                    buf_index,
                },
                Err(_) => Ticket::Failed,
            }
        }
    }
}

/// Stream the resolved byte range to the client, in object order, with
/// per-stripe ownership fan-out.
///
/// Single-shard deployments keep the original behavior: one pipelined
/// local read across every stripe so the pool prefetches across stripe
/// boundaries. Multi-shard deployments dispatch a bounded stripe window
/// so owner work overlaps without unbounded owner-side pins.
#[allow(clippy::too_many_arguments)]
async fn stream_body<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    handle: &NetHandle,
    fanout: &Rc<FanoutTable>,
    conn_fd: RawFd,
    backend_id: &str,
    cache_id: Option<&str>,
    path: &str,
    page_size: usize,
    resolved: ResolvedRange,
    stripe_size: u64,
    bypass: bool,
) -> Result<(), ()> {
    let slices = stripe_set(resolved, stripe_size);

    // A bypass frontend never fans out across shards: every stripe is
    // served from the local pool (which bridges straight to the origin),
    // so take the single pipelined-read path regardless of shard count.
    if bypass || fanout.shard_count() <= 1 {
        let plans: Vec<StripePlan<StripeReq>> = slices
            .into_iter()
            .map(|slice| {
                let (_key, req) =
                    stripe_request(backend_id, cache_id, path, stripe_size, slice, bypass);
                StripePlan {
                    req,
                    intra_offset: slice.intra_offset,
                    intra_len: slice.intra_len,
                }
            })
            .collect();
        let mut rs = pool.read_pipelined(plans, usize::MAX).map_err(|_| ())?;
        while let Some(page) = rs.next_page().await {
            let page = page.map_err(|_| ())?;
            let pr = page.page_ref();
            let offset = pr.page_idx as usize * page_size + pr.offset as usize;
            send_region(handle, conn_fd, 0, offset, pr.len as usize).await?;
            drop(page);
        }
        return Ok(());
    }

    let mut next = 0usize;
    let mut window: VecDeque<Ticket> = VecDeque::new();
    while next < slices.len() && window.len() < multi_shard_window() {
        window.push_back(
            dispatch_ticket(
                fanout,
                backend_id,
                cache_id,
                path,
                stripe_size,
                slices[next],
                bypass,
            )
            .await,
        );
        next += 1;
    }

    let local_plan = |slice: StripeSlice| {
        let (_key, req) = stripe_request(backend_id, cache_id, path, stripe_size, slice, bypass);
        StripePlan {
            req,
            intra_offset: slice.intra_offset,
            intra_len: slice.intra_len,
        }
    };

    while let Some(ticket) = window.pop_front() {
        match ticket {
            Ticket::Local { slice } => {
                // Coalesce a maximal run of contiguous local stripes into
                // one pipelined read so the pool prefetches across their
                // boundaries, matching the single-shard pipeline depth
                // instead of stalling one stripe at a time. Remote
                // tickets between locals break the run because their
                // bytes must go on the wire in object order.
                let mut plans = vec![local_plan(slice)];
                while matches!(window.front(), Some(Ticket::Local { .. })) {
                    if let Some(Ticket::Local { slice }) = window.pop_front() {
                        plans.push(local_plan(slice));
                    }
                }
                let mut rs = pool.read_pipelined(plans, usize::MAX).map_err(|_| ())?;
                while let Some(page) = rs.next_page().await {
                    let page = page.map_err(|_| ())?;
                    let pr = page.page_ref();
                    let offset = pr.page_idx as usize * page_size + pr.offset as usize;
                    send_region(handle, conn_fd, 0, offset, pr.len as usize).await?;
                    drop(page);
                }
            }
            Ticket::Remote {
                mut stream,
                mut pending,
                buf_index,
            } => {
                send_remote_stream(handle, conn_fd, &mut stream, &mut pending, buf_index).await?;
            }
            Ticket::Failed => {
                return Err(());
            }
        }

        while next < slices.len() && window.len() < multi_shard_window() {
            window.push_back(
                dispatch_ticket(
                    fanout,
                    backend_id,
                    cache_id,
                    path,
                    stripe_size,
                    slices[next],
                    bypass,
                )
                .await,
            );
            next += 1;
        }
    }
    Ok(())
}

fn multi_shard_window() -> usize {
    8
}

/// Resolve an object's length by reading its dedicated content-addressed
/// metadata entry through the pool. The entry's single page carries the
/// object's [`ObjectMetadata`]; the HTTP backend fills it from an origin
/// `HEAD` on a miss, while local-disk and peer hits avoid the origin
/// entirely. The whole page is read (a zero-copy borrow) and decoded.
async fn read_object_length<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    backend_id: &str,
    cache_id: Option<&str>,
    path: &str,
    stripe_size: u64,
    page_size: usize,
    bypass: bool,
) -> LenResult {
    let origin_ref = OriginRef::metadata_entry(backend_id, path);
    let key = cache_id
        .map(|cache_id| origin_ref.stripe_key_for_cache(cache_id, stripe_size))
        .unwrap_or_else(|| origin_ref.stripe_key());
    let req = StripeReq::new(key)
        .with_origin(origin_ref)
        .with_cache_id(cache_id.map(ToOwned::to_owned))
        .with_bypass(bypass);
    let mut rs: ReadStream = match pool.read(&req, 0, page_size as u64).await {
        Ok(rs) => rs,
        Err(e) => return classify_len_error(e),
    };
    let page = match rs.next_page().await {
        Some(Ok(page)) => page,
        Some(Err(e)) => return classify_len_error(e),
        None => return LenResult::Other,
    };
    match ObjectMetadata::decode(page.as_slice()) {
        Ok(meta) => LenResult::Len(meta.length),
        Err(e) => classify_len_error(e),
    }
}

#[derive(Debug, PartialEq, Eq)]
enum LenResult {
    Len(u64),
    NotFound,
    Other,
}

fn classify_len_error(err: PoolError) -> LenResult {
    match err {
        PoolError::OriginNotFound => LenResult::NotFound,
        _ => LenResult::Other,
    }
}

async fn recv_with_deadline(
    handle: &NetHandle,
    conn_fd: RawFd,
    max_len: usize,
    deadline: Option<Instant>,
) -> Result<Vec<u8>, ()> {
    let mut fut = Box::pin(handle.recv(conn_fd, max_len));
    poll_fn(|cx| {
        if deadline.is_some_and(|deadline| Instant::now() >= deadline) {
            return Poll::Ready(Err(()));
        }
        match fut.as_mut().poll(cx) {
            Poll::Ready(res) => Poll::Ready(res.map_err(|_| ())),
            Poll::Pending => {
                if deadline.is_some() {
                    cx.waker().wake_by_ref();
                }
                Poll::Pending
            }
        }
    })
    .await
}

fn finish_request(buf: &mut Vec<u8>, header_end: usize) {
    buf.drain(..header_end);
}

fn next_step(keep_alive: bool) -> ServeStep {
    if keep_alive {
        ServeStep::KeepAlive
    } else {
        ServeStep::Close
    }
}

/// Format the `200 OK` head for serving a whole object.
fn full_head(len: u64, keep_alive: bool) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_LENGTH, len.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, connection_header_value(keep_alive))
        .body(())
        .expect("valid full-object response head");
    serialize_response_head(&resp)
}

/// Format the `206 Partial Content` head for a resolved byte range.
/// `END` in `Content-Range` is inclusive (`resolved.end - 1`).
fn partial_head(resolved: ResolvedRange, total: u64, keep_alive: bool) -> Vec<u8> {
    let start = resolved.start;
    let end_incl = resolved.end - 1;
    let clen = resolved.len();
    let resp = Response::builder()
        .status(StatusCode::PARTIAL_CONTENT)
        .header(CONTENT_RANGE, format!("bytes {start}-{end_incl}/{total}"))
        .header(CONTENT_LENGTH, clen.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, connection_header_value(keep_alive))
        .body(())
        .expect("valid partial-content response head");
    serialize_response_head(&resp)
}

/// Format a `416 Range Not Satisfiable` head with `Content-Range: bytes
/// */LEN`.
fn unsatisfiable_response(total: u64, keep_alive: bool) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::RANGE_NOT_SATISFIABLE)
        .header(CONTENT_RANGE, format!("bytes */{total}"))
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, connection_header_value(keep_alive))
        .body(())
        .expect("valid unsatisfiable-range response head");
    serialize_response_head(&resp)
}

/// Format a bodyless status-line response (`Content-Length: 0`) for the
/// simple error statuses this frontend emits.
fn status_line_response(status: u16, keep_alive: bool) -> Vec<u8> {
    let status = StatusCode::from_u16(status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let resp = Response::builder()
        .status(status)
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, connection_header_value(keep_alive))
        .body(())
        .expect("valid status-line response head");
    serialize_response_head(&resp)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::{Error, PipelinedRead, ReadStream, WindowedRead};
    use crate::config::HttpFrontendConfig;
    use crate::fanout::NumaShardTable;
    use crate::frontend::range::StripeSlice;
    use crate::http::{request_is_bodyless, request_wants_keep_alive};
    use crate::storage::disks::CacheDirectorySet;

    fn spec(id: &str, addr: &str) -> FrontendSpec {
        FrontendSpec {
            name: id.to_string(),
            source: "primary".to_string(),
            config: Some(frontend_spec::Config::Http(HttpFrontendConfig {
                addr: addr.to_string(),
                max_requests_per_connection: None,
            })),
        }
    }

    #[test]
    fn from_spec_validates_kind_and_bind() {
        let f = HttpFrontend::from_spec(&spec("workload", "0.0.0.0:9000")).unwrap();
        assert_eq!(f.id(), "workload");
        assert_eq!(f.bind(), "0.0.0.0:9000".parse().unwrap());

        let bad = HttpFrontend::from_spec(&spec("f", "not-an-addr"));
        assert!(matches!(bad, Err(FrontendError::BadBind(_))));
    }

    #[test]
    fn from_spec_defaults_and_validates_request_cap() {
        let f = HttpFrontend::from_spec(&spec("workload", "0.0.0.0:9000")).unwrap();
        assert_eq!(
            f.max_requests_per_connection(),
            DEFAULT_HTTP_FRONTEND_MAX_REQUESTS_PER_CONNECTION as usize
        );

        let mut capped = spec("workload", "0.0.0.0:9000");
        let Some(frontend_spec::Config::Http(cfg)) = capped.config.as_mut() else {
            panic!("expected http config");
        };
        cfg.max_requests_per_connection = Some(7);
        let f = HttpFrontend::from_spec(&capped).unwrap();
        assert_eq!(f.max_requests_per_connection(), 7);

        let Some(frontend_spec::Config::Http(cfg)) = capped.config.as_mut() else {
            panic!("expected http config");
        };
        cfg.max_requests_per_connection = Some(0);
        assert!(matches!(
            HttpFrontend::from_spec(&capped),
            Err(FrontendError::BadConfig(_))
        ));
    }

    #[test]
    fn full_head_exact_bytes() {
        let head = full_head(4096, true);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-length: 4096\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: keep-alive\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_exact_bytes_inclusive_end() {
        // Resolved [0, 100) of a 1000-byte object -> bytes 0-99/1000.
        let head = partial_head(ResolvedRange { start: 0, end: 100 }, 1000, true);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 206 Partial Content\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes 0-99/1000\r\n"), "got: {s}");
        assert!(s.contains("content-length: 100\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: keep-alive\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_mid_object() {
        // Resolved [70, 100) of 100 -> bytes 70-99/100, length 30.
        let head = partial_head(
            ResolvedRange {
                start: 70,
                end: 100,
            },
            100,
            false,
        );
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.contains("content-range: bytes 70-99/100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 30\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
    }

    #[test]
    fn unsatisfiable_response_exact_bytes() {
        let head = unsatisfiable_response(100, false);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes */100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 0\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn status_405_and_400_exact_bytes() {
        let r405 = status_line_response(405, false);
        let s405 = std::str::from_utf8(&r405).unwrap();
        assert!(
            s405.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"),
            "got: {s405}"
        );
        assert!(s405.contains("content-length: 0\r\n"), "got: {s405}");
        assert!(s405.contains("connection: close\r\n"), "got: {s405}");
        assert!(s405.ends_with("\r\n\r\n"), "got: {s405}");

        let r400 = status_line_response(400, false);
        let s400 = std::str::from_utf8(&r400).unwrap();
        assert!(
            s400.starts_with("HTTP/1.1 400 Bad Request\r\n"),
            "got: {s400}"
        );
        assert!(s400.contains("content-length: 0\r\n"), "got: {s400}");
        assert!(s400.contains("connection: close\r\n"), "got: {s400}");
        assert!(s400.ends_with("\r\n\r\n"), "got: {s400}");
    }

    #[test]
    fn status_404_exact_bytes() {
        let r404 = status_line_response(404, false);
        let s404 = std::str::from_utf8(&r404).unwrap();
        assert!(
            s404.starts_with("HTTP/1.1 404 Not Found\r\n"),
            "got: {s404}"
        );
        assert!(s404.contains("content-length: 0\r\n"), "got: {s404}");
        assert!(s404.contains("connection: close\r\n"), "got: {s404}");
        assert!(s404.ends_with("\r\n\r\n"), "got: {s404}");
    }

    #[test]
    fn request_keep_alive_follows_http_connection_rules() {
        let req = HttpRequest::parse(b"GET / HTTP/1.1\r\n\r\n").unwrap();
        assert!(request_wants_keep_alive(&req));

        let req = HttpRequest::parse(b"GET / HTTP/1.1\r\nConnection: close\r\n\r\n").unwrap();
        assert!(!request_wants_keep_alive(&req));

        let req = HttpRequest::parse(b"GET / HTTP/1.0\r\n\r\n").unwrap();
        assert!(!request_wants_keep_alive(&req));

        let req = HttpRequest::parse(b"GET / HTTP/1.0\r\nConnection: keep-alive\r\n\r\n").unwrap();
        assert!(request_wants_keep_alive(&req));
    }

    #[test]
    fn request_has_body_detects_unsupported_body_headers() {
        let req = HttpRequest::parse(b"GET / HTTP/1.1\r\n\r\n").unwrap();
        assert!(request_is_bodyless(&req));

        let req = HttpRequest::parse(b"GET / HTTP/1.1\r\nContent-Length: 0\r\n\r\n").unwrap();
        assert!(request_is_bodyless(&req));

        let req = HttpRequest::parse(b"GET / HTTP/1.1\r\nContent-Length: 1\r\n\r\n").unwrap();
        assert!(!request_is_bodyless(&req));

        let req =
            HttpRequest::parse(b"GET / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n").unwrap();
        assert!(!request_is_bodyless(&req));
    }

    #[test]
    fn duplicate_body_headers_disable_keep_alive() {
        let req = HttpRequest::parse(
            b"GET / HTTP/1.1\r\nContent-Length: 0\r\nContent-Length: 10\r\n\r\n",
        )
        .unwrap();
        assert!(!request_is_bodyless(&req));

        let req = HttpRequest::parse(
            b"GET / HTTP/1.1\r\nConnection: keep-alive\r\nConnection: close\r\n\r\n",
        )
        .unwrap();
        assert!(!request_wants_keep_alive(&req));
    }

    #[test]
    fn range_to_stripe_set_wiring_no_range() {
        // No Range header -> full object over the stripe set. A 10-byte
        // object at stripe 4 covers stripes 0,1,2.
        let resolved = full_object(10);
        let slices = stripe_set(resolved, 4);
        assert_eq!(
            slices,
            vec![
                StripeSlice {
                    stripe_idx: 0,
                    intra_offset: 0,
                    intra_len: 4
                },
                StripeSlice {
                    stripe_idx: 1,
                    intra_offset: 0,
                    intra_len: 4
                },
                StripeSlice {
                    stripe_idx: 2,
                    intra_offset: 0,
                    intra_len: 2
                },
            ]
        );
    }

    #[test]
    fn range_to_stripe_set_wiring_with_range() {
        // Range bytes=5-6 on a 10-byte object resolves to [5,7), which
        // at stripe 4 is stripe 1 intra [1,3).
        let br = ByteRange::parse("bytes=5-6").unwrap();
        let resolved = br.resolve(10).unwrap();
        assert_eq!(resolved, ResolvedRange { start: 5, end: 7 });
        let slices = stripe_set(resolved, 4);
        assert_eq!(
            slices,
            vec![StripeSlice {
                stripe_idx: 1,
                intra_offset: 1,
                intra_len: 2
            }]
        );
    }

    #[test]
    fn unsatisfiable_range_resolves_to_error() {
        // bytes=100-200 on a 100-byte object is unsatisfiable.
        let br = ByteRange::parse("bytes=100-200").unwrap();
        assert!(matches!(
            br.resolve(100),
            Err(RangeError::Unsatisfiable { object_len: 100 })
        ));
    }

    #[test]
    fn stripe_request_stamps_bypass_flag() {
        use crate::bufferpool::Req;

        let slice = StripeSlice {
            stripe_idx: 2,
            intra_offset: 0,
            intra_len: 4,
        };
        // Non-bypass requests carry the cache path (bypass == false).
        let (_k, normal) = stripe_request("primary", Some("cache-a"), "/o", 4096, slice, false);
        assert!(!normal.bypass());
        assert_eq!(normal.cache_id(), Some("cache-a"));
        // Bridge-mode frontends stamp bypass == true onto every request.
        let (k, bridged) = stripe_request("primary", None, "/o", 4096, slice, true);
        assert!(bridged.bypass());
        // Cache id is the only logical key prefix, so cached and uncached
        // requests for the same origin intentionally use different keys.
        assert_ne!(normal.key(), k);
        assert_eq!(bridged.origin(), normal.origin());
    }

    #[test]
    fn multi_shard_window_allows_bounded_overlap() {
        assert!(
            multi_shard_window() > 1,
            "multi-shard streaming must overlap owner fetches",
        );
    }

    #[test]
    fn metadata_entry_request_is_well_formed() {
        use crate::bufferpool::Req;
        use crate::storage::{METADATA_STRIPE_IDX, stripe_key};

        let origin_ref = OriginRef::metadata_entry("primary", "/o");
        assert!(origin_ref.is_metadata_entry());

        let req = StripeReq::new(origin_ref.stripe_key_for_cache("cache-a", 4096))
            .with_origin(origin_ref);
        assert!(req.origin().unwrap().is_metadata_entry());
        assert_eq!(
            req.key(),
            stripe_key("cache-a", 4096, "/o", METADATA_STRIPE_IDX)
        );
        assert_eq!(METADATA_STRIPE_IDX, u64::MAX);
    }

    /// A mock pool whose `read` never constructs a `ReadStream` (that
    /// constructor is crate-internal to bufferpool): it always errors.
    /// Sufficient to wire an [`HttpDriver`] for accept-loop tests, where
    /// the serve path is not exercised against a real pool.
    struct MockPool {
        err: Error,
    }

    impl BufferPool for MockPool {
        type Req = StripeReq;

        async fn read<'p>(
            &'p self,
            _req: &'p StripeReq,
            _offset: u64,
            _len: u64,
        ) -> Result<ReadStream<'p>, Error> {
            Err(self.err.clone())
        }

        fn read_windowed<'p>(
            &'p self,
            _req: &'p StripeReq,
            _offset: u64,
            _len: u64,
            _window: usize,
        ) -> Result<WindowedRead<'p>, Error> {
            Err(self.err.clone())
        }

        fn read_pipelined<'p>(
            &'p self,
            _stripes: Vec<StripePlan<StripeReq>>,
            _window: usize,
        ) -> Result<PipelinedRead<'p>, Error> {
            Err(self.err.clone())
        }
    }

    fn block_on<F: Future>(fut: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = Box::pin(fut);
        for _ in 0..1_000_000 {
            match fut.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => std::thread::yield_now(),
            }
        }
        panic!("future did not complete");
    }

    #[test]
    fn read_object_length_maps_origin_not_found_to_len_result() {
        let pool = Rc::new(MockPool {
            err: Error::OriginNotFound,
        });

        let result = block_on(read_object_length(
            &pool,
            "primary",
            None,
            "/missing",
            4 * 1024 * 1024,
            4096,
            false,
        ));

        assert_eq!(result, LenResult::NotFound);
    }

    #[test]
    fn read_object_length_maps_other_errors_to_other() {
        let pool = Rc::new(MockPool {
            err: Error::from("mock pool has no data"),
        });

        let result = block_on(read_object_length(
            &pool,
            "primary",
            None,
            "/broken",
            4 * 1024 * 1024,
            4096,
            false,
        ));

        assert_eq!(result, LenResult::Other);
    }

    #[test]
    fn driver_idle_progress_returns_false_without_clients() {
        // Needs a real socket ring; skip gracefully when unavailable.
        let ring = match crate::ring::NetworkRing::new(16) {
            Ok(r) => Rc::new(r),
            Err(e) => {
                eprintln!("driver_idle_progress: ring unavailable: {e}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(ring);
        let listener = match bind_socket("127.0.0.1:0".parse().unwrap()).and_then(|b| b.listen()) {
            Ok(listener) => listener,
            Err(e) => {
                eprintln!("driver_idle_progress: bind failed: {e}; skipping");
                return;
            }
        };
        let mut driver = HttpDriver::new(
            Rc::new(MockPool {
                err: Error::from("mock pool has no data"),
            }),
            handle,
            listener,
            Rc::from("primary"),
            "primary".to_string(),
            None,
            4 * 1024 * 1024,
            2 * 1024 * 1024,
            Rc::new(FanoutTable::new(
                0,
                vec![None],
                NumaShardTable::from_shards([(0, None)]),
                CacheDirectorySet::new(),
            )),
            false,
            DEFAULT_HTTP_FRONTEND_MAX_REQUESTS_PER_CONNECTION as usize,
        );
        // No client has connected: accept is pending, no conns, so the
        // engine reports no work and stays idle.
        assert!(!driver.progress());
        assert_eq!(driver.conn_count(), 0);
        assert!(driver.is_idle());
    }
}
