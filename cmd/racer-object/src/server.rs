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
use crate::racer::{Client, Frame, Scratch, Subscription};
use crate::splice::{Pipe, forward, send_more};
use std::io::IoSlice;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc, watch};

/// Largest accepted request head. Go's net/http allows MaxHeaderBytes
/// (32 KiB) plus 4 KiB of slack; matching it keeps every request the Go
/// sidecar accepted working.
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
                // Like Go's limited listener: take a slot before accepting,
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
                // Like Go's ErrAbortHandler: close without completing the
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
        // Go uses the header timeout for the first request and the idle
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

    /// Parse the buffered head into connection facts. `None` is a 400.
    fn parse(&self) -> Option<Request> {
        let mut headers = [httparse::EMPTY_HEADER; MAX_HEADERS];
        let mut req = httparse::Request::new(&mut headers);
        let head_len = match req.parse(&self.rbuf) {
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
        let plan = method_ok.then(|| plan(self.shared.api, &req, head));
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
        while !slices.is_empty() {
            match self.stream.write_vectored(slices).await {
                Ok(0) | Err(_) => return Next::Abort,
                Ok(n) => IoSlice::advance_slices(&mut slices, n),
            }
        }
        next
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
            // net/http answers `OPTIONS *` itself before any handler.
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
            let sub = client
                .subscribe(&plan.id, None, range, &mut self.scratch)
                .await?;
            let selection = range
                .resolve(sub.meta.size)
                .map_err(|_| Failure::BadGateway)?;
            if selection.offset != sub.first || selection.offset + selection.length != sub.end {
                return Err(Failure::BadGateway);
            }
            return self
                .stream_body(sub, &plan, selection, connection, next)
                .await;
        }
        let meta = client.stat(&plan.id, &mut self.scratch).await?;
        if plan.if_match.present && !plan.if_match.matches(&meta.etag, false) {
            return Err(Failure::PreconditionFailed);
        }
        if plan.if_none.present && plan.if_none.matches(&meta.etag, true) {
            let mut h = HeadBuilder::new(&mut self.out, 304);
            h.header("Etag", &meta.etag);
            if self.shared.api == Api::Azure {
                h.header("X-Ms-Version", AZURE_VERSION.as_bytes());
                h.azure_object(&plan.version);
            }
            h.finish(connection);
            self.body.clear();
            return Ok(self.write_response(next).await);
        }
        let selection = plan.range?.resolve(meta.size)?;
        if head {
            self.success_head(
                &plan,
                &meta.etag,
                meta.content_type.as_deref(),
                selection,
                connection,
            );
            self.body.clear();
            return Ok(self.write_response(next).await);
        }
        // Pin the version Stat selected, with the exact resolved range.
        let range = if selection.offset != 0 || selection.length != 0 {
            RangeSpec::Closed(selection.offset, selection.offset + selection.length - 1)
        } else {
            RangeSpec::Full
        };
        let sub = client
            .subscribe(&plan.id, Some(&meta.etag), range, &mut self.scratch)
            .await
            .map_err(|f| match f {
                // Go reports a pinned 416 without Content-Range.
                Failure::RangeNotSatisfiable(_) => Failure::RangeNotSatisfiable(None),
                other => other,
            })?;
        // An ETag pin must not change the headers selected by Stat.
        if sub.meta.size != meta.size || sub.meta.content_type != meta.content_type {
            return Err(Failure::BadGateway);
        }
        if sub.first != selection.offset || sub.len() != selection.length {
            // Go's SDK reports a short tail as an unsatisfiable range.
            if sub.end == sub.meta.size && selection.length > sub.len() {
                return Err(Failure::RangeNotSatisfiable(None));
            }
            return Err(Failure::BadGateway);
        }
        self.stream_body(sub, &plan, selection, connection, next)
            .await
    }

    /// Success head (Go `sidecarRead.writeHeaders`) without the final CRLF.
    fn success_head(
        &mut self,
        plan: &Plan,
        etag: &[u8],
        content_type: Option<&[u8]>,
        selection: Selection,
        connection: Connection,
    ) {
        let status = if selection.partial { 206 } else { 200 };
        let mut h = HeadBuilder::new(&mut self.out, status);
        h.header("Etag", etag);
        h.header("Accept-Ranges", b"bytes");
        h.number("Content-Length", selection.length);
        h.header(
            "Content-Type",
            content_type.unwrap_or(b"application/octet-stream"),
        );
        match self.shared.api {
            Api::Azure => {
                h.header("X-Ms-Version", AZURE_VERSION.as_bytes());
                h.azure_object(&plan.version);
            }
            Api::S3 if !plan.version.is_empty() => {
                h.header("X-Amz-Version-Id", &plan.version);
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
            read_all(&mut sub, &mut self.body).await?;
            sub.recycle(&mut self.scratch);
            self.success_head(plan, &etag, content_type.as_deref(), selection, connection);
            return Ok(self.write_response(next).await);
        }
        // Validate the first frame before committing so a broken stream
        // still gets a proper error status.
        let first = sub.next_frame().await?;
        self.success_head(plan, &etag, content_type.as_deref(), selection, connection);
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

    /// Forward every page, withholding the final byte until Racer confirms
    /// completion so truncation is never mistaken for a complete body.
    async fn forward_pages(&mut self, sub: &mut Subscription, first: Frame) -> bool {
        let mut pending = Some(first);
        loop {
            let frame = match pending.take() {
                Some(frame) => frame,
                None => match sub.next_frame().await {
                    Ok(frame) => frame,
                    Err(_) => return false,
                },
            };
            let last = sub.last_page();
            let mut remaining = u64::from(frame.length) - u64::from(last);
            // Bytes that arrived with the frame header are written from
            // userspace; the rest is spliced.
            if sub.buffered() > 0 && remaining > 0 {
                let chunk = sub.take_buffered(remaining as usize);
                remaining -= chunk.len() as u64;
                if write_all_more(&self.stream, chunk).await.is_err() {
                    return false;
                }
            }
            if remaining > 0 {
                let ok = match &self.pipe {
                    Some(pipe) => forward(&sub.stream, pipe, &self.stream, remaining, true)
                        .await
                        .is_ok(),
                    None => copy(sub, &self.stream, remaining, &mut self.body).await,
                };
                if !ok {
                    return false;
                }
            }
            if !last {
                if sub.credit(&frame).await.is_err() {
                    return false;
                }
                continue;
            }
            let mut final_byte = [0u8; 1];
            if sub.read_exact(&mut final_byte).await.is_err() {
                return false;
            }
            // Racer may wait for the final credit before completing, or
            // complete without reading it; a 12-byte write never blocks on
            // an open socket, so sending it first cannot deadlock.
            if sub.credit(&frame).await.is_err() || sub.complete().await.is_err() {
                return false;
            }
            return self.stream.write_all(&final_byte).await.is_ok();
        }
    }
}

/// Everything decided from the request alone (Go `sidecar.request` plus
/// condition and range parsing).
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
async fn copy(sub: &mut Subscription, dst: &TcpStream, mut len: u64, buf: &mut Vec<u8>) -> bool {
    buf.resize(256 << 10, 0);
    while len > 0 {
        let want = (len as usize).min(buf.len());
        let Ok(n) = sub.read_some(&mut buf[..want]).await else {
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
