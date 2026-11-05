// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Minimal HTTP/1.1 server and the sidecar request flow.
//!
//! One task per client connection. Requests are parsed in place from a
//! per-connection buffer (pipelined requests are served from what is already
//! buffered), and responses are built in per-connection buffers so the steady
//! state does not allocate.

use crate::http::{Connection, HeadBuilder, failure_response};
use crate::object::{
    AZURE_VERSION, Api, Condition, Failure, Identity, RangeSpec, Selection, azure_version,
    bucket_key, identity, parse_condition, parse_range, s3_version, split_target, supported_header,
};
use crate::racer::{Client, Frame, Meta, Scratch, Subscription};
use crate::splice::{ClientWatch, Pipe, forward, send_more};
use std::io::IoSlice;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc, watch};

/// Largest accepted request head: 32 KiB of headers plus 4 KiB of slack
/// for the request line.
const MAX_REQUEST_HEAD: usize = (32 << 10) + 4096;
const MAX_HEADERS: usize = 256;
const IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const HEADER_TIMEOUT: Duration = Duration::from_secs(5);
/// Bodies up to this size are read into userspace and sent with the head in
/// one writev. That costs one copy but saves the pipe round trip and lets a
/// Racer failure still produce a proper error status.
pub const SMALL_BODY: u64 = 64 << 10;
/// Shutdown grace period for in-flight requests.
pub const SHUTDOWN_GRACE: Duration = Duration::from_secs(5);
/// How long to drain unread request bytes before closing, so the kernel does
/// not reset the connection and discard the response we just sent.
const CLOSE_LINGER: Duration = Duration::from_millis(500);

/// Server configuration.
pub struct Config {
    pub api: Api,
    pub request_timeout: Duration,
    /// Connection slots, `max_connections` permits, shared by every worker
    /// so the limit is global.
    pub slots: Arc<Semaphore>,
    pub max_connections: usize,
}

struct Shared {
    api: Api,
    request_timeout: Duration,
    header_timeout: Duration,
    client: Client,
}

/// An accepted connection handed to a worker with its slot.
pub type Handoff = (std::net::TcpStream, OwnedSemaphorePermit);

/// Another worker's inbox, as seen by the accepting worker.
pub struct Peer {
    pub inbox: mpsc::UnboundedSender<Handoff>,
    /// Connections the worker is serving.
    pub active: Arc<AtomicUsize>,
}

/// What a worker serves: the listener (the accepting worker, which hands
/// connections to `peers` too) or connections handed to it.
pub enum Source {
    Listener {
        listener: TcpListener,
        peers: Vec<Peer>,
    },
    Inbox(mpsc::UnboundedReceiver<Handoff>),
}

/// Serve until `shutdown` turns true, then let in-flight requests finish for
/// up to `SHUTDOWN_GRACE`. `active` counts this worker's connections.
pub async fn serve(
    source: Source,
    client: Client,
    config: Config,
    active: Arc<AtomicUsize>,
    mut shutdown: watch::Receiver<bool>,
) {
    let shared = Arc::new(Shared {
        api: config.api,
        request_timeout: config.request_timeout,
        header_timeout: config.request_timeout.min(HEADER_TIMEOUT),
        client,
    });
    let slots = config.slots;
    let conn_shutdown = shutdown.clone();
    let spawn = |stream: TcpStream, permit: OwnedSemaphorePermit| {
        let shared = shared.clone();
        let shutdown = conn_shutdown.clone();
        let active = active.clone();
        active.fetch_add(1, Ordering::Relaxed);
        tokio::spawn(async move {
            let _ = stream.set_nodelay(true);
            Conn::new(stream, shared, shutdown).run().await;
            active.fetch_sub(1, Ordering::Relaxed);
            drop(permit);
        });
    };
    match source {
        Source::Listener { listener, peers } => {
            let mut next = 0usize;
            loop {
                // Take a slot before accepting,
                // so excess connections wait in the kernel backlog instead
                // of being accepted.
                let permit = tokio::select! {
                    biased;
                    _ = crate::stopped(&mut shutdown) => break,
                    permit = slots.clone().acquire_owned() => match permit {
                        Ok(permit) => permit,
                        Err(_) => break,
                    },
                };
                let stream = tokio::select! {
                    biased;
                    _ = crate::stopped(&mut shutdown) => break,
                    accepted = listener.accept() => match accepted {
                        Ok((stream, _)) => stream,
                        Err(_) => {
                            // Typically EMFILE or ECONNABORTED: back off briefly.
                            drop(permit);
                            tokio::time::sleep(Duration::from_millis(5)).await;
                            continue;
                        }
                    },
                };
                if peers.is_empty() {
                    spawn(stream, permit);
                    continue;
                }
                // Hand the connection to the worker serving the fewest,
                // rotating the starting point to spread ties.
                let workers = peers.len() + 1;
                next = (next + 1) % workers;
                let load = |i: usize| match i {
                    0 => active.load(Ordering::Relaxed),
                    i => peers[i - 1].active.load(Ordering::Relaxed),
                };
                let pick = (0..workers)
                    .map(|k| (next + k) % workers)
                    .min_by_key(|&i| load(i))
                    .unwrap_or(0);
                if pick == 0 {
                    spawn(stream, permit);
                } else if let Ok(stream) = stream.into_std() {
                    // Count it now so back-to-back accepts see it.
                    let peer = &peers[pick - 1];
                    peer.active.fetch_add(1, Ordering::Relaxed);
                    if let Err(mpsc::error::SendError((stream, permit))) =
                        peer.inbox.send((stream, permit))
                    {
                        // The worker has stopped (shutdown races the accept):
                        // serve the connection here rather than drop it.
                        peer.active.fetch_sub(1, Ordering::Relaxed);
                        if let Ok(stream) = TcpStream::from_std(stream) {
                            spawn(stream, permit);
                        }
                    }
                }
            }
            drop(listener);
        }
        Source::Inbox(mut inbox) => loop {
            let (stream, permit) = tokio::select! {
                biased;
                _ = crate::stopped(&mut shutdown) => {
                    // Connections already accepted are served (and closed
                    // when idle), as if accepted here.
                    inbox.close();
                    while let Ok((stream, permit)) = inbox.try_recv() {
                        active.fetch_sub(1, Ordering::Relaxed);
                        if let Ok(stream) = TcpStream::from_std(stream) {
                            spawn(stream, permit);
                        }
                    }
                    break;
                }
                handoff = inbox.recv() => match handoff {
                    Some(handoff) => handoff,
                    None => break,
                },
            };
            // The accepting worker already counted this connection.
            active.fetch_sub(1, Ordering::Relaxed);
            if let Ok(stream) = TcpStream::from_std(stream) {
                spawn(stream, permit);
            }
        },
    }
    // Every connection task, on any worker, holds one slot; all slots back
    // means all done. Workers drain in turn, each releasing them on return.
    let all = u32::try_from(config.max_connections).unwrap_or(u32::MAX);
    let _ = tokio::time::timeout(SHUTDOWN_GRACE, slots.acquire_many(all)).await;
}

/// What happens to the connection after a response.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Next {
    KeepAlive,
    Close,
    /// The response was committed but could not be completed.
    Abort,
}

