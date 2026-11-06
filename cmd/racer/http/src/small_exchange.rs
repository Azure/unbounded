//! Bounded request and response exchange for small control-plane messages.
//!
//! [`HttpIo::exchange_small`] sends caller-serialized request bytes on a pooled
//! connection, then receives one response whose head the caller parses and
//! whose body is decoded by [`racer_wire::http::small`] (fixed-length, chunked,
//! or close-delimited) into a capped, zeroizing buffer. It works the same over
//! plain TCP, kernel TLS, and the userspace TLS relay.
//!
//! The caller owns head policy: it receives the accumulated plaintext and
//! returns `None` until a complete head is present, then the framing, decoder
//! limits, and a value describing the response. Bytes beyond the framed body
//! are rejected as pipelining and close the connection. A completed exchange
//! leaves the lease reusable unless the head asked to close or the body was
//! close-delimited; callers may still [`ConnectionLease::poison`] it.

use crate::connection::{BufferRange, ConnectionLease, Context, HttpIo, Operation, OwnedBuffer};
use racer_io::Scope as _;
use racer_io::ring::IoBuffer as _;
use racer_wire::http::small;
use zeroize::{Zeroize as _, Zeroizing};

/// Receive chunk size; also the read-ahead granularity.
const CHUNK: usize = 16 * 1024;

/// Framing and limits selected by the caller once a complete head arrived.
pub struct Plan<E, T> {
    /// Bytes of accumulated plaintext occupied by the head, including its final
    /// empty line.
    pub head_length: usize,

    /// Body framing for this response.
    pub framing: small::Framing,

    /// Decoded body bounds.
    pub limits: small::Limits,

    /// Whether the head requires closing the connection after this exchange.
    pub close: bool,

    /// Map body decoding failures onto the caller's error type.
    pub body_error: fn(small::Error) -> E,

    /// Caller response metadata returned with the body.
    pub value: T,
}

/// A completed small exchange and the connection that carried it.
pub struct Exchanged<C: Context, T> {
    /// The connection, reusable only if framing allowed it.
    pub connection: ConnectionLease<C>,

    /// The caller's response metadata from [`Plan::value`].
    pub value: T,

    /// Decoded body bytes, erased on drop.
    pub body: Zeroizing<Vec<u8>>,
}

/// Parse accumulated plaintext into a plan, or `None` while the head is incomplete.
pub type Parse<'a, E, T> = &'a mut dyn FnMut(&[u8]) -> Result<Option<Plan<E, T>>, E>;

