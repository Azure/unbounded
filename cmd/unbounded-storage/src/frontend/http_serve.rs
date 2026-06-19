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
//! The hot serve path is: parse the request, resolve the object metadata
//! by reading the object's dedicated metadata entry through the pool,
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
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use ::http::header::{
    ACCEPT_RANGES, CACHE_CONTROL, CONNECTION, CONTENT_LENGTH, CONTENT_RANGE, ETAG,
};
use ::http::{Response, StatusCode};

use crate::bufferpool::{BufferPool, Error, ReadStream, StripeKey, StripePlan};
use crate::config::{FrontendKind, FrontendSpec};
use crate::fanout::{FanoutTable, FetchChannel, FetchReply, Owner};
use crate::frontend::FrontendError;
use crate::frontend::range::{
    ByteRange, RangeError, ResolvedRange, StripeSlice, full_object, stripe_set,
};
use crate::frontend::serve_metrics::{ConnGuard, ReqOutcome};
use crate::http::{
    FdGuard, HttpRequest, MAX_HEADER_BYTES, Method, ParseError, RECV_CHUNK, bind_listener,
    send_all, serialize_response_head, split_query,
};
use crate::ring::NetHandle;
use crate::runtime::noop_waker;
use crate::storage::{ObjectMetadata, OriginRef, StripeReq};

const VERSIONED_BODY_VERIFY_ATTEMPTS: usize = 2;

/// HTTP serving frontend factory. Built once per [`FrontendSpec`];
/// holds only the immutable configuration distilled from the spec.
///
/// The per-shard [`HttpDriver`] is generic over the concrete bufferpool
/// type, which is only nameable in the binary, so the binary binds the
/// listener via [`HttpFrontend::bind_listener`] and constructs the
/// driver directly.
pub struct HttpFrontend {
    id: String,
    bind: SocketAddr,
    backend_id: String,
}

