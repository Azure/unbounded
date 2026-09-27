//! Non-cloneable transit bodies retain both sockets and their chunk through I/O.
use super::{
    transfer::{Transfers, WireBuffer},
    wire::{SignedRequest, WireCodec},
};
use crate::{
    error::{Error, Result},
    http::pool::{ConnectionLease, Endpoint},
    runtime::{
        admission::{Reservation, TRANSIT_CHUNK},
        deadline::RequestScope,
    },
    security::forwarding::{ForwardedHead, Forwarding, RequestBinding},
};

pub(crate) struct TransitBody {
    downstream: ConnectionLease,
    chunk: WireBuffer,
    remaining: usize,
}

#[cfg(test)]
#[allow(clippy::items_after_test_module)]
mod tests {
    use super::*;
    use crate::{
        http::{codec::Codec, io::HttpIo, pool::HttpPool},
        model::limits::ResourceClass,
        peer::wire::PeerResponse,
        runtime::{admission::Admission, reactor::Reactor},
    };
    use std::{
        io::{Read, Write},
        os::unix::net::UnixStream,
        rc::Rc,
        task::{Context, Poll},
        time::{Duration, Instant},
    };

    #[test]
    fn stream_partial_io_short_body_cancellation_and_abandonment_fence_owners() {
        for case in [
            "complete",
            "short",
            "cancel-receive",
            "drop-receive",
            "cancel-send",
            "drop-send",
        ] {
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            admission.enable_transit().unwrap();
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(
                    super::super::wire::MAX_ENVELOPE_HEAD,
                    crate::model::range::PAGE_BYTES + 16,
                ),
                admission.clone(),
            ));
            let transfers = Transfers::new(
                Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
                io,
                None,
            );
            let (down, mut producer) = UnixStream::pair().unwrap();
            let (up, mut consumer) = UnixStream::pair().unwrap();
            producer.set_nonblocking(true).unwrap();
            consumer.set_nonblocking(true).unwrap();
            let mut downstream = ConnectionLease::from_accepted(down.into(), &admission).unwrap();
            let mut upstream = ConnectionLease::from_accepted(up.into(), &admission).unwrap();
            upstream.rx_remaining = Some(0);
            let length = 512 * 1024;
            downstream.rx_remaining = Some(length as u64);
            downstream.tx_remaining = Some(0);
            let signers = super::super::tests::signers();
            let a = Forwarding::new(signers[0].clone());
            let c = Forwarding::new(signers[2].clone());
            let request = super::super::tests::request(&admission, 1);
            let (signed, _) = a.sign_request(request).unwrap();
            let admitted = c.verify_request(signed).unwrap();
            // Body framing is tested independently here; descriptor/length
            // authentication has its own real signed-head tests.
            let response = c
                .sign_response(admitted.binding(), PeerResponse::Miss)
                .unwrap();
            drop(admitted);
            let scope = RequestScope::new(
                crate::model::identity::RequestId([7; 16]),
                Instant::now() + Duration::from_secs(3),
            )
            .unwrap();
            let body = TransitBody {
                downstream,
                remaining: length,
                chunk: WireBuffer::transit(
                    admission.reserve_transit().unwrap(),
                    admission.reserve(None, ResourceClass::Relay, 1).unwrap(),
                )
                .unwrap(),
            };
            let mut work =
                Box::pin(body.send(&transfers, upstream, response.authentication, &scope));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut sent = 0;
            let mut received = Vec::new();
            let mut complete = false;
            let mut cancel_at = None;
            let started = Instant::now();
            let payload = vec![0x5a; 4096];
            for turn in 0..100000 {
                assert!(started.elapsed() < Duration::from_secs(3), "{case}");
                let feed = case == "complete" || case == "short" || case.ends_with("send");
                let maximum = if case == "short" { 8192 } else { length };
                if feed && sent < maximum {
                    match producer.write(&payload[..4096.min(maximum - sent)]) {
                        Ok(n) => sent += n,
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                        Err(e) => panic!("{case}: {e}"),
                    }
                }
                if case == "short" && sent == maximum {
                    producer.shutdown(std::net::Shutdown::Write).unwrap();
                }
                if !case.ends_with("send") {
                    let mut bytes = [0; 8192];
                    loop {
                        match consumer.read(&mut bytes) {
                            Ok(0) => break,
                            Ok(n) => received.extend_from_slice(&bytes[..n]),
                            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                            Err(e) => panic!("{case}: {e}"),
                        }
                    }
                }
                match work.as_mut().poll(&mut cx) {
                    Poll::Ready(result) => {
                        if case == "complete" {
                            assert!(result.is_ok());
                        } else {
                            assert!(result.is_err(), "{case}");
                        }
                        drop(result);
                        complete = true;
                        break;
                    }
                    Poll::Pending => {}
                }
                reactor.poll_budgeted(64).unwrap();
                reactor.wait(Duration::from_micros(100)).unwrap();
                if cancel_at.is_none()
                    && turn > 64
                    && (case.ends_with("receive") || (case.ends_with("send") && sent >= 128 * 1024))
                {
                    assert!(admission.used(ResourceClass::Ciphertext) >= TRANSIT_CHUNK);
                    assert_eq!(admission.used(ResourceClass::Relay), 1);
                    if case.starts_with("drop") {
                        break;
                    }
                    if cancel_at.is_none() {
                        scope.cancel().unwrap();
                        cancel_at = Some(turn);
                    }
                }
            }
            if !case.starts_with("drop") {
                assert!(complete, "{case}");
            }
            drop(work);
            let mut drain = Box::pin(reactor.drain());
            while drain.as_mut().poll(&mut cx).is_pending() {
                reactor.poll_budgeted(64).unwrap();
                reactor.wait(Duration::from_micros(100)).unwrap();
                assert!(started.elapsed() < Duration::from_secs(3));
            }
            if case == "complete" {
                let mut bytes = [0; 8192];
                loop {
                    match consumer.read(&mut bytes) {
                        Ok(0) => break,
                        Ok(n) => received.extend_from_slice(&bytes[..n]),
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                        Err(e) => panic!("{e}"),
                    }
                }
                let start = received.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
                assert_eq!(&received[start..], vec![0x5a; length]);
            }
            for class in [
                ResourceClass::Ciphertext,
                ResourceClass::Relay,
                ResourceClass::Connection,
            ] {
                assert_eq!(admission.used(class), 0, "{case}: {class:?}");
            }
        }
    }
}

