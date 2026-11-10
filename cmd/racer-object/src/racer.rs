// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Racer client-socket protocol: metadata `HEAD` and credit-paced
//! subscription `POST` over HTTP/1.1 on a Unix socket.
//!
//! The protocol is the one `pkg/racersdk` and `pkg/racersdk/internal/wire`
//! speak. Errors are mapped straight to sidecar
//! failures with the SDK's classification: transport problems are
//! unavailable (503), protocol violations are bad gateway (502), and Racer
//! status codes keep their meaning.

use crate::object::{Failure, Identity, RangeSpec, fmt_u64, itoa_buf};
use std::io;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::time::{Duration, Instant};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tokio::sync::{OwnedSemaphorePermit, Semaphore};

/// Racer page size; only the last page of an object may be shorter.
pub const PAGE_SIZE: u64 = 16 * 1024 * 1024;
/// Page frame header: kind, number, offset, length.
pub const FRAME_SIZE: usize = 21;
/// Largest response head Racer may send.
const MAX_HEAD: usize = 32 * 1024;
const PAGE_FRAME: u8 = 1;
const COMPLETE_FRAME: u8 = 2;
/// Pages Racer may send ahead of the reader (the SDK default).
const PAGE_CREDITS: u64 = 2;
/// Racer holds an ingress slot per open connection, so only a few idle Stat
/// connections are kept in total, shared by all workers.
const STAT_IDLE: usize = 4;
const QUEUE_TIMEOUT: Duration = Duration::from_secs(5);
const DIAL_TIMEOUT: Duration = Duration::from_secs(5);
const HEADER_TIMEOUT: Duration = Duration::from_secs(60);
/// Bound on each body read, credit write, or splice wait.
pub const BODY_TIMEOUT: Duration = Duration::from_secs(60);
const IDLE_TIMEOUT: Duration = Duration::from_secs(90);
const MAX_CONN_AGE: Duration = Duration::from_secs(300);

/// Object metadata reported by Racer.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Meta {
    pub size: u64,
    pub etag: Vec<u8>,
    pub content_type: Option<Vec<u8>>,
}

struct IdleConn {
    stream: UnixStream,
    expires: Instant,
    idle_since: Instant,
}

/// Racer client shared by all connections.
pub struct Client {
    path: PathBuf,
    idle: Mutex<Vec<IdleConn>>,
    /// Idle connections held by all workers' pools together.
    idle_total: Arc<AtomicUsize>,
    /// Process-wide cap on `idle_total`.
    max_idle: usize,
    stat_lane: Arc<Semaphore>,
    bulk_lane: Arc<Semaphore>,
    jitter: AtomicU64,
}

/// Per-connection scratch buffers so requests do not allocate.
#[derive(Default)]
pub struct Scratch {
    pub request: Vec<u8>,
    pub response: Vec<u8>,
}

impl Client {
    /// `max_connections` bounds concurrent Stats and subscriptions.
    pub fn new(path: PathBuf, max_connections: usize) -> Self {
        let seed = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(1, |d| d.as_nanos() as u64 | 1);
        Self {
            path,
            idle: Mutex::new(Vec::with_capacity(STAT_IDLE)),
            idle_total: Arc::new(AtomicUsize::new(0)),
            max_idle: STAT_IDLE.min(max_connections),
            stat_lane: Arc::new(Semaphore::new(max_connections)),
            bulk_lane: Arc::new(Semaphore::new(max_connections)),
            jitter: AtomicU64::new(seed),
        }
    }

    /// A client for another worker: its own idle pool (no cross-thread
    /// wakeups), sharing this client's read limits and idle budget so they
    /// stay global.
    pub fn sibling(&self) -> Self {
        Self {
            path: self.path.clone(),
            idle: Mutex::new(Vec::with_capacity(self.max_idle)),
            idle_total: self.idle_total.clone(),
            max_idle: self.max_idle,
            stat_lane: self.stat_lane.clone(),
            bulk_lane: self.bulk_lane.clone(),
            jitter: AtomicU64::new(self.jitter.load(Ordering::Relaxed).rotate_left(17) | 1),
        }
    }

    #[cfg(test)]
    fn idle_len(&self) -> usize {
        self.idle.lock().unwrap_or_else(|e| e.into_inner()).len()
    }

    async fn admit(lane: &Arc<Semaphore>) -> Result<OwnedSemaphorePermit, Failure> {
        match tokio::time::timeout(QUEUE_TIMEOUT, lane.clone().acquire_owned()).await {
            Ok(Ok(permit)) => Ok(permit),
            _ => Err(Failure::Unavailable),
        }
    }

    async fn dial(&self) -> Result<UnixStream, Failure> {
        match tokio::time::timeout(DIAL_TIMEOUT, UnixStream::connect(&self.path)).await {
            Ok(Ok(stream)) => Ok(stream),
            _ => Err(Failure::Unavailable),
        }
    }

    /// Rotate pooled connections over 75-100% of the maximum age so Racer
    /// restarts and rebalancing take effect.
    fn expiry(&self, now: Instant) -> Instant {
        let mut x = self.jitter.load(Ordering::Relaxed);
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.jitter.store(x, Ordering::Relaxed);
        let quarter = MAX_CONN_AGE.as_millis() as u64 / 4;
        now + MAX_CONN_AGE - Duration::from_millis(x % (quarter + 1))
    }

    fn pooled(&self) -> Option<(UnixStream, Instant)> {
        let now = Instant::now();
        let mut idle = self.idle.lock().unwrap_or_else(|e| e.into_inner());
        while let Some(conn) = idle.pop() {
            self.idle_total.fetch_sub(1, Ordering::Relaxed);
            if now < conn.expires && now.duration_since(conn.idle_since) < IDLE_TIMEOUT {
                return Some((conn.stream, conn.expires));
            }
        }
        None
    }

