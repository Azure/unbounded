//! Level-triggered host descriptor index with one completion-owned idle wait.
//!
//! Callers own registration lifetimes, generation changes, scope cancellation,
//! and retry policy. This index never retains the registered descriptor owners.
use super::Descriptor;
use crate::{Error, Operation, Result};
use std::{
    collections::{BTreeMap, VecDeque},
    os::fd::{AsRawFd, FromRawFd, RawFd},
    rc::Rc,
    task::{Context, Poll},
};

pub struct ReadySet<E = Error> {
    fd: Rc<Descriptor>,
    ready: VecDeque<usize>,
    wait: Option<Operation<'static, u32, E>>,
    registrations: BTreeMap<RawFd, usize>,
}

impl<E: From<Error>> ReadySet<E> {
    pub fn new() -> Result<Self, E> {
        // SAFETY: epoll_create1 returns a uniquely owned descriptor.
        let fd = unsafe { libc::epoll_create1(libc::EPOLL_CLOEXEC) };
        if fd < 0 {
            return Err(Error::Io.into());
        }
        Ok(Self {
            fd: Rc::new(unsafe { Descriptor::from_raw_fd(fd) }),
            ready: VecDeque::new(),
            wait: None,
            registrations: BTreeMap::new(),
        })
    }

    /// Register a host descriptor with a unique caller-chosen index. Call remove
    /// before closing or reusing the FD/index. Closing an FD does NOT remove an
    /// epoll registration while dup'd aliases retain the open file description.
    /// The caller owns all descriptors; this host-only index does not retain them.
    pub fn insert(&mut self, fd: RawFd, index: usize) -> Result<(), E> {
        if self
            .registrations
            .values()
            .any(|existing| *existing == index)
        {
            return Err(Error::AlreadyExists.into());
        }
        let mut event = libc::epoll_event {
            events: libc::EPOLLIN as u32,
            u64: index as u64,
        };
        // SAFETY: the initialized event lives through ctl. Invalid FDs fail ctl.
        if unsafe { libc::epoll_ctl(self.fd.as_raw_fd(), libc::EPOLL_CTL_ADD, fd, &mut event) } < 0
        {
            return Err(Error::Io.into());
        }
        self.registrations.insert(fd, index);
        Ok(())
    }

    /// Deregister while `fd` still names the registered open file description,
    /// discarding any cached event. Unknown descriptors return NotFound.
    pub fn remove(&mut self, fd: RawFd) -> Result<(), E> {
        let index = *self.registrations.get(&fd).ok_or(Error::NotFound)?;
        // SAFETY: DEL ignores its event argument; fd must still be live.
        if unsafe {
            libc::epoll_ctl(
                self.fd.as_raw_fd(),
                libc::EPOLL_CTL_DEL,
                fd,
                std::ptr::null_mut(),
            )
        } < 0
        {
            return Err(Error::Io.into());
        }
        self.registrations.remove(&fd);
        self.ready.retain(|queued| *queued != index);
        Ok(())
    }

    /// Abandon an idle wait and discard queued indices before rebuilding.
    /// This does not deregister descriptors; use remove before closing each one.
    /// Cancel its caller-owned scope first; accepted I/O retains its CQE leases.
    pub fn clear(&mut self) {
        self.wait.take();
        self.ready.clear();
    }

