//! Fixed-length opaque transit on the owning reactor. Every readiness operation
//! retains both connections, pipe, fallback storage and relay admission.
use super::{
    connection::ConnectionLease,
    io::{HttpIo, OwnedBuffer},
};
use crate::{
    error::{Error, Result},
    memory::pipe::{MAX_PIPE_BYTES, PipeLease},
    runtime::{deadline::RequestScope, reactor::IoBuffer},
};
use std::{cell::RefCell, rc::Rc, task::Poll, time::Duration};

struct Transit {
    source: ConnectionLease,
    destination: ConnectionLease,
    pipe: PipeLease,
    fallback: Option<OwnedBuffer>,
    pending: std::ops::Range<usize>,
    copied: bool,
    #[cfg(test)]
    fallback_at: Option<usize>,
}

fn unsupported(error: &std::io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}
#[cfg(test)]
#[path = "relay_tests.rs"]
mod tests;

impl HttpIo {
    pub(crate) async fn relay_body(
        &self,
        source: ConnectionLease,
        destination: ConnectionLease,
        pipe: Option<PipeLease>,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        if source.rx_remaining != destination.tx_remaining || source.rx_remaining.is_none() {
            return Err(Error::InvalidRequest);
        }
        if source.rx_remaining == Some(0) {
            let mut source = source;
            let mut destination = destination;
            finish(&mut source, &mut destination)?;
            destination.relay_reservation = None;
            return Ok(destination);
        }
        #[cfg(test)]
        let copied = destination.relay_fallback;
        #[cfg(test)]
        let fallback_at = destination.relay_fallback_at;
        #[cfg(not(test))]
        let copied = false;
        let state = Rc::new(RefCell::new(Transit {
            source,
            destination,
            pipe: pipe.ok_or(Error::InvalidRequest)?,
            fallback: None,
            pending: 0..0,
            copied,
            #[cfg(test)]
            fallback_at,
        }));
        loop {
            scope.check()?;
            let wait = {
                let mut state = state.borrow_mut();
                let s = &mut *state;
                if s.destination.fd.peer_read_closed() {
                    return Err(Error::Io);
                }
                let mut wait = None;
                // Bound synchronous work per poll so a hot link cannot monopolize
                // the worker. Drain each chunk before receiving the next one.
                for _ in 0..32 {
                    let remaining = s.source.rx_remaining.ok_or(Error::InvalidRequest)? as usize;
                    if s.pending.is_empty() && s.pipe.buffered() == 0 && remaining == 0 {
                        break;
                    }
                    let writing = !s.pending.is_empty() || s.pipe.buffered() != 0;
                    let result = if !s.pending.is_empty() {
                        s.destination
                            .fd
                            .try_send(&s.fallback.as_ref().unwrap().bytes()?[s.pending.clone()])
                    } else if s.pipe.buffered() != 0 {
                        #[cfg(test)]
                        if s.fallback_at
                            .is_some_and(|threshold| remaining <= threshold)
                            && !s.copied
                        {
                            s.copied =
                                unsupported(&std::io::Error::from_raw_os_error(libc::EOPNOTSUPP));
                        }
                        if s.copied {
                            if s.fallback.is_none() {
                                s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                            }
                            let n = s
                                .pipe
                                .try_read(s.fallback.as_mut().unwrap().bytes_mut()?)
                                .map_err(|_| Error::Io)?;
                            s.pending = 0..n;
                            continue;
                        }
                        s.pipe.try_splice_connection(&s.destination)
                    } else if let Some((ahead, range)) = s.source.read_ahead.take() {
                        let count = remaining.min(range.len());
                        if s.fallback.is_none() {
                            s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                        }
                        // Read-ahead may have a large head allocation but the tail
                        // is bounded by the receive growth step. Consume in chunks.
                        let count = count.min(MAX_PIPE_BYTES);
                        s.fallback.as_mut().unwrap().bytes_mut()?[..count]
                            .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                        if range.len() > count {
                            s.source.read_ahead = Some((ahead, range.start + count..range.end));
                        }
                        s.pending = 0..count;
                        s.source.rx_remaining = Some((remaining - count) as u64);
                        continue;
                    } else if s.copied {
                        if s.fallback.is_none() {
                            s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                        }
                        s.source.fd.try_recv(
                            &mut s.fallback.as_mut().unwrap().bytes_mut()?
                                [..remaining.min(MAX_PIPE_BYTES)],
                        )
                    } else {
                        s.pipe.try_splice_from(&s.source.fd, remaining)
                    };
                    match result {
                        Ok(0) => return Err(Error::Io),
                        Ok(n) => {
                            if writing {
                                if !s.pending.is_empty() {
                                    s.pending.start += n;
                                }
                                s.destination.tx_remaining = Some(
                                    s.destination.tx_remaining.ok_or(Error::InvalidRequest)?
                                        - n as u64,
                                );
                            } else {
                                s.source.rx_remaining = Some((remaining - n) as u64);
                                if s.copied {
                                    s.pending = 0..n;
                                }
                            }
                        }
                        Err(error) if unsupported(&error) => {
                            s.copied = true;
                        }
                        Err(error) if error.kind() == std::io::ErrorKind::Interrupted => (),
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            wait = Some((
                                if writing {
                                    s.destination.socket()
                                } else {
                                    s.source.socket()
                                },
                                if writing { libc::POLLOUT } else { libc::POLLIN },
                            ));
                            break;
                        }
                        Err(_) => return Err(Error::Io),
                    }
                }
                if s.source.rx_remaining == Some(0) && s.destination.tx_remaining == Some(0) {
                    break;
                }
                wait
            };
            if let Some((fd, interest)) = wait {
                // A bounded poll interval also notices reverse disconnect while
                // the downstream source is silent. It never extends the deadline.
                let mut tick = scope.clone();
                tick.deadline.0 = tick
                    .deadline
                    .0
                    .min(crate::runtime::environment::now() + Duration::from_millis(10));
                match self
                    .reactor()
                    .readiness_with_lease(fd, interest as u32, state.clone(), &tick)
                    .await
                {
                    Ok(_) | Err(Error::DeadlineExceeded) => (),
                    Err(error) => return Err(error),
                }
            } else {
                let mut yielded = false;
                std::future::poll_fn(|cx| {
                    if yielded {
                        Poll::Ready(())
                    } else {
                        yielded = true;
                        cx.waker().wake_by_ref();
                        Poll::Pending
                    }
                })
                .await;
            }
        }
        scope.check()?;
        let mut state = Rc::try_unwrap(state)
            .map_err(|_| Error::Internal)?
            .into_inner();
        // Both finish checks must pass before either connection can be pooled.
        finish(&mut state.source, &mut state.destination)?;
        state.destination.relay_reservation = None;
        Ok(state.destination)
    }
}
fn finish(source: &mut ConnectionLease, destination: &mut ConnectionLease) -> Result<()> {
    if let Err(error) = source
        .finish_exchange()
        .and_then(|()| destination.finish_exchange())
    {
        source.poison();
        destination.poison();
        return Err(error);
    }
    Ok(())
}