    fn recycle(&self, stream: UnixStream, expires: Instant) {
        // Reserve a slot in the process-wide budget; without one the
        // connection is dropped (closed).
        let reserved = self
            .idle_total
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |n| {
                (n < self.max_idle).then_some(n + 1)
            })
            .is_ok();
        if reserved {
            let mut idle = self.idle.lock().unwrap_or_else(|e| e.into_inner());
            idle.push(IdleConn {
                stream,
                expires,
                idle_since: Instant::now(),
            });
        }
    }

    /// Metadata of the current version, over a pooled keep-alive connection.
    pub async fn stat(&self, id: &Identity, scratch: &mut Scratch) -> Result<Meta, Failure> {
        let _permit = Self::admit(&self.stat_lane).await?;
        stat_request(id, &mut scratch.request);

        let mut fresh = false;
        loop {
            let (mut stream, expires, reused) = match (!fresh).then(|| self.pooled()).flatten() {
                Some((stream, expires)) => (stream, expires, true),
                None => (self.dial().await?, self.expiry(Instant::now()), false),
            };
            let exchange = async {
                stream.write_all(&scratch.request).await?;
                read_head(&mut stream, &mut scratch.response).await
            };
            let result = match tokio::time::timeout(HEADER_TIMEOUT, exchange).await {
                Ok(result) => result,
                Err(_) => return Err(Failure::Unavailable),
            };
            let head_len = match result {
                Ok(n) => n,
                // A pooled connection may have been closed by Racer while
                // idle. Retry once on a fresh connection if nothing arrived.
                Err(HeadError::Io(kind, started)) => {
                    if reused && !started && stale(kind) {
                        fresh = true;
                        continue;
                    }
                    return Err(Failure::Unavailable);
                }
                Err(HeadError::Protocol) => return Err(Failure::BadGateway),
            };
            let head = parse_head(&scratch.response[..head_len])?;
            let result = head.stat();
            // Well-formed error responses (404, 412, ...) leave the
            // connection in a clean state; only protocol violations and
            // upstream 5xx gateway failures are not reused.
            if result != Err(Failure::BadGateway)
                && !head.close
                && scratch.response.len() == head_len
            {
                self.recycle(stream, expires);
            }
            return result;
        }
    }

    /// Start a subscription. `pin` fixes the version; the range is forwarded
    /// to Racer in canonical form.
    pub async fn subscribe(
        &self,
        id: &Identity,
        pin: Option<&[u8]>,
        range: RangeSpec,
        scratch: &mut Scratch,
    ) -> Result<Subscription, Failure> {
        let permit = Self::admit(&self.bulk_lane).await?;
        subscribe_request(id, pin, range, &mut scratch.request);

        let mut stream = self.dial().await?;
        // The subscription keeps the head buffer for already-received body
        // bytes; `Subscription::recycle` returns it to the scratch space.
        let mut response = std::mem::take(&mut scratch.response);
        let exchange = async {
            stream.write_all(&scratch.request).await?;
            read_head(&mut stream, &mut response).await
        };
        let head_len = match tokio::time::timeout(HEADER_TIMEOUT, exchange).await {
            Ok(Ok(n)) => n,
            Ok(Err(HeadError::Protocol)) => return Err(Failure::BadGateway),
            Ok(Err(HeadError::Io(..))) | Err(_) => return Err(Failure::Unavailable),
        };
        let head = parse_head(&response[..head_len])?;
        let (meta, first, end) = head.subscription(pin)?;
        let seq = Sequence::new(first, end);
        Ok(Subscription {
            stream,
            buf: response,
            pos: head_len,
            meta,
            first,
            end,
            pages: seq.pages,
            seq,
            _permit: permit,
        })
    }
}

/// Metadata request.
pub fn stat_request(id: &Identity, request: &mut Vec<u8>) {
    request.clear();
    request.extend_from_slice(b"HEAD /v1/objects/");
    request.extend_from_slice(&id.key);
    request.extend_from_slice(b" HTTP/1.1\r\nHost: racer\r\nRacer-Metadata: ");
    request.extend_from_slice(&id.metadata);
    request.extend_from_slice(b"\r\n\r\n");
}

/// Subscription request: initial credits, the canonical range (omitted for
/// a full read), and an optional `If-Match` version pin.
pub fn subscribe_request(
    id: &Identity,
    pin: Option<&[u8]>,
    range: RangeSpec,
    request: &mut Vec<u8>,
) {
    let mut buf = itoa_buf();
    request.clear();
    request.extend_from_slice(b"POST /v1/objects/");
    request.extend_from_slice(&id.key);
    request.extend_from_slice(
        b" HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: ",
    );
    request.extend_from_slice(fmt_u64(PAGE_CREDITS, &mut buf));
    request.extend_from_slice(b"\r\nRacer-Byte-Credits: ");
    request.extend_from_slice(fmt_u64(PAGE_CREDITS * PAGE_SIZE, &mut buf));
    request.extend_from_slice(b"\r\n");
    let at = request.len();
    request.extend_from_slice(b"Range: ");
    if range.racer_header(request) {
        request.extend_from_slice(b"\r\n");
    } else {
        request.truncate(at);
    }
    if let Some(pin) = pin {
        request.extend_from_slice(b"If-Match: ");
        request.extend_from_slice(pin);
        request.extend_from_slice(b"\r\n");
    }
    request.extend_from_slice(b"Racer-Metadata: ");
    request.extend_from_slice(&id.metadata);
    request.extend_from_slice(b"\r\n\r\n");
}

fn stale(kind: io::ErrorKind) -> bool {
    matches!(
        kind,
        io::ErrorKind::UnexpectedEof | io::ErrorKind::ConnectionReset | io::ErrorKind::BrokenPipe
    )
}

enum HeadError {
    /// Transport failure, and whether any response byte arrived.
    Io(io::ErrorKind, bool),
    Protocol,
}

impl From<io::Error> for HeadError {
    fn from(e: io::Error) -> Self {
        Self::Io(e.kind(), false)
    }
}

/// Read one response head into `buf` (cleared first) and return its length.
/// Bytes after the head stay in `buf`: they belong to the body.
async fn read_head(stream: &mut UnixStream, buf: &mut Vec<u8>) -> Result<usize, HeadError> {
    buf.clear();
    let mut scanned: usize = 0;
    loop {
        if buf.capacity() - buf.len() < 1024 {
            buf.reserve(4096);
        }
        let n = stream
            .read_buf(buf)
            .await
            .map_err(|e| HeadError::Io(e.kind(), !buf.is_empty()))?;
        if n == 0 {
            return Err(HeadError::Io(io::ErrorKind::UnexpectedEof, !buf.is_empty()));
        }
        let from = scanned.saturating_sub(3);
        if let Some(i) = buf[from..].windows(4).position(|w| w == b"\r\n\r\n") {
            let len = from + i + 4;
            return if len > MAX_HEAD {
                Err(HeadError::Protocol)
            } else {
                Ok(len)
            };
        }
        scanned = buf.len();
        if scanned > MAX_HEAD {
            return Err(HeadError::Protocol);
        }
    }
}

