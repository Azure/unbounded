//! Validate v2 subscriptions and HEAD metadata while preserving opaque fields.
//!
//! Wire values follow pkg/racersdk: canonical lowercase keys, quoted strong pins,
//! signed-63-bit decimal ranges, and byte-preserving opaque context.
use crate::admission::AdmissionPolicy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::ConnectionLease;
use crate::http::Delivery;
use crate::http::HttpIo;
use crate::http::OwnedBuffer;
use crate::http::ReaderLease;
use crate::model::ByteRange;
#[cfg(test)]
use crate::model::CacheKey;
#[cfg(test)]
use crate::model::MAX_FIELD_BYTES;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::PAGE_BYTES;
use crate::model::PageNumber;
use crate::model::ResolvedRange;
use crate::read::ReadResponse;
use crate::read::range_stream::RangeStream;
use crate::runtime::RequestScope;
use crate::security::Authorization;
use crate::security::OpaqueMetadata;
use crate::security::OriginContext;
use crate::telemetry::Observer;
use http1::Header;
use http1::MessageHead;
#[cfg(test)]
use http1::StartLine;
use racer_control_wire::CacheId;
use std::collections::BTreeMap;
use std::rc::Rc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use uring_runtime::reactor::IoBuffer;

pub mod listener;

use racer_object_wire::client::MAX_HEAD_BYTES;
pub use racer_object_wire::client::ReadKind;
use racer_object_wire::client::frame;

pub struct ClientRequest {
    pub kind: ReadKind,

    pub origin: OriginContext,
}

#[derive(Clone)]
pub struct RequestParser {
    wire: racer_object_wire::client::RequestParser,
}
impl RequestParser {
    pub fn new(header_limit: usize) -> Self {
        Self {
            wire: racer_object_wire::client::RequestParser::new(header_limit),
        }
    }
    /// Apply this cap to raw HTTP framing before calling the semantic parser.
    pub(crate) fn header_limit(&self) -> usize {
        self.wire.header_limit()
    }
    /// Codec must validate the raw head and its byte limit before calling this.
    /// In particular, context fields must have exactly one separator space and
    /// must not have their value trimmed by the codec. Decoded fields cannot
    /// reconstruct wire length: unknown fields need not have a separator SP.
    pub fn parse(&self, cache: &CacheId, head: MessageHead) -> Result<ClientRequest> {
        let request = self.wire.parse(&head)?;
        Ok(ClientRequest {
            kind: request.kind,
            origin: OriginContext {
                object: ObjectId {
                    cache: cache.clone(),
                    key: request.key,
                },
                metadata: request
                    .metadata
                    .map(OpaqueMetadata::from_header)
                    .transpose()?,
                authorization: request
                    .authorization
                    .map(Authorization::from_header)
                    .transpose()?,
            },
        })
    }
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
            // A nested NextSlice record may precede this generic send outcome.
            admission.policy().observer().record(
                crate::telemetry::Failure::new(crate::telemetry::Stage::ClientWrite, error)
                    .request(scope),
            );
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
    racer_object_wire::client::subscription_head(metadata, range).map_err(Into::into)
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
        socket: &Rc<uring_runtime::reactor::descriptor::Descriptor>,
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
    Ok(io.write_body_bytes(connection, &bytes, scope).await?.lease)
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
    Header::new(name, value)
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
        | Error::Replay | Error::Io | Error::Os(_) => 503,
        Error::RenameUncertain(_) | Error::PublishedNotDurable(_) => 500,
        // A bare unsatisfiable error has lost required version metadata. Never
        // invent a total length to make a syntactically valid but false 416.
        _ => 500,
    };
    let length = match error {
        Error::UnsatisfiableRangeWithLength(length) => Some(length),
        _ => None,
    };
    racer_object_wire::client::error_head(status, length).map_err(Into::into)
}
fn success_head(metadata: &ObjectMetadata, range: Option<ResolvedRange>) -> Result<MessageHead> {
    racer_object_wire::client::success_head(metadata, range).map_err(Into::into)
}

#[cfg(test)]
mod tests;
