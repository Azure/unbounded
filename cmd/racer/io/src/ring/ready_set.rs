//! A level-triggered epoll index over many host descriptors with one
//! completion-owned ring wait, for listeners that would otherwise need one
//! outstanding operation per descriptor.
use super::{Descriptor, OpCtx, Reactor, ops::PollReady};
use crate::{Error, Result};
use std::{
    collections::{BTreeMap, VecDeque},
    future::Future,
    os::fd::{FromRawFd, RawFd},
    pin::Pin,
    rc::Rc,
    task::{Context, Poll},
};

/// Registered descriptors stay caller-owned and must be removed before reuse.
pub struct ReadySet {
    fd: Rc<Descriptor>,

    ready: VecDeque<usize>,

    wait: Option<PollReady<()>>,

    registrations: BTreeMap<RawFd, usize>,
}

impl ReadySet {
    /// An empty, close-on-exec epoll index (host only).
    pub fn new() -> Result<Self> {
        // SAFETY: epoll_create1 returns a uniquely owned descriptor.
        let fd = unsafe { libc::epoll_create1(libc::EPOLL_CLOEXEC) };
        if fd < 0 {
            return Err(Error::last_os_error());
        }
        Ok(Self {
            // SAFETY: the descriptor was just created.
            fd: Rc::new(unsafe { Descriptor::from_raw_fd(fd) }),
            ready: VecDeque::new(),
            wait: None,
            registrations: BTreeMap::new(),
        })
    }

    /// Register `fd` under a unique caller-chosen `index`. Remove it before
    /// closing or reusing the descriptor or index.
    pub fn insert(&mut self, fd: RawFd, index: usize) -> Result<()> {
        if self
            .registrations
            .values()
            .any(|existing| *existing == index)
        {
            return Err(Error::AlreadyExists);
        }
        let mut event = libc::epoll_event {
            events: libc::EPOLLIN as u32,
            u64: index as u64,
        };
        // SAFETY: the event lives through the call; invalid descriptors fail.
        if unsafe { libc::epoll_ctl(self.fd.raw(), libc::EPOLL_CTL_ADD, fd, &mut event) } < 0 {
            return Err(Error::last_os_error());
        }
        self.registrations.insert(fd, index);
        Ok(())
    }

    /// Deregister `fd`, discarding any cached event for it.
    pub fn remove(&mut self, fd: RawFd) -> Result<()> {
        let index = *self.registrations.get(&fd).ok_or(Error::NotFound)?;
        // SAFETY: DEL ignores its event argument; fd must still be open.
        if unsafe { libc::epoll_ctl(self.fd.raw(), libc::EPOLL_CTL_DEL, fd, std::ptr::null_mut()) }
            < 0
        {
            return Err(Error::last_os_error());
        }
        self.registrations.remove(&fd);
        self.ready.retain(|queued| *queued != index);
        Ok(())
    }

    /// Abandon the idle wait and discard cached indices.
    pub fn clear(&mut self) {
        self.wait.take();
        self.ready.clear();
    }

    /// Next ready index, refilling at most 64 events at a time; with none
    /// ready, arm one ring poll on the epoll descriptor and return `None`
    /// (the task is woken when it completes).
    pub fn next(
        &mut self,
        cx: &mut Context<'_>,
        budget: usize,
        reactor: &Reactor,
        ctx: &OpCtx,
    ) -> Result<Option<usize>> {
        if budget == 0 {
            return Ok(None);
        }
        if let Some(index) = self.ready.pop_front() {
            return Ok(Some(index));
        }
        if self.poll_wait(cx)?.is_pending() {
            return Ok(None);
        }
        self.refill(budget.clamp(1, 64))?;
        if let Some(index) = self.ready.pop_front() {
            return Ok(Some(index));
        }
        self.wait = Some(reactor.poll_ready(self.fd.clone(), libc::POLLIN as u32, (), ctx));
        if self.poll_wait(cx)?.is_ready() {
            cx.waker().wake_by_ref();
        }
        Ok(None)
    }

    /// Whether an index is cached or ready now (without waiting).
    pub fn has_ready(&mut self) -> Result<bool> {
        if !self.ready.is_empty() {
            return Ok(true);
        }
        if self.wait.is_some() {
            return Ok(false);
        }
        self.refill(64)?;
        Ok(!self.ready.is_empty())
    }

    fn refill(&mut self, max: usize) -> Result<()> {
        let mut events = [libc::epoll_event { events: 0, u64: 0 }; 64];
        // SAFETY: events is writable for `max` <= 64 entries.
        let count = unsafe { libc::epoll_wait(self.fd.raw(), events.as_mut_ptr(), max as i32, 0) };
        if count < 0 {
            if std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                return Ok(());
            }
            return Err(Error::last_os_error());
        }
        self.ready.extend(
            events[..count as usize]
                .iter()
                .map(|event| event.u64 as usize),
        );
        Ok(())
    }

    /// Consume a completed wait so it is never polled again.
    fn poll_wait(&mut self, cx: &mut Context<'_>) -> Result<Poll<()>> {
        let Some(wait) = &mut self.wait else {
            return Ok(Poll::Ready(()));
        };
        match Pin::new(wait).poll(cx) {
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
    use super::*;
    use std::{io::Write, os::fd::AsRawFd, os::unix::net::UnixStream};

    #[test]
    fn indices_are_level_triggered_and_waits_ride_the_ring() {
        let Some(reactor) = crate::ring::tests::kernel(8) else {
            return;
        };
        let mut set = ReadySet::new().unwrap();
        let (reader, mut writer) = UnixStream::pair().unwrap();
        set.insert(reader.as_raw_fd(), 17).unwrap();
        assert_eq!(
            set.insert(reader.as_raw_fd(), 18).unwrap_err(),
            Error::AlreadyExists
        );
        let ctx = OpCtx::none();
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert_eq!(set.next(&mut cx, 0, &reactor, &ctx), Ok(None));
        assert!(!set.has_ready().unwrap());
        assert_eq!(set.next(&mut cx, 1, &reactor, &ctx), Ok(None));
        assert!(set.wait.is_some());
        assert_eq!(reactor.in_flight(), 1);
        writer.write_all(b"x").unwrap();
        // Drive the ring until the idle wait completes.
        while reactor.in_flight() != 0 {
            reactor.reap(16).unwrap();
            if reactor.in_flight() != 0 {
                reactor.park(None).unwrap();
            }
        }
        assert_eq!(set.next(&mut cx, 1, &reactor, &ctx), Ok(Some(17)));
        assert_eq!(
            set.next(&mut cx, 1, &reactor, &ctx),
            Ok(Some(17)),
            "level triggered"
        );
        set.remove(reader.as_raw_fd()).unwrap();
        assert_eq!(set.remove(reader.as_raw_fd()).unwrap_err(), Error::NotFound);
        set.clear();
        assert!(set.ready.is_empty());
    }
}
