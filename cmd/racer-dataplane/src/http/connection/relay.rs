//! Racer-specific opaque relay. Both connections remain completion-owned.
use super::*;
use flow_control::pipe::{MAX_PIPE_BYTES, PipeLease};
struct Transit {
    source: ConnectionLease,
    destination: ConnectionLease,
    pipe: PipeLease<AdmissionPolicy>,
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
impl HttpIo {
    pub(crate) async fn relay_body(
        &self,
        source: ConnectionLease,
        destination: ConnectionLease,
        pipe: Option<PipeLease<AdmissionPolicy>>,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        if source.receive_remaining() != destination.send_remaining()
            || source.receive_remaining().is_none()
        {
            return Err(Error::InvalidRequest);
        }
        if source.receive_remaining() == Some(0) {
            let mut source = source;
            let mut destination = destination;
            finish(&mut source, &mut destination)?;
            destination.state_mut().relay_reservation = None;
            return Ok(destination);
        }
        #[cfg(test)]
        let copied = destination.state().relay_fallback;
        #[cfg(test)]
        let fallback_at = destination.state().relay_fallback_at;
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
            let wait = self.relay_chunk(&state)?;
            {
                let s = state.borrow();
                if s.source.receive_remaining() == Some(0)
                    && s.destination.send_remaining() == Some(0)
                {
                    break;
                }
            }
            self.wait_relay_progress(state.clone(), wait, scope).await?;
        }
        scope.check()?;
        let mut state = Rc::try_unwrap(state)
            .map_err(|_| Error::Internal)?
            .into_inner();
        finish(&mut state.source, &mut state.destination)?;
        state.destination.state_mut().relay_reservation = None;
        Ok(state.destination)
    }
    fn relay_chunk(&self, state: &RefCell<Transit>) -> Result<Option<(Rc<Descriptor>, i16)>> {
        let mut state = state.borrow_mut();
        let s = &mut *state;
        if s.destination.socket().peer_read_closed() {
            return Err(Error::Io);
        }
        let mut wait = None;
        for _ in 0..32 {
            let remaining = s.source.receive_remaining().ok_or(Error::InvalidRequest)? as usize;
            if s.pending.is_empty() && s.pipe.buffered() == 0 && remaining == 0 {
                break;
            }
            let writing = !s.pending.is_empty() || s.pipe.buffered() != 0;
            let result = if !s.pending.is_empty() {
                s.destination
                    .socket()
                    .try_send(&s.fallback.as_ref().unwrap().bytes()?[s.pending.clone()])
            } else if s.pipe.buffered() != 0 {
                #[cfg(test)]
                if s.fallback_at
                    .is_some_and(|threshold| remaining <= threshold)
                    && !s.copied
                {
                    s.copied = unsupported(&std::io::Error::from_raw_os_error(libc::EOPNOTSUPP));
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
                s.pipe.try_splice_connection(&s.destination.socket())
            } else if let Some((ahead, range)) = s.source.take_read_ahead() {
                let count = remaining.min(range.len()).min(MAX_PIPE_BYTES);
                if s.fallback.is_none() {
                    s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                }
                s.fallback.as_mut().unwrap().bytes_mut()?[..count]
                    .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                if range.len() > count {
                    // Preserve all excess so finish_exchange still rejects pipelining.
                    s.source
                        .restore_read_ahead(ahead, range.start + count..range.end)?;
                }
                s.pending = 0..count;
                s.source.consume_received(count)?;
                continue;
            } else if s.copied {
                if s.fallback.is_none() {
                    s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                }
                s.source.socket().try_recv(
                    &mut s.fallback.as_mut().unwrap().bytes_mut()?[..remaining.min(MAX_PIPE_BYTES)],
                )
            } else {
                s.pipe.try_splice_from(&s.source.socket(), remaining)
            };
            match result {
                Ok(0) => return Err(Error::Io),
                Ok(n) => {
                    if writing {
                        if !s.pending.is_empty() {
                            s.pending.start += n;
                        }
                        s.destination.consume_sent(n)?;
                    } else {
                        s.source.consume_received(n)?;
                        if s.copied {
                            s.pending = 0..n;
                        }
                    }
                }
                Err(error) if unsupported(&error) => s.copied = true,
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
        Ok(wait)
    }
    async fn wait_relay_progress(
        &self,
        state: Rc<RefCell<Transit>>,
        wait: Option<(Rc<Descriptor>, i16)>,
        scope: &RequestScope,
    ) -> Result<()> {
        if let Some((fd, interest)) = wait {
            let mut tick = scope.clone();
            tick.deadline.0 = tick
                .deadline
                .0
                .min(uring_runtime::environment::now() + Duration::from_millis(10));
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
        Ok(())
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
