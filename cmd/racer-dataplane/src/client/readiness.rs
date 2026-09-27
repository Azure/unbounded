//! Level-triggered listener index. Idle turns wait on one completion-owned poll.
use super::*;
use crate::runtime::reactor::Descriptor;

#[derive(Default)]
pub(super) struct ReadyListeners {
    generation: u64,
    fd: Option<Rc<Descriptor>>,
    listeners: Vec<std::rc::Weak<BoundListener>>,
    ready: VecDeque<usize>,
    wait: Option<Operation<'static, u32>>,
    scope: Option<RequestScope>,
}
impl Drop for ReadyListeners {
    fn drop(&mut self) {
        if let Some(scope) = &self.scope {
            let _ = scope.cancel();
        }
    }
}
impl ReadyListeners {
    pub fn next(
        &mut self,
        owner: &ClientListeners,
        cx: &mut Context<'_>,
        budget: usize,
    ) -> Result<Option<Rc<BoundListener>>> {
        if self.generation != owner.generation.get() {
            if let Some(scope) = self.scope.take() {
                scope.cancel()?;
            }
            self.wait.take();
            self.ready.clear();
            self.listeners = owner
                .listeners
                .borrow()
                .values()
                .map(Rc::downgrade)
                .collect();
            // SAFETY: epoll_create1 returns a uniquely owned descriptor.
            let fd = unsafe { libc::epoll_create1(libc::EPOLL_CLOEXEC) };
            if fd < 0 {
                return Err(Error::Io);
            }
            let fd = Rc::new(unsafe { Descriptor::from_raw_fd(fd) });
            for (index, listener) in self.listeners.iter().enumerate() {
                let listener = listener.upgrade().ok_or(Error::Unavailable)?;
                let socket = match &listener.listener {
                    Listener::Real(socket) => socket,
                    #[cfg(test)]
                    _ => return Err(Error::InvalidConfiguration),
                };
                let mut event = libc::epoll_event {
                    events: libc::EPOLLIN as u32,
                    u64: index as u64,
                };
                // SAFETY: both descriptors and the initialized event live through ctl.
                if unsafe {
                    libc::epoll_ctl(
                        fd.as_raw_fd(),
                        libc::EPOLL_CTL_ADD,
                        socket.as_raw_fd(),
                        &mut event,
                    )
                } < 0
                {
                    return Err(Error::Io);
                }
            }
            self.fd = Some(fd);
            self.scope = Some(new_scope(
                Duration::from_secs(365 * 24 * 3600),
                Cancellation::new()?,
            )?);
            self.generation = owner.generation.get();
        }
        if let Some(index) = self.ready.pop_front() {
            return Ok(self.listeners[index].upgrade());
        }
        if let Some(wait) = &mut self.wait {
            match wait.as_mut().poll(cx) {
                Poll::Pending => return Ok(None),
                Poll::Ready(result) => {
                    result?;
                    self.wait.take();
                }
            }
        }
        let Some(fd) = &self.fd else {
            return Ok(None);
        };
        let mut events = [libc::epoll_event { events: 0, u64: 0 }; 64];
        // SAFETY: events is writable for the bounded requested event count.
        let count = unsafe {
            libc::epoll_wait(
                fd.as_raw_fd(),
                events.as_mut_ptr(),
                budget.clamp(1, 64) as i32,
                0,
            )
        };
        if count < 0 {
            return Err(Error::Io);
        }
        self.ready.extend(
            events[..count as usize]
                .iter()
                .map(|event| event.u64 as usize),
        );
        if let Some(index) = self.ready.pop_front() {
            return Ok(self.listeners[index].upgrade());
        }
        let reactor = owner.io.reactor().clone();
        let fd = fd.clone();
        let scope = self.scope.as_ref().ok_or(Error::Internal)?.clone();
        self.wait = Some(Box::pin(async move {
            reactor
                .readiness_with_lease(fd.clone(), libc::POLLIN as u32, fd, &scope)
                .await
        }));
        if let Some(wait) = &mut self.wait {
            let _ = wait.as_mut().poll(cx);
        }
        Ok(None)
    }
}