struct Conn {
    stream: TcpStream,
    shared: Arc<Shared>,
    shutdown: watch::Receiver<bool>,
    /// Request bytes; `rbuf[rpos..]` is unparsed.
    rbuf: Vec<u8>,
    rpos: usize,
    /// Response head scratch.
    out: Vec<u8>,
    /// Small-body and error-body scratch.
    body: Vec<u8>,
    scratch: Scratch,
    pipe: Option<Pipe>,
    /// Set once any response byte was handed to the socket.
    committed: bool,
}

/// Request facts the handler needs, copied out of the parse so the request
/// buffer is not borrowed across awaits.
struct Request {
    head: bool,
    method_ok: bool,
    options_star: bool,
    head_len: usize,
    connection: Connection,
    close: bool,
    /// Sidecar decisions, computed only for GET and HEAD.
    plan: Option<Result<Plan, Failure>>,
}

impl Conn {
    fn new(stream: TcpStream, shared: Arc<Shared>, shutdown: watch::Receiver<bool>) -> Self {
        Self {
            stream,
            shared,
            shutdown,
            rbuf: Vec::with_capacity(8192),
            rpos: 0,
            out: Vec::with_capacity(512),
            body: Vec::new(),
            scratch: Scratch::default(),
            pipe: None,
            committed: false,
        }
    }

    async fn run(mut self) {
        let mut first = true;
        loop {
            let next = match self.read_request(first).await {
                ReadResult::Ready => self.serve_one().await,
                ReadResult::TooLarge => {
                    self.send_failure(Failure::HeaderTooLarge, false, Connection::Close)
                        .await;
                    Next::Close
                }
                ReadResult::Bad => {
                    self.send_failure(Failure::BadRequest, false, Connection::Close)
                        .await;
                    Next::Close
                }
                ReadResult::Closed => return,
            };
            first = false;
            match next {
                Next::KeepAlive if !*self.shutdown.borrow() => {}
                Next::KeepAlive | Next::Close => {
                    self.close().await;
                    return;
                }
                // Close without completing the
                // body. The FIN after a short body (per Content-Length)
                // tells the client the transfer failed; a reset could
                // discard the head still queued behind MSG_MORE.
                Next::Abort => return,
            }
        }
    }

    /// Half-close and briefly drain so unread request bytes do not turn the
    /// close into a reset that destroys the response in flight.
    async fn close(&mut self) {
        let _ = self.stream.shutdown().await;
        let mut sink = [0u8; 4096];
        let _ = tokio::time::timeout(CLOSE_LINGER, async {
            let mut total = 0usize;
            while total < (256 << 10) {
                match self.stream.read(&mut sink).await {
                    Ok(0) | Err(_) => break,
                    Ok(n) => total += n,
                }
            }
        })
        .await;
    }

    /// Ensure a complete request head is buffered at `rbuf[rpos..]`.
    async fn read_request(&mut self, first: bool) -> ReadResult {
        // Compact consumed bytes so the buffer does not grow.
        if self.rpos > 0 {
            self.rbuf.drain(..self.rpos);
            self.rpos = 0;
        }
        let header_timeout = self.shared.header_timeout;
        // Use the header timeout for the first request and the idle
        // timeout while waiting for the first byte of later ones.
        let mut deadline = tokio::time::Instant::now()
            + if first || !self.rbuf.is_empty() {
                header_timeout
            } else {
                IDLE_TIMEOUT
            };
        let mut started = !self.rbuf.is_empty();
        loop {
            if !self.rbuf.is_empty() {
                match head_complete(&self.rbuf) {
                    HeadState::Complete(len) if len > MAX_REQUEST_HEAD => {
                        return ReadResult::TooLarge;
                    }
                    HeadState::Complete(_) => return ReadResult::Ready,
                    HeadState::Invalid => return ReadResult::Bad,
                    HeadState::TooLarge => return ReadResult::TooLarge,
                    HeadState::Partial if self.rbuf.len() > MAX_REQUEST_HEAD => {
                        return ReadResult::TooLarge;
                    }
                    HeadState::Partial => {}
                }
            }
            if self.rbuf.capacity() - self.rbuf.len() < 2048 {
                self.rbuf.reserve(8192);
            }
            let mut shutdown = self.shutdown.clone();
            let read = tokio::select! {
                biased;
                // Idle connections close on shutdown; partial requests may
                // still finish within the header timeout.
                _ = crate::stopped(&mut shutdown), if !started => return ReadResult::Closed,
                r = tokio::time::timeout_at(deadline, self.stream.read_buf(&mut self.rbuf)) => r,
            };
            match read {
                Ok(Ok(0)) | Ok(Err(_)) | Err(_) => return ReadResult::Closed,
                Ok(Ok(_)) => {
                    if !started {
                        started = true;
                        deadline = tokio::time::Instant::now() + header_timeout;
                    }
                }
            }
        }
    }

    async fn serve_one(&mut self) -> Next {
        let mut request = match self.parse() {
            Some(r) => r,
            None => {
                self.send_failure(Failure::BadRequest, false, Connection::Close)
                    .await;
                return Next::Close;
            }
        };
        self.committed = false;
        let timeout = self.shared.request_timeout;
        let result = tokio::time::timeout(timeout, self.handle(&mut request)).await;
        self.rpos = request.head_len;
        let next = match result {
            Ok(next) => next,
            Err(_) if self.committed => Next::Abort,
            Err(_) => {
                self.send_failure(Failure::Unavailable, request.head, request.connection)
                    .await
            }
        };
        match next {
            Next::KeepAlive if request.close => Next::Close,
            other => other,
        }
    }

    fn parse(&self) -> Option<Request> {
        parse_request(self.shared.api, &self.rbuf)
    }

    async fn send_failure(&mut self, failure: Failure, head: bool, connection: Connection) -> Next {
        failure_response(
            &mut self.out,
            &mut self.body,
            self.shared.api,
            failure,
            head,
            connection,
        );
        let next = if connection == Connection::Close {
            Next::Close
        } else {
            Next::KeepAlive
        };
        self.write_response(next).await
    }