/// Fields of a Racer response head after raw validation.
#[derive(Default)]
struct Head<'a> {
    status: u16,
    content_length: Option<&'a [u8]>,
    content_type: Option<&'a [u8]>,
    content_range: Option<&'a [u8]>,
    etag: Option<&'a [u8]>,
    expires: Option<&'a [u8]>,
    racer_content_type: Option<&'a [u8]>,
    object_length: Option<&'a [u8]>,
    range_start: Option<&'a [u8]>,
    range_end: Option<&'a [u8]>,
    allow: Option<&'a [u8]>,
    close: bool,
    forbidden: bool,
}

fn parse_head(raw: &[u8]) -> Result<Head<'_>, Failure> {
    let bad = Failure::BadGateway;
    let mut headers = [httparse::EMPTY_HEADER; 64];
    let mut response = httparse::Response::new(&mut headers);
    match response.parse(raw) {
        Ok(httparse::Status::Complete(n)) if n == raw.len() => {}
        _ => return Err(bad),
    }
    if response.version != Some(1) {
        return Err(bad);
    }
    let mut head = Head {
        status: response.code.ok_or(bad)?,
        ..Head::default()
    };
    for h in response.headers.iter() {
        let value = h.value;
        if value.iter().any(|&c| (c < 0x20 && c != b'\t') || c == 0x7f) {
            return Err(bad);
        }
        let name = h.name;
        let slot = if name.eq_ignore_ascii_case("content-length") {
            &mut head.content_length
        } else if name.eq_ignore_ascii_case("content-type") {
            &mut head.content_type
        } else if name.eq_ignore_ascii_case("content-range") {
            &mut head.content_range
        } else if name.eq_ignore_ascii_case("etag") {
            &mut head.etag
        } else if name.eq_ignore_ascii_case("racer-expires-at") {
            &mut head.expires
        } else if name.eq_ignore_ascii_case("racer-content-type") {
            &mut head.racer_content_type
        } else if name.eq_ignore_ascii_case("racer-object-length") {
            &mut head.object_length
        } else if name.eq_ignore_ascii_case("racer-range-start") {
            &mut head.range_start
        } else if name.eq_ignore_ascii_case("racer-range-end") {
            &mut head.range_end
        } else if name.eq_ignore_ascii_case("allow") {
            if head.allow.is_none() {
                head.allow = Some(value);
            }
            continue;
        } else if name.eq_ignore_ascii_case("connection") {
            for token in value.split(|&c| c == b',') {
                let token = token.trim_ascii();
                head.close |= token.eq_ignore_ascii_case(b"close");
                head.forbidden |= token.eq_ignore_ascii_case(b"upgrade");
            }
            continue;
        } else {
            head.forbidden |= [
                "transfer-encoding",
                "content-encoding",
                "trailer",
                "upgrade",
                "expect",
                "if-none-match",
                "if-modified-since",
                "if-unmodified-since",
                "if-range",
            ]
            .iter()
            .any(|f| name.eq_ignore_ascii_case(f));
            // Other singleton fields only matter for duplicate detection.
            continue;
        };
        // Singleton fields must not repeat.
        if slot.replace(value).is_some() {
            return Err(bad);
        }
    }
    if head.forbidden {
        return Err(bad);
    }
    Ok(head)
}

/// Canonical wire decimal: no sign, no leading zeros, at most `i64::MAX`.
pub fn decimal(s: &[u8]) -> Option<u64> {
    if s.is_empty() || s.len() > 19 || (s.len() > 1 && s[0] == b'0') {
        return None;
    }
    let mut n: u64 = 0;
    for &c in s {
        if !c.is_ascii_digit() {
            return None;
        }
        n = n * 10 + u64::from(c - b'0');
    }
    (n <= i64::MAX as u64).then_some(n)
}

/// A strong entity tag as Racer and the SDK require.
pub fn valid_etag(s: &[u8]) -> bool {
    s.len() >= 2
        && s.len() <= 8192
        && s[0] == b'"'
        && s[s.len() - 1] == b'"'
        && s[1..s.len() - 1]
            .iter()
            .all(|&c| c == 0x21 || (0x23..=0x7e).contains(&c))
}

fn token_char(c: u8) -> bool {
    c.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&c)
}

/// Racer content type check: a media type with unique parameters.
pub fn valid_content_type(s: &[u8]) -> bool {
    if s.is_empty() {
        return true;
    }
    if s.len() > 256 || s.trim_ascii() != s || s.iter().any(|&c| !(0x20..=0x7e).contains(&c)) {
        return false;
    }
    let mut rest = s;
    let token = |rest: &mut &[u8]| -> usize {
        let n = rest.iter().take_while(|&&c| token_char(c)).count();
        *rest = &rest[n..];
        n
    };
    let consume = |rest: &mut &[u8], b: u8| -> bool {
        if rest.first() == Some(&b) {
            *rest = &rest[1..];
            true
        } else {
            false
        }
    };
    let spaces = |rest: &mut &[u8]| {
        while rest.first() == Some(&b' ') {
            *rest = &rest[1..];
        }
    };
    if token(&mut rest) == 0 || !consume(&mut rest, b'/') || token(&mut rest) == 0 {
        return false;
    }
    let mut names: Vec<&[u8]> = Vec::new();
    while !rest.is_empty() {
        spaces(&mut rest);
        if !consume(&mut rest, b';') {
            return false;
        }
        spaces(&mut rest);
        let before = rest;
        let n = token(&mut rest);
        if n == 0 {
            return false;
        }
        let name = &before[..n];
        if names.iter().any(|old| old.eq_ignore_ascii_case(name)) {
            return false;
        }
        names.push(name);
        spaces(&mut rest);
        if !consume(&mut rest, b'=') {
            return false;
        }
        spaces(&mut rest);
        if consume(&mut rest, b'"') {
            loop {
                let Some((&b, tail)) = rest.split_first() else {
                    return false;
                };
                rest = tail;
                if b == b'"' {
                    break;
                }
                if b == b'\\' {
                    let Some((_, tail)) = rest.split_first() else {
                        return false;
                    };
                    rest = tail;
                }
            }
        } else if token(&mut rest) == 0 {
            return false;
        }
    }
    true
}

