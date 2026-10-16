//! Level-triggered host descriptor index with one completion-owned idle wait.
//! Callers own registration lifetimes, generation changes, scope cancellation,
//! and retry policy. This index never retains registered descriptor owners.
use super::descriptor::Descriptor;
use crate::{Error, Operation, Result};
use std::{
    collections::{BTreeMap, VecDeque},
    os::fd::{AsRawFd, FromRawFd, RawFd},
    rc::Rc,
    task::{Context, Poll},
};

/// A host-only, level-triggered readiness index with one owned idle wait.
/// Registered descriptors remain caller-owned and must be removed before reuse.
pub struct ReadySet<E = Error> {
    fd: Rc<Descriptor>,

    ready: VecDeque<usize>,

    wait: Option<Operation<'static, u32, E>>,

    registrations: BTreeMap<RawFd, usize>,
}

impl<E: From<Error>> ReadySet<E> {
    /// Create an empty, close-on-exec epoll index without registering handles.
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

    /// Register a host descriptor with a unique caller-chosen index. Remove it
    /// before closing or reusing its FD/index. Closing does not remove an epoll
    /// registration while duplicated aliases retain the open file description.
    /// This host-only index retains no registered descriptor owners.
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
        // SAFETY: initialized event lives through ctl. Invalid FDs fail ctl.
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

    /// Abandon an idle wait and discard cached indices, without deregistration.
    /// Cancel the caller-owned scope first; accepted I/O retains its CQE leases.
    pub fn clear(&mut self) {
        self.wait.take();
        self.ready.clear();
    }

    /// Drain at most 64 events per refill, using one idle wait when none are ready.
    /// `wait` must retain the epoll owner through its completion fence. Completed
    /// idle waits are hints to refill. Zero budget neither polls nor creates work.
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
        if self.poll_wait(cx)?.is_pending() {
            return Ok(None);
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
        if self.poll_wait(cx)?.is_ready() {
            cx.waker().wake_by_ref();
        }
        Ok(None)
    }

    /// Consume completed waits before propagating failure, preventing repolling.
    /// With no wait the caller can inspect the level-triggered index immediately.
    fn poll_wait(&mut self, cx: &mut Context<'_>) -> Result<Poll<()>, E> {
        let Some(wait) = &mut self.wait else {
            return Ok(Poll::Ready(()));
        };
        match wait.as_mut().poll(cx) {
            Poll::Pending => Ok(Poll::Pending),
            Poll::Ready(result) => {
                self.wait.take();
                result.map(|_| Poll::Ready(()))
            }
        }
    }
}

#[cfg(test)]
mod tests {
    //! Level-triggered registration, cached-event, and idle-wait ownership contracts.
    use super::*;
    use std::{
        cell::Cell,
        io::{Read, Write},
        os::unix::net::UnixStream,
    };

    #[test]
    /// Consume an immediate idle completion before requesting another refill.
    fn immediately_completed_idle_wait_is_consumed_and_requests_a_refill() {
        let mut set = ReadySet::<Error>::new().unwrap();
        let counter = std::sync::Arc::new(crate::test_util::WakeCounter::default());
        let waker = std::task::Waker::from(counter.clone());
        let mut cx = Context::from_waker(&waker);
        assert_eq!(
            set.next(&mut cx, 1, |_| Box::pin(async { Ok(0) })),
            Ok(None)
        );
        assert!(set.wait.is_none());
        assert_eq!(counter.count(), 1);
        assert_eq!(
            set.next(&mut cx, 1, |_| Box::pin(std::future::pending())),
            Ok(None)
        );
        assert!(set.wait.is_some());
        assert_eq!(counter.count(), 1);
    }

    #[test]
    /// Verify repeated level readiness without retaining registered socket owners.
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
            set.next(&mut cx, 1, |_| Box::pin(async { Err(Error::Canceled) })),
            Err(Error::Canceled)
        );
    }

    #[test]
    /// Drain cached readiness before refilling and discard it on clear.
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
    /// Keep the epoll owner alive until an abandoned idle operation is fenced.
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
        request.cancel();
        drop(set);
        assert!(weak.upgrade().is_some());
        drive(&reactor, reactor.fence_matching(|_| true)).unwrap();
        assert_eq!(reactor.in_flight(), 0);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    /// Remove cached events for each independently registered descriptor alias.
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
    /// Retire failed waits so later calls never poll completed futures again.
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
