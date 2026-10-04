//! Validate v2 subscriptions and HEAD metadata while preserving opaque fields.
//!
//! Wire values follow pkg/racersdk: canonical lowercase keys, quoted strong pins,
//! signed-63-bit decimal ranges, and byte-preserving opaque context.
pub mod listener;

use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::ConnectionLease;
use crate::security::Authorization;
use crate::model::ByteRange;
use crate::model::CacheId;
use crate::model::CacheKey;
use crate::model::ObjectId;
use crate::security::OpaqueMetadata;
use crate::security::OriginContext;
use crate::model::PAGE_BYTES;
use crate::model::StrongEtag;
use crate::read::ReadResponse;
use crate::runtime::HashSet;

use crate::admission::AdmissionPolicy;
use crate::http::Delivery;
use crate::http::HttpIo;
use crate::http::OwnedBuffer;
use crate::http::ReaderLease;
use crate::model::ObjectMetadata;
use crate::model::PageNumber;
use crate::model::ResolvedRange;
use crate::read::range_stream::RangeStream;
use crate::runtime::RequestScope;
use crate::telemetry::Observer;
use std::collections::BTreeMap;
use std::rc::Rc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use std::time::UNIX_EPOCH;
use uring_runtime::reactor::IoBuffer;

use crate::http::MAX_HEAD_BYTES;
use crate::model::MAX_FIELD_BYTES;
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use http1::is_token;
use http1::trim_ows;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReadKind {
    Head,
    HeadPinned {
        etag: StrongEtag,
    },
    Subscription {
        pin: Option<StrongEtag>,
        range: Option<ByteRange>,
        page_credits: usize,
        byte_credits: u64,
        ordered: bool,
    },
}

impl ReadKind {
    pub fn is_head(&self) -> bool {
        matches!(self, Self::Head | Self::HeadPinned { .. })
    }

    pub fn pin(&self) -> Option<&StrongEtag> {
        match self {
            Self::HeadPinned { etag } => Some(etag),
            Self::Subscription { pin, .. } => pin.as_ref(),
            _ => None,
        }
    }
}

pub struct ClientRequest {
    pub kind: ReadKind,
    pub origin: OriginContext,
}