/// Racer status to sidecar failure.
fn status_failure(status: u16) -> Failure {
    match status {
        400 | 405 | 431 => Failure::BadRequest,
        401 | 403 => Failure::Forbidden,
        404 => Failure::NotFound,
        412 => Failure::PreconditionFailed,
        416 => Failure::RangeNotSatisfiable(None),
        503 => Failure::Unavailable,
        _ => Failure::BadGateway,
    }
}

impl Head<'_> {
    fn metadata(&self) -> Result<Meta, Failure> {
        let bad = Failure::BadGateway;
        let etag = self.etag.filter(|t| valid_etag(t)).ok_or(bad)?;
        self.expires.and_then(decimal).ok_or(bad)?;
        let content_type = match self.racer_content_type {
            Some(t) if t.is_empty() || !valid_content_type(t) => return Err(bad),
            other => other.map(<[u8]>::to_vec),
        };
        Ok(Meta {
            size: 0,
            etag: etag.to_vec(),
            content_type,
        })
    }

    /// Validate a metadata response.
    fn stat(&self) -> Result<Meta, Failure> {
        let bad = Failure::BadGateway;
        let length = self.content_length.and_then(decimal).ok_or(bad)?;
        if self.status != 200 && self.status != 206 {
            let failure = status_failure(self.status);
            let known = matches!(
                self.status,
                400 | 401 | 403 | 404 | 405 | 412 | 416 | 431 | 500 | 502 | 503
            );
            if !known
                || length != 0
                || self.etag.is_some()
                || self.expires.is_some()
                || self.racer_content_type.is_some()
            {
                return Err(bad);
            }
            if self.status == 416 {
                let cr = self.content_range.ok_or(bad)?;
                cr.strip_prefix(b"bytes */").and_then(decimal).ok_or(bad)?;
            } else if self.content_range.is_some() {
                return Err(bad);
            }
            if self.status == 405 && self.allow != Some(b"HEAD, POST") {
                return Err(bad);
            }
            return Err(failure);
        }
        if self.status != 200 || self.content_range.is_some() {
            return Err(bad);
        }
        let mut meta = self.metadata()?;
        meta.size = length;
        Ok(meta)
    }

    /// Validate a subscription response and return metadata plus the selected `[first, end)`.
    fn subscription(&self, pin: Option<&[u8]>) -> Result<(Meta, u64, u64), Failure> {
        let bad = Failure::BadGateway;
        let length = self.content_length.and_then(decimal).ok_or(bad)?;
        if self.status != 200 {
            if length != 0 {
                return Err(bad);
            }
            let content_range = self.content_range.filter(|v| !v.is_empty());
            if self.status == 416 {
                let size = content_range
                    .and_then(|v| v.strip_prefix(b"bytes */"))
                    .and_then(decimal)
                    .ok_or(bad)?;
                return Err(Failure::RangeNotSatisfiable(Some(size)));
            } else if content_range.is_some() {
                return Err(bad);
            }
            return Err(status_failure(self.status));
        }
        if !self.close
            || self.content_range.is_some_and(|v| !v.is_empty())
            || self.content_type != Some(b"application/octet-stream")
        {
            return Err(bad);
        }
        let size = self.object_length.and_then(decimal).ok_or(bad)?;
        let first = self.range_start.and_then(decimal).ok_or(bad)?;
        let end = self.range_end.and_then(decimal).ok_or(bad)?;
        let mut meta = self.metadata()?;
        meta.size = size;
        if first > end || end > size || pin.is_some_and(|p| p != meta.etag.as_slice()) {
            return Err(bad);
        }
        let pages = page_count(first, end);
        let framing = (pages + 1) * FRAME_SIZE as u64;
        if length != (end - first).checked_add(framing).ok_or(bad)? {
            return Err(bad);
        }
        Ok((meta, first, end))
    }
}

/// Number of pages covering `[first, end)`.
pub fn page_count(first: u64, end: u64) -> u64 {
    if end <= first {
        0
    } else {
        (end - 1) / PAGE_SIZE - first / PAGE_SIZE + 1
    }
}

/// One page frame header.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Frame {
    pub kind: u8,
    pub number: u64,
    pub offset: u64,
    pub length: u32,
}

impl Frame {
    pub fn decode(b: &[u8; FRAME_SIZE]) -> Self {
        let u64_at = |i: usize| u64::from_be_bytes(b[i..i + 8].try_into().unwrap_or_default());
        Self {
            kind: b[0],
            number: u64_at(1),
            offset: u64_at(9),
            length: u32::from_be_bytes(b[17..21].try_into().unwrap_or_default()),
        }
    }

    pub fn encode(&self) -> [u8; FRAME_SIZE] {
        let mut b = [0; FRAME_SIZE];
        b[0] = self.kind;
        b[1..9].copy_from_slice(&self.number.to_be_bytes());
        b[9..17].copy_from_slice(&self.offset.to_be_bytes());
        b[17..].copy_from_slice(&self.length.to_be_bytes());
        b
    }
}

/// Credit returned for a consumed page: page number and length.
pub fn credit_bytes(f: &Frame) -> [u8; 12] {
    let mut b = [0u8; 12];
    b[..8].copy_from_slice(&f.number.to_be_bytes());
    b[8..].copy_from_slice(&f.length.to_be_bytes());
    b
}

/// Frame validation state for one subscription.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Sequence {
    first: u64,
    end: u64,
    pub pages: u64,
    delivered: u64,
    bytes: u64,
}

impl Sequence {
    /// Expect the pages covering `[first, end)`.
    pub fn new(first: u64, end: u64) -> Self {
        Self {
            first,
            end,
            pages: page_count(first, end),
            delivered: 0,
            bytes: 0,
        }
    }

