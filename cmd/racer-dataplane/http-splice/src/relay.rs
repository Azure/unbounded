use flow_control::{
    Policy,
    pipe::{MAX_PIPE_BYTES, PipeLease},
};
use http1::connection::{ConnectionLease, Context, HttpIo, OwnedBuffer, Result};
use std::{io, ops::Range, rc::Rc};
use uring_runtime::reactor::{Descriptor, IoBuffer};

/// Synchronous, nonblocking pipe operations. Successful counts must not exceed
/// the requested bytes or buffered bytes; buffered() tracks the exact suffix.
/// Implementations own the pipe and any admission for its entire lifetime.
pub trait RelayPipe: 'static {
    fn buffered(&self) -> usize;
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize>;
    fn receive(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize>;
    fn send(&mut self, socket: &Descriptor) -> io::Result<usize>;
}
impl<P: Policy> RelayPipe for PipeLease<P> {
    fn buffered(&self) -> usize {
        self.buffered()
    }
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        self.try_read(bytes)
    }
    fn receive(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize> {
        self.try_splice_from(socket, count)
    }
    fn send(&mut self, socket: &Descriptor) -> io::Result<usize> {
        self.try_splice_connection(socket)
    }
}

/// A readiness descriptor is not the owner of the transfer. Any submitted wait
/// must retain the complete Relay as its runtime lease through the final fence.
pub enum Step {
    Complete,
    Yield,
    Readiness {
        socket: Rc<Descriptor>,
        interest: i16,
    },
}

/// Exact opaque HTTP body transit. Does not finish exchanges or select deadlines.
pub struct Relay<C: Context, P: RelayPipe> {
    source: ConnectionLease<C>,
    destination: ConnectionLease<C>,
    pipe: P,
    fallback: Option<OwnedBuffer<C>>,
    pending: Range<usize>,
    copied: bool,
    #[cfg(any(test, feature = "test-util"))]
    fallback_at: Option<usize>,
}
impl<C: Context, P: RelayPipe> Relay<C, P> {
    /// Require equal known body lengths. The caller admits an empty pipe and
    /// validates any application envelope before constructing a nonempty relay.
    pub fn new(
        source: ConnectionLease<C>,
        destination: ConnectionLease<C>,
        pipe: P,
    ) -> Result<C, Self> {
        if source.receive_remaining().is_none()
            || source.receive_remaining() != destination.send_remaining()
        {
            return Err(http1::Error::Malformed.into());
        }
        Ok(Self {
            source,
            destination,
            pipe,
            fallback: None,
            pending: 0..0,
            copied: false,
            #[cfg(any(test, feature = "test-util"))]
            fallback_at: None,
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn force_fallback(&mut self, copied: bool, at_remaining: Option<usize>) {
        self.copied = copied;
        self.fallback_at = at_remaining;
    }

    /// Run at most 32 nonblocking actions, retaining pending pipe/copy suffixes.
    /// The caller checks its scope before each step and before finalization.
    pub fn step(&mut self, io: &HttpIo<C>) -> Result<C, Step> {
        if self.destination.socket().peer_read_closed() {
            return Err(uring_runtime::Error::Io.into());
        }
        let mut wait = Step::Yield;
        for _ in 0..32 {
            let remaining = self
                .source
                .receive_remaining()
                .ok_or(http1::Error::Malformed)? as usize;
            if self.pending.is_empty() && self.pipe.buffered() == 0 && remaining == 0 {
                break;
            }
            let writing = !self.pending.is_empty() || self.pipe.buffered() != 0;
            let result = if !self.pending.is_empty() {
                self.destination
                    .socket()
                    .try_send(&self.fallback.as_ref().unwrap().bytes()?[self.pending.clone()])
            } else if self.pipe.buffered() != 0 {
                #[cfg(any(test, feature = "test-util"))]
                if self
                    .fallback_at
                    .is_some_and(|threshold| remaining <= threshold)
                    && !self.copied
                {
                    self.copied = true;
                }
                if self.copied {
                    if self.fallback.is_none() {
                        self.fallback = Some(io.buffer(MAX_PIPE_BYTES)?);
                    }
                    let n = self
                        .pipe
                        .drain(self.fallback.as_mut().unwrap().bytes_mut()?)
                        .map_err(|_| uring_runtime::Error::Io)?;
                    self.pending = 0..n;
                    continue;
                }
                self.pipe.send(&self.destination.socket())
            } else if let Some((ahead, range)) = self.source.take_read_ahead() {
                let count = remaining.min(range.len()).min(MAX_PIPE_BYTES);
                if self.fallback.is_none() {
                    self.fallback = Some(io.buffer(MAX_PIPE_BYTES)?);
                }
                self.fallback.as_mut().unwrap().bytes_mut()?[..count]
                    .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                if range.len() > count {
                    self.source
                        .restore_read_ahead(ahead, range.start + count..range.end)?;
                }
                self.pending = 0..count;
                self.source.consume_received(count)?;
                continue;
            } else if self.copied {
                if self.fallback.is_none() {
                    self.fallback = Some(io.buffer(MAX_PIPE_BYTES)?);
                }
                self.source.socket().try_recv(
                    &mut self.fallback.as_mut().unwrap().bytes_mut()?
                        [..remaining.min(MAX_PIPE_BYTES)],
                )
            } else {
                self.pipe.receive(&self.source.socket(), remaining)
            };
            match result {
                Ok(0) => return Err(uring_runtime::Error::Io.into()),
                Ok(n) => {
                    if writing {
                        if !self.pending.is_empty() {
                            self.pending.start += n;
                        }
                        self.destination.consume_sent(n)?;
                    } else {
                        self.source.consume_received(n)?;
                        if self.copied {
                            self.pending = 0..n;
                        }
                    }
                }
                Err(error) if unsupported(&error) => self.copied = true,
                Err(error) if error.kind() == io::ErrorKind::Interrupted => (),
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                    wait = Step::Readiness {
                        socket: if writing {
                            self.destination.socket()
                        } else {
                            self.source.socket()
                        },
                        interest: if writing { libc::POLLOUT } else { libc::POLLIN },
                    };
                    break;
                }
                Err(_) => return Err(uring_runtime::Error::Io.into()),
            }
        }
        if self.source.receive_remaining() == Some(0)
            && self.destination.send_remaining() == Some(0)
        {
            Ok(Step::Complete)
        } else {
            Ok(wait)
        }
    }

    /// Finalize both connections while pipe and scratch owners remain retained.
    pub fn connections_mut(&mut self) -> (&mut ConnectionLease<C>, &mut ConnectionLease<C>) {
        (&mut self.source, &mut self.destination)
    }

    /// Return the destination without marking it reusable. Drop the source before
    /// pipe/scratch owners, after the caller's completion and poison policy.
    pub fn into_destination(self) -> ConnectionLease<C> {
        self.destination
    }
}
fn unsupported(error: &io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}

#[cfg(test)]
mod tests;