    /// Write `out` followed by `body` with as few syscalls as possible.
    async fn write_response(&mut self, next: Next) -> Next {
        self.committed = true;
        let mut slices = [IoSlice::new(&self.out), IoSlice::new(&self.body)];
        let mut slices: &mut [IoSlice<'_>] = &mut slices;
        if self.body.is_empty() {
            slices = &mut slices[..1];
        }
        // Bound the whole write: error responses are sent outside the
        // request deadline, and a client that stops reading must not hold a
        // connection slot forever. On expiry the connection is dropped.
        let stream = &mut self.stream;
        let write = tokio::time::timeout(self.shared.request_timeout, async {
            while !slices.is_empty() {
                match stream.write_vectored(slices).await {
                    Ok(0) | Err(_) => return false,
                    Ok(n) => IoSlice::advance_slices(&mut slices, n),
                }
            }
            true
        });
        match write.await {
            Ok(true) => next,
            Ok(false) | Err(_) => Next::Abort,
        }
    }

    async fn handle(&mut self, request: &mut Request) -> Next {
        let head = request.head;
        let connection = request.connection;
        let next = if request.close {
            Next::Close
        } else {
            Next::KeepAlive
        };
        if request.options_star {
            // Answer `OPTIONS *` with an empty 200 before any routing.
            let mut h = HeadBuilder::new(&mut self.out, 200);
            h.header("Content-Length", b"0");
            h.finish(connection);
            self.body.clear();
            return self.write_response(next).await;
        }
        if !request.method_ok {
            return self
                .send_failure(Failure::MethodNotAllowed, head, connection)
                .await;
        }
        let plan = match request.plan.take() {
            Some(Ok(plan)) => plan,
            Some(Err(f)) => return self.send_failure(f, head, connection).await,
            None => return self.send_failure(Failure::Internal, head, connection).await,
        };
        match self.execute(plan, head, connection, next).await {
            Ok(next) => next,
            Err(f) => self.send_failure(f, head, connection).await,
        }
    }

    async fn execute(
        &mut self,
        plan: Plan,
        head: bool,
        connection: Connection,
        next: Next,
    ) -> Result<Next, Failure> {
        let client = &self.shared.client;
        let conditional = plan.if_match.present || plan.if_none.present;
        if !head
            && !conditional
            && let Ok(range) = plan.range
        {
            // Unconditional GET: one subscription, no Stat. Racer resolves
            // the forwarded range exactly as the sidecar would, and the
            // result is cross-checked below.
            let sub = client.subscribe(&plan.id, None, range, &mut self.scratch);
            let Some(sub) = ClientWatch::new(&self.stream, &mut self.rbuf, MAX_REQUEST_HEAD)
                .guard(sub)
                .await
            else {
                return Ok(Next::Abort);
            };
            let sub = sub?;
            let selection = unpinned_selection(range, sub.meta.size, sub.first, sub.end)?;
            return self
                .stream_body(sub, &plan, selection, connection, next)
                .await;
        }
        let meta = client.stat(&plan.id, &mut self.scratch);
        let Some(meta) = ClientWatch::new(&self.stream, &mut self.rbuf, MAX_REQUEST_HEAD)
            .guard(meta)
            .await
        else {
            return Ok(Next::Abort);
        };
        let meta = meta?;
        if plan.if_match.present && !plan.if_match.matches(&meta.etag, false) {
            return Err(Failure::PreconditionFailed);
        }
        if plan.if_none.present && plan.if_none.matches(&meta.etag, true) {
            not_modified_head(
                &mut self.out,
                self.shared.api,
                &plan.version,
                &meta.etag,
                connection,
            );
            self.body.clear();
            return Ok(self.write_response(next).await);
        }
        let selection = plan.range?.resolve(meta.size)?;
        if head {
            success_head(
                &mut self.out,
                self.shared.api,
                &plan.version,
                &meta.etag,
                meta.content_type.as_deref(),
                selection,
                connection,
            );
            self.body.clear();
            return Ok(self.write_response(next).await);
        }
        // Pin the version Stat selected, with the exact resolved range.
        let range = pinned_range(selection);
        let sub = client.subscribe(&plan.id, Some(&meta.etag), range, &mut self.scratch);
        let Some(sub) = ClientWatch::new(&self.stream, &mut self.rbuf, MAX_REQUEST_HEAD)
            .guard(sub)
            .await
        else {
            return Ok(Next::Abort);
        };
        let sub = sub.map_err(|f| match f {
            // A pinned 416 has no Content-Range.
            Failure::RangeNotSatisfiable(_) => Failure::RangeNotSatisfiable(None),
            other => other,
        })?;
        check_pinned(&meta, &sub.meta, sub.first, sub.end, selection)?;
        self.stream_body(sub, &plan, selection, connection, next)
            .await
    }

    /// Deliver the subscription payload. Errors returned here happen before
    /// anything was committed and become error responses; failures after the
    /// head was sent return `Next::Abort`.
    async fn stream_body(
        &mut self,
        mut sub: Subscription,
        plan: &Plan,
        selection: Selection,
        connection: Connection,
        next: Next,
    ) -> Result<Next, Failure> {
        let content_type = sub.meta.content_type.take();
        let etag = std::mem::take(&mut sub.meta.etag);
        let length = selection.length;
        if length <= SMALL_BODY {
            // Small: validate the whole transfer, including completion,
            // before committing anything, then send head and body together.
            self.body.clear();
            self.body.resize(length as usize, 0);
            let read = read_all(&mut sub, &mut self.body);
            let Some(read) = ClientWatch::new(&self.stream, &mut self.rbuf, MAX_REQUEST_HEAD)
                .guard(read)
                .await
            else {
                return Ok(Next::Abort);
            };
            read?;
            sub.recycle(&mut self.scratch);
            success_head(
                &mut self.out,
                self.shared.api,
                &plan.version,
                &etag,
                content_type.as_deref(),
                selection,
                connection,
            );
            return Ok(self.write_response(next).await);
        }
        // Validate the first frame before committing so a broken stream
        // still gets a proper error status.
        let first = sub.next_frame();
        let Some(first) = ClientWatch::new(&self.stream, &mut self.rbuf, MAX_REQUEST_HEAD)
            .guard(first)
            .await
        else {
            return Ok(Next::Abort);
        };
        let first = first?;
        success_head(
            &mut self.out,
            self.shared.api,
            &plan.version,
            &etag,
            content_type.as_deref(),
            selection,
            connection,
        );
        self.committed = true;
        if send_more(&self.stream, &self.out).await.is_err() {
            return Ok(Next::Abort);
        }
        if self.pipe.is_none() {
            self.pipe = Pipe::new().ok();
        }
        let ok = self.forward_pages(&mut sub, first).await;
        sub.recycle(&mut self.scratch);
        Ok(if ok { next } else { Next::Abort })
    }

    async fn forward_pages(&mut self, sub: &mut Subscription, first: Frame) -> bool {
        forward_pages(
            sub,
            first,
            &mut self.stream,
            &mut self.rbuf,
            self.pipe.as_ref(),
            &mut self.body,
        )
        .await
    }
}

/// Forward every page of `sub` to `stream`, withholding the final byte until
/// Racer confirms completion so truncation is never mistaken for a complete body.
/// `rbuf` buffers client bytes read while watching for a disconnect.
async fn forward_pages(
    sub: &mut Subscription,
    first: Frame,
    stream: &mut TcpStream,
    rbuf: &mut Vec<u8>,
    pipe: Option<&Pipe>,
    body: &mut Vec<u8>,
) -> bool {
    // One watch for the whole body: after a half-close it stays off.
    let out = stream;
    let stream = &*out;
    let mut watch = ClientWatch::new(stream, rbuf, MAX_REQUEST_HEAD);
    let mut pending = Some(first);
    loop {
        let frame = match pending.take() {
            Some(frame) => frame,
            None => match watch.guard(sub.next_frame()).await {
                Some(Ok(frame)) => frame,
                _ => return false,
            },
        };
        let last = sub.last_page();
        let mut remaining = u64::from(frame.length) - u64::from(last);
        // Bytes that arrived with the frame header are written from
        // userspace; the rest is spliced.
        if sub.buffered() > 0 && remaining > 0 {
            let chunk = sub.take_buffered(remaining as usize);
            remaining -= chunk.len() as u64;
            if write_all_more(stream, chunk).await.is_err() {
                return false;
            }
        }
        if remaining > 0 {
            let ok = match pipe {
                Some(pipe) => forward(&sub.stream, pipe, stream, remaining, true, &mut watch)
                    .await
                    .is_ok(),
                None => copy(sub, stream, remaining, body, &mut watch).await,
            };
            if !ok {
                return false;
            }
        }
        if !last {
            if !matches!(watch.guard(sub.credit(&frame)).await, Some(Ok(()))) {
                return false;
            }
            continue;
        }
        let mut final_byte = [0u8; 1];
        if !matches!(
            watch.guard(sub.read_exact(&mut final_byte)).await,
            Some(Ok(()))
        ) {
            return false;
        }
        // Racer may wait for the final credit before completing, or
        // complete without reading it; a 12-byte write never blocks on
        // an open socket, so sending it first cannot deadlock.
        let done = async {
            sub.credit(&frame).await?;
            sub.complete().await
        };
        if !matches!(watch.guard(done).await, Some(Ok(()))) {
            return false;
        }
        return out.write_all(&final_byte).await.is_ok();
    }
}

/// Parse a buffered request head into connection facts. `None` is a 400.
fn parse_request(api: Api, buf: &[u8]) -> Option<Request> {
    let mut headers = [httparse::EMPTY_HEADER; MAX_HEADERS];
    let mut req = httparse::Request::new(&mut headers);
    let head_len = match req.parse(buf) {
        Ok(httparse::Status::Complete(n)) => n,
        _ => return None,
    };
    let version = req.version?;
    let mut hosts = 0;
    let mut content_length: Option<&[u8]> = None;
    let mut body = false;
    let mut close_token = false;
    let mut keep_alive_token = false;
    for h in req.headers.iter() {
        if h.name.eq_ignore_ascii_case("host") {
            hosts += 1;
        } else if h.name.eq_ignore_ascii_case("content-length") {
            let v = h.value.trim_ascii();
            if v.is_empty() || !v.iter().all(u8::is_ascii_digit) {
                return None;
            }
            if content_length.is_some_and(|old| old != v) {
                return None;
            }
            content_length = Some(v);
            body |= v.iter().any(|&c| c != b'0');
        } else if h.name.eq_ignore_ascii_case("transfer-encoding") {
            body = true;
        } else if h.name.eq_ignore_ascii_case("connection") {
            for token in h.value.split(|&c| c == b',') {
                let token = token.trim_ascii();
                close_token |= token.eq_ignore_ascii_case(b"close");
                keep_alive_token |= token.eq_ignore_ascii_case(b"keep-alive");
            }
        }
    }
    // HTTP/1.1 requires exactly one Host; HTTP/1.0 allows none.
    if hosts > 1 || (version == 1 && hosts == 0) {
        return None;
    }
    let target = req.path?.as_bytes();
    split_target(target)?;
    let method = req.method?;
    let (connection, close) = if version == 0 {
        if keep_alive_token && !close_token {
            (Connection::KeepAlive, false)
        } else {
            (Connection::Close, true)
        }
    } else if close_token {
        (Connection::Close, true)
    } else {
        (Connection::Default, false)
    };
    // A request body is never read; answer, then close.
    let (connection, close) = if body {
        (Connection::Close, true)
    } else {
        (connection, close)
    };
    let head = method == "HEAD";
    let method_ok = head || method == "GET";
    let plan = method_ok.then(|| plan(api, &req, head));
    Some(Request {
        head,
        method_ok,
        options_star: method == "OPTIONS" && target == b"*",
        head_len,
        connection,
        close,
        plan,
    })
}

/// Success head without the final CRLF.
pub(crate) fn success_head(
    out: &mut Vec<u8>,
    api: Api,
    version: &[u8],
    etag: &[u8],
    content_type: Option<&[u8]>,
    selection: Selection,
    connection: Connection,
) {
    let status = if selection.partial { 206 } else { 200 };
    let mut h = HeadBuilder::new(out, status);
    h.header("Etag", etag);
    h.header("Accept-Ranges", b"bytes");
    h.number("Content-Length", selection.length);
    h.header(
        "Content-Type",
        content_type.unwrap_or(b"application/octet-stream"),
    );
    match api {
        Api::Azure => {
            h.header("X-Ms-Version", AZURE_VERSION.as_bytes());
            h.azure_object(version);
        }
        Api::S3 if !version.is_empty() => {
            h.header("X-Amz-Version-Id", version);
        }
        Api::S3 => {}
    }
    if selection.partial {
        h.content_range(
            selection.offset,
            selection.offset + selection.length - 1,
            selection.size,
        );
    }
    h.finish(connection);
}

/// `304 Not Modified` head for a matching `If-None-Match`.
pub(crate) fn not_modified_head(
    out: &mut Vec<u8>,
    api: Api,
    version: &[u8],
    etag: &[u8],
    connection: Connection,
) {
    let mut h = HeadBuilder::new(out, 304);
    h.header("Etag", etag);
    if api == Api::Azure {
        h.header("X-Ms-Version", AZURE_VERSION.as_bytes());
        h.azure_object(version);
    }
    h.finish(connection);
}

/// Cross-check an unconditional subscription: Racer must have selected
/// exactly what the sidecar resolves for the forwarded range.
fn unpinned_selection(
    range: RangeSpec,
    size: u64,
    first: u64,
    end: u64,
) -> Result<Selection, Failure> {
    let selection = range.resolve(size).map_err(|_| Failure::BadGateway)?;
    if selection.offset != first || selection.offset + selection.length != end {
        return Err(Failure::BadGateway);
    }
    Ok(selection)
}

/// The exact range forwarded with a version pin.
fn pinned_range(selection: Selection) -> RangeSpec {
    if selection.offset != 0 || selection.length != 0 {
        RangeSpec::Closed(selection.offset, selection.offset + selection.length - 1)
    } else {
        RangeSpec::Full
    }
}

/// Cross-check a pinned subscription against the Stat that chose the pin.
fn check_pinned(
    stat: &Meta,
    sub: &Meta,
    first: u64,
    end: u64,
    selection: Selection,
) -> Result<(), Failure> {
    // An ETag pin must not change the headers selected by Stat.
    if sub.size != stat.size || sub.content_type != stat.content_type {
        return Err(Failure::BadGateway);
    }
    let len = end - first;
    if first != selection.offset || len != selection.length {
        // A short tail is an unsatisfiable range.
        if end == sub.size && selection.length > len {
            return Err(Failure::RangeNotSatisfiable(None));
        }
        return Err(Failure::BadGateway);
    }
    Ok(())
}

/// Everything decided from the request alone, including condition and
/// range parsing.
fn plan(api: Api, req: &httparse::Request<'_, '_>, head: bool) -> Result<Plan, Failure> {
    if !req
        .headers
        .iter()
        .all(|h| supported_header(api, h.name.as_bytes()))
    {
        return Err(Failure::NotImplemented);
    }
    let target =
        split_target(req.path.unwrap_or_default().as_bytes()).ok_or(Failure::BadRequest)?;
    let (mut bucket, mut key) = (Vec::new(), Vec::new());
    if !bucket_key(target.path, &mut bucket, &mut key) {
        return Err(Failure::BadRequest);
    }
    let version = match api {
        Api::S3 => s3_version(target.query, head)?,
        Api::Azure => azure_version(target.query)?,
    };
    let id = identity(&bucket, &key, &version).ok_or(Failure::BadRequest)?;
    let values = |name: &str| -> Vec<&[u8]> {
        req.headers
            .iter()
            .filter(|h| h.name.eq_ignore_ascii_case(name))
            .map(|h| h.value)
            .collect()
    };
    let if_match = parse_condition(&values("if-match")).ok_or(Failure::BadRequest)?;
    let if_none = parse_condition(&values("if-none-match")).ok_or(Failure::BadRequest)?;
    let range = match api {
        Api::S3 => parse_range(&values("range")),
        // Get Blob Properties always reports the full size.
        Api::Azure if head => Ok(RangeSpec::Full),
        Api::Azure => {
            let ms = values("x-ms-range");
            if ms.is_empty() {
                parse_range(&values("range"))
            } else {
                parse_range(&ms)
            }
        }
    };
    Ok(Plan {
        id,
        version,
        if_match,
        if_none,
        range,
    })
}

/// Write without pushing a partial segment: more payload follows.
async fn write_all_more(stream: &TcpStream, data: &[u8]) -> std::io::Result<()> {
    send_more(stream, data).await
}

/// Userspace fallback when no pipe could be created.
async fn copy(
    sub: &mut Subscription,
    dst: &TcpStream,
    mut len: u64,
    buf: &mut Vec<u8>,
    watch: &mut ClientWatch<'_>,
) -> bool {
    buf.resize(256 << 10, 0);
    while len > 0 {
        let want = (len as usize).min(buf.len());
        let Some(Ok(n)) = watch.guard(sub.read_some(&mut buf[..want])).await else {
            return false;
        };
        if send_more(dst, &buf[..n]).await.is_err() {
            return false;
        }
        len -= n as u64;
    }
    true
}

/// Read a whole (small) selection into `out`, which has the exact length,
/// and validate completion.
async fn read_all(sub: &mut Subscription, out: &mut [u8]) -> Result<(), Failure> {
    let mut at = 0usize;
    loop {
        let frame = sub.next_frame().await?;
        if frame.kind != 1 {
            // Completion: only valid once every byte arrived (checked by the
            // sequence), which here means the selection was empty.
            return Ok(());
        }
        let len = frame.length as usize;
        let page = out.get_mut(at..at + len).ok_or(Failure::BadGateway)?;
        sub.read_exact(page).await?;
        at += len;
        if sub.last_page() {
            sub.credit(&frame).await?;
            return sub.complete().await;
        }
        sub.credit(&frame).await?;
    }
}

/// Request-derived decisions.
struct Plan {
    id: Identity,
    version: Vec<u8>,
    if_match: Condition,
    if_none: Condition,
    range: Result<RangeSpec, Failure>,
}

enum ReadResult {
    Ready,
    TooLarge,
    Bad,
    Closed,
}

enum HeadState {
    Complete(usize),
    TooLarge,
    Partial,
    Invalid,
}

fn head_complete(buf: &[u8]) -> HeadState {
    // Cheap terminator scan first so partial heads are not reparsed.
    if !buf.windows(2).any(|w| w == b"\n\n" || w == b"\n\r") {
        return HeadState::Partial;
    }
    let mut headers = [httparse::EMPTY_HEADER; MAX_HEADERS];
    let mut req = httparse::Request::new(&mut headers);
    match req.parse(buf) {
        Ok(httparse::Status::Complete(n)) => HeadState::Complete(n),
        Ok(httparse::Status::Partial) => HeadState::Partial,
        Err(httparse::Error::TooManyHeaders) => HeadState::TooLarge,
        Err(_) => HeadState::Invalid,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(api: Api, raw: &str) -> Option<Request> {
        parse_request(api, raw.as_bytes())
    }

    fn s3(raw: &str) -> Option<Request> {
        request(Api::S3, raw)
    }

    fn plan_of(api: Api, raw: &str) -> Result<Plan, Failure> {
        request(api, raw).unwrap().plan.unwrap()
    }

    #[test]
    fn connection_disposition() {
        for (raw, connection, close) in [
            (
                "GET /b/k HTTP/1.1\r\nHost: x\r\n\r\n",
                Connection::Default,
                false,
            ),
            (
                "GET /b/k HTTP/1.1\r\nHost: x\r\nConnection: keep-alive, Close\r\n\r\n",
                Connection::Close,
                true,
            ),
            ("GET /b/k HTTP/1.0\r\n\r\n", Connection::Close, true),
            (
                "GET /b/k HTTP/1.0\r\nConnection: keep-alive\r\n\r\n",
                Connection::KeepAlive,
                false,
            ),
            (
                "GET /b/k HTTP/1.0\r\nConnection: keep-alive, close\r\n\r\n",
                Connection::Close,
                true,
            ),
            // A request body is never read: answer, then close.
            (
                "GET /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\n",
                Connection::Close,
                true,
            ),
            (
                "GET /b/k HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n",
                Connection::Close,
                true,
            ),
            (
                "GET /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: 000\r\n\r\n",
                Connection::Default,
                false,
            ),
            (
                "GET /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\nContent-Length: 4\r\n\r\n",
                Connection::Close,
                true,
            ),
        ] {
            let r = s3(raw).unwrap();
            assert_eq!((r.connection, r.close), (connection, close), "{raw}");
            assert_eq!(r.head_len, raw.len());
        }
    }

    #[test]
    fn malformed_requests() {
        for raw in [
            // HTTP/1.1 needs exactly one Host.
            "GET /b/k HTTP/1.1\r\n\r\n",
            "GET /b/k HTTP/1.1\r\nHost: x\r\nHost: y\r\n\r\n",
            // Content-Length must be digits and must not conflict.
            "GET /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: -1\r\n\r\n",
            "GET /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: \r\n\r\n",
            "GET /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n",
            // Invalid escapes are rejected before the method is checked.
            "PUT /b/%zz HTTP/1.1\r\nHost: x\r\n\r\n",
            "\x01\x02 nonsense\r\n\r\n",
            "GET /b/k HTTP/1.1\r\nHost: x\r\n",
        ] {
            assert!(s3(raw).is_none(), "{raw:?}");
        }
    }

    #[test]
    fn methods() {
        let r = s3("HEAD /b/k HTTP/1.1\r\nHost: x\r\n\r\n").unwrap();
        assert!(r.head && r.method_ok && !r.options_star && r.plan.is_some());
        for method in ["PUT", "POST", "DELETE", "OPTIONS", "get"] {
            let r = s3(&format!("{method} /b/k HTTP/1.1\r\nHost: x\r\n\r\n")).unwrap();
            assert!(
                !r.method_ok && !r.options_star && r.plan.is_none(),
                "{method}"
            );
        }
        let r = s3("OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n").unwrap();
        assert!(r.options_star && !r.method_ok);
    }

    #[test]
    fn pipelined_heads() {
        let one = "GET /b/k HTTP/1.1\r\nHost: x\r\n\r\n";
        let two = format!("{one}HEAD /b/k HTTP/1.1\r\nHost: x\r\n\r\n");
        assert!(matches!(head_complete(two.as_bytes()), HeadState::Complete(n) if n == one.len()));
        assert_eq!(s3(&two).unwrap().head_len, one.len());
        assert!(matches!(
            head_complete(b"GET /b/k HTTP/1.1\r\nHost"),
            HeadState::Partial
        ));
        assert!(matches!(
            head_complete(b"\x01 x\r\n\r\n"),
            HeadState::Invalid
        ));
        let mut many = String::from("GET /b/k HTTP/1.1\r\n");
        for i in 0..=MAX_HEADERS {
            many.push_str(&format!("X-{i}: v\r\n"));
        }
        many.push_str("\r\n");
        assert!(matches!(
            head_complete(many.as_bytes()),
            HeadState::TooLarge
        ));
    }

    #[test]
    fn plans() {
        let p = plan_of(
            Api::S3,
            "GET /b/k?versionId=v1 HTTP/1.1\r\nHost: x\r\nIf-Match: \"a\"\r\nIf-None-Match: *\r\nRange: bytes=1-2\r\n\r\n",
        )
        .unwrap();
        assert_eq!(p.version, b"v1");
        assert_eq!(p.id, identity(b"b", b"k", b"v1").unwrap());
        assert!(p.if_match.present && p.if_none.wildcard);
        assert_eq!(p.range, Ok(RangeSpec::Closed(1, 2)));
        // Azure prefers x-ms-range, and HEAD always reads the full blob.
        let raw =
            "GET /b/k HTTP/1.1\r\nHost: x\r\nRange: bytes=0-0\r\nx-ms-range: bytes=3-\r\n\r\n";
        assert_eq!(
            plan_of(Api::Azure, raw).unwrap().range,
            Ok(RangeSpec::From(3))
        );
        let raw = "GET /b/k HTTP/1.1\r\nHost: x\r\nRange: bytes=0-0\r\n\r\n";
        assert_eq!(
            plan_of(Api::Azure, raw).unwrap().range,
            Ok(RangeSpec::Closed(0, 0))
        );
        let raw = raw.replace("GET", "HEAD");
        assert_eq!(
            plan_of(Api::Azure, &raw).unwrap().range,
            Ok(RangeSpec::Full)
        );
        // Range errors wait for Stat; everything else fails the plan.
        let raw = "GET /b/k HTTP/1.1\r\nHost: x\r\nRange: bytes=2-1\r\n\r\n";
        assert_eq!(
            plan_of(Api::S3, raw).unwrap().range,
            Err(Failure::BadRequest)
        );
        for (api, raw, want) in [
            (
                Api::S3,
                "GET /b HTTP/1.1\r\nHost: x\r\n\r\n",
                Failure::BadRequest,
            ),
            (
                Api::S3,
                "GET /b/k HTTP/1.1\r\nHost: x\r\nIf-Match: bare\r\n\r\n",
                Failure::BadRequest,
            ),
            (
                Api::S3,
                "GET /b/k HTTP/1.1\r\nHost: x\r\nIf-None-Match: bare\r\n\r\n",
                Failure::BadRequest,
            ),
            (
                Api::S3,
                "GET /b/k HTTP/1.1\r\nHost: x\r\nIf-Range: \"v\"\r\n\r\n",
                Failure::NotImplemented,
            ),
            (
                Api::S3,
                "GET /b/k?partNumber=1 HTTP/1.1\r\nHost: x\r\n\r\n",
                Failure::NotImplemented,
            ),
            (
                Api::Azure,
                "GET /b/k HTTP/1.1\r\nHost: x\r\nx-ms-lease-id: l\r\n\r\n",
                Failure::NotImplemented,
            ),
            (
                Api::Azure,
                "GET /b/k?timeout=bad HTTP/1.1\r\nHost: x\r\n\r\n",
                Failure::BadRequest,
            ),
        ] {
            assert_eq!(plan_of(api, raw).err(), Some(want), "{raw}");
        }
    }

    /// Response head without the Date header, which changes every second.
    fn text(out: &[u8]) -> String {
        String::from_utf8(out.to_vec())
            .unwrap()
            .split("\r\n")
            .filter(|l| !l.starts_with("Date: "))
            .collect::<Vec<_>>()
            .join("\r\n")
    }

    fn selection(offset: u64, length: u64, size: u64, partial: bool) -> Selection {
        Selection {
            offset,
            length,
            size,
            partial,
        }
    }

    #[test]
    fn success_heads() {
        let mut out = Vec::new();
        success_head(
            &mut out,
            Api::S3,
            b"",
            b"\"v\"",
            None,
            selection(0, 4, 4, false),
            Connection::Default,
        );
        assert_eq!(
            text(&out),
            "HTTP/1.1 200 OK\r\nEtag: \"v\"\r\nAccept-Ranges: bytes\r\nContent-Length: 4\r\nContent-Type: application/octet-stream\r\n\r\n"
        );
        success_head(
            &mut out,
            Api::S3,
            b"v1",
            b"\"v\"",
            Some(b"text/plain"),
            selection(1, 2, 4, true),
            Connection::Close,
        );
        assert_eq!(
            text(&out),
            "HTTP/1.1 206 Partial Content\r\nEtag: \"v\"\r\nAccept-Ranges: bytes\r\nContent-Length: 2\r\nContent-Type: text/plain\r\nX-Amz-Version-Id: v1\r\nContent-Range: bytes 1-2/4\r\nConnection: close\r\n\r\n"
        );
        success_head(
            &mut out,
            Api::Azure,
            b"v1",
            b"\"v\"",
            Some(b"text/plain"),
            selection(0, 4, 4, false),
            Connection::KeepAlive,
        );
        assert_eq!(
            text(&out),
            "HTTP/1.1 200 OK\r\nEtag: \"v\"\r\nAccept-Ranges: bytes\r\nContent-Length: 4\r\nContent-Type: text/plain\r\nX-Ms-Version: 2026-02-06\r\nX-Ms-Blob-Type: BlockBlob\r\nX-Ms-Version-Id: v1\r\nConnection: keep-alive\r\n\r\n"
        );
    }

    #[test]
    fn not_modified_heads() {
        let mut out = Vec::new();
        not_modified_head(&mut out, Api::S3, b"v1", b"\"v\"", Connection::Default);
        assert_eq!(
            text(&out),
            "HTTP/1.1 304 Not Modified\r\nEtag: \"v\"\r\n\r\n"
        );
        not_modified_head(&mut out, Api::Azure, b"", b"\"v\"", Connection::Default);
        assert_eq!(
            text(&out),
            "HTTP/1.1 304 Not Modified\r\nEtag: \"v\"\r\nX-Ms-Version: 2026-02-06\r\nX-Ms-Blob-Type: BlockBlob\r\n\r\n"
        );
    }

    fn meta(size: u64, content_type: Option<&[u8]>) -> Meta {
        Meta {
            size,
            etag: b"\"v\"".to_vec(),
            content_type: content_type.map(<[u8]>::to_vec),
        }
    }

    #[test]
    fn subscription_cross_checks() {
        // Unconditional reads trust Racer only when it agrees with the
        // sidecar's own resolution.
        assert_eq!(
            unpinned_selection(RangeSpec::Suffix(2), 10, 8, 10),
            Ok(selection(8, 2, 10, true))
        );
        assert_eq!(
            unpinned_selection(RangeSpec::Full, 0, 0, 0),
            Ok(selection(0, 0, 0, false))
        );
        assert_eq!(
            unpinned_selection(RangeSpec::Suffix(2), 10, 7, 10),
            Err(Failure::BadGateway)
        );
        assert_eq!(
            unpinned_selection(RangeSpec::From(20), 10, 0, 0),
            Err(Failure::BadGateway)
        );
        // Pinned reads forward the exact range.
        assert_eq!(pinned_range(selection(0, 0, 0, false)), RangeSpec::Full);
        assert_eq!(
            pinned_range(selection(0, 4, 4, false)),
            RangeSpec::Closed(0, 3)
        );
        assert_eq!(
            pinned_range(selection(3, 2, 9, true)),
            RangeSpec::Closed(3, 4)
        );
        // ...and must match what Stat reported.
        let stat = meta(10, Some(b"text/plain"));
        let sel = selection(2, 4, 10, true);
        assert_eq!(check_pinned(&stat, &stat, 2, 6, sel), Ok(()));
        assert_eq!(
            check_pinned(&stat, &meta(11, Some(b"text/plain")), 2, 6, sel),
            Err(Failure::BadGateway)
        );
        assert_eq!(
            check_pinned(&stat, &meta(10, None), 2, 6, sel),
            Err(Failure::BadGateway)
        );
        assert_eq!(
            check_pinned(&stat, &stat, 3, 6, sel),
            Err(Failure::BadGateway)
        );
        assert_eq!(
            check_pinned(&stat, &stat, 2, 5, sel),
            Err(Failure::BadGateway)
        );
        // A short tail is reported as an unsatisfiable range.
        let tail = selection(8, 4, 12, true);
        assert_eq!(
            check_pinned(&stat, &stat, 8, 10, tail),
            Err(Failure::RangeNotSatisfiable(None))
        );
    }
}

/// Function-level tests for `forward_pages`: the test plays Racer on one end
/// of a Unix socket pair, writing raw frame bytes inline, and plays the
/// client on a loopback TCP connection.
#[cfg(test)]
mod page_tests {
    use super::*;
    use crate::racer::{FRAME_SIZE, PAGE_SIZE};
    use tokio::net::{TcpSocket, UnixStream};
    use tokio::task::JoinHandle;

    const PAGE: u8 = 1;
    const COMPLETE: u8 = 2;

    fn frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; FRAME_SIZE] {
        let mut b = [0; FRAME_SIZE];
        b[0] = kind;
        b[1..9].copy_from_slice(&number.to_be_bytes());
        b[9..17].copy_from_slice(&offset.to_be_bytes());
        b[17..].copy_from_slice(&length.to_be_bytes());
        b
    }

    fn credit(number: u64, length: u32) -> [u8; 12] {
        let mut b = [0; 12];
        b[..8].copy_from_slice(&number.to_be_bytes());
        b[8..].copy_from_slice(&length.to_be_bytes());
        b
    }

    /// Object bytes at absolute offsets `[first, end)`; a prime modulus keeps
    /// the pattern from lining up with page or pipe sizes.
    fn object(first: u64, end: u64) -> Vec<u8> {
        (first..end).map(|i| (i % 251) as u8).collect()
    }

    /// The page frames covering `[first, end)`.
    fn pages(first: u64, end: u64) -> Vec<(u64, u64, u32)> {
        let mut out = Vec::new();
        let mut at = first;
        while at < end {
            let number = at / PAGE_SIZE;
            let stop = end.min((number + 1) * PAGE_SIZE);
            out.push((number, at, (stop - at) as u32));
            at = stop;
        }
        out
    }

    /// Server and client ends of a loopback TCP connection. `rcvbuf` shrinks
    /// the client's receive buffer to force backpressure.
    async fn tcp_pair(rcvbuf: Option<u32>) -> (TcpStream, TcpStream) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let socket = TcpSocket::new_v4().unwrap();
        if let Some(size) = rcvbuf {
            socket.set_recv_buffer_size(size).unwrap();
        }
        let (client, accepted) = tokio::join!(
            socket.connect(listener.local_addr().unwrap()),
            listener.accept()
        );
        (accepted.unwrap().0, client.unwrap())
    }

