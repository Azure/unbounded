//! Validate subscriptions and HEAD metadata while preserving opaque fields.
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
use crate::read::ReadPayload;
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
use racer_platform::reactor::IoBuffer;
use std::collections::BTreeMap;
use std::rc::Rc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;

pub mod listener;
pub mod origin_directory;

use racer_object_wire::client::MAX_HEAD_BYTES;
pub use racer_object_wire::client::ReadKind;
use racer_object_wire::client::frame;

/// A validated client operation and its byte-preserving origin context.
pub struct ClientRequest {
    pub retain: Option<Duration>,
    pub kind: ReadKind,

    pub origin: OriginContext,
}

/// Apply the shared wire parser before attaching the socket's authorized cache.
#[derive(Clone)]
pub struct RequestParser {
    wire: racer_object_wire::client::RequestParser,
}

impl RequestParser {
    /// Set the raw HTTP head cap used by both framing and semantic validation.
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
        self.parse_with_delivery(cache, head)
            .map(|(request, _)| request)
    }

    /// Parse a request and whether its subscription asked for mapped delivery.
    pub fn parse_with_delivery(
        &self,
        cache: &CacheId,
        head: MessageHead,
    ) -> Result<(ClientRequest, bool)> {
        let request = self.wire.parse(&head)?;
        let mapped = request.mapped;
        let request = ClientRequest {
            retain: request.retain,
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
        };
        Ok((request, mapped))
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
    mapped: bool,
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
    let watch_scope = RequestScope::new(scope.request, scope.deadline.0)?;
    let socket = connection.socket();
    // Keep four slots for duplex sends, receives, and listener progress. Tiny
    // rings retain the synchronous fallback rather than losing delivery capacity.
    let watch_room = admission
        .policy()
        .limits()
        .queue_entries
        .get()
        .saturating_sub(responses.io.reactor().in_flight())
        > 4;
    let mut watch = watch_room.then(|| responses.io.disconnected(&connection, &watch_scope));
    let mut poll_disconnect = !watch_room;
    let mut send = if matches!(kind, ReadKind::Subscription { .. }) {
        responses.send_subscription(
            connection,
            response,
            scope,
            observation,
            timeout,
            mapped && page_alloc::shared::installed(),
        )
    } else {
        responses.send_observed(connection, response, scope, observation)
    };
    let result = std::future::poll_fn(|cx| {
        // Give delivery first use of queue capacity. A synchronous response
        // needs no watch, and its I/O must not be rejected by auxiliary polling.
        if let Poll::Ready(result) = send.as_mut().poll(cx) {
            return Poll::Ready(result);
        }
        if let Some(Poll::Ready(result)) = watch.as_mut().map(|watch| watch.as_mut().poll(cx)) {
            watch = None;
            match result {
                Ok(events)
                    if events & (libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) as u32 == 0 =>
                {
                    // Linux can add RDHUP even to a HUP-only PollAdd. Do not
                    // cancel a reader or rearm a permanently ready half-close.
                    poll_disconnect = true;
                }
                // The watch is auxiliary. It must neither consume delivery's
                // queue headroom nor impose the acquisition deadline on a body
                // that has its own write-stall deadline.
                Err(Error::Overloaded | Error::DeadlineExceeded) => poll_disconnect = true,
                _ => {
                    let _ = scope.cancel();
                }
            }
        }
        if poll_disconnect && socket.peer_disconnected() {
            let _ = scope.cancel();
        }
        Poll::Pending
    })
    .await;
    drop(send);
    // Fence the watch before returning a reusable connection or releasing its slot.
    let _ = watch_scope.cancel();
    if let Some(watch) = watch {
        let _ = watch.await;
    }
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

/// Validate admitted reads and deliver HEAD or duplex subscription responses.
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
/// Retain a partial release across polls without an unbounded input queue.
struct Releases {
    bytes: [u8; 12],

    used: usize,

    ahead: Option<(OwnedBuffer, std::ops::Range<usize>)>,

    provisional: bool,
}

impl Releases {
    /// Consume bounded exact releases, retaining an early final-send acknowledgment.
    fn poll(
        &mut self,
        cx: &mut Context<'_>,
        socket: &Rc<racer_platform::reactor::descriptor::Descriptor>,
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

/// Write one complete subscription frame while retaining the connection lease.
async fn send_frame(
    io: &HttpIo,
    connection: ConnectionLease,
    bytes: &[u8],
    scope: &RequestScope,
) -> Result<ConnectionLease> {
    Ok(io.write_body_bytes(connection, bytes, scope).await?.lease)
}

/// Page bytes a mapped client may read in place until it credits them.
enum MappedPage {
    /// The page's own plaintext, already in shared memory.
    Shared(#[allow(dead_code)] crate::memory::VerifiedPage),
    /// A shared-memory copy of plaintext that lived elsewhere.
    Copied(#[allow(dead_code)] zeroize::Zeroizing<Vec<u8>>),
}

/// Consume releases until none are outstanding, the client closes, or the
/// scope's deadline passes. Only malformed releases are errors.
async fn drain_releases<L: Clone + 'static>(
    io: &HttpIo,
    socket: &Rc<racer_platform::reactor::descriptor::Descriptor>,
    reservation: L,
    releases: &mut Releases,
    stream: &mut RangeStream,
    outstanding: &mut BTreeMap<PageNumber, u32>,
    scope: &RequestScope,
) -> Result<()> {
    let mut ready: Option<Operation<'_, u32>> = None;
    std::future::poll_fn(|cx| {
        loop {
            match releases.poll(cx, socket, stream, outstanding, None) {
                Ok(_) if outstanding.is_empty() => return Poll::Ready(Ok(())),
                Ok(_) => {}
                // The client closed: it no longer reads any page.
                Err(Error::Io) => return Poll::Ready(Ok(())),
                Err(error) => return Poll::Ready(Err(error)),
            }
            if ready.is_none() {
                ready = Some(io.reactor().readiness_with_lease(
                    socket.clone(),
                    libc::POLLIN as u32,
                    reservation.clone(),
                    scope,
                ));
            }
            match ready.as_mut().unwrap().as_mut().poll(cx) {
                Poll::Pending => return Poll::Pending,
                Poll::Ready(Ok(_)) => ready = None,
                // Deadline or cancellation: stop waiting; the caller decides.
                Poll::Ready(Err(_)) => return Poll::Ready(Ok(())),
            }
        }
    })
    .await
}

/// After a failed mapped delivery, half-close and wait (bounded) for the client
/// to close, so pinned pages are not reused while it may still read them.
async fn linger_until_closed<L: Clone + 'static>(
    io: &HttpIo,
    socket: Rc<racer_platform::reactor::descriptor::Descriptor>,
    reservation: L,
    scope: &RequestScope,
    timeout: Duration,
) {
    // SAFETY: shutdown only changes socket state; the fd stays owned by `socket`.
    unsafe {
        libc::shutdown(std::os::fd::AsRawFd::as_raw_fd(&*socket), libc::SHUT_WR);
    }
    let Ok(linger) = RequestScope::new(scope.request, racer_platform::environment::now() + timeout)
    else {
        return;
    };
    let mut discard = [0u8; 256];
    loop {
        if linger.check().is_err() {
            return;
        }
        match socket.try_recv(&mut discard) {
            Ok(0) => return,
            Ok(_) => continue,
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
            Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(_) => return,
        }
        if io
            .reactor()
            .readiness_with_lease(
                socket.clone(),
                libc::POLLIN as u32,
                reservation.clone(),
                &linger,
            )
            .await
            .is_err()
        {
            return;
        }
    }
}

/// Tag only an error returned by this branch.
fn boundary_error(
    error: Error,
    boundary: &std::cell::Cell<crate::telemetry::ClientBoundary>,
    site: crate::telemetry::ClientBoundary,
) -> Error {
    boundary.set(site);
    error
}

/// An admitted payload keeps running through auxiliary capacity or idle errors.
fn payload_readiness(result: Result<u32>) -> Result<()> {
    match result {
        Ok(_) | Err(Error::DeadlineExceeded | Error::Overloaded) => Ok(()),
        Err(error) => Err(error),
    }
}

/// Tag only an error returned by this branch.
fn observe_boundary<T>(
    result: Poll<Result<T>>,
    boundary: &std::cell::Cell<crate::telemetry::ClientBoundary>,
    site: crate::telemetry::ClientBoundary,
) -> Poll<Result<T>> {
    if matches!(result, Poll::Ready(Err(_))) {
        boundary.set(site);
    }
    result
}

/// Prefer an acquired slice over auxiliary release-readiness admission.
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
            self.send_subscription(
                connection,
                response,
                scope,
                &mut observation,
                timeout,
                false,
            )
            .await
        })
    }

    /// Deliver a subscription with request metrics and progress-based deadlines.
    pub(crate) fn send_subscription<'a>(
        &'a self,
        mut connection: ConnectionLease,
        mut response: ReadResponse,
        scope: &'a RequestScope,
        observation: &'a mut crate::telemetry::RequestMetrics,
        timeout: Duration,
        mapped: bool,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            // Mapped pages the client may still be reading, and the socket whose
            // close tells us it stopped. Both outlive the delivery body.
            let mut held: BTreeMap<PageNumber, MappedPage> = BTreeMap::new();
            let mut linger = None;
            let result: Result<ConnectionLease> = async {
                let head = if mapped {
                    racer_object_wire::client::mapped_subscription_head(
                        &response.metadata,
                        response.payload.range(),
                    )?
                } else {
                    subscription_head(&response.metadata, response.payload.range())?
                };
                let expected = response.payload.range().map_or(0, |r| r.len());
                let mut first = if let ReadPayload::Stream(stream) = &mut response.payload {
                    match stream.next_slice().await {
                        Ok(Some(reader)) => Some(reader),
                        result => {
                            let error = result.err().unwrap_or(Error::BadGateway);
                            self.observer.record(
                                crate::telemetry::Failure::new(
                                    crate::telemetry::Stage::FirstSlice,
                                    error,
                                )
                                .request(scope)
                                .detail(
                                    crate::telemetry::Detail::ClientDelivery {
                                        boundary: crate::telemetry::ClientBoundary::Stream,
                                        sent: 0,
                                        expected,
                                    },
                                ),
                            );
                            observation.fail(error);
                            return self.send_error(connection, error, scope).await;
                        }
                    }
                } else {
                    None
                };
                connection.poison();
                connection = if mapped {
                    let memory = page_alloc::shared::readonly_fd().ok_or(Error::Overloaded)?;
                    self.io
                        .send_head_with_descriptor(
                            connection,
                            head,
                            scope,
                            std::os::fd::AsRawFd::as_raw_fd(&memory),
                        )
                        .await?
                        .connection
                } else {
                    self.io.send_head(connection, head, scope).await?.connection
                };
                let socket = connection.socket();
                let reservation = connection.slot().cloned();
                if mapped {
                    linger = Some((socket.clone(), reservation.clone()));
                }
                let mut releases = Releases {
                    bytes: [0; 12],
                    used: 0,
                    ahead: connection.take_read_ahead(),
                    provisional: false,
                };
                let mut outstanding = BTreeMap::new();
                let mut sent = 0;
                let mut pages = 0;
                if let ReadPayload::Stream(stream) = &mut response.payload {
                    let range = stream.range();
                    stream.enable_progress(timeout);
                    while sent < expected {
                        held.retain(|page, _| outstanding.contains_key(page));
                        let mut progress_scope = scope.clone();
                        progress_scope.deadline.0 = racer_platform::environment::now() + timeout;
                        let reader = if let Some(reader) = first.take() {
                            reader
                        } else {
                            // Pending acquisition and pipe admission live in the stream.
                            // Drop only the borrowing future so duplex release_page can
                            // update credits without discarding the pipe waiter's wake.
                            let mut ready = None;
                            let boundary =
                                std::cell::Cell::new(crate::telemetry::ClientBoundary::Stream);
                            std::future::poll_fn(|cx| {
                                if let Err(error) = progress_scope.check() {
                                    return Poll::Ready(Err(boundary_error(
                                        error,
                                        &boundary,
                                        crate::telemetry::ClientBoundary::Scope,
                                    )));
                                }
                                if let Err(error) =
                                    releases.poll(cx, &socket, stream, &mut outstanding, None)
                                {
                                    return Poll::Ready(Err(boundary_error(
                                        error,
                                        &boundary,
                                        crate::telemetry::ClientBoundary::ReleaseProcessing,
                                    )));
                                }
                                poll_slice_or_readiness(
                                    cx,
                                    |cx| {
                                        observe_boundary(
                                            stream.next_slice().as_mut().poll(cx),
                                            &boundary,
                                            crate::telemetry::ClientBoundary::Stream,
                                        )
                                    },
                                    |cx| {
                                        if ready.is_none() {
                                            ready = Some(self.io.reactor().readiness_with_lease(
                                                socket.clone(),
                                                libc::POLLIN as u32,
                                                reservation.clone(),
                                                &progress_scope,
                                            ));
                                        }
                                        let result = observe_boundary(
                                            ready.as_mut().unwrap().as_mut().poll(cx),
                                            &boundary,
                                            crate::telemetry::ClientBoundary::ReleaseReadiness,
                                        );
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
                                    .detail(
                                        crate::telemetry::Detail::ClientDelivery {
                                            boundary: boundary.get(),
                                            sent,
                                            expected,
                                        },
                                    ),
                                );
                            })?
                            .ok_or(Error::BadGateway)?
                        };
                        let slice = reader.slice();
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
                        if mapped {
                            // The client reads the bytes in place; pin them until its
                            // credit (or close) says it stopped. Pages outside the
                            // shared memory are copied into it once.
                            let (memory, page) =
                                match page_alloc::shared::offset_of(reader.payload()) {
                                    Some(memory) => (memory, MappedPage::Shared(reader.page())),
                                    None => {
                                        let mut copy = page_alloc::shared::vec_with_capacity(
                                            reader.payload().len(),
                                        )
                                        .ok_or(Error::Overloaded)?;
                                        copy.extend_from_slice(reader.payload());
                                        let memory = page_alloc::shared::offset_of(&copy)
                                            .ok_or(Error::BadGateway)?;
                                        self.delivery
                                            .record(crate::telemetry::Event::ClientMappedCopies, 1);
                                        (memory, MappedPage::Copied(zeroize::Zeroizing::new(copy)))
                                    }
                                };
                            drop(reader);
                            held.insert(slice.page, page);
                            outstanding.insert(slice.page, slice.length);
                            connection = send_frame(
                                &self.io,
                                connection,
                                &racer_object_wire::client::mapped_frame(
                                    slice.page.0,
                                    offset,
                                    slice.length,
                                    memory,
                                ),
                                &progress_scope,
                            )
                            .await?;
                            self.delivery
                                .record(crate::telemetry::Event::ClientMappedPages, 1);
                            sent += u64::from(slice.length);
                            pages += 1;
                            continue;
                        }
                        // Acquisition owns no delivery pipe and continues even while
                        // the page frame or payload is blocked on this socket.
                        let final_send = crate::http::FinalSend::default();
                        let mut write = Box::pin(async {
                            let connection = send_frame(
                                &self.io,
                                connection,
                                &frame(1, slice.page.0, offset, slice.length),
                                &progress_scope,
                            )
                            .await?;
                            self.delivery
                                .finish_subscription(
                                    reader,
                                    connection,
                                    &progress_scope,
                                    &final_send,
                                )
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
                                receive_scope.deadline.0 =
                                    racer_platform::environment::now() + timeout;
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
                                payload_readiness(result)?;
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
                final_scope.deadline.0 = racer_platform::environment::now() + timeout;
                connection = send_frame(
                    &self.io,
                    connection,
                    &frame(2, pages, sent, 0),
                    &final_scope,
                )
                .await?;
                if mapped && let ReadPayload::Stream(stream) = &mut response.payload {
                    // Mapped bytes must stay put until the client credits them or
                    // goes away, so wait (bounded) for every outstanding release.
                    drain_releases(
                        &self.io,
                        &socket,
                        reservation.clone(),
                        &mut releases,
                        stream,
                        &mut outstanding,
                        &final_scope,
                    )
                    .await?;
                    held.clear();
                }
                // Completion closes the subscription without waiting for final releases.
                observation.success();
                Ok(connection)
            }
            .await;
            if result.is_err()
                && !held.is_empty()
                && let Some((socket, reservation)) = linger
            {
                // The client may be reading a pinned page right now. Tell it to
                // stop, then keep the bytes until it closes or the bound passes.
                linger_until_closed(&self.io, socket, reservation, scope, timeout).await;
            }
            drop(held);
            result
        })
    }

    /// Attach delivery resources with a no-op observer until configured.
    pub fn new(io: Rc<HttpIo>, delivery: Rc<Delivery>) -> Self {
        Self {
            io,
            delivery,
            observer: Observer::default(),
        }
    }

    /// Route detailed delivery failures to the worker's observer.
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

    /// Send a final error head with a bounded grace period for an expired request.
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
                    racer_platform::environment::now() + Duration::from_secs(1),
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
                let expected = expected_subscription_range(*range, response.metadata.length)?;
                if response.payload.range() != expected {
                    return Err(Error::BadGateway);
                }
                subscription_head(&response.metadata, response.payload.range())?;
                return Ok(());
            }
            ReadKind::Head | ReadKind::HeadPinned { .. } => {
                if !matches!(&response.payload, ReadPayload::Empty) {
                    return Err(Error::BadGateway);
                }
            }
        }
        success_head(&response.metadata, None)?;
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

    /// Deliver HEAD metadata and record successful completion.
    pub(crate) fn send_observed<'a>(
        &'a self,
        connection: ConnectionLease,
        response: ReadResponse,
        scope: &'a RequestScope,
        observation: &'a mut crate::telemetry::RequestMetrics,
    ) -> Operation<'a, ConnectionLease> {
        self.send_inner(connection, response, scope, Some(observation))
    }

    /// Reject streams before committing a metadata-only response.
    fn send_inner<'a>(
        &'a self,
        mut connection: ConnectionLease,
        response: ReadResponse,
        scope: &'a RequestScope,
        mut observation: Option<&'a mut crate::telemetry::RequestMetrics>,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            scope.check()?;
            if !matches!(&response.payload, ReadPayload::Empty) {
                return Err(Error::BadGateway);
            }
            let head = racer_object_wire::client::success_head_retained(
                &response.metadata,
                None,
                response.retain_until,
            )?;
            connection = self.io.send_head(connection, head, scope).await?.connection;
            connection.finish_exchange()?;
            if let Some(observation) = observation.as_mut() {
                observation.success();
            }
            Ok(connection)
        })
    }
}

/// Resolve the request's expected body shape, treating invalid successes as 502.
fn expected_subscription_range(
    range: Option<ByteRange>,
    length: u64,
) -> Result<Option<ResolvedRange>> {
    if length == 0 && range.is_none() {
        return Ok(None);
    }
    range
        .unwrap_or(ByteRange::From(0))
        .resolve(length)
        .map(Some)
        .map_err(|_| Error::BadGateway)
}

/// Construct a header without normalizing its value bytes.
fn header(name: &str, value: impl AsRef<[u8]>) -> Header {
    Header::new(name, value)
}

/// Map acquisition and validation errors to the SDK's HTTP response contract.
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

/// Encode metadata-only success headers using the shared wire contract.
fn success_head(metadata: &ObjectMetadata, range: Option<ResolvedRange>) -> Result<MessageHead> {
    racer_object_wire::client::success_head(metadata, range).map_err(Into::into)
}

#[cfg(test)]
mod tests;