    /// Validate the next frame header. Page
    /// frames advance the sequence.
    pub fn accept(&mut self, f: &Frame) -> Result<(), Failure> {
        let bad = Failure::BadGateway;
        if f.kind == COMPLETE_FRAME {
            if f.number != self.pages
                || self.delivered != self.pages
                || f.offset != self.end - self.first
                || self.bytes != f.offset
                || f.length != 0
            {
                return Err(bad);
            }
            return Ok(());
        }
        if f.kind != PAGE_FRAME
            || f.length == 0
            || f.offset < self.first
            || f.offset >= self.end
            || f.number != f.offset / PAGE_SIZE
            || self.delivered == self.pages
        {
            return Err(bad);
        }
        let start = self.first.max(f.number * PAGE_SIZE);
        let end = self.end.min((f.number + 1) * PAGE_SIZE);
        if f.offset != start
            || u64::from(f.length) != end - start
            || f.number != self.first / PAGE_SIZE + self.delivered
        {
            return Err(bad);
        }
        self.delivered += 1;
        self.bytes += u64::from(f.length);
        Ok(())
    }

    /// Whether every page was delivered.
    pub fn last_page(&self) -> bool {
        self.delivered == self.pages
    }
}

/// An open subscription. Pages arrive strictly in order; the caller consumes
/// each payload (from `buffered()` first, then the socket) before the next
/// frame, returning a credit after every page but the last.
pub struct Subscription {
    pub stream: UnixStream,
    /// Bytes read past the response head; `buf[pos..]` is unread body.
    buf: Vec<u8>,
    pos: usize,
    pub meta: Meta,
    pub first: u64,
    pub end: u64,
    pub pages: u64,
    seq: Sequence,
    _permit: OwnedSemaphorePermit,
}

/// Map a body read error: EOF and resets are truncation, which the SDK
/// classifies as unavailable; timeouts are too.
fn body_io(_: io::Error) -> Failure {
    Failure::Unavailable
}

impl Subscription {
    pub fn len(&self) -> u64 {
        self.end - self.first
    }

    pub fn is_empty(&self) -> bool {
        self.end == self.first
    }

    /// A subscription over `stream` for `[first, end)` whose response head
    /// was already consumed, with `buffered` body bytes read past it.
    #[cfg(test)]
    pub fn for_test(stream: UnixStream, first: u64, end: u64, buffered: Vec<u8>) -> Self {
        let seq = Sequence::new(first, end);
        let permit = Arc::new(Semaphore::new(1)).try_acquire_owned().unwrap();
        Self {
            stream,
            buf: buffered,
            pos: 0,
            meta: Meta::default(),
            first,
            end,
            pages: seq.pages,
            seq,
            _permit: permit,
        }
    }

    /// Give the head buffer back to the connection's scratch space.
    pub fn recycle(self, scratch: &mut Scratch) {
        let mut buf = self.buf;
        buf.clear();
        scratch.response = buf;
    }

    /// Unread body bytes already buffered in userspace.
    pub fn buffered(&self) -> usize {
        self.buf.len() - self.pos
    }

    /// Copy up to `out.len()` payload bytes, from the buffer first, then
    /// with a single socket read. Returns the number of bytes copied.
    pub async fn read_some(&mut self, out: &mut [u8]) -> Result<usize, Failure> {
        let buffered = self.buffered();
        if buffered > 0 {
            let n = buffered.min(out.len());
            out[..n].copy_from_slice(&self.buf[self.pos..self.pos + n]);
            self.pos += n;
            return Ok(n);
        }
        match tokio::time::timeout(BODY_TIMEOUT, self.stream.read(out)).await {
            Ok(Ok(0)) => Err(Failure::Unavailable),
            Ok(Ok(n)) => Ok(n),
            Ok(Err(e)) => Err(body_io(e)),
            Err(_) => Err(Failure::Unavailable),
        }
    }

    /// Take up to `max` buffered payload bytes without copying.
    pub fn take_buffered(&mut self, max: usize) -> &[u8] {
        let n = self.buffered().min(max);
        let start = self.pos;
        self.pos += n;
        &self.buf[start..start + n]
    }

    pub async fn read_exact(&mut self, mut out: &mut [u8]) -> Result<(), Failure> {
        while !out.is_empty() {
            let n = self.read_some(out).await?;
            out = &mut out[n..];
        }
        Ok(())
    }

    /// Read and validate the next frame header.
    pub async fn next_frame(&mut self) -> Result<Frame, Failure> {
        let mut raw = [0; FRAME_SIZE];
        self.read_exact(&mut raw).await?;
        let f = Frame::decode(&raw);
        self.seq.accept(&f)?;
        Ok(f)
    }

    /// Read the completion frame.
    pub async fn complete(&mut self) -> Result<(), Failure> {
        let f = self.next_frame().await?;
        if f.kind != COMPLETE_FRAME {
            return Err(Failure::BadGateway);
        }
        Ok(())
    }

    /// Whether the most recent page frame was the last.
    pub fn last_page(&self) -> bool {
        self.seq.last_page()
    }

    /// Return a consumed page's credit. Racer may finish and close while a
    /// credit is in flight; the next read reports real truncation, so stale
    /// connection errors are ignored here.
    pub async fn credit(&mut self, f: &Frame) -> Result<(), Failure> {
        let b = credit_bytes(f);
        match tokio::time::timeout(BODY_TIMEOUT, self.stream.write_all(&b)).await {
            Ok(Ok(())) => Ok(()),
            Ok(Err(e)) if stale(e.kind()) => Ok(()),
            _ => Err(Failure::Unavailable),
        }
    }
}