    /// Read everything the client receives until EOF or error. `slow`
    /// reads in small chunks and yields between them.
    fn client_reader(mut client: TcpStream, slow: bool) -> JoinHandle<Vec<u8>> {
        tokio::spawn(async move {
            let mut got = Vec::new();
            let mut buf = vec![0u8; if slow { 4096 } else { 1 << 20 }];
            let mut reads = 0u32;
            loop {
                match client.read(&mut buf).await {
                    Ok(0) | Err(_) => return got,
                    Ok(n) => got.extend_from_slice(&buf[..n]),
                }
                reads += 1;
                if slow && reads.is_multiple_of(8) {
                    tokio::time::sleep(Duration::from_millis(1)).await;
                }
            }
        })
    }

    /// How Racer misbehaves, if at all.
    #[derive(Clone, Copy)]
    enum Fault {
        None,
        /// Close after this many payload bytes of the last page.
        Truncate(usize),
        /// Close after the payload without a completion frame.
        NoCompletion,
        /// Send this completion frame instead of the right one.
        Completion([u8; FRAME_SIZE]),
        /// Send this header instead of the second page's.
        SecondFrame([u8; FRAME_SIZE]),
    }

    /// Play Racer for `[first, end)`: frame headers and payload, collecting
    /// the credits received. The first page's header (and, if `split`, the
    /// first 1000 payload bytes) are assumed already buffered by the caller.
    fn racer(
        mut racer: UnixStream,
        first: u64,
        end: u64,
        split: bool,
        fault: Fault,
    ) -> JoinHandle<Vec<[u8; 12]>> {
        tokio::spawn(async move {
            let pages = pages(first, end);
            let mut credits = Vec::new();
            for (i, &(number, offset, length)) in pages.iter().enumerate() {
                let last = i + 1 == pages.len();
                let mut data = object(offset, offset + u64::from(length));
                if i == 0 {
                    if split {
                        data.drain(..1000);
                    }
                } else {
                    let header = match fault {
                        Fault::SecondFrame(h) if i == 1 => h,
                        _ => frame(PAGE, number, offset, length),
                    };
                    if racer.write_all(&header).await.is_err() {
                        return credits;
                    }
                }
                if let (true, Fault::Truncate(keep)) = (last, fault) {
                    let keep = keep - if i == 0 && split { 1000 } else { 0 };
                    let _ = racer.write_all(&data[..keep]).await;
                    return credits;
                }
                if racer.write_all(&data).await.is_err() {
                    return credits;
                }
                let mut c = [0u8; 12];
                if racer.read_exact(&mut c).await.is_err() {
                    return credits;
                }
                credits.push(c);
            }
            let completion = match fault {
                Fault::NoCompletion => return credits,
                Fault::Completion(f) => f,
                _ => frame(COMPLETE, pages.len() as u64, end - first, 0),
            };
            let _ = racer.write_all(&completion).await;
            credits
        })
    }

