//! Immutable-body delivery, distinct from opaque relay's readiness-driven profile.
use flow_control::{Policy, pipe::PipeLease};
use http1::connection::{ConnectionLease, Context, OwnedBuffer, Result};
use std::{io, task::Poll, time::Instant};
use uring_runtime::reactor::{Descriptor, IoBuffer, SendBuffer};

pub const CHUNK_BYTES: usize = 64 * 1024;
const TURN_BYTES: usize = 256 * 1024;
const TURN_CALLS: usize = 32;

/// Nonblocking staging pipe. Writes/drains expose EINTR and EAGAIN; successful
/// counts must be bounded by the supplied slice or requested count.
pub trait DeliveryPipe: 'static {
    fn buffered(&self) -> usize;
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize>;
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize>;
    fn send(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize>;
}
impl<P: Policy> DeliveryPipe for PipeLease<P> {
    fn buffered(&self) -> usize {
        self.buffered()
    }
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.try_write(bytes)
    }
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        self.try_read(bytes)
    }
    fn send(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize> {
        self.try_splice_descriptor(socket, count)
    }
}

/// The complete admitted reader: immutable bytes, a staging pipe, and the exact
/// socket-accepted cursor. Application identity/range validation precedes use.
pub trait Owner<C: Context>: 'static {
    type Pipe: DeliveryPipe;
    type View: SendBuffer<Error = C::Error>;
    fn remaining(&self) -> usize;
    /// Advance only by a checked positive socket-accepted count.
    fn advance(&mut self, count: usize);
    /// Return immutable bytes at the accepted cursor and the independent pipe.
    /// Bytes must cover min(remaining, CHUNK_BYTES).
    fn parts(&mut self) -> (&[u8], &mut Self::Pipe);
    /// Stable owning view of `count` bytes at the accepted cursor. Must retain
    /// the full backing owner through its runtime completion fence.
    fn view(&self, count: usize) -> Result<C, Self::View>;
}

/// Caller policy and observation at the same boundaries as the send mechanism.
/// Callbacks may reject a send but must not release any completion-owned resource.
pub trait Observer<C: Context> {
    /// Select and check the next send scope, using last socket progress.
    fn scope(&self, stalled_at: Instant) -> Result<C, C::Scope>;
    fn direct_bytes(&self, _count: usize) {}
    fn pipe_drained(&self) {}
    fn before_send(&self, _count: usize, _remaining: usize) {}
    fn after_send(&self, _count: usize, _accepted: usize) -> Result<C, ()> {
        Ok(())
    }
}

enum Buffer<C: Context, V: SendBuffer<Error = C::Error>> {
    Pipe(OwnedBuffer<C>),
    View(V),
}
// SAFETY: each variant retains stable immutable backing through completion.
unsafe impl<C: Context, V: SendBuffer<Error = C::Error>> SendBuffer for Buffer<C, V> {
    type Error = C::Error;
    fn send_bytes(&self) -> Result<C, &[u8]> {
        match self {
            Self::Pipe(buffer) => buffer.send_bytes(),
            Self::View(buffer) => buffer.send_bytes(),
        }
    }
}

/// Deliver the owner's remaining slice without finishing or consuming HTTP
/// framing. The caller validates framing/socket first and consumes framing only
/// after success. Every async send owns the complete (reader, connection) tuple.
/// The caller must invalidate connection reuse with begin_io before handing it
/// to the future, including abandonment before the first poll.
pub async fn send<C, O, H>(
    context: &C,
    reactor: &C::Reactor,
    mut owner: O,
    mut connection: ConnectionLease<C>,
    observer: &H,
) -> Result<C, (O, ConnectionLease<C>)>
where
    C: Context,
    O: Owner<C>,
    H: Observer<C>,
{
    let mut stalled_at = uring_runtime::environment::now();
    let mut copying = false;
    let mut budget = 0;
    let mut calls = 0;
    while owner.remaining() != 0 {
        let send_scope = observer.scope(stalled_at)?;
        let count = owner.remaining().min(CHUNK_BYTES);
        let result = (|| -> io::Result<usize> {
            let (bytes, pipe) = owner.parts();
            let bytes = &bytes[..count];
            if copying {
                connection.socket().try_send(bytes)
            } else if pipe.buffered() == 0 && pipe.write(bytes)? == 0 {
                Err(io::ErrorKind::WriteZero.into())
            } else {
                pipe.send(&connection.socket(), count)
            }
        })();
        let sent = match result {
            Ok(sent) => {
                if copying {
                    observer.direct_bytes(sent);
                }
                sent
            }
            Err(error) if !copying && unsupported(&error) => {
                copying = true;
                continue;
            }
            Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                yield_once().await;
                continue;
            }
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                let buffer = if copying {
                    Buffer::View(owner.view(owner.remaining().min(CHUNK_BYTES))?)
                } else {
                    let (_, pipe) = owner.parts();
                    let count = pipe.buffered();
                    let mut buffer = OwnedBuffer::new(context, count)?;
                    match pipe.drain(buffer.bytes_mut()?) {
                        Ok(read) if read == count && count != 0 => {}
                        Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                            yield_once().await;
                            continue;
                        }
                        _ => return Err(uring_runtime::Error::Io.into()),
                    }
                    observer.pipe_drained();
                    Buffer::Pipe(buffer)
                };
                let count = buffer.send_bytes()?.len();
                observer.before_send(count, owner.remaining());
                let completion = reactor
                    .send(
                        connection.socket(),
                        buffer,
                        (owner, connection),
                        &send_scope,
                    )
                    .await?;
                (owner, connection) = completion.lease;
                observer.after_send(count, completion.bytes)?;
                if completion.bytes > count {
                    return Err(uring_runtime::Error::Io.into());
                }
                // Reconstruct any unsent suffix from immutable backing, not the
                // now-empty pipe. Subsequent backpressure owns immutable views.
                copying = true;
                observer.direct_bytes(completion.bytes);
                completion.bytes
            }
            Err(_) => return Err(uring_runtime::Error::Io.into()),
        };
        if sent == 0 || sent > owner.remaining() {
            return Err(uring_runtime::Error::Io.into());
        }
        owner.advance(sent);
        stalled_at = uring_runtime::environment::now();
        budget += sent;
        calls += 1;
        if (budget >= TURN_BYTES || calls >= TURN_CALLS) && owner.remaining() != 0 {
            yield_once().await;
            budget = 0;
            calls = 0;
        }
    }
    Ok((owner, connection))
}

fn unsupported(error: &io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}
async fn yield_once() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await
}

#[cfg(test)]
mod tests;