impl HttpFrontend {
    /// Construct from a [`FrontendSpec`], validating the kind and
    /// parsing the bind address.
    pub fn from_spec(spec: &FrontendSpec) -> Result<Self, FrontendError> {
        if spec.kind() != FrontendKind::Http {
            return Err(FrontendError::UnsupportedKind("non-http frontend kind"));
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
    /// it; [`HttpDriver`] only reads it for `accept`.
    pub fn bind_listener(&self) -> Result<RawFd, FrontendError> {
        bind_listener(self.bind).map_err(|_| FrontendError::BadBind(self.bind.to_string()))
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
    listen_fd: RawFd,
    frontend_id: Rc<str>,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    fanout: Rc<FanoutTable>,
    accept_fut: Pin<Box<dyn Future<Output = std::io::Result<RawFd>>>>,
    conns: Vec<Pin<Box<dyn Future<Output = ()>>>>,
    waker: Waker,
}

impl<P: BufferPool<Req = StripeReq> + 'static> HttpDriver<P> {
    /// Build a serving engine over a bound `listen_fd`.
    ///
    /// `stripe_size` and `page_size` come from the shard's pool
    /// geometry. `fanout` is this shard's view of the stripe-ownership
    /// ring; for single-shard deployments it routes every stripe to the
    /// local pool.
    pub fn new(
        pool: Rc<P>,
        handle: NetHandle,
        listen_fd: RawFd,
        frontend_id: Rc<str>,
        backend_id: String,
        stripe_size: u64,
        page_size: usize,
        fanout: Rc<FanoutTable>,
    ) -> Self {
        let accept_fut = Box::pin(handle.accept(listen_fd));
        Self {
            pool,
            handle,
            listen_fd,
            frontend_id,
            backend_id,
            stripe_size,
            page_size,
            fanout,
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
                        let serve = serve_connection(
                            Rc::clone(&self.pool),
                            self.handle.clone(),
                            fd,
                            Rc::clone(&self.frontend_id),
                            self.backend_id.clone(),
                            self.stripe_size,
                            self.page_size,
                            Rc::clone(&self.fanout),
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

impl<P: BufferPool<Req = StripeReq> + 'static> Drop for HttpDriver<P> {
    /// Close the listen fd the driver owns. The driver is the sole owner
    /// of this `SO_REUSEPORT` listener (the embedder hands it the fd from
    /// `bind_listener` and never touches it again), so dropping the
    /// driver (on shard shutdown or a live frontend removal) must close
    /// it or the fd leaks. The accept future borrows only the fd number,
    /// not ownership, so closing here is sound once the driver is gone.
    fn drop(&mut self) {
        if self.listen_fd >= 0 {
            // SAFETY: `listen_fd` was returned by `bind_listener` and is
            // owned exclusively by this driver; it is closed exactly once.
            unsafe {
                libc::close(self.listen_fd);
            }
        }
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
    stripe_size: u64,
    page_size: usize,
    fanout: Rc<FanoutTable>,
) {
    let _fd = FdGuard(conn_fd);
    let _conn = ConnGuard::new();
    let mut log = crate::obs::ReqLog::new("frontend.http");
    let start = std::time::Instant::now();
    let mut outcome = ReqOutcome::default();
    let result = serve_request(
        &pool,
        &handle,
        conn_fd,
        &backend_id,
        stripe_size,
        page_size,
        &fanout,
        &mut log,
        &mut outcome,
    )
    .await;
    outcome.record(&frontend_id, start.elapsed().as_secs_f64());
    match result {
        Ok(()) => log.finish_ok(),
        Err(()) => log.finish_err("connection error"),
    }
}

/// The fallible serve body. Returns `Err(())` on any I/O, parse, or
/// pool error; the caller closes the fd regardless. Error responses
/// (400/405/416) are best-effort sends followed by `Ok(())`.
async fn serve_request<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    handle: &NetHandle,
    conn_fd: RawFd,
    backend_id: &str,
    stripe_size: u64,
    page_size: usize,
    fanout: &Rc<FanoutTable>,
    log: &mut crate::obs::ReqLog,
    outcome: &mut ReqOutcome,
) -> Result<(), ()> {
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
                let _ = send_all(handle, conn_fd, status_line_response(400)).await;
                log.field("status", 400);
                outcome.status = 400;
                return Ok(());
            }
        }
    }
    let req = HttpRequest::parse(&buf).map_err(|_| ())?;

    // 2. Only GET and HEAD are served. HEAD resolves length and builds
    // the same head as GET but never streams a body.
    let is_head = match req.method {
        Method::GET => false,
        Method::HEAD => true,
        _ => {
            let _ = send_all(handle, conn_fd, status_line_response(405)).await;
            log.field("status", 405);
            outcome.status = 405;
            return Ok(());
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
                let _ = send_all(handle, conn_fd, status_line_response(400)).await;
                log.field("status", 400);
                outcome.status = 400;
                return Ok(());
            }
        },
        None => None,
    };

    let mut verify_attempts = 0;
    let (meta, cache_key_version, origin_match_version, resolved) = loop {
        // 5. Resolve object metadata through the pool. The HTTP backend
        // fills it from an origin HEAD on a miss or after a backend-provided
        // TTL expires. The first read honors that TTL: forcing a re-HEAD on
        // every GET would defeat the cache and serialize hot objects behind
        // an origin round-trip. Correctness for versioned bodies is upheld
        // by the verify pass below, which does a conditional GET (If-Match
        // on the cached ETag); a stale ETag returns 412 and we retry. Only
        // that retry forces a re-HEAD, because re-reading the same stale
        // metadata would loop on the same stale If-Match.
        let force_refresh = verify_attempts > 0;
        let meta = read_object_metadata(pool, backend_id, &path, page_size, force_refresh)
            .await
            .map_err(|_| ())?;
        let len = meta.length;
        let cache_key_version = meta.cache_key_version().map(str::to_string);
        let origin_match_version = meta.origin_match_version().map(str::to_string);

        // 6. Resolve the requested range against the length.
        let resolved = match range {
            None => full_object(len),
            Some(br) => match br.resolve(len) {
                Ok(r) => r,
                Err(RangeError::Unsatisfiable { object_len }) => {
                    let _ = send_all(handle, conn_fd, unsatisfiable_response(object_len)).await;
                    log.field("status", 416);
                    outcome.status = 416;
                    return Ok(());
                }
                Err(_) => {
                    let _ = send_all(handle, conn_fd, status_line_response(400)).await;
                    log.field("status", 400);
                    outcome.status = 400;
                    return Ok(());
                }
            },
        };

        if is_head || cache_key_version.is_none() || resolved.len() == 0 {
            break (meta, cache_key_version, origin_match_version, resolved);
        }

        let verify_result = match process_body(
            pool,
            BodyMode::Verify,
            BodySource::OriginAllowed,
            fanout,
            backend_id,
            &path,
            cache_key_version.as_deref(),
            origin_match_version.as_deref(),
            page_size,
            resolved,
            stripe_size,
        )
        .await
        {
            Ok(()) => {
                process_body(
                    pool,
                    BodyMode::Verify,
                    BodySource::CacheOnly,
                    fanout,
                    backend_id,
                    &path,
                    cache_key_version.as_deref(),
                    origin_match_version.as_deref(),
                    page_size,
                    resolved,
                    stripe_size,
                )
                .await
            }
            Err(e) => Err(e),
        };

        match verify_result {
            Ok(()) => break (meta, cache_key_version, origin_match_version, resolved),
            Err(Error::OriginVersionMismatch)
                if verify_attempts + 1 < VERSIONED_BODY_VERIFY_ATTEMPTS =>
            {
                verify_attempts += 1;
                continue;
            }
            Err(_) => {
                let _ = send_all(handle, conn_fd, status_line_response(500)).await;
                log.field("status", 500);
                outcome.status = 500;
                return Ok(());
            }
        }
    };
    let len = meta.length;

    // 7. Response head: 206 if the client sent a Range, else 200.
    let head = if range.is_some() {
        partial_head(resolved, &meta)
    } else {
        full_head(&meta)
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
        return Ok(());
    }

    // 8. Stream the body, fanning each stripe out to its content-address
    // owner shard (or the local pool when this shard owns it). Versioned
    // bodies were verified before the success head, so the send pass is
    // cache-only and cannot reach the origin with a now-stale If-Match.
    let body_source = if cache_key_version.is_some() {
        BodySource::CacheOnly
    } else {
        BodySource::OriginAllowed
    };
    match process_body(
        pool,
        BodyMode::Send { handle, conn_fd },
        body_source,
        fanout,
        backend_id,
        &path,
        cache_key_version.as_deref(),
        origin_match_version.as_deref(),
        page_size,
        resolved,
        stripe_size,
    )
    .await
    {
        Ok(()) => Ok(()),
        Err(Error::OriginVersionMismatch) => {
            let _ = read_object_metadata(pool, backend_id, &path, page_size, true).await;
            Err(())
        }
        Err(_) => Err(()),
    }
}

/// Type-erased cross-shard fetch future. Holds the channel round-trip to
/// a peer shard's [`FetchService`](crate::fanout::FetchService) and
/// resolves to the owner-side page locations (or a pool error).
type FetchFut = Pin<Box<dyn Future<Output = Result<FetchReply, crate::bufferpool::Error>>>>;

/// One stripe's place in the in-order send window. The window is drained
/// strictly front-to-back so bytes go onto the single TCP stream in
/// object order, while later stripes' fetches overlap with the head's
/// send.
enum Ticket {
    /// This shard owns the stripe; stream it from the local pool at the
    /// head of the window (deferred so local NVMe reads do not block the
    /// remote kicks behind them).
    Local { slice: StripeSlice },
    /// A peer shard owns the stripe. `fut` is already kicked (its fetch
    /// command is in flight to the owner); `buf_index` is the owner's
    /// registered backing region and `channel` releases the owner's pin
    /// once the bytes are on the wire. `slice` is retained so a
    /// transient [`Error::Busy`](crate::bufferpool::Error::Busy) reply
    /// (the owner pool was momentarily out of free pages) can be
    /// re-dispatched without losing the stripe's place in object order.
    Remote {
        fut: FetchFut,
        buf_index: u16,
        channel: FetchChannel,
        slice: StripeSlice,
    },
}

enum BodyMode<'a> {
    Verify,
    Send {
        handle: &'a NetHandle,
        conn_fd: RawFd,
    },
}