    struct Outcome {
        ok: bool,
        got: Vec<u8>,
        credits: Vec<[u8; 12]>,
    }

    /// Drive `forward_pages` over `[first, end)` the way `stream_body` does:
    /// validate the first frame, then forward.
    async fn run(
        first: u64,
        end: u64,
        use_pipe: bool,
        split: bool,
        fault: Fault,
        rcvbuf: Option<u32>,
    ) -> Outcome {
        let (ours, theirs) = UnixStream::pair().unwrap();
        let (number, offset, length) = pages(first, end)[0];
        let mut buffered = frame(PAGE, number, offset, length).to_vec();
        if split {
            buffered.extend_from_slice(&object(first, first + 1000));
        }
        let racer = racer(theirs, first, end, split, fault);
        let mut sub = Subscription::for_test(ours, first, end, buffered);
        let (mut server, client) = tcp_pair(rcvbuf).await;
        let reader = client_reader(client, rcvbuf.is_some());
        let pipe = use_pipe.then(|| Pipe::new().unwrap());
        let head = sub.next_frame().await.unwrap();
        let mut body = Vec::new();
        let ok = forward_pages(
            &mut sub,
            head,
            &mut server,
            &mut Vec::new(),
            pipe.as_ref(),
            &mut body,
        )
        .await;
        drop(server);
        drop(sub);
        let got = reader.await.unwrap();
        let credits = racer.await.unwrap();
        Outcome { ok, got, credits }
    }