    /// Drain at most 64 events per refill, using one idle wait when none are ready.
    /// `wait` must retain the supplied epoll owner through the completion fence.
    /// A completed idle wait is only a hint: the next turn refills the index.
    /// A zero budget performs no polling, consumes no indices, and creates no wait.
    pub fn next(
        &mut self,
        cx: &mut Context<'_>,
        budget: usize,
        wait: impl FnOnce(Rc<Descriptor>) -> Operation<'static, u32, E>,
    ) -> Result<Option<usize>, E> {
        if budget == 0 {
            return Ok(None);
        }
        if let Some(index) = self.ready.pop_front() {
            return Ok(Some(index));
        }
        if let Some(wait) = &mut self.wait {
            match wait.as_mut().poll(cx) {
                Poll::Pending => return Ok(None),
                Poll::Ready(result) => {
                    self.wait.take();
                    result?;
                }
            }
        }
        let mut events = [libc::epoll_event { events: 0, u64: 0 }; 64];
        // SAFETY: events is writable for the bounded requested event count.
        let count = unsafe {
            libc::epoll_wait(
                self.fd.as_raw_fd(),
                events.as_mut_ptr(),
                budget.clamp(1, 64) as i32,
                0,
            )
        };
        if count < 0 {
            if std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                cx.waker().wake_by_ref();
                return Ok(None);
            }
            return Err(Error::Io.into());
        }
        self.ready.extend(
            events[..count as usize]
                .iter()
                .map(|event| event.u64 as usize),
        );
        if let Some(index) = self.ready.pop_front() {
            return Ok(Some(index));
        }
        self.wait = Some(wait(self.fd.clone()));
        if let Some(wait) = &mut self.wait
            && let Poll::Ready(result) = wait.as_mut().poll(cx)
        {
            self.wait.take();
            result?;
            cx.waker().wake_by_ref();
        }
        Ok(None)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        cell::Cell,
        io::{Read, Write},
        os::unix::net::UnixStream,
    };

    #[test]
    fn indices_are_level_triggered_and_registration_does_not_retain_owners() {
        let mut set = ReadySet::<Error>::new().unwrap();
        let (mut reader, mut writer) = UnixStream::pair().unwrap();
        reader.set_nonblocking(true).unwrap();
        set.insert(reader.as_raw_fd(), 17).unwrap();
        assert_eq!(set.insert(reader.as_raw_fd(), 18), Err(Error::Io));
        assert_eq!(set.insert(-1, 19), Err(Error::Io));
        writer.write_all(b"x").unwrap();
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert_eq!(set.next(&mut cx, 0, |_| panic!("zero budget")), Ok(None));
        for budget in [1, usize::MAX] {
            assert_eq!(set.next(&mut cx, budget, |_| panic!("ready")), Ok(Some(17)));
        }
        reader.read_exact(&mut [0]).unwrap();
        let calls = Cell::new(0);
        for _ in 0..3 {
            assert_eq!(
                set.next(&mut cx, 1, |_| {
                    calls.set(calls.get() + 1);
                    Box::pin(std::future::pending())
                }),
                Ok(None)
            );
        }
        assert_eq!(calls.get(), 1);
        set.clear();
        drop(reader);
        assert_eq!(
            set.next(&mut cx, 1, |_| Box::pin(async { Err(Error::Cancelled) })),
            Err(Error::Cancelled)
        );
    }

    #[test]
    fn ready_batch_is_drained_before_refilling_and_clear_discards_it() {
        let mut set = ReadySet::<Error>::new().unwrap();
        let mut sockets = Vec::new();
        for index in 0..3 {
            let (reader, mut writer) = UnixStream::pair().unwrap();
            set.insert(reader.as_raw_fd(), index).unwrap();
            writer.write_all(b"x").unwrap();
            sockets.push((reader, writer));
        }
        let mut cx = Context::from_waker(std::task::Waker::noop());
        let mut indices = Vec::new();
        for _ in 0..3 {
            indices.push(set.next(&mut cx, 3, |_| panic!("ready")).unwrap().unwrap());
        }
        indices.sort();
        assert_eq!(indices, [0, 1, 2]);
        set.next(&mut cx, 3, |_| panic!("ready")).unwrap();
        assert_eq!(set.ready.len(), 2);
        set.clear();
        assert!(set.ready.is_empty());
    }

    #[test]
    fn abandoned_idle_wait_retains_epoll_until_completion_fence() {
        use crate::reactor::tests::{drive, kernel_reactor, scope};
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        let reactor = Rc::new(reactor);
        let request = scope();
        let mut set = ReadySet::<Error>::new().unwrap();
        let weak = Rc::downgrade(&set.fd);
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert_eq!(
            set.next(&mut cx, 1, |fd| {
                let reactor = reactor.clone();
                let request = request.clone();
                Box::pin(async move {
                    reactor
                        .readiness_with_lease(fd.clone(), libc::POLLIN as u32, fd, &request)
                        .await
                })
            }),
            Ok(None)
        );
        assert_eq!(reactor.in_flight(), 1);
        request.cancel().unwrap();
        drop(set);
        assert!(weak.upgrade().is_some());
        drive(&reactor, reactor.file_fence(())).unwrap();
        assert_eq!(reactor.in_flight(), 0);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn remove_discards_cached_events_and_handles_duplicated_fds() {
        let mut set = ReadySet::<Error>::new().unwrap();
        let (reader, mut writer) = UnixStream::pair().unwrap();
        let duplicate = reader.try_clone().unwrap();
        set.insert(reader.as_raw_fd(), 5).unwrap();
        assert_eq!(
            set.insert(duplicate.as_raw_fd(), 5),
            Err(Error::AlreadyExists)
        );
        set.insert(duplicate.as_raw_fd(), 6).unwrap();
        writer.write_all(b"ready").unwrap();
        let mut cx = Context::from_waker(std::task::Waker::noop());
        set.next(&mut cx, 2, |_| panic!("ready")).unwrap();
        assert_eq!(set.ready.len(), 1);
        set.remove(reader.as_raw_fd()).unwrap();
        set.remove(duplicate.as_raw_fd()).unwrap();
        assert!(set.ready.is_empty());
        assert_eq!(set.remove(reader.as_raw_fd()), Err(Error::NotFound));
        assert_eq!(
            set.next(&mut cx, 1, |_| Box::pin(std::future::pending())),
            Ok(None)
        );
    }

    #[test]
    fn failed_idle_wait_is_consumed_not_polled_again() {
        let mut set = ReadySet::<Error>::new().unwrap();
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert_eq!(
            set.next(&mut cx, 1, |_| Box::pin(async { Err(Error::Io) })),
            Err(Error::Io)
        );
        assert!(set.wait.is_none());
        assert_eq!(
            set.next(&mut cx, 1, |_| Box::pin(std::future::pending())),
            Ok(None)
        );
    }
}