#[derive(Clone, Copy)]
enum BodySource {
    OriginAllowed,
    CacheOnly,
}

/// Build the content-addressed key and pool request for one stripe slice.
fn stripe_request(
    backend_id: &str,
    path: &str,
    cache_key_version: Option<&str>,
    origin_match_version: Option<&str>,
    slice: StripeSlice,
    source: BodySource,
) -> (StripeKey, StripeReq) {
    let mut origin_ref = OriginRef::new(backend_id, path, slice.stripe_idx);
    if let Some(version) = cache_key_version {
        origin_ref = origin_ref.with_cache_key_version(version);
    }
    if let Some(version) = origin_match_version {
        origin_ref = origin_ref.with_origin_match_version(version);
    }
    let key = origin_ref.stripe_key();
    let mut req = StripeReq::new(key);
    if matches!(source, BodySource::OriginAllowed) {
        req = req.with_origin(origin_ref);
    }
    (key, req)
}

/// Cooperatively yield once, returning control to the shard loop so
/// other futures (and other shards' progress) can advance before this
/// one is re-polled. Used to space out retries of a transiently busy
/// remote fetch instead of hot-spinning.
async fn yield_now() {
    let mut yielded = false;
    poll_fn(|cx| {
        if yielded {
            Poll::Ready(())
        } else {
            yielded = true;
            // Wake so executors that only re-poll woken tasks (the DST
            // scheduler) reschedule us; the production shard loop
            // re-polls every tick regardless.
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await;
}

/// Send `len` bytes from registered fixed buffer `buf_index` starting at
/// `offset`, looping over short `SEND_ZC` completions.
async fn send_region(
    handle: &NetHandle,
    conn_fd: RawFd,
    buf_index: u16,
    offset: usize,
    len: usize,
) -> Result<(), Error> {
    let mut sent_offset = offset;
    let mut remaining = len;
    while remaining > 0 {
        let sent = handle
            .send_zc_fixed(conn_fd, buf_index, sent_offset, remaining)
            .await
            .map_err(|_| Error::Io(libc::EIO))?;
        if sent == 0 {
            return Err(Error::Io(libc::EIO));
        }
        sent_offset += sent;
        remaining -= sent;
    }
    Ok(())
}

async fn process_region(
    mode: &BodyMode<'_>,
    buf_index: u16,
    offset: usize,
    len: usize,
) -> Result<(), Error> {
    match mode {
        BodyMode::Verify => Ok(()),
        BodyMode::Send { handle, conn_fd } => {
            send_region(handle, *conn_fd, buf_index, offset, len).await
        }
    }
}

/// Route one stripe to its owner and, for remote owners, kick the fetch
/// so its command reaches the owner shard before we start sending the
/// stripes ahead of it. A single poll is enough: `FetchChannel::fetch`
/// enqueues the command on its first poll, and the shard loop re-polls
/// this serve future every tick thereafter, so the owner progresses
/// concurrently on its own core.
async fn dispatch_ticket(
    fanout: &Rc<FanoutTable>,
    backend_id: &str,
    path: &str,
    cache_key_version: Option<&str>,
    origin_match_version: Option<&str>,
    slice: StripeSlice,
    source: BodySource,
) -> Ticket {
    let (key, req) = stripe_request(
        backend_id,
        path,
        cache_key_version,
        origin_match_version,
        slice,
        source,
    );
    match fanout.owner_of(&key, slice.intra_offset) {
        Owner::Local => Ticket::Local { slice },
        Owner::Peer(peer) => {
            let buf_index = peer.buf_index;
            let channel = peer.channel.clone();
            let fetch_channel = channel.clone();
            let intra_offset = slice.intra_offset;
            let intra_len = slice.intra_len;
            let mut fut: FetchFut =
                Box::pin(async move { fetch_channel.fetch(req, intra_offset, intra_len).await });
            poll_fn(|cx| {
                let _ = fut.as_mut().poll(cx);
                Poll::Ready(())
            })
            .await;
            Ticket::Remote {
                fut,
                buf_index,
                channel,
                slice,
            }
        }
    }
}

/// Stream the resolved byte range to the client, in object order, with
/// per-stripe ownership fan-out.
///
/// Single-shard deployments keep the original behavior: one pipelined
/// local read across every stripe so the pool prefetches across stripe
/// boundaries. Multi-shard deployments dispatch a bounded window of
/// stripes ahead of the send cursor; remote stripes are kicked to their
/// owner shards (which fetch and pin pages on their own cores) while the
/// head stripe is being sent, so a single TCP connection drives all
/// shards' NICs.
#[allow(clippy::too_many_arguments)]
async fn process_body<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    mode: BodyMode<'_>,
    source: BodySource,
    fanout: &Rc<FanoutTable>,
    backend_id: &str,
    path: &str,
    cache_key_version: Option<&str>,
    origin_match_version: Option<&str>,
    page_size: usize,
    resolved: ResolvedRange,
    stripe_size: u64,
) -> Result<(), Error> {
    let slices = stripe_set(resolved, stripe_size);

    if fanout.shard_count() <= 1 {
        let plans: Vec<StripePlan<StripeReq>> = slices
            .into_iter()
            .map(|slice| {
                let (_key, req) = stripe_request(
                    backend_id,
                    path,
                    cache_key_version,
                    origin_match_version,
                    slice,
                    source,
                );
                StripePlan {
                    req,
                    intra_offset: slice.intra_offset,
                    intra_len: slice.intra_len,
                }
            })
            .collect();
        let mut rs = pool.read_pipelined(plans, usize::MAX)?;
        while let Some(page) = rs.next_page().await {
            let page = page?;
            let pr = page.page_ref();
            let offset = pr.page_idx as usize * page_size + pr.offset as usize;
            process_region(&mode, 0, offset, pr.len as usize).await?;
            drop(page);
        }
        return Ok(());
    }

    const WINDOW: usize = 8;
    let mut next = 0usize;
    let mut window: VecDeque<Ticket> = VecDeque::new();
    while next < slices.len() && window.len() < WINDOW {
        window.push_back(
            dispatch_ticket(
                fanout,
                backend_id,
                path,
                cache_key_version,
                origin_match_version,
                slices[next],
                source,
            )
            .await,
        );
        next += 1;
    }

    let local_plan = |slice: StripeSlice| {
        let (_key, req) = stripe_request(
            backend_id,
            path,
            cache_key_version,
            origin_match_version,
            slice,
            source,
        );
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
                let mut rs = pool.read_pipelined(plans, usize::MAX)?;
                while let Some(page) = rs.next_page().await {
                    let page = page?;
                    let pr = page.page_ref();
                    let offset = pr.page_idx as usize * page_size + pr.offset as usize;
                    process_region(&mode, 0, offset, pr.len as usize).await?;
                    drop(page);
                }
            }
            Ticket::Remote {
                fut,
                buf_index,
                channel,
                slice,
            } => {
                let reply = match fut.await {
                    Ok(reply) => reply,
                    // Transient owner-side page pressure: the owner pool
                    // refused a non-blocking head allocation rather than
                    // parking (which could deadlock across shards). Yield
                    // so we do not hot-spin while the owner drains other
                    // coordinators' pins, then re-dispatch this same
                    // stripe at the head of the window so object order is
                    // preserved. Forward progress holds because every
                    // owner always replies in bounded time and releases
                    // its pins, so retries eventually succeed.
                    Err(crate::bufferpool::Error::Busy) => {
                        yield_now().await;
                        let retry = dispatch_ticket(
                            fanout,
                            backend_id,
                            path,
                            cache_key_version,
                            origin_match_version,
                            slice,
                            source,
                        )
                        .await;
                        window.push_front(retry);
                        continue;
                    }
                    Err(e) => return Err(e),
                };
                // Send every page in stripe order from the owner's
                // backing, then release the owner's pin. The pin must
                // outlive the final SEND_ZC notification (each
                // `send_region` resolves on its notification CQE), so we
                // release only after the loop. Release even on a send
                // error so a dropped client does not leak the owner pin.
                let mut result = Ok(());
                for ploc in &reply.pages {
                    if process_region(
                        &mode,
                        buf_index,
                        ploc.page_byte_offset as usize,
                        ploc.len as usize,
                    )
                    .await
                    .is_err()
                    {
                        result = Err(Error::Io(libc::EIO));
                        break;
                    }
                }
                channel.release(reply.pin_token);
                result?;
            }
        }

        while next < slices.len() && window.len() < WINDOW {
            window.push_back(
                dispatch_ticket(
                    fanout,
                    backend_id,
                    path,
                    cache_key_version,
                    origin_match_version,
                    slices[next],
                    source,
                )
                .await,
            );
            next += 1;
        }
    }
    Ok(())
}