    fn expected_credits(first: u64, end: u64) -> Vec<[u8; 12]> {
        pages(first, end)
            .into_iter()
            .map(|(number, _, length)| credit(number, length))
            .collect()
    }

    #[tokio::test]
    async fn forwards_multiple_pages_exactly() {
        let (first, end) = (0, PAGE_SIZE + 100_000);
        for use_pipe in [true, false] {
            let out = run(first, end, use_pipe, false, Fault::None, None).await;
            assert!(out.ok, "pipe={use_pipe}");
            assert!(out.got == object(first, end), "pipe={use_pipe}");
            assert_eq!(out.credits, expected_credits(first, end));
        }
    }

    #[tokio::test]
    async fn forwards_unaligned_range_across_page_boundary() {
        // Starts mid-page with payload already buffered past the header.
        let (first, end) = (PAGE_SIZE - 70_000, PAGE_SIZE + 70_001);
        for use_pipe in [true, false] {
            let out = run(first, end, use_pipe, true, Fault::None, None).await;
            assert!(out.ok, "pipe={use_pipe}");
            assert!(out.got == object(first, end), "pipe={use_pipe}");
            assert_eq!(out.credits, expected_credits(first, end));
            assert_eq!(out.credits[0], credit(0, 70_000));
            assert_eq!(out.credits[1], credit(1, 70_001));
        }
    }