#[derive(Clone)]
pub struct RequestParser {
    header_limit: usize,
}
impl RequestParser {
    pub fn new(header_limit: usize) -> Self {
        Self {
            header_limit: header_limit.min(MAX_HEAD_BYTES),
        }
    }
    /// Apply this cap to raw HTTP framing before calling the semantic parser.
    pub(crate) fn header_limit(&self) -> usize {
        self.header_limit
    }
    /// Codec must validate the raw head and its byte limit before calling this.
    /// In particular, context fields must have exactly one separator space and
    /// must not have their value trimmed by the codec. Decoded fields cannot
    /// reconstruct wire length: unknown fields need not have a separator SP.
    pub fn parse(&self, cache: &CacheId, head: MessageHead) -> Result<ClientRequest> {
        let StartLine::Request { method, target } = head.start else {
            return Err(Error::InvalidRequest.into());
        };
        // Bound manually constructed input data as well, without pretending this
        // is a wire-length check. Framing owns start-line/colon/OWS/CRLF accounting.
        let mut decoded_bytes = method.len().saturating_add(target.len());
        let mut seen = HashSet::default();
        let mut host = None;
        let mut pin = None;
        let mut range = None;
        let mut metadata = None;
        let mut authorization = None;
        let mut content_length = false;
        let mut page_credits = 2;
        let mut byte_credits = 2 * PAGE_BYTES;
        let mut ordered = false;
        for header in head.headers {
            decoded_bytes = decoded_bytes
                .saturating_add(header.name.len())
                .saturating_add(header.value.len());
            if decoded_bytes > self.header_limit {
                return Err(Error::HeaderTooLarge);
            }
            if header.name.is_empty()
                || !header.name.bytes().all(is_token)
                || header
                    .value
                    .iter()
                    .any(|&b| b == 0x7f || (b < 0x20 && b != b'\t'))
            {
                return Err(Error::InvalidRequest.into());
            }
            let name = header.name.to_ascii_lowercase();
            let value = header.value.as_slice();
            if matches!(
                name.as_str(),
                "host"
                    | "content-length"
                    | "content-type"
                    | "content-range"
                    | "etag"
                    | "if-match"
                    | "range"
                    | "racer-expires-at"
                    | "racer-content-type"
                    | "racer-metadata"
                    | "authorization"
                    | "racer-page-credits"
                    | "racer-byte-credits"
                    | "racer-ordered"
            ) && !seen.insert(name.clone())
            {
                return Err(Error::InvalidRequest.into());
            }
            match name.as_str() {
                "host" => host = Some(value == b"racer"),
                "content-length" if value == b"0" => content_length = true,
                "content-length"
                | "content-range"
                | "etag"
                | "racer-expires-at"
                | "racer-content-type"
                | "transfer-encoding"
                | "content-encoding"
                | "trailer"
                | "upgrade"
                | "expect"
                | "if-none-match"
                | "if-modified-since"
                | "if-unmodified-since"
                | "if-range" => return Err(Error::InvalidRequest.into()),
                "connection"
                    if value
                        .split(|&b| b == b',')
                        .any(|token| trim_ows(token).eq_ignore_ascii_case(b"upgrade")) =>
                {
                    return Err(Error::InvalidRequest.into());
                }
                "if-match" => {
                    if value.len() > MAX_FIELD_BYTES {
                        return Err(Error::HeaderTooLarge);
                    }
                    pin = Some(StrongEtag::parse(value)?);
                }
                "range" => range = Some(ByteRange::parse(value)?),
                "racer-page-credits" => page_credits = decimal(value, 1, 64)? as usize,
                "racer-byte-credits" => {
                    byte_credits = decimal(value, PAGE_BYTES, 64 * PAGE_BYTES)?;
                }
                "racer-ordered" => {
                    ordered = match value {
                        b"0" => false,
                        b"1" => true,
                        _ => return Err(Error::InvalidRequest.into()),
                    }
                }
                "racer-metadata" => {
                    validate_opaque(value)?;
                    metadata = Some(OpaqueMetadata::from_header(value)?);
                }
                "authorization" => {
                    validate_opaque(value)?;
                    authorization = Some(Authorization::from_header(value)?);
                }
                _ => {}
            }
        }
        if decoded_bytes > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        if host != Some(true) {
            return Err(Error::InvalidRequest.into());
        }
        let key = CacheKey::parse_hex(
            target
                .strip_prefix("/v2/objects/")
                .ok_or(Error::InvalidRequest)?
                .as_bytes(),
        )?;
        let kind = match method.as_str() {
            "HEAD" if range.is_none() => match pin {
                Some(etag) => ReadKind::HeadPinned { etag },
                None => ReadKind::Head,
            },
            "HEAD" => return Err(Error::InvalidRequest.into()),
            "POST" if content_length => ReadKind::Subscription {
                pin,
                range,
                page_credits,
                byte_credits,
                ordered,
            },
            "POST" => return Err(Error::InvalidRequest.into()),
            _ => return Err(Error::MethodNotAllowed),
        };
        Ok(ClientRequest {
            kind,
            origin: OriginContext {
                object: ObjectId {
                    cache: cache.clone(),
                    key,
                },
                metadata,
                authorization,
            },
        })
    }
}

fn decimal(value: &[u8], minimum: u64, maximum: u64) -> Result<u64> {
    if value.is_empty() || value[0] == b'0' || !value.iter().all(u8::is_ascii_digit) {
        return Err(Error::InvalidRequest);
    }
    let number = value
        .iter()
        .try_fold(0u64, |n, digit| {
            n.checked_mul(10)?.checked_add(u64::from(digit - b'0'))
        })
        .ok_or(Error::InvalidRequest)?;
    if !(minimum..=maximum).contains(&number) {
        return Err(Error::InvalidRequest);
    }
    Ok(number)
}

fn validate_opaque(value: &[u8]) -> Result<()> {
    if value.len() > MAX_FIELD_BYTES {
        return Err(Error::HeaderTooLarge);
    }
    if value.is_empty()
        || value.first() == Some(&b' ')
        || value.last() == Some(&b' ')
        || value.iter().any(|&b| b < 0x20 || b == 0x7f)
    {
        return Err(Error::InvalidRequest.into());
    }
    Ok(())
}