/// Resolve an object's metadata by reading its dedicated content-addressed
/// metadata entry through the pool. The entry's single page carries the
/// object's [`ObjectMetadata`]; the HTTP backend fills it from an origin
/// `HEAD` on a miss, while local-disk and peer hits avoid the origin
/// entirely. The whole page is read (a zero-copy borrow) and decoded.
async fn read_object_metadata<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    backend_id: &str,
    path: &str,
    page_size: usize,
    force_refresh: bool,
) -> Result<ObjectMetadata, ()> {
    let origin_ref = OriginRef::metadata_entry(backend_id, path);
    let mut req = StripeReq::new(origin_ref.stripe_key()).with_origin(origin_ref);
    if force_refresh {
        req = req.with_force_refresh_cached_page();
    }
    let mut rs: ReadStream = pool.read(&req, 0, page_size as u64).await.map_err(|_| ())?;
    let page = rs.next_page().await.ok_or(())?.map_err(|_| ())?;
    ObjectMetadata::decode(page.as_slice()).map_err(|_| ())
}

/// Format the `200 OK` head for serving a whole object.
fn full_head(meta: &ObjectMetadata) -> Vec<u8> {
    let builder = response_metadata_headers(Response::builder(), meta);
    let resp = builder
        .status(StatusCode::OK)
        .header(CONTENT_LENGTH, meta.length.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid full-object response head");
    serialize_response_head(&resp)
}

/// Format the `206 Partial Content` head for a resolved byte range.
/// `END` in `Content-Range` is inclusive (`resolved.end - 1`).
fn partial_head(resolved: ResolvedRange, meta: &ObjectMetadata) -> Vec<u8> {
    let start = resolved.start;
    let end_incl = resolved.end - 1;
    let clen = resolved.len();
    let builder = response_metadata_headers(Response::builder(), meta);
    let resp = builder
        .status(StatusCode::PARTIAL_CONTENT)
        .header(
            CONTENT_RANGE,
            format!("bytes {start}-{end_incl}/{}", meta.length),
        )
        .header(CONTENT_LENGTH, clen.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid partial-content response head");
    serialize_response_head(&resp)
}

fn response_metadata_headers(
    mut builder: ::http::response::Builder,
    meta: &ObjectMetadata,
) -> ::http::response::Builder {
    if let Some(etag) = meta.etag() {
        builder = builder.header(ETAG, etag);
    }
    if meta.etag().is_some() {
        if let Some(cache_control) = meta.cache_control() {
            builder = builder.header(CACHE_CONTROL, cache_control);
        }
    }
    builder
}

/// Format a `416 Range Not Satisfiable` head with `Content-Range: bytes
/// */LEN`.
fn unsatisfiable_response(total: u64) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::RANGE_NOT_SATISFIABLE)
        .header(CONTENT_RANGE, format!("bytes */{total}"))
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid unsatisfiable-range response head");
    serialize_response_head(&resp)
}

