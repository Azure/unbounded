//! Duplex subscription framing. Delivered leases stay pinned until exact release.
use super::{Responses, header};
use crate::{
    error::{Error, Operation, Result},
    http::{
        MessageHead, StartLine,
        connection::ConnectionLease,
        connection::{HttpIo, OwnedBuffer},
    },
    model::{ObjectMetadata, PAGE_BYTES, PageNumber, ResolvedRange},
    read::{ReadResponse, range_stream::RangeStream},
    runtime::{deadline::RequestScope, reactor::IoBuffer},
};
use std::{
    collections::BTreeMap,
    task::{Context, Poll},
    time::Duration,
};

pub(super) fn success_head(
    metadata: &ObjectMetadata,
    range: Option<ResolvedRange>,
) -> Result<MessageHead> {
    let mut head = super::success_head(metadata, None)?;
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
        socket: &std::rc::Rc<crate::runtime::reactor::Descriptor>,
        stream: &mut RangeStream,
        outstanding: &mut BTreeMap<PageNumber, u32>,
        current: Option<(crate::model::PageSlice, &crate::memory::delivery::FinalSend)>,
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

fn poll_slice_or_readiness<T>(
    cx: &mut Context<'_>,
    slice: impl FnOnce(&mut Context<'_>) -> Poll<Result<T>>,
    readiness: impl FnOnce(&mut Context<'_>) -> Poll<Result<u32>>,
) -> Poll<Result<T>> {
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
            let metrics = crate::telemetry::metrics::Metrics::default();
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
        observation: &'a mut crate::telemetry::metrics::RequestMetrics,
        timeout: Duration,
    ) -> Operation<'a, ConnectionLease> {
        Box::pin(async move {
            let head = success_head(&response.metadata, response.range)?;
            let expected = response.range.map_or(0, |r| r.len());
            let mut first = if let Some(stream) = response.body.as_mut() {
                match stream.next_slice().await {
                    Ok(Some(reader)) => Some(reader),
                    result => {
                        let error = result.err().unwrap_or(Error::BadGateway);
                        self.observer.record(
                            crate::telemetry::failures::Failure::new(
                                crate::telemetry::failures::Stage::FirstSlice,
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
            let reservation = connection.reservation.clone();
            let mut releases = Releases {
                bytes: [0; 12],
                used: 0,
                ahead: connection.read_ahead.take(),
                provisional: false,
            };
            let mut outstanding = BTreeMap::new();
            let mut sent = 0;
            let mut pages = 0;
            if let Some(stream) = response.body.as_mut() {
                stream.enable_progress(timeout);
                while sent < expected {
                    let mut progress_scope = scope.clone();
                    progress_scope.deadline.0 = crate::runtime::environment::now() + timeout;
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
                                crate::telemetry::failures::Failure::new(
                                    crate::telemetry::failures::Stage::NextSlice,
                                    error,
                                )
                                .request(scope)
                                .detail(
                                    crate::telemetry::failures::Detail::Delivery { sent, expected },
                                ),
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
                    let final_send = crate::memory::delivery::FinalSend::default();
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
                            receive_scope.deadline.0 = crate::runtime::environment::now() + timeout;
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
            final_scope.deadline.0 = crate::runtime::environment::now() + timeout;
            connection =
                send_frame(&self.io, connection, frame(2, pages, sent, 0), &final_scope).await?;
            // Completion closes the subscription without waiting for final releases.
            observation.success();
            Ok(connection)
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn ready_slice_precedes_auxiliary_overload() {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut polled = false;
        let result = poll_slice_or_readiness(
            &mut cx,
            |_| {
                polled = true;
                Poll::Ready(Ok(7))
            },
            |_| Poll::Ready(Err(Error::Overloaded)),
        );
        assert_eq!(result, Poll::Ready(Ok(7)));
        assert!(
            polled,
            "an available slice must not be discarded by auxiliary admission"
        );
    }
    #[test]
    fn pending_slice_preserves_readiness_errors_without_retry() {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for error in [
            Error::Overloaded,
            Error::Io,
            Error::InvalidRequest,
            Error::Cancelled,
            Error::DeadlineExceeded,
        ] {
            assert_eq!(
                poll_slice_or_readiness::<()>(
                    &mut cx,
                    |_| Poll::Pending,
                    |_| Poll::Ready(Err(error))
                ),
                Poll::Ready(Err(error)),
            );
        }
    }
    #[test]
    fn slice_failure_never_submits_auxiliary_readiness() {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for error in [
            Error::Cancelled,
            Error::DeadlineExceeded,
            Error::Unavailable,
        ] {
            assert_eq!(
                poll_slice_or_readiness::<()>(
                    &mut cx,
                    |_| Poll::Ready(Err(error)),
                    |_| panic!("terminal acquisition must not submit readiness")
                ),
                Poll::Ready(Err(error)),
            );
        }
    }
    #[test]
    fn pending_slice_readiness_wakes_once_and_pending_does_not_spin() {
        use std::sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        };
        #[derive(Default)]
        struct Wakes(AtomicUsize);
        impl futures::task::ArcWake for Wakes {
            fn wake_by_ref(this: &Arc<Self>) {
                this.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let wakes = Arc::new(Wakes::default());
        let waker = futures::task::waker(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        assert_eq!(
            poll_slice_or_readiness::<()>(&mut cx, |_| Poll::Pending, |_| Poll::Pending),
            Poll::Pending
        );
        assert_eq!(wakes.0.load(Ordering::Relaxed), 0);
        assert_eq!(
            poll_slice_or_readiness::<()>(
                &mut cx,
                |_| Poll::Pending,
                |_| Poll::Ready(Ok(libc::POLLIN as u32))
            ),
            Poll::Pending
        );
        assert_eq!(wakes.0.load(Ordering::Relaxed), 1);
    }
    #[test]
    fn frames_use_exact_network_order_fields() {
        assert_eq!(
            frame(1, 0x0102030405060708, 9, 10),
            [
                1, 1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 9, 0, 0, 0, 10
            ]
        );
        assert_eq!(frame(2, 3, 4, 0)[17..], [0; 4]);
    }
    #[test]
    fn subscription_head_counts_partial_pages_and_empty_completion() {
        let metadata = super::super::tests::metadata(PAGE_BYTES + 10);
        let range = crate::model::ByteRange::Closed {
            first: PAGE_BYTES - 2,
            last: PAGE_BYTES + 3,
        }
        .resolve(metadata.length)
        .unwrap();
        let head = success_head(&metadata, Some(range)).unwrap();
        assert!(matches!(head.start, StartLine::Response { status: 200 }));
        assert_eq!(head.unique("Content-Length").unwrap().unwrap(), b"69");
        assert_eq!(
            head.unique("Racer-Range-End").unwrap().unwrap(),
            (PAGE_BYTES + 4).to_string().as_bytes()
        );
        assert_eq!(head.unique("Connection").unwrap().unwrap(), b"close");
        let head = success_head(&super::super::tests::metadata(0), None).unwrap();
        assert_eq!(head.unique("Content-Length").unwrap().unwrap(), b"21");
        assert!(success_head(&metadata, None).is_err());
    }
}