/// Validate a completed read before committing success headers. Keep this boundary
/// independent of acquisition so canceled or inconsistent successes fail closed.
#[allow(clippy::too_many_arguments)]
async fn handle_read_result(
    connection: ConnectionLease,
    kind: &ReadKind,
    object: &ObjectId,
    read_result: Result<ReadResponse>,
    responses: &Responses,
    admission: &flow_control::Quotas<AdmissionPolicy>,
    scope: &RequestScope,
    observation: &mut crate::telemetry::RequestMetrics,
    timeout: Duration,
) -> Result<Option<ConnectionLease>> {
    let response = match read_result {
        Ok(response) => response,
        Err(error) => {
            admission.policy().observer().record(
                crate::telemetry::Failure::new(crate::telemetry::Stage::ClientRead, error)
                    .request(scope),
            );
            let error = if error == Error::NotFound && kind.pin().is_some() {
                Error::VersionUnavailable
            } else {
                error
            };
            observation.fail(error);
            responses.send_error(connection, error, scope).await?;
            return Ok(None);
        }
    };
    if let Err(error) = scope.check() {
        observation.fail(error);
        responses.send_error(connection, error, scope).await?;
        return Ok(None);
    }
    if &response.metadata.version.object != object {
        responses
            .send_error(connection, Error::BadGateway, scope)
            .await?;
        return Ok(None);
    }
    if let Err(error) = responses.validate(kind, &response) {
        observation.fail(error);
        responses.send_error(connection, error, scope).await?;
        return Ok(None);
    }
    let socket = connection.socket();
    let mut send = if matches!(kind, ReadKind::Subscription { .. }) {
        responses.send_subscription(connection, response, scope, observation, timeout)
    } else {
        responses.send_observed(connection, response, scope, observation)
    };
    let result = std::future::poll_fn(|cx| {
        if socket.peer_disconnected() {
            let _ = scope.cancel();
        }
        send.as_mut().poll(cx)
    })
    .await;
    drop(send);
    drop(socket);
    match result {
        Ok(connection) => Ok(Some(connection)),
        Err(error) => {
            observation.fail(error);
            Err(error)
        }
    }
}