impl TransitBody {
    #[allow(clippy::too_many_arguments)]
    pub(crate) async fn open(
        transfers: &Transfers,
        endpoint: &Endpoint,
        request: SignedRequest,
        scope: &RequestScope,
        chunk: Reservation,
        permit: Reservation,
        forwarding: &Forwarding,
        binding: &RequestBinding,
        previous: &crate::model::identity::NodeId,
    ) -> Result<(ForwardedHead, Self)> {
        let chunk = WireBuffer::transit(chunk, permit)?;
        let connection = transfers.http.checkout(endpoint, scope).await?;
        // No native accept is attached. Downstream uses the existing HTTP fallback
        // without allocating a full relay page or repeating an acquisition.
        let sent = transfers
            .io
            .send_head(
                connection,
                WireCodec::encode(&request.authentication, false, 0)?,
                scope,
            )
            .await?;
        let received = transfers.io.receive_head(sent.connection, scope).await?;
        let head_bytes = received
            .value
            .headers
            .iter()
            .try_fold(0usize, |sum, header| {
                sum.checked_add(header.name.len() + header.value.len())
                    .ok_or(Error::InvalidRequest)
            })?;
        let _head = transfers
            .wire
            .as_ref()
            .ok_or(Error::InvalidConfiguration)?
            .0
            .reserve(
                None,
                crate::model::limits::ResourceClass::RequestContext,
                head_bytes
                    .checked_mul(3)
                    .ok_or(Error::InvalidRequest)?
                    .max(1),
            )?;
        let (authentication, length) = WireCodec::decode(received.value, true)?;
        let verified = forwarding.verify_response_head(authentication, length, binding)?;
        let authentication = forwarding.append_response_head(verified, previous)?;
        Ok((
            authentication,
            Self {
                downstream: received.connection,
                chunk,
                remaining: length,
            },
        ))
    }

    pub(crate) async fn send(
        mut self,
        transfers: &Transfers,
        upstream: ConnectionLease,
        authentication: ForwardedHead,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        let sent = transfers
            .io
            .send_head(
                upstream,
                WireCodec::encode(&authentication, true, self.remaining)?,
                scope,
            )
            .await?;
        let mut upstream = sent.connection;
        while self.remaining != 0 {
            let received = transfers
                .io
                .read_body_range(
                    self.downstream,
                    self.chunk,
                    0..self.remaining.min(TRANSIT_CHUNK),
                    scope,
                )
                .await?;
            if received.bytes == 0 {
                return Err(Error::Io);
            }
            self.downstream = received.lease;
            let sent = transfers
                .io
                .write_body_range(upstream, received.buffer, 0..received.bytes, scope)
                .await?;
            self.remaining -= sent.bytes;
            upstream = sent.lease;
            self.chunk = sent.buffer;
        }
        self.downstream.finish_exchange()?;
        upstream.finish_exchange()?;
        Ok(upstream)
    }
}