    #[tokio::test]
    async fn forwards_exactly_under_backpressure() {
        let (first, end) = (5, 600_005);
        let out = run(first, end, true, true, Fault::None, Some(4096)).await;
        assert!(out.ok);
        assert!(out.got == object(first, end));
        assert_eq!(out.credits, expected_credits(first, end));
    }

    #[tokio::test]
    async fn fails_when_client_closes() {
        let (ours, theirs) = UnixStream::pair().unwrap();
        let end = 4 << 20;
        let racer = racer(theirs, 0, end, false, Fault::None);
        let mut sub = Subscription::for_test(ours, 0, end, frame(PAGE, 0, 0, end as u32).to_vec());
        let (mut server, client) = tcp_pair(None).await;
        drop(client);
        let pipe = Pipe::new().unwrap();
        let head = sub.next_frame().await.unwrap();
        let ok = forward_pages(
            &mut sub,
            head,
            &mut server,
            &mut Vec::new(),
            Some(&pipe),
            &mut Vec::new(),
        )
        .await;
        assert!(!ok);
        drop(sub);
        // Racer never got the page's credit.
        assert!(racer.await.unwrap().is_empty());
    }

    /// A failed transfer must never deliver the full Content-Length.
    fn assert_incomplete(out: &Outcome, first: u64, end: u64) {
        assert!(!out.ok);
        let want = object(first, end);
        assert!(
            out.got.len() < want.len(),
            "failed transfer looked complete"
        );
        assert!(want.starts_with(&out.got));
    }