/// Format a bodyless status-line response (`Content-Length: 0`) for the
/// simple error statuses this frontend emits.
fn status_line_response(status: u16) -> Vec<u8> {
    let status = StatusCode::from_u16(status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let resp = Response::builder()
        .status(status)
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid status-line response head");
    serialize_response_head(&resp)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::{Error, PipelinedRead, ReadStream, WindowedRead};
    use crate::fanout::NumaShardTable;
    use crate::frontend::range::StripeSlice;
    use crate::storage::disks::DiskChannelDirectory;
    use std::cell::RefCell;

    fn spec(id: &str, bind: &str) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: FrontendKind::Http as i32,
            bind: bind.to_string(),
            backend: "primary".to_string(),
        }
    }

    #[test]
    fn from_spec_validates_kind_and_bind() {
        let f = HttpFrontend::from_spec(&spec("workload", "0.0.0.0:9000")).unwrap();
        assert_eq!(f.id(), "workload");
        assert_eq!(f.backend_id(), "primary");
        assert_eq!(f.bind(), "0.0.0.0:9000".parse().unwrap());

        let bad = HttpFrontend::from_spec(&spec("f", "not-an-addr"));
        assert!(matches!(bad, Err(FrontendError::BadBind(_))));
    }

    #[test]
    fn full_head_exact_bytes() {
        let head = full_head(&ObjectMetadata::new(4096));
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-length: 4096\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn full_head_includes_strong_validator_metadata() {
        let meta = ObjectMetadata::from_origin_head(
            4096,
            Some("\"etag-1\""),
            Some("public, max-age=60"),
            100,
        );
        let head = full_head(&meta);
        let s = std::str::from_utf8(&head).unwrap();

        assert!(s.contains("etag: \"etag-1\"\r\n"), "got: {s}");
        assert!(
            s.contains("cache-control: public, max-age=60\r\n"),
            "got: {s}"
        );
    }

    #[test]
    fn full_head_omits_unvalidated_local_cache_metadata() {
        let meta = ObjectMetadata::from_origin_head(4096, None, Some("max-age=60"), 100);
        let head = full_head(&meta);
        let s = std::str::from_utf8(&head).unwrap();

        assert!(!s.contains("etag:"), "got: {s}");
        assert!(!s.contains("cache-control:"), "got: {s}");
    }

    #[test]
    fn partial_head_exact_bytes_inclusive_end() {
        // Resolved [0, 100) of a 1000-byte object -> bytes 0-99/1000.
        let head = partial_head(
            ResolvedRange { start: 0, end: 100 },
            &ObjectMetadata::new(1000),
        );
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 206 Partial Content\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes 0-99/1000\r\n"), "got: {s}");
        assert!(s.contains("content-length: 100\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
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
            &ObjectMetadata::new(100),
        );
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.contains("content-range: bytes 70-99/100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 30\r\n"), "got: {s}");
    }

    #[test]
    fn unsatisfiable_response_exact_bytes() {
        let head = unsatisfiable_response(100);
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
        let r405 = status_line_response(405);
        let s405 = std::str::from_utf8(&r405).unwrap();
        assert!(
            s405.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"),
            "got: {s405}"
        );
        assert!(s405.contains("content-length: 0\r\n"), "got: {s405}");
        assert!(s405.contains("connection: close\r\n"), "got: {s405}");
        assert!(s405.ends_with("\r\n\r\n"), "got: {s405}");

        let r400 = status_line_response(400);
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
    fn metadata_entry_request_is_well_formed() {
        use crate::bufferpool::Req;
        use crate::storage::{METADATA_STRIPE_IDX, stripe_key};

        let origin_ref = OriginRef::metadata_entry("primary", "/o");
        assert!(origin_ref.is_metadata_entry());

        let req = StripeReq::new(origin_ref.stripe_key()).with_origin(origin_ref);
        assert!(req.origin().unwrap().is_metadata_entry());
        assert_eq!(req.key(), stripe_key("primary", "/o", METADATA_STRIPE_IDX));
        assert_eq!(METADATA_STRIPE_IDX, u64::MAX);
    }

    #[test]
    fn stripe_request_uses_etag_as_cache_key_version() {
        use crate::bufferpool::Req;

        let slice = StripeSlice {
            stripe_idx: 1,
            intra_offset: 0,
            intra_len: 10,
        };
        let (base_key, _) = stripe_request(
            "primary",
            "/o",
            None,
            None,
            slice,
            BodySource::OriginAllowed,
        );
        let (v1_key, v1_req) = stripe_request(
            "primary",
            "/o",
            Some("etag-1"),
            Some("etag-1"),
            slice,
            BodySource::OriginAllowed,
        );
        let (v2_key, v2_req) = stripe_request(
            "primary",
            "/o",
            Some("etag-2"),
            Some("etag-2"),
            slice,
            BodySource::OriginAllowed,
        );

        assert_ne!(base_key, v1_key);
        assert_ne!(v1_key, v2_key);
        assert_eq!(v1_req.key(), v1_key);
        assert_eq!(v1_req.origin().unwrap().origin_object_id, "/o");
        assert_eq!(
            v1_req.origin().unwrap().origin_match_version.as_deref(),
            Some("etag-1")
        );
        assert_eq!(v2_req.origin().unwrap().origin_object_id, "/o");
    }

    #[test]
    fn stripe_request_can_use_local_cache_version_without_origin_match() {
        use crate::bufferpool::Req;

        let slice = StripeSlice {
            stripe_idx: 1,
            intra_offset: 0,
            intra_len: 10,
        };
        let (base_key, _) = stripe_request(
            "primary",
            "/o",
            None,
            None,
            slice,
            BodySource::OriginAllowed,
        );
        let (local_key, local_req) = stripe_request(
            "primary",
            "/o",
            Some("unvalidated:10:1"),
            None,
            slice,
            BodySource::OriginAllowed,
        );

        assert_ne!(base_key, local_key);
        assert_eq!(local_req.key(), local_key);
        let origin = local_req.origin().unwrap();
        assert_eq!(
            origin.cache_key_version.as_deref(),
            Some("unvalidated:10:1")
        );
        assert_eq!(origin.origin_match_version, None);
    }

    #[test]
    fn stripe_request_can_disable_origin_fetches() {
        use crate::bufferpool::Req;

        let slice = StripeSlice {
            stripe_idx: 1,
            intra_offset: 0,
            intra_len: 10,
        };
        let (origin_key, origin_req) = stripe_request(
            "primary",
            "/o",
            Some("etag-1"),
            Some("etag-1"),
            slice,
            BodySource::OriginAllowed,
        );
        let (cache_key, cache_req) = stripe_request(
            "primary",
            "/o",
            Some("etag-1"),
            Some("etag-1"),
            slice,
            BodySource::CacheOnly,
        );

        assert_eq!(cache_key, origin_key);
        assert_eq!(cache_req.key(), origin_key);
        assert!(origin_req.origin().is_some());
        assert!(cache_req.origin().is_none());
    }

    /// A mock pool whose `read` never constructs a `ReadStream` (that
    /// constructor is crate-internal to bufferpool): it always errors.
    /// Sufficient to wire an [`HttpDriver`] for accept-loop tests, where
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

        fn read_pipelined<'p>(
            &'p self,
            _stripes: Vec<StripePlan<StripeReq>>,
            _window: usize,
        ) -> Result<PipelinedRead<'p>, Error> {
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
        let mut driver = HttpDriver::new(
            Rc::new(MockPool),
            handle,
            listen_fd,
            Rc::from("primary"),
            "primary".to_string(),
            4 * 1024 * 1024,
            2 * 1024 * 1024,
            Rc::new(FanoutTable::new(
                0,
                vec![None],
                NumaShardTable::from_shards([(0, None)]),
                DiskChannelDirectory::new(),
            )),
        );
        // No client has connected: accept is pending, no conns, so the
        // engine reports no work and stays idle.
        assert!(!driver.progress());
        assert_eq!(driver.conn_count(), 0);
        assert!(driver.is_idle());
    }
}