pub struct Responses {
    observer: Observer,
    io: Rc<HttpIo>,
    delivery: Rc<Delivery>,
}
/// Duplex subscription framing. Delivered leases stay pinned until exact release.
fn subscription_head(
    metadata: &ObjectMetadata,
    range: Option<ResolvedRange>,
) -> Result<MessageHead> {
    let mut head = success_head(metadata, None)?;
    head.headers
        .retain(|h| !h.name.eq_ignore_ascii_case("Content-Length"));
    let (start, end, pages) = match range {
        Some(range) if range.end() <= metadata.length => (
            range.start(),
            range.end(),
            range.last_page().0 - range.first_page().0 + 1,
        ),
        None if metadata.length == 0 => (0, 0, 0),
        _ => return Err(Error::BadGateway),
    };
    let length = pages
        .checked_add(1)
        .and_then(|n| n.checked_mul(21))
        .and_then(|n| n.checked_add(end - start))
        .ok_or(Error::BadGateway)?;
    head.start = StartLine::Response { status: 200 };
    head.headers.extend([
        header("Racer-Object-Length", metadata.length.to_string()),
        header("Racer-Range-Start", start.to_string()),
        header("Racer-Range-End", end.to_string()),
        header("Content-Length", length.to_string()),
        header("Connection", "close"),
    ]);
    Ok(head)
}
fn frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; 21] {
    let mut bytes = [0; 21];
    bytes[0] = kind;
    bytes[1..9].copy_from_slice(&number.to_be_bytes());
    bytes[9..17].copy_from_slice(&offset.to_be_bytes());
    bytes[17..].copy_from_slice(&length.to_be_bytes());
    bytes
}
// A partial release is retained across polls without an unbounded input queue.
struct Releases {
    bytes: [u8; 12],
    used: usize,
    ahead: Option<(OwnedBuffer, std::ops::Range<usize>)>,
    provisional: bool,
}
impl Releases {
    fn poll(
        &mut self,
        cx: &mut Context<'_>,
        socket: &Rc<uring_runtime::reactor::Descriptor>,
        stream: &mut RangeStream,
        outstanding: &mut BTreeMap<PageNumber, u32>,
        current: Option<(crate::model::PageSlice, &crate::http::FinalSend)>,
    ) -> Result<bool> {
        let mut released = false;
        // At most 64 releases per turn, even for malicious input.
        for _ in 0..64 {
            let count = if let Some((buffer, available)) = self.ahead.as_mut() {
                let count = (12 - self.used).min(available.len());
                self.bytes[self.used..self.used + count]
                    .copy_from_slice(&buffer.bytes()?[available.start..available.start + count]);
                available.start += count;
                if available.start == available.end {
                    self.ahead = None;
                }
                count
            } else {
                match socket.try_recv(&mut self.bytes[self.used..]) {
                    Ok(0) => return Err(Error::Io),
                    Ok(count) => count,
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        return Ok(released);
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::Interrupted => {
                        cx.waker().wake_by_ref();
                        return Ok(released);
                    }
                    Err(_) => return Err(Error::Io),
                }
            };
            self.used += count;
            if self.used == 12 {
                let number = PageNumber(u64::from_be_bytes(self.bytes[..8].try_into().unwrap()));
                let length = u32::from_be_bytes(self.bytes[8..].try_into().unwrap());
                if let Some(expected) = outstanding.get(&number) {
                    if *expected != length {
                        return Err(Error::InvalidRequest);
                    }
                    stream.release_page(number, length)?;
                    outstanding.remove(&number);
                    released = true;
                } else if let Some((slice, state)) = current {
                    if slice.page != number || slice.length != length || self.provisional {
                        return Err(Error::InvalidRequest);
                    }
                    state.provisional_release()?;
                    self.provisional = true;
                } else {
                    return Err(Error::InvalidRequest);
                }
                self.used = 0;
            }
        }
        cx.waker().wake_by_ref();
        Ok(released)
    }
}
async fn send_frame(
    io: &HttpIo,
    connection: ConnectionLease,
    bytes: [u8; 21],
    scope: &RequestScope,
) -> Result<ConnectionLease> {
    let mut buffer = io.buffer(21)?;
    buffer.bytes_mut()?.copy_from_slice(&bytes);
    Ok(io.write_body(connection, buffer, scope).await?.lease)
}
fn poll_slice_or_readiness(
    cx: &mut Context<'_>,
    slice: impl FnOnce(&mut Context<'_>) -> Poll<Result<Option<ReaderLease>>>,
    readiness: impl FnOnce(&mut Context<'_>) -> Poll<Result<u32>>,
) -> Poll<Result<Option<ReaderLease>>> {
    // A ready page needs no release-readiness submission. Do not let auxiliary
    // admission discard it. If acquisition needs to wait, readiness still owns
    // the release wakeup and its errors remain terminal, without a busy retry.
    if let Poll::Ready(result) = slice(cx) {
        return Poll::Ready(result);
    }
    if let Poll::Ready(result) = readiness(cx) {
        result?;
        cx.waker().wake_by_ref();
    }
    Poll::Pending
}
impl Responses {
    /// Send a validated client subscription, including page and completion frames.
    /// Unlike `send`, this keeps page leases pinned until exact duplex releases.
    pub fn send_subscription_unobserved<'a>(
        &'a self,
        connection: ConnectionLease,
        response: ReadResponse,
        scope: &'a RequestScope,
        timeout: Duration,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            let metrics = crate::telemetry::Metrics::default();
            let mut observation = metrics.request()?;
            self.send_subscription(connection, response, scope, &mut observation, timeout)
                .await
        })
    }
    pub(crate) fn send_subscription<'a>(
        &'a self,
        mut connection: ConnectionLease,
        mut response: ReadResponse,
        scope: &'a RequestScope,
        observation: &'a mut crate::telemetry::RequestMetrics,
        timeout: Duration,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            let head = subscription_head(&response.metadata, response.range)?;
            let expected = response.range.map_or(0, |r| r.len());
            let mut first = if let Some(stream) = response.body.as_mut() {
                match stream.next_slice().await {
                    Ok(Some(reader)) => Some(reader),
                    result => {
                        let error = result.err().unwrap_or(Error::BadGateway);
                        self.observer.record(
                            crate::telemetry::Failure::new(
                                crate::telemetry::Stage::FirstSlice,
                                error,
                            )
                            .request(scope),
                        );
                        observation.fail(error);
                        return self.send_error(connection, error, scope).await;
                    }
                }
            } else {
                None
            };
            connection.poison();
            connection = self.io.send_head(connection, head, scope).await?.connection;
            let socket = connection.socket();
            let reservation = connection.slot().cloned();
            let mut releases = Releases {
                bytes: [0; 12],
                used: 0,
                ahead: connection.take_read_ahead(),
                provisional: false,
            };
            let mut outstanding = BTreeMap::new();
            let mut sent = 0;
            let mut pages = 0;
            if let Some(stream) = response.body.as_mut() {
                stream.enable_progress(timeout);
                while sent < expected {
                    let mut progress_scope = scope.clone();
                    progress_scope.deadline.0 = uring_runtime::environment::now() + timeout;
                    let reader = if let Some(reader) = first.take() {
                        reader
                    } else {
                        // Pending acquisition and pipe admission live in the stream.
                        // Drop only the borrowing future so duplex release_page can
                        // update credits without discarding the pipe waiter's wake.
                        let mut ready = None;
                        std::future::poll_fn(|cx| {
                            if let Err(error) = progress_scope.check() {
                                return Poll::Ready(Err(error));
                            }
                            if let Err(error) =
                                releases.poll(cx, &socket, stream, &mut outstanding, None)
                            {
                                return Poll::Ready(Err(error));
                            }
                            poll_slice_or_readiness(
                                cx,
                                |cx| stream.next_slice().as_mut().poll(cx),
                                |cx| {
                                    if ready.is_none() {
                                        ready = Some(self.io.reactor().readiness_with_lease(
                                            socket.clone(),
                                            libc::POLLIN as u32,
                                            reservation.clone(),
                                            &progress_scope,
                                        ));
                                    }
                                    let result = ready.as_mut().unwrap().as_mut().poll(cx);
                                    if result.is_ready() {
                                        ready = None;
                                    }
                                    result
                                },
                            )
                        })
                        .await
                        .inspect_err(|&error| {
                            self.observer.record(
                                crate::telemetry::Failure::new(
                                    crate::telemetry::Stage::NextSlice,
                                    error,
                                )
                                .request(scope)
                                .detail(crate::telemetry::Detail::Delivery { sent, expected }),
                            );
                        })?
                        .ok_or(Error::BadGateway)?
                    };
                    let slice = reader.slice();
                    let range = response.range.ok_or(Error::BadGateway)?;
                    if range.slice_at(slice.page)? != Some(slice)
                        || reader.bytes_sent() != 0
                        || slice.length == 0
                        || u64::from(slice.length) > expected - sent
                        || outstanding.contains_key(&slice.page)
                    {
                        return Err(Error::BadGateway);
                    }
                    let offset = slice
                        .page
                        .0
                        .checked_mul(PAGE_BYTES)
                        .and_then(|n| n.checked_add(u64::from(slice.offset)))
                        .ok_or(Error::BadGateway)?;
                    // Acquisition owns no delivery pipe and continues even while
                    // the page frame or payload is blocked on this socket.
                    let final_send = crate::http::FinalSend::default();
                    let mut write = Box::pin(async {
                        let connection = send_frame(
                            &self.io,
                            connection,
                            frame(1, slice.page.0, offset, slice.length),
                            &progress_scope,
                        )
                        .await?;
                        self.delivery
                            .finish_subscription(reader, connection, &progress_scope, &final_send)
                            .await
                    });
                    let mut ready: Option<Operation<'_, u32>> = None;
                    let result = std::future::poll_fn(|cx| {
                        // A peer can see the final bytes before we observe their
                        // CQE. Retain one exact provisional release, without
                        // releasing credit or any I/O owner before full completion.
                        if let Poll::Ready(result) = write.as_mut().poll(cx) {
                            return Poll::Ready(result);
                        }
                        if let Err(error) = releases.poll(
                            cx,
                            &socket,
                            stream,
                            &mut outstanding,
                            Some((slice, &final_send)),
                        ) {
                            return Poll::Ready(Err(error));
                        }
                        // No release can replenish credit until a previous page
                        // has completed. Avoid consuming a reactor slot solely
                        // to watch input when there is nothing releasable.
                        if ready.is_none() && !outstanding.is_empty() {
                            let reactor = self.io.reactor().clone();
                            let socket = socket.clone();
                            let reservation = reservation.clone();
                            let mut receive_scope = scope.clone();
                            receive_scope.deadline.0 = uring_runtime::environment::now() + timeout;
                            ready = Some(Box::pin(async move {
                                reactor
                                    .readiness_with_lease(
                                        socket,
                                        libc::POLLIN as u32,
                                        reservation,
                                        &receive_scope,
                                    )
                                    .await
                            }));
                        }
                        if let Some(wait) = ready.as_mut()
                            && let Poll::Ready(result) = wait.as_mut().poll(cx)
                        {
                            ready = None;
                            // Payload delivery owns its progress-based stall
                            // deadline. An idle release direction must not turn
                            // that into an absolute page-duration limit.
                            if let Err(error) = result {
                                // Read readiness is auxiliary: a full reactor
                                // must leave the already admitted write alive.
                                if !matches!(error, Error::DeadlineExceeded | Error::Overloaded) {
                                    return Poll::Ready(Err(error));
                                }
                            }
                            cx.waker().wake_by_ref();
                        }
                        stream.poll_prefetch(cx);
                        Poll::Pending
                    })
                    .await?;
                    connection = result;
                    if std::mem::take(&mut releases.provisional) {
                        stream.release_page(slice.page, slice.length)?;
                    } else {
                        outstanding.insert(slice.page, slice.length);
                    }
                    sent += u64::from(slice.length);
                    pages += 1;
                }
            }
            let mut final_scope = scope.clone();
            final_scope.deadline.0 = uring_runtime::environment::now() + timeout;
            connection =
                send_frame(&self.io, connection, frame(2, pages, sent, 0), &final_scope).await?;
            // Completion closes the subscription without waiting for final releases.
            observation.success();
            Ok(connection)
        })
    }
    pub fn new(io: Rc<HttpIo>, delivery: Rc<Delivery>) -> Self {
        Self {
            io,
            delivery,
            observer: Observer::default(),
        }
    }
    pub(crate) fn with_observer(mut self, observer: Observer) -> Self {
        self.observer = observer;
        self
    }
    /// Version unavailable -> 412, transient unavailable -> 503, range -> 206.
    /// Define malformed/unsatisfiable/unsupported method mappings in one place.
    #[cfg(test)]
    pub fn error_head(&self, error: Error) -> Result<MessageHead> {
        error_head(error)
    }
    pub fn send_error<'a>(
        &'a self,
        mut connection: ConnectionLease,
        error: Error,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            let mut head = error_head(error)?;
            head.headers.push(header("Connection", "close"));
            connection.poison();
            // A failed operation's expired scope cannot write its own 503. Allow
            // only a short, independently bounded final head, then close.
            let final_scope;
            let scope = if scope.check().is_err() {
                final_scope = RequestScope::new(
                    scope.request,
                    uring_runtime::environment::now() + Duration::from_secs(1),
                )?;
                &final_scope
            } else {
                scope
            };
            Ok(self.io.send_head(connection, head, scope).await?.connection)
        })
    }
    /// Check the coordinator's immutable result against the original operation
    /// before any headers escape. The request's sensitive context is not retained.
    pub fn validate(&self, kind: &ReadKind, response: &ReadResponse) -> Result<()> {
        if kind
            .pin()
            .is_some_and(|pin| pin != &response.metadata.version.etag)
        {
            return Err(Error::BadGateway);
        }
        match kind {
            ReadKind::Subscription { range, .. } => {
                let expected = if response.metadata.length == 0 && range.is_none() {
                    None
                } else {
                    Some(
                        range
                            .unwrap_or(ByteRange::From(0))
                            .resolve(response.metadata.length)
                            .map_err(|_| Error::BadGateway)?,
                    )
                };
                if response.range != expected || response.body.is_some() != expected.is_some() {
                    return Err(Error::BadGateway);
                }
                subscription_head(&response.metadata, response.range)?;
                return Ok(());
            }
            ReadKind::Head | ReadKind::HeadPinned { .. } => {
                if response.range.is_some() || response.body.is_some() {
                    return Err(Error::BadGateway);
                }
            }
        }
        success_head(&response.metadata, response.range)?;
        Ok(())
    }
    /// Send HEAD metadata. Object bodies use the duplex subscription sender.
    pub fn send<'a>(
        &'a self,
        connection: ConnectionLease,
        response: ReadResponse,
        scope: &'a RequestScope,
    ) -> Operation<'a, ConnectionLease> {
        self.send_inner(connection, response, scope, None)
    }
    pub(crate) fn send_observed<'a>(
        &'a self,
        connection: ConnectionLease,
        response: ReadResponse,
        scope: &'a RequestScope,
        observation: &'a mut crate::telemetry::RequestMetrics,
    ) -> Operation<'a, ConnectionLease> {
        self.send_inner(connection, response, scope, Some(observation))
    }
    fn send_inner<'a>(
        &'a self,
        mut connection: ConnectionLease,
        response: ReadResponse,
        scope: &'a RequestScope,
        mut observation: Option<&'a mut crate::telemetry::RequestMetrics>,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            scope.check()?;
            if response.body.is_some() || response.range.is_some() {
                return Err(Error::BadGateway);
            }
            let head = success_head(&response.metadata, None)?;
            connection = self.io.send_head(connection, head, scope).await?.connection;
            connection.finish_exchange()?;
            if let Some(observation) = observation.as_mut() {
                observation.success();
            }
            Ok(connection)
        })
    }
}
fn header(name: &str, value: impl AsRef<[u8]>) -> Header {
    Header {
        name: name.into(),
        value: value.as_ref().to_vec(),
    }
}
fn error_head(error: Error) -> Result<MessageHead> {
    let status = match error {
        Error::InvalidRequest | Error::InvalidRange => 400,
        Error::MethodNotAllowed => 405,
        Error::HeaderTooLarge => 431,
        Error::Unauthorized | Error::OriginRejected => 401,
        Error::Forbidden | Error::OriginForbidden => 403,
        Error::NotFound => 404,
        Error::VersionUnavailable => 412,
        Error::UnsatisfiableRangeWithLength(length) if length <= i64::MAX as u64 => 416,
        Error::BadGateway | Error::CorruptRecord => 502,
        Error::Unavailable | Error::Overloaded | Error::DeadlineExceeded | Error::Cancelled | Error::StaleFlight | Error::IncompatibleMembership | Error::HopBudgetExhausted | Error::MissingKey
        // Clock rollback or peer restart can reject an otherwise valid attempt's
        // freshness. It is unavailable to this client, not an internal failure.
        | Error::Replay | Error::Io => 503,
        // A bare unsatisfiable error has lost required version metadata. Never
        // invent a total length to make a syntactically valid but false 416.
        _ => 500,
    };
    let mut headers = vec![header("Content-Length", "0")];
    if status == 405 {
        headers.push(header("Allow", "HEAD, POST"));
    }
    if status == 416 {
        let Error::UnsatisfiableRangeWithLength(length) = error else {
            return Err(Error::Internal);
        };
        headers.push(header("Content-Range", format!("bytes */{length}")));
    }
    Ok(MessageHead {
        start: StartLine::Response { status },
        headers,
    })
}
fn success_head(metadata: &ObjectMetadata, range: Option<ResolvedRange>) -> Result<MessageHead> {
    let expiry = metadata
        .expires_at
        .as_system_time()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| Error::BadGateway)?;
    if metadata.length > i64::MAX as u64
        || expiry.as_millis() > i64::MAX as u128
        || expiry.subsec_nanos() % 1_000_000 != 0
    {
        return Err(Error::BadGateway);
    }
    let mut headers = vec![
        header("ETag", metadata.version.etag.as_bytes()),
        header("Racer-Expires-At", expiry.as_millis().to_string()),
    ];
    if let Some(content_type) = &metadata.content_type {
        headers.push(header("Racer-Content-Type", content_type.as_bytes()));
    }
    if range.is_some() {
        return Err(Error::BadGateway);
    }
    headers.push(header("Content-Length", metadata.length.to_string()));
    headers.push(header("Content-Type", "application/octet-stream"));
    Ok(MessageHead {
        start: StartLine::Response { status: 200 },
        headers,
    })
}

#[cfg(test)]
mod tests;