    #[tokio::test]
    async fn fails_on_truncated_payload() {
        let (first, end) = (0, PAGE_SIZE + 100_000);
        let last = (end - PAGE_SIZE) as usize;
        for keep in [0, 1, last / 2, last - 1] {
            for use_pipe in [true, false] {
                let out = run(first, end, use_pipe, false, Fault::Truncate(keep), None).await;
                assert_incomplete(&out, first, end);
                assert_eq!(out.credits, expected_credits(first, end)[..1]);
            }
        }
    }

    #[tokio::test]
    async fn fails_on_truncated_single_page() {
        let (first, end) = (0, 200_000);
        for keep in [1000, 150_000, 199_999] {
            let out = run(first, end, true, true, Fault::Truncate(keep), None).await;
            assert_incomplete(&out, first, end);
            assert!(out.credits.is_empty());
        }
    }

    #[tokio::test]
    async fn withholds_final_byte_without_completion() {
        let (first, end) = (0, PAGE_SIZE + 100_000);
        let out = run(first, end, true, false, Fault::NoCompletion, None).await;
        assert_incomplete(&out, first, end);
        assert_eq!(out.got.len() as u64, end - first - 1);
        // The final credit is still sent before waiting for completion.
        assert_eq!(out.credits, expected_credits(first, end));
    }

    #[tokio::test]
    async fn withholds_final_byte_on_invalid_completion() {
        let (first, end) = (0, PAGE_SIZE + 100_000);
        let bytes = end - first;
        for bad in [
            frame(COMPLETE, 1, bytes, 0),
            frame(COMPLETE, 2, bytes - 1, 0),
            frame(COMPLETE, 2, bytes, 1),
            frame(PAGE, 2, bytes, 0),
            frame(9, 2, bytes, 0),
        ] {
            let out = run(first, end, true, false, Fault::Completion(bad), None).await;
            assert_incomplete(&out, first, end);
            assert_eq!(out.got.len() as u64, bytes - 1);
        }
    }

    #[tokio::test]
    async fn fails_on_wrong_page_frame() {
        let (first, end) = (0, PAGE_SIZE + 100_000);
        let len = 100_000;
        for bad in [
            frame(PAGE, 2, PAGE_SIZE, len),
            frame(PAGE, 0, PAGE_SIZE, len),
            frame(PAGE, 1, PAGE_SIZE + 1, len),
            frame(PAGE, 1, PAGE_SIZE, len - 1),
            frame(COMPLETE, 1, PAGE_SIZE, 0),
        ] {
            let out = run(first, end, true, false, Fault::SecondFrame(bad), None).await;
            assert_incomplete(&out, first, end);
            // Only the first page was delivered.
            assert_eq!(out.got.len() as u64, PAGE_SIZE);
        }
    }
}