impl Drop for Client {
    /// Release the budget held by connections still in this pool.
    fn drop(&mut self) {
        let n = self.idle.get_mut().unwrap_or_else(|e| e.into_inner()).len();
        self.idle_total.fetch_sub(n, Ordering::Relaxed);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn conn() -> UnixStream {
        UnixStream::pair().unwrap().0
    }

    fn far() -> Instant {
        Instant::now() + MAX_CONN_AGE
    }

    #[tokio::test]
    async fn idle_budget_is_process_wide() {
        let a = Client::new(PathBuf::from("/nonexistent"), 64);
        let workers: Vec<Client> = (0..7).map(|_| a.sibling()).collect();
        for w in std::iter::once(&a).chain(&workers) {
            for _ in 0..3 {
                w.recycle(conn(), far());
            }
        }
        let held: usize = a.idle_len() + workers.iter().map(Client::idle_len).sum::<usize>();
        assert_eq!(held, STAT_IDLE);
        assert_eq!(a.idle_total.load(Ordering::Relaxed), STAT_IDLE);

        // Taking a connection frees budget for another worker.
        let last = workers.iter().rev().find(|w| w.idle_len() > 0).unwrap();
        let other = workers.iter().find(|w| w.idle_len() == 0).unwrap();
        assert!(last.pooled().is_some());
        other.recycle(conn(), far());
        assert_eq!(other.idle_len(), 1);
        other.recycle(conn(), far());
        assert_eq!(other.idle_len(), 1);

        // Dropping a pool releases its budget.
        drop(workers);
        assert_eq!(a.idle_total.load(Ordering::Relaxed), a.idle_len());
    }

    #[tokio::test]
    async fn idle_budget_respects_max_connections() {
        let a = Client::new(PathBuf::from("/nonexistent"), 1);
        let b = a.sibling();
        a.recycle(conn(), far());
        b.recycle(conn(), far());
        assert_eq!(a.idle_len() + b.idle_len(), 1);
    }

    #[tokio::test]
    async fn expired_idle_releases_budget() {
        let a = Client::new(PathBuf::from("/nonexistent"), 64);
        let b = a.sibling();
        for _ in 0..STAT_IDLE {
            a.recycle(conn(), Instant::now());
        }
        assert_eq!(a.idle_total.load(Ordering::Relaxed), STAT_IDLE);
        assert!(a.pooled().is_none());
        assert_eq!(a.idle_total.load(Ordering::Relaxed), 0);
        b.recycle(conn(), far());
        assert_eq!(b.idle_len(), 1);
    }

    async fn serve_one(listener: tokio::net::UnixListener, responses: Vec<&'static [u8]>) {
        use tokio::io::AsyncReadExt;
        let (mut s, _) = listener.accept().await.unwrap();
        for r in responses {
            let mut buf = Vec::new();
            while !buf.ends_with(b"\r\n\r\n") {
                let mut b = [0u8; 1];
                if s.read(&mut b).await.unwrap() == 0 {
                    return;
                }
                buf.push(b[0]);
            }
            s.write_all(r).await.unwrap();
        }
        // Keep the connection open until the client hangs up.
        let mut b = [0u8; 1];
        let _ = s.read(&mut b).await;
    }

    fn sock(name: &str) -> PathBuf {
        let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target");
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join(format!("{name}-{}.sock", std::process::id()));
        let _ = std::fs::remove_file(&p);
        p
    }

    #[tokio::test]
    async fn stat_error_reuses_connection() {
        let path = sock("se");
        let listener = tokio::net::UnixListener::bind(&path).unwrap();
        let not_found: &[u8] = b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n";
        let server = tokio::spawn(serve_one(listener, vec![not_found, not_found]));
        let client = Client::new(path.clone(), 64);
        let mut scratch = Scratch::default();
        assert_eq!(
            client.stat(&id(), &mut scratch).await,
            Err(Failure::NotFound)
        );
        assert_eq!(client.idle_len(), 1);
        // The listener accepts once, so the second stat must reuse it.
        assert_eq!(
            client.stat(&id(), &mut scratch).await,
            Err(Failure::NotFound)
        );
        assert_eq!(client.idle_len(), 1);
        drop(client);
        server.await.unwrap();
        let _ = std::fs::remove_file(&path);
    }

    #[tokio::test]
    async fn stat_bad_error_not_pooled() {
        for (name, resp) in [
            (
                "sb1",
                &b"HTTP/1.1 404 Not Found\r\nContent-Length: 1\r\n\r\nx"[..],
            ),
            (
                "sb2",
                b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
            ),
            (
                "sb3",
                b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n",
            ),
        ] {
            let path = sock(name);
            let listener = tokio::net::UnixListener::bind(&path).unwrap();
            let server = tokio::spawn(serve_one(listener, vec![resp]));
            let client = Client::new(path.clone(), 64);
            let mut scratch = Scratch::default();
            assert!(client.stat(&id(), &mut scratch).await.is_err(), "{name}");
            assert_eq!(client.idle_len(), 0, "{name}");
            drop(client);
            server.await.unwrap();
            let _ = std::fs::remove_file(&path);
        }
    }

    #[test]
    fn decimals() {
        assert_eq!(decimal(b"0"), Some(0));
        assert_eq!(decimal(b"9223372036854775807"), Some(i64::MAX as u64));
        for bad in [
            &b""[..],
            b"01",
            b"+1",
            b"9223372036854775808",
            b"1 ",
            b"99999999999999999999",
        ] {
            assert_eq!(decimal(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn etags() {
        assert!(valid_etag(b"\"v\""));
        assert!(valid_etag(b"\"\""));
        assert!(valid_etag(b"\"a,b\\c\""));
        for bad in [&b"v"[..], b"\"", b"W/\"v\"", b"\"a b\"", b"\"a\"b\""] {
            assert!(!valid_etag(bad), "{bad:?}");
        }
    }

    #[test]
    fn content_types() {
        for good in [
            &b""[..],
            b"text/plain",
            b"text/plain; charset=utf-8",
            b"a/b;x=\"q\\\"\";y=1",
        ] {
            assert!(valid_content_type(good), "{good:?}");
        }
        for bad in [
            &b" text/plain"[..],
            b"text",
            b"text/",
            b"text/plain;",
            b"text/plain; a=1; A=2",
            b"text/plain; a=\"x",
            b"t\x01/x",
        ] {
            assert!(!valid_content_type(bad), "{bad:?}");
        }
    }

    #[test]
    fn pages() {
        assert_eq!(page_count(0, 0), 0);
        assert_eq!(page_count(0, 1), 1);
        assert_eq!(page_count(0, PAGE_SIZE), 1);
        assert_eq!(page_count(PAGE_SIZE - 4, PAGE_SIZE + 7), 2);
        let f = Frame {
            kind: 1,
            number: 3,
            offset: 3 * PAGE_SIZE,
            length: 9,
        };
        assert_eq!(Frame::decode(&f.encode()), f);
    }

    fn head(raw: &str) -> Result<Meta, Failure> {
        parse_head(raw.as_bytes())?.stat()
    }

    #[test]
    fn stat_heads() {
        let ok = head("HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nRacer-Content-Type: text/plain\r\nContent-Length: 10\r\n\r\n").unwrap();
        assert_eq!(ok.size, 10);
        assert_eq!(ok.etag, b"\"v\"");
        assert_eq!(ok.content_type.as_deref(), Some(&b"text/plain"[..]));
        for (status, want) in [
            (400, Failure::BadRequest),
            (401, Failure::Forbidden),
            (403, Failure::Forbidden),
            (404, Failure::NotFound),
            (412, Failure::PreconditionFailed),
            (431, Failure::BadRequest),
            (500, Failure::BadGateway),
            (502, Failure::BadGateway),
            (503, Failure::Unavailable),
            (418, Failure::BadGateway),
        ] {
            let got = head(&format!("HTTP/1.1 {status} X\r\nContent-Length: 0\r\n\r\n"));
            assert_eq!(got, Err(want), "{status}");
        }
        assert_eq!(
            head("HTTP/1.1 405 X\r\nContent-Length: 0\r\nAllow: HEAD, POST\r\n\r\n"),
            Err(Failure::BadRequest)
        );
        for bad in [
            "HTTP/1.1 405 X\r\nContent-Length: 0\r\n\r\n",
            "HTTP/1.1 404 X\r\nContent-Length: 1\r\n\r\n",
            "HTTP/1.1 404 X\r\nContent-Length: 0\r\nETag: \"v\"\r\n\r\n",
            "HTTP/1.1 416 X\r\nContent-Length: 0\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: v\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\nTransfer-Encoding: chunked\r\n\r\n",
            "HTTP/1.0 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 206 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
        ] {
            assert_eq!(head(bad), Err(Failure::BadGateway), "{bad}");
        }
    }

    fn sub(raw: &str, pin: Option<&[u8]>) -> Result<(Meta, u64, u64), Failure> {
        parse_head(raw.as_bytes())?.subscription(pin)
    }

    #[test]
    fn subscription_heads() {
        let ok = "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Type: application/octet-stream\r\nRacer-Object-Length: 10\r\nRacer-Range-Start: 2\r\nRacer-Range-End: 5\r\nContent-Length: 45\r\nConnection: close\r\n\r\n";
        let (meta, first, end) = sub(ok, None).unwrap();
        assert_eq!((meta.size, first, end), (10, 2, 5));
        assert_eq!(sub(ok, Some(b"\"w\"")), Err(Failure::BadGateway));
        assert_eq!(
            sub(
                &ok.replace("Content-Length: 45", "Content-Length: 44"),
                None
            ),
            Err(Failure::BadGateway)
        );
        assert_eq!(
            sub(&ok.replace("Connection: close\r\n", ""), None),
            Err(Failure::BadGateway)
        );
        assert_eq!(
            sub(
                "HTTP/1.1 416 X\r\nContent-Length: 0\r\nContent-Range: bytes */7\r\nConnection: close\r\n\r\n",
                None
            ),
            Err(Failure::RangeNotSatisfiable(Some(7)))
        );
        assert_eq!(
            sub("HTTP/1.1 416 X\r\nContent-Length: 0\r\n\r\n", None),
            Err(Failure::BadGateway)
        );
        assert_eq!(
            sub("HTTP/1.1 412 X\r\nContent-Length: 0\r\n\r\n", None),
            Err(Failure::PreconditionFailed)
        );
        for (from, to) in [
            ("Racer-Range-Start: 2", "Racer-Range-Start: 6"),
            ("Racer-Range-End: 5", "Racer-Range-End: 11"),
            ("Racer-Range-Start: 2", "Racer-Range-Start: 02"),
            ("Racer-Object-Length: 10\r\n", ""),
            ("Racer-Range-End: 5\r\n", ""),
            ("application/octet-stream", "text/plain"),
            ("ETag: \"v\"", "ETag: v"),
            ("Racer-Expires-At: 1\r\n", ""),
            (
                "Connection: close",
                "Connection: close\r\nContent-Range: bytes 2-4/10",
            ),
            (
                "Connection: close",
                "Connection: close\r\nContent-Encoding: gzip",
            ),
        ] {
            assert_eq!(
                sub(&ok.replace(from, to), None),
                Err(Failure::BadGateway),
                "{to}"
            );
        }
        let (meta, ..) = sub(
            &ok.replace("Connection", "Racer-Content-Type: text/plain\r\nConnection"),
            Some(b"\"v\""),
        )
        .unwrap();
        assert_eq!(meta.content_type.as_deref(), Some(&b"text/plain"[..]));
        assert_eq!(
            sub("HTTP/1.1 404 X\r\nContent-Length: 0\r\n\r\n", None),
            Err(Failure::NotFound)
        );
        assert_eq!(
            sub("HTTP/1.1 404 X\r\nContent-Length: 3\r\n\r\n", None),
            Err(Failure::BadGateway)
        );
    }

    fn id() -> Identity {
        crate::object::identity(b"bucket", b"key", b"").unwrap()
    }

    #[test]
    fn stat_requests() {
        let id = id();
        let mut out = b"stale".to_vec();
        stat_request(&id, &mut out);
        let want = format!(
            "HEAD /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nRacer-Metadata: {}\r\n\r\n",
            id.key_str(),
            std::str::from_utf8(&id.metadata).unwrap()
        );
        assert_eq!(String::from_utf8(out).unwrap(), want);
    }

    #[test]
    fn subscribe_requests() {
        let id = id();
        let request = |pin: Option<&[u8]>, range| {
            let mut out = b"stale".to_vec();
            subscribe_request(&id, pin, range, &mut out);
            String::from_utf8(out).unwrap()
        };
        let prefix = format!(
            "POST /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: 2\r\nRacer-Byte-Credits: 33554432\r\n",
            id.key_str()
        );
        let metadata = format!(
            "Racer-Metadata: {}\r\n\r\n",
            std::str::from_utf8(&id.metadata).unwrap()
        );
        assert_eq!(
            request(None, RangeSpec::Full),
            format!("{prefix}{metadata}")
        );
        assert_eq!(
            request(None, RangeSpec::Suffix(3)),
            format!("{prefix}Range: bytes=-3\r\n{metadata}")
        );
        assert_eq!(
            request(Some(b"\"v\""), RangeSpec::Closed(2, 5)),
            format!("{prefix}Range: bytes=2-5\r\nIf-Match: \"v\"\r\n{metadata}")
        );
        assert_eq!(
            request(Some(b"\"v\""), RangeSpec::Full),
            format!("{prefix}If-Match: \"v\"\r\n{metadata}")
        );
    }

    #[test]
    fn credits() {
        let f = Frame {
            kind: PAGE_FRAME,
            number: 0x0102,
            offset: 0,
            length: 0x0a0b0c0d,
        };
        assert_eq!(
            credit_bytes(&f),
            [0, 0, 0, 0, 0, 0, 1, 2, 0x0a, 0x0b, 0x0c, 0x0d]
        );
    }

    fn page(number: u64, offset: u64, length: u32) -> Frame {
        Frame {
            kind: PAGE_FRAME,
            number,
            offset,
            length,
        }
    }

    fn complete(pages: u64, bytes: u64) -> Frame {
        Frame {
            kind: COMPLETE_FRAME,
            number: pages,
            offset: bytes,
            length: 0,
        }
    }

    #[test]
    fn sequence_accepts_valid_streams() {
        // Empty selection: completion only.
        let mut s = Sequence::new(0, 0);
        assert!(s.last_page());
        assert_eq!(s.accept(&complete(0, 0)), Ok(()));
        // A small range inside one page.
        let mut s = Sequence::new(2, 5);
        assert_eq!(s.accept(&page(0, 2, 3)), Ok(()));
        assert!(s.last_page());
        assert_eq!(s.accept(&complete(1, 3)), Ok(()));
        // A small range crossing a page boundary.
        let mut s = Sequence::new(PAGE_SIZE - 2, PAGE_SIZE + 3);
        assert_eq!(s.accept(&page(0, PAGE_SIZE - 2, 2)), Ok(()));
        assert!(!s.last_page());
        assert_eq!(s.accept(&page(1, PAGE_SIZE, 3)), Ok(()));
        assert_eq!(s.accept(&complete(2, 5)), Ok(()));
        // Full pages followed by a short last page.
        let end = 2 * PAGE_SIZE + 1;
        let mut s = Sequence::new(0, end);
        assert_eq!(s.accept(&page(0, 0, PAGE_SIZE as u32)), Ok(()));
        assert_eq!(s.accept(&page(1, PAGE_SIZE, PAGE_SIZE as u32)), Ok(()));
        assert_eq!(s.accept(&page(2, 2 * PAGE_SIZE, 1)), Ok(()));
        assert_eq!(s.accept(&complete(3, end)), Ok(()));
    }

    #[test]
    fn sequence_rejects_invalid_frames() {
        let bad = Err(Failure::BadGateway);
        let fresh = || Sequence::new(PAGE_SIZE - 2, PAGE_SIZE + 3);
        for f in [
            // Unknown kind.
            Frame {
                kind: 3,
                ..page(0, PAGE_SIZE - 2, 2)
            },
            // Empty page.
            page(0, PAGE_SIZE - 2, 0),
            // Wrong offset, length, or number.
            page(0, PAGE_SIZE - 3, 3),
            page(0, PAGE_SIZE - 2, 1),
            page(1, PAGE_SIZE - 2, 2),
            // Out of order: the second page first.
            page(1, PAGE_SIZE, 3),
            // Outside the selection.
            page(1, PAGE_SIZE + 3, 1),
            // Completion before any page (truncated stream).
            complete(2, 5),
            complete(0, 0),
        ] {
            assert_eq!(fresh().accept(&f), bad, "{f:?}");
        }
        // Completion after a partial delivery.
        let mut s = fresh();
        assert_eq!(s.accept(&page(0, PAGE_SIZE - 2, 2)), Ok(()));
        assert_eq!(s.accept(&complete(2, 5)), bad);
        assert_eq!(s.accept(&complete(1, 2)), bad);
        // A repeated page.
        assert_eq!(s.accept(&page(0, PAGE_SIZE - 2, 2)), bad);
        // Extra pages after the last, and a mismatched completion.
        assert_eq!(s.accept(&page(1, PAGE_SIZE, 3)), Ok(()));
        assert_eq!(s.accept(&page(1, PAGE_SIZE, 3)), bad);
        assert_eq!(s.accept(&complete(2, 4)), bad);
        assert_eq!(s.accept(&complete(3, 5)), bad);
        assert_eq!(
            s.accept(&Frame {
                length: 1,
                ..complete(2, 5)
            }),
            bad
        );
        assert_eq!(s.accept(&complete(2, 5)), Ok(()));
    }

    async fn read(chunks: &[&[u8]], close: bool) -> (Result<usize, HeadError>, Vec<u8>) {
        let (mut ours, mut theirs) = UnixStream::pair().unwrap();
        let chunks: Vec<Vec<u8>> = chunks.iter().map(|c| c.to_vec()).collect();
        let writer = tokio::spawn(async move {
            for c in chunks {
                theirs.write_all(&c).await.unwrap();
                tokio::task::yield_now().await;
            }
            if !close {
                std::future::pending::<()>().await;
            }
        });
        let mut buf = b"stale".to_vec();
        let result = read_head(&mut ours, &mut buf).await;
        writer.abort();
        (result, buf)
    }

    #[tokio::test]
    async fn response_heads() {
        // Split across reads, including inside the terminator; body bytes
        // that arrive with the head stay buffered.
        let (r, buf) = read(&[b"HTTP/1.1 200 OK\r\nA: b\r", b"\n\r", b"\nbody"], false).await;
        let len = r.ok().unwrap();
        assert_eq!(&buf[..len], b"HTTP/1.1 200 OK\r\nA: b\r\n\r\n");
        assert_eq!(&buf[len..], b"body");
        // EOF before anything arrived, and after a partial head.
        let (r, _) = read(&[], true).await;
        assert!(matches!(
            r,
            Err(HeadError::Io(io::ErrorKind::UnexpectedEof, false))
        ));
        let (r, _) = read(&[b"HTTP/1.1 200"], true).await;
        assert!(matches!(
            r,
            Err(HeadError::Io(io::ErrorKind::UnexpectedEof, true))
        ));
        // Oversized heads, with and without a terminator.
        let filler = format!("X: {}\r\n", "a".repeat(MAX_HEAD));
        let (r, _) = read(&[b"HTTP/1.1 200 OK\r\n", filler.as_bytes(), b"\r\n"], true).await;
        assert!(matches!(r, Err(HeadError::Protocol)));
        let (r, _) = read(&[b"HTTP/1.1 200 OK\r\n", filler.as_bytes()], false).await;
        assert!(matches!(r, Err(HeadError::Protocol)));
    }
}