impl<C: Context> HttpIo<C> {
    /// Send `request` and receive one response of at most `head_limit` head
    /// bytes, decoded under the plan returned by `parse`.
    ///
    /// The peer closing the connection before a complete head, or before a
    /// length-delimited or chunked body completes, fails with an I/O error.
    pub fn exchange_small<'a, T: 'a>(
        &'a self,
        mut connection: ConnectionLease<C>,
        request: &'a [u8],
        head_limit: usize,
        parse: Parse<'a, C::Error, T>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Exchanged<C, T>> {
        Box::pin(async move {
            scope.check()?;
            if connection.receive_remaining().is_some_and(|n| n != 0)
                || connection.send_remaining().is_some_and(|n| n != 0)
                || connection.take_read_ahead().is_some()
            {
                connection.poison();
                return Err(crate::Error::Malformed.into());
            }
            connection.begin_io();
            let sent = self.send_small(connection, request, scope).await?;
            connection = sent;
            let mut received = Zeroizing::new(Vec::new());
            let mut eof = false;
            let plan = loop {
                if let Some(plan) = parse(&received)? {
                    break plan;
                }
                if eof {
                    connection.poison();
                    return Err(racer_io::Error::Io.into());
                }
                if received.len() >= head_limit {
                    connection.poison();
                    return Err(crate::Error::HeadTooLarge.into());
                }
                let (next, read) = self.receive_small(connection, &mut received, scope).await?;
                connection = next;
                eof = read == 0;
            };
            if plan.head_length > received.len() {
                connection.poison();
                return Err(crate::Error::Malformed.into());
            }
            let mut decoder =
                small::Decoder::new(plan.framing, plan.limits).map_err(plan.body_error)?;
            let mut input = Zeroizing::new(received[plan.head_length..].to_vec());
            received.zeroize();
            loop {
                let status = match decoder.feed(&input) {
                    Ok(status) => status,
                    Err(error) => {
                        connection.poison();
                        return Err((plan.body_error)(error));
                    }
                };
                match status {
                    small::Status::Done(n) if n == input.len() => break,
                    // Read-ahead past one response is never pipelined.
                    small::Status::Done(_) => {
                        connection.poison();
                        return Err(crate::Error::Malformed.into());
                    }
                    small::Status::Need if eof => {
                        connection.poison();
                        match decoder.finish() {
                            Ok(()) => break,
                            Err(small::Error::Truncated) => {
                                return Err(racer_io::Error::Io.into());
                            }
                            Err(error) => return Err((plan.body_error)(error)),
                        }
                    }
                    small::Status::Need => {
                        input.zeroize();
                        input.clear();
                        let (next, read) =
                            self.receive_small(connection, &mut input, scope).await?;
                        connection = next;
                        eof = read == 0;
                    }
                }
            }
            connection.mark_small_complete();
            connection.finish_exchange()?;
            if plan.close || eof || plan.framing == small::Framing::Close {
                connection.poison();
            }
            Ok(Exchanged {
                connection,
                value: plan.value,
                body: Zeroizing::new(decoder.into_body()),
            })
        })
    }

    /// Copy request bytes into admitted storage and send them completely.
    async fn send_small(
        &self,
        connection: ConnectionLease<C>,
        request: &[u8],
        scope: &C::Scope,
    ) -> crate::connection::Result<C, ConnectionLease<C>> {
        if request.is_empty() {
            return Ok(connection);
        }
        let buffer = OwnedBuffer::copy_from(self.context(), request)?;
        let mut connection = connection;
        let mut offset = 0;
        let mut buffer = buffer;
        while offset < request.len() {
            let range = BufferRange::new::<C::Error>(buffer, offset..request.len())?;
            let completed = self
                .reactor()
                .send(connection.socket(), range, connection, scope)
                .await?;
            if completed.bytes == 0 || completed.bytes > request.len() - offset {
                return Err(racer_io::Error::Io.into());
            }
            offset += completed.bytes;
            buffer = completed.buffer.into_inner();
            connection = completed.lease;
        }
        Ok(connection)
    }

    /// Append one received fragment; zero bytes means the peer closed.
    async fn receive_small(
        &self,
        connection: ConnectionLease<C>,
        into: &mut Zeroizing<Vec<u8>>,
        scope: &C::Scope,
    ) -> crate::connection::Result<C, (ConnectionLease<C>, usize)> {
        let buffer = self.buffer(CHUNK)?;
        let range = BufferRange::new::<C::Error>(buffer, 0..CHUNK)?;
        let completed = self.receive_plain(connection, range, scope).await?;
        let mut buffer = completed.buffer.into_inner();
        let bytes = completed.bytes.min(CHUNK);
        append_sensitive(into, &buffer.bytes()?[..bytes]);
        buffer.bytes_mut()?[..bytes].zeroize();
        Ok((completed.lease, bytes))
    }
}

/// Grow without freeing an allocation that still contains plaintext.
fn append_sensitive(into: &mut Zeroizing<Vec<u8>>, bytes: &[u8]) {
    if into.capacity() - into.len() < bytes.len() {
        let capacity = (into.len() + bytes.len()).max(into.capacity().saturating_mul(2));
        let mut next = Zeroizing::new(Vec::with_capacity(capacity));
        next.extend_from_slice(into);
        std::mem::swap(into, &mut next);
    }
    into.extend_from_slice(bytes);
}
