//! State-machine tests against a scripted backend. These tests make no
//! syscalls and run under Miri.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::rc::{Rc, Weak};
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Wake, Waker};
use std::time::{Duration, Instant};

use super::*;
use crate::backend::{
    BackendConfig, CompletionEntry, Op, OpKind, PushError, PushHints, RemoteWaker,
};
use crate::{
    BufResult, CancelToken, Entropy, ErrorKind, IoBuf, IoBufMut, SocketAddress, SystemClock,
    SystemEntropy, Unscoped,
};

type MockFd = Rc<i32>;

/// One pushed operation, with the buffer pointer a kernel would use.
#[derive(Clone, Debug)]
struct Pushed {
    user_data: u64,
    kind: OpKind,
    buf: Option<(*mut u8, u32)>,
    /// The bytes a vectored write gathers, captured at push.
    gathered: Option<Vec<u8>>,
}

#[derive(Default)]
struct MockState {
    /// Pushed but not submitted.
    queued: Vec<Pushed>,
    /// Submitted, awaiting a scripted completion.
    pending: Vec<Pushed>,
    /// Cancel requests `(user_data, target)` submitted.
    cancels: Vec<(u64, u64)>,
    completions: VecDeque<CompletionEntry>,
    /// Push returns `Full` this many more times.
    full: usize,
    cancel_full: usize,
    unsupported: bool,
    submit_error: Option<Error>,
    submits: usize,
    waits: usize,
    /// Complete canceled targets with -ECANCELED automatically on submit.
    auto_cancel: bool,
    adopted: Vec<Weak<i32>>,
    /// Hints of every `push_with` call, in order.
    hints: Vec<(OpKind, PushHints)>,
}

/// A scripted backend. Tests keep a [`Handle`] to drive it.
struct Mock {
    state: Rc<RefCell<MockState>>,
    remote: Arc<AtomicUsize>,
}

#[derive(Clone)]
struct Handle {
    state: Rc<RefCell<MockState>>,
    remote: Arc<AtomicUsize>,
}

impl Handle {
    fn pending(&self) -> Vec<Pushed> {
        self.state.borrow().pending.clone()
    }

    fn only_pending(&self) -> Pushed {
        let pending = self.pending();
        assert_eq!(pending.len(), 1, "{pending:?}");
        pending[0].clone()
    }

    fn cancels(&self) -> Vec<(u64, u64)> {
        self.state.borrow().cancels.clone()
    }

    fn complete(&self, user_data: u64, result: i32) {
        let mut state = self.state.borrow_mut();
        state.pending.retain(|op| op.user_data != user_data);
        state.completions.push_back(CompletionEntry {
            user_data,
            result,
            flags: 0,
        });
    }

    fn complete_cancel(&self, target: u64, result: i32) {
        let mut state = self.state.borrow_mut();
        let index = state
            .cancels
            .iter()
            .position(|(_, t)| *t == target)
            .expect("cancel was pushed");
        let (user_data, _) = state.cancels.remove(index);
        state.completions.push_back(CompletionEntry {
            user_data,
            result,
            flags: 0,
        });
    }

    /// Write `bytes` into the buffer of a pending read, like the kernel.
    fn fill(&self, op: &Pushed, bytes: &[u8]) {
        let (ptr, len) = op.buf.expect("operation has a buffer");
        assert!(bytes.len() <= len as usize);
        // SAFETY: the reactor keeps the buffer alive and unaliased until the
        // completion is popped.
        unsafe { ptr.copy_from_nonoverlapping(bytes.as_ptr(), bytes.len()) };
    }
}

fn mock() -> (Mock, Handle) {
    let state = Rc::new(RefCell::new(MockState::default()));
    let remote = Arc::new(AtomicUsize::new(0));
    (
        Mock {
            state: state.clone(),
            remote: remote.clone(),
        },
        Handle { state, remote },
    )
}

impl Backend for Mock {
    type Fd = MockFd;
    type Clock = SystemClock;
    type Entropy = SystemEntropy;

    fn clock(&self) -> &SystemClock {
        &SystemClock
    }

    fn entropy(&self) -> &SystemEntropy {
        &SystemEntropy
    }

    fn init(&mut self, config: &BackendConfig) -> Result<()> {
        assert!(config.capacity >= 2);
        Ok(())
    }

    unsafe fn push(&mut self, user_data: u64, op: &Op<'_, MockFd>) -> Result<(), PushError> {
        let mut state = self.state.borrow_mut();
        if state.unsupported {
            return Err(PushError::Unsupported);
        }
        if state.full > 0 {
            state.full -= 1;
            return Err(PushError::Full);
        }
        let buf = match *op {
            Op::Read { buf, len, .. } | Op::Recv { buf, len, .. } => Some((buf, len)),
            Op::Write { buf, len, .. } | Op::Send { buf, len, .. } => Some((buf.cast_mut(), len)),
            Op::Timeout { timespec, .. } => {
                // SAFETY: the reactor keeps the scratch valid until popped.
                unsafe { timespec.write(Default::default()) };
                None
            }
            _ => None,
        };
        let gathered = match *op {
            Op::Writev { iovecs, count, .. } => {
                // SAFETY: the reactor keeps `count` iovecs and their bytes
                // valid until the completion is popped.
                let iovecs = unsafe { std::slice::from_raw_parts(iovecs, count as usize) };
                let mut bytes = Vec::new();
                for iov in iovecs {
                    // SAFETY: as above, each segment is valid for `iov_len`.
                    bytes.extend_from_slice(unsafe {
                        std::slice::from_raw_parts(iov.iov_base.cast::<u8>(), iov.iov_len)
                    });
                }
                Some(bytes)
            }
            _ => None,
        };
        state.queued.push(Pushed {
            user_data,
            kind: op.kind(),
            buf,
            gathered,
        });
        Ok(())
    }

    unsafe fn push_with(
        &mut self,
        user_data: u64,
        op: &Op<'_, MockFd>,
        hints: PushHints,
    ) -> Result<(), PushError> {
        self.state.borrow_mut().hints.push((op.kind(), hints));
        // SAFETY: forwarded from our caller.
        unsafe { self.push(user_data, op) }
    }

    fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError> {
        let mut state = self.state.borrow_mut();
        if state.cancel_full > 0 {
            state.cancel_full -= 1;
            return Err(PushError::Full);
        }
        state.cancels.push((user_data, target));
        Ok(())
    }

    unsafe fn adopt_fd(&mut self, raw: i32) -> MockFd {
        let fd = Rc::new(raw);
        self.state.borrow_mut().adopted.push(Rc::downgrade(&fd));
        fd
    }

    fn submit(&mut self) -> Result<usize> {
        let mut state = self.state.borrow_mut();
        state.submits += 1;
        if let Some(error) = state.submit_error.take() {
            return Err(error);
        }
        let queued = std::mem::take(&mut state.queued);
        let n = queued.len();
        state.pending.extend(queued);
        if state.auto_cancel {
            for (user_data, target) in std::mem::take(&mut state.cancels) {
                state.completions.push_back(CompletionEntry {
                    user_data,
                    result: 0,
                    flags: 0,
                });
                if let Some(index) = state.pending.iter().position(|op| op.user_data == target) {
                    state.pending.remove(index);
                    state.completions.push_back(CompletionEntry {
                        user_data: target,
                        result: -libc::ECANCELED,
                        flags: 0,
                    });
                }
            }
        }
        Ok(n)
    }

    fn pop(&mut self) -> Option<CompletionEntry> {
        self.state.borrow_mut().completions.pop_front()
    }

    fn wait(&mut self, _: Duration) -> Result<()> {
        self.state.borrow_mut().waits += 1;
        Ok(())
    }

    fn remote_waker(&self) -> Result<RemoteWaker> {
        let remote = self.remote.clone();
        Ok(RemoteWaker::new(move || {
            remote.fetch_add(1, Ordering::SeqCst);
        }))
    }

    fn supports(&self, _: OpKind) -> bool {
        true
    }
}

fn reactor_with(config: ReactorConfig) -> (Reactor<Mock>, Handle) {
    let (backend, handle) = mock();
    (Reactor::new(backend, config).unwrap(), handle)
}

/// Cancel and fence everything still in flight, so that tests do not leak.
fn settle(reactor: &Reactor<Mock>, mock: &Handle) {
    mock.state.borrow_mut().auto_cancel = true;
    reactor.shutdown(Duration::from_secs(5)).unwrap();
}

fn reactor() -> (Reactor<Mock>, Handle) {
    reactor_with(ReactorConfig::default())
}

/// Counts wakes.
#[derive(Default)]
struct Counter(AtomicUsize);

impl Wake for Counter {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}

impl Counter {
    fn count(&self) -> usize {
        self.0.load(Ordering::SeqCst)
    }
}

fn poll<F: Future + Unpin>(future: &mut F, waker: &Waker) -> Poll<F::Output> {
    Pin::new(future).poll(&mut Context::from_waker(waker))
}

fn poll_noop<F: Future + Unpin>(future: &mut F) -> Poll<F::Output> {
    poll(future, Waker::noop())
}

fn ready<T>(poll: Poll<T>) -> T {
    match poll {
        Poll::Ready(value) => value,
        Poll::Pending => panic!("expected Ready"),
    }
}

/// A buffer that records when it is dropped.
struct Tracked {
    bytes: Vec<u8>,
    dropped: Rc<Cell<bool>>,
}

impl Tracked {
    fn new(capacity: usize) -> (Self, Rc<Cell<bool>>) {
        let dropped = Rc::new(Cell::new(false));
        (
            Self {
                bytes: Vec::with_capacity(capacity),
                dropped: dropped.clone(),
            },
            dropped,
        )
    }
}

impl Drop for Tracked {
    fn drop(&mut self) {
        self.dropped.set(true);
    }
}

// SAFETY: forwards to the Vec, whose heap storage is stable across moves.
unsafe impl IoBuf for Tracked {
    fn stable_ptr(&self) -> *const u8 {
        self.bytes.stable_ptr()
    }

    fn bytes_init(&self) -> usize {
        self.bytes.bytes_init()
    }
}

// SAFETY: forwards to the Vec.
unsafe impl IoBufMut for Tracked {
    fn stable_mut_ptr(&mut self) -> *mut u8 {
        self.bytes.stable_mut_ptr()
    }

    fn bytes_total(&mut self) -> usize {
        self.bytes.bytes_total()
    }

    unsafe fn set_init(&mut self, len: usize) {
        // SAFETY: forwarded from the caller.
        unsafe { self.bytes.set_init(len) };
    }
}

/// A request scope with an id and a manual failure switch.
#[derive(Clone, Debug)]
struct Request {
    id: u32,
    failed: Rc<Cell<bool>>,
    checks: Rc<Cell<usize>>,
}

impl Request {
    fn new(id: u32) -> Self {
        Self {
            id,
            failed: Rc::default(),
            checks: Rc::default(),
        }
    }
}

impl Scope for Request {
    type Error = Error;

    fn check(&self) -> Result<()> {
        self.checks.set(self.checks.get() + 1);
        if self.failed.get() {
            Err(Error::new(ErrorKind::DeadlineExceeded))
        } else {
            Ok(())
        }
    }
}

#[test]
fn read_returns_bytes_and_buffer() {
    let (reactor, mock) = reactor();
    let fd: MockFd = Rc::new(3);
    let counter = Arc::new(Counter::default());
    let waker = Waker::from(counter.clone());
    let mut read = reactor.read_at(&fd, 10, Vec::with_capacity(8), &Unscoped);
    assert!(!read.is_submitted());
    assert!(poll(&mut read, &waker).is_pending());
    assert!(read.is_submitted());
    assert_eq!(reactor.in_flight(), 1);
    assert!(
        mock.pending().is_empty(),
        "queued until the reactor submits"
    );
    assert_eq!(reactor.poll(16).unwrap(), 0);
    let op = mock.only_pending();
    assert_eq!(op.kind, OpKind::Read);
    assert_eq!(op.buf.unwrap().1, 8);
    assert_eq!(
        Rc::strong_count(&fd),
        2,
        "the operation holds a descriptor clone"
    );
    mock.fill(&op, b"abc");
    mock.complete(op.user_data, 3);
    assert_eq!(reactor.poll(16).unwrap(), 1);
    assert_eq!(counter.count(), 1);
    assert_eq!(reactor.in_flight(), 0);
    let BufResult { result, buf } = ready(poll(&mut read, &waker));
    assert_eq!(result.unwrap(), 3);
    assert_eq!(buf, b"abc");
    drop(read);
    assert_eq!(Rc::strong_count(&fd), 1);
}

#[test]
fn every_operation_round_trips() {
    let (reactor, mock) = reactor();
    let fd: MockFd = Rc::new(3);
    let scope = Unscoped;

    let mut write = reactor.write_at(&fd, 0, b"hi".as_slice(), &scope);
    let mut send = reactor.send(&fd, vec![1, 2, 3], &scope);
    let mut recv = reactor.recv(&fd, Vec::with_capacity(4), &scope);
    let mut readiness = reactor.readiness(&fd, libc::POLLIN as u32, &scope);
    let mut connect = reactor.connect(
        &fd,
        &SocketAddress::from(std::net::SocketAddr::from(([127, 0, 0, 1], 80))),
        &scope,
    );
    let mut sleep = reactor.sleep_until(Instant::now(), &scope);
    let mut fsync = reactor.fsync(&fd, true, &scope);
    let mut rename = reactor.rename_at(None, c"a", Some(&fd), c"b", 0, &scope);
    let mut unlink = reactor.unlink_at(None, c"a", 0, &scope);
    let mut mkdir = reactor.mkdir_at(None, c"d", 0o700, &scope);
    let mut statx = reactor.statx(None, c"s", 0, libc::STATX_SIZE, &scope);
    let mut nop = reactor.nop(&scope);
    assert!(poll_noop(&mut write).is_pending());
    assert!(poll_noop(&mut send).is_pending());
    assert!(poll_noop(&mut recv).is_pending());
    assert!(poll_noop(&mut readiness).is_pending());
    assert!(poll_noop(&mut connect).is_pending());
    assert!(poll_noop(&mut sleep).is_pending());
    assert!(poll_noop(&mut fsync).is_pending());
    assert!(poll_noop(&mut rename).is_pending());
    assert!(poll_noop(&mut unlink).is_pending());
    assert!(poll_noop(&mut mkdir).is_pending());
    assert!(poll_noop(&mut statx).is_pending());
    assert!(poll_noop(&mut nop).is_pending());
    reactor.poll(0).unwrap();
    let pending = mock.pending();
    let kinds: Vec<OpKind> = pending.iter().map(|op| op.kind).collect();
    assert_eq!(
        kinds,
        [
            OpKind::Write,
            OpKind::Send,
            OpKind::Recv,
            OpKind::PollAdd,
            OpKind::Connect,
            OpKind::Timeout,
            OpKind::Fsync,
            OpKind::RenameAt,
            OpKind::UnlinkAt,
            OpKind::MkDirAt,
            OpKind::Statx,
            OpKind::Nop,
        ]
    );
    mock.fill(&pending[2], b"xy");
    let results = [
        1,
        3,
        2,
        i32::from(libc::POLLIN),
        0,
        -libc::ETIME,
        0,
        0,
        -libc::ENOENT,
        0,
        0,
        0,
    ];
    for (op, result) in pending.iter().zip(results) {
        mock.complete(op.user_data, result);
    }
    assert_eq!(reactor.poll(64).unwrap(), 12);
    let write = ready(poll_noop(&mut write));
    assert_eq!((write.result.unwrap(), write.buf), (1, b"hi".as_slice()));
    let send = ready(poll_noop(&mut send));
    assert_eq!((send.result.unwrap(), send.buf), (3, vec![1, 2, 3]));
    let recv = ready(poll_noop(&mut recv));
    assert_eq!((recv.result.unwrap(), recv.buf), (2, b"xy".to_vec()));
    assert_eq!(
        ready(poll_noop(&mut readiness)).unwrap(),
        libc::POLLIN as u32
    );
    ready(poll_noop(&mut connect)).unwrap();
    ready(poll_noop(&mut sleep)).unwrap();
    ready(poll_noop(&mut fsync)).unwrap();
    ready(poll_noop(&mut rename)).unwrap();
    let error = ready(poll_noop(&mut unlink)).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::NotFound);
    ready(poll_noop(&mut mkdir)).unwrap();
    assert_eq!(ready(poll_noop(&mut statx)).unwrap().stx_size, 0);
    ready(poll_noop(&mut nop)).unwrap();
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn connect_error_keeps_errno_and_bad_address_is_not_submitted() {
    let (reactor, mock) = reactor();
    let fd: MockFd = Rc::new(3);
    let address = SocketAddress::from(std::net::SocketAddr::from(([127, 0, 0, 1], 1)));
    let mut connect = reactor.connect(&fd, &address, &Unscoped);
    assert!(poll_noop(&mut connect).is_pending());
    reactor.poll(0).unwrap();
    mock.complete(mock.only_pending().user_data, -libc::ECONNREFUSED);
    reactor.poll(8).unwrap();
    let error = ready(poll_noop(&mut connect)).unwrap_err();
    assert_eq!(error.raw_os_error(), Some(libc::ECONNREFUSED));

    let invalid = SocketAddress::Unix(std::path::PathBuf::new());
    let mut connect = reactor.connect(&fd, &invalid, &Unscoped);
    let error = ready(poll_noop(&mut connect)).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::InvalidInput);
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn sentinel_offsets_are_rejected_before_submission() {
    let (reactor, mock) = reactor();
    let fd: MockFd = Rc::new(3);
    let mut read = reactor.read_at(&fd, u64::MAX, Vec::with_capacity(1), &Unscoped);
    let outcome = ready(poll_noop(&mut read));
    assert_eq!(outcome.result.unwrap_err().kind(), ErrorKind::InvalidInput);
    assert_eq!(outcome.buf.capacity(), 1);
    let mut write = reactor.write_at(&fd, 1 << 63, b"x".as_slice(), &Unscoped);
    assert!(ready(poll_noop(&mut write)).result.is_err());
    reactor.poll(1).unwrap();
    assert!(mock.pending().is_empty());
}

#[test]
fn failed_scope_returns_inputs_without_submitting() {
    let (reactor, mock) = reactor();
    let observed = Rc::new(Cell::new(0));
    let seen = observed.clone();
    reactor.set_rejection_observer(move |_| seen.set(seen.get() + 1));
    let token = CancelToken::new();
    token.cancel();
    let (buf, dropped) = Tracked::new(4);
    let mut read = reactor.read_at(&Rc::new(1), 0, buf, &token);
    let BufResult { result, buf } = ready(poll_noop(&mut read));
    assert!(result.unwrap_err().is_canceled());
    assert!(!dropped.get());
    drop(buf);
    assert!(dropped.get());
    reactor.poll(1).unwrap();
    assert!(mock.pending().is_empty());
    assert_eq!(observed.get(), 0, "scope failures are not rejections");
}

#[test]
fn writev_gathers_segments_and_returns_them() {
    let (reactor, mock) = reactor();
    let fd: MockFd = Rc::new(3);
    let mut write = reactor.writev_at(&fd, 7, (vec![1u8, 2], b"xyz".as_slice()), &Unscoped);
    assert!(poll_noop(&mut write).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    assert_eq!(op.kind, OpKind::Writev);
    assert_eq!(op.gathered.as_deref(), Some(&[1, 2, b'x', b'y', b'z'][..]));
    mock.complete(op.user_data, 3);
    reactor.poll(8).unwrap();
    let BufResult { result, buf } = ready(poll_noop(&mut write));
    assert_eq!(result.unwrap(), 3, "short writes are reported as is");
    assert_eq!(buf, (vec![1, 2], b"xyz".as_slice()));
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn writev_error_returns_buffers() {
    let (reactor, mock) = reactor();
    let segments: Vec<Vec<u8>> = vec![vec![1], vec![], vec![2, 3]];
    let mut write = reactor.writev_at(&Rc::new(3), 0, segments, &Unscoped);
    assert!(poll_noop(&mut write).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    assert_eq!(op.gathered.as_deref(), Some(&[1, 2, 3][..]));
    mock.complete(op.user_data, -libc::EIO);
    reactor.poll(8).unwrap();
    let BufResult { result, buf } = ready(poll_noop(&mut write));
    assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EIO));
    assert_eq!(buf, [vec![1], vec![], vec![2, 3]]);
}

#[test]
fn writev_rejects_invalid_input_before_submission() {
    let (reactor, mock) = reactor();
    let fd: MockFd = Rc::new(3);
    let mut offset = reactor.writev_at(&fd, 1 << 63, (vec![1u8], vec![2u8]), &Unscoped);
    let outcome = ready(poll_noop(&mut offset));
    assert_eq!(outcome.result.unwrap_err().kind(), ErrorKind::InvalidInput);
    assert_eq!(outcome.buf, (vec![1], vec![2]));
    let many: Vec<&'static [u8]> = vec![b"x"; 1025];
    let mut many = reactor.writev_at(&fd, 0, many, &Unscoped);
    let outcome = ready(poll_noop(&mut many));
    assert_eq!(outcome.result.unwrap_err().kind(), ErrorKind::InvalidInput);
    assert_eq!(outcome.buf.len(), 1025);
    reactor.poll(1).unwrap();
    assert!(mock.pending().is_empty());
}

#[test]
fn dropped_writev_keeps_segments_until_both_completions() {
    let (reactor, mock) = reactor();
    let (header, header_dropped) = Tracked::new(4);
    let (body, body_dropped) = Tracked::new(4);
    let mut write = reactor
        .writev_at(&Rc::new(1), 0, (header, body), &Unscoped)
        .offload();
    assert!(poll_noop(&mut write).is_pending());
    assert_eq!(
        mock.state.borrow().hints,
        [(OpKind::Writev, PushHints::new().with_offload(true))]
    );
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    drop(write);
    reactor.poll(0).unwrap();
    mock.complete(op.user_data, 0);
    reactor.poll(8).unwrap();
    assert!(!header_dropped.get() && !body_dropped.get());
    mock.complete_cancel(op.user_data, -libc::EALREADY);
    reactor.poll(8).unwrap();
    assert!(header_dropped.get() && body_dropped.get());
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn dropped_future_is_canceled_and_released_after_both_completions() {
    for cancel_first in [false, true] {
        let (reactor, mock) = reactor();
        let (buf, dropped) = Tracked::new(4);
        let mut read = reactor.read_at(&Rc::new(1), 0, buf, &Unscoped);
        assert!(poll_noop(&mut read).is_pending());
        reactor.poll(0).unwrap();
        let op = mock.only_pending();
        drop(read);
        assert!(!dropped.get());
        reactor.poll(0).unwrap();
        assert_eq!(mock.cancels(), [(op.user_data | CANCEL_BIT, op.user_data)]);
        if cancel_first {
            mock.complete_cancel(op.user_data, 0);
            reactor.poll(8).unwrap();
            assert!(
                !dropped.get(),
                "the original completion is still outstanding"
            );
            mock.complete(op.user_data, -libc::ECANCELED);
        } else {
            mock.fill(&op, b"late");
            mock.complete(op.user_data, 4);
            reactor.poll(8).unwrap();
            assert!(!dropped.get(), "the cancel completion is still outstanding");
            assert_eq!(reactor.in_flight(), 1);
            mock.complete_cancel(op.user_data, -libc::EALREADY);
        }
        reactor.poll(8).unwrap();
        assert!(dropped.get());
        assert_eq!(reactor.in_flight(), 0);
    }
}

#[test]
fn dropping_an_unpolled_or_finished_operation_is_free() {
    let (reactor, mock) = reactor();
    let (buf, dropped) = Tracked::new(1);
    drop(reactor.read_at(&Rc::new(1), 0, buf, &Unscoped));
    assert!(dropped.get());
    let (buf, dropped) = Tracked::new(1);
    let mut read = reactor.read_at(&Rc::new(1), 0, buf, &Unscoped);
    assert!(poll_noop(&mut read).is_pending());
    reactor.poll(0).unwrap();
    mock.complete(mock.only_pending().user_data, 0);
    reactor.poll(1).unwrap();
    assert!(!dropped.get());
    drop(read);
    assert!(
        dropped.get(),
        "a fenced but uncollected result is freed by the future"
    );
    reactor.poll(1).unwrap();
    assert!(mock.cancels().is_empty());
}

#[test]
fn delayed_cancel_completion_holds_the_result() {
    let (reactor, mock) = reactor();
    let token = CancelToken::new();
    let counter = Arc::new(Counter::default());
    let waker = Waker::from(counter.clone());
    let mut recv = reactor.recv(&Rc::new(1), Vec::with_capacity(4), &token);
    assert!(poll(&mut recv, &waker).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    token.cancel();
    reactor.poll(0).unwrap();
    assert_eq!(mock.cancels().len(), 1);
    mock.fill(&op, b"ok");
    mock.complete(op.user_data, 2);
    reactor.poll(8).unwrap();
    assert_eq!(counter.count(), 0, "not woken before the cancel fence");
    assert!(poll(&mut recv, &waker).is_pending());
    mock.complete_cancel(op.user_data, -libc::EALREADY);
    reactor.poll(8).unwrap();
    assert_eq!(counter.count(), 1);
    let outcome = ready(poll(&mut recv, &waker));
    assert_eq!(
        outcome.result.unwrap(),
        2,
        "the kernel result wins by default"
    );
    assert_eq!(outcome.buf, b"ok");
}

#[test]
fn cancel_wins_reports_cancellation_and_drops_results() {
    let (reactor, mock) = reactor_with(ReactorConfig::default().with_cancel_wins(true));
    let token = CancelToken::new();
    let listener: MockFd = Rc::new(5);
    let mut accept = reactor.accept(&listener, &token);
    assert!(poll_noop(&mut accept).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    token.cancel();
    reactor.poll(0).unwrap();
    mock.complete(op.user_data, 9);
    reactor.poll(8).unwrap();
    let adopted = mock.state.borrow().adopted[0].clone();
    assert!(
        adopted.upgrade().is_some(),
        "retained until the cancel fence"
    );
    mock.complete_cancel(op.user_data, 0);
    reactor.poll(8).unwrap();
    let error = ready(poll_noop(&mut accept)).unwrap_err();
    assert!(error.is_canceled());
    assert!(
        adopted.upgrade().is_none(),
        "the accepted descriptor was closed"
    );
}

#[test]
fn canceled_failure_reports_the_scope_error() {
    let (reactor, mock) = reactor();
    let scope = Request::new(1);
    let mut read = reactor.read_at(&Rc::new(1), 0, Vec::with_capacity(1), &scope);
    assert!(poll_noop(&mut read).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    scope.failed.set(true);
    reactor.poll(0).unwrap();
    mock.complete_cancel(op.user_data, 0);
    mock.complete(op.user_data, -libc::ECANCELED);
    reactor.poll(8).unwrap();
    let error = ready(poll_noop(&mut read)).result.unwrap_err();
    assert_eq!(error.kind(), ErrorKind::DeadlineExceeded);

    // Reactor-initiated cancellation of a scope that still passes.
    let scope = Request::new(2);
    let mut read = reactor.read_at(&Rc::new(1), 0, Vec::with_capacity(1), &scope);
    assert!(poll_noop(&mut read).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    drop(reactor.fence_matching(|_: &Request| true));
    reactor.poll(0).unwrap();
    mock.complete(op.user_data, -libc::ECANCELED);
    mock.complete_cancel(op.user_data, 0);
    reactor.poll(8).unwrap();
    assert!(
        ready(poll_noop(&mut read))
            .result
            .unwrap_err()
            .is_canceled()
    );
}

#[test]
fn abandoned_accept_closes_its_descriptor_after_the_fence() {
    let (reactor, mock) = reactor();
    let mut accept = reactor.accept(&Rc::new(5), &Unscoped);
    assert!(poll_noop(&mut accept).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    drop(accept);
    reactor.poll(0).unwrap();
    mock.complete(op.user_data, 11);
    reactor.poll(8).unwrap();
    let adopted = mock.state.borrow().adopted[0].clone();
    assert!(adopted.upgrade().is_some());
    mock.complete_cancel(op.user_data, -libc::EALREADY);
    reactor.poll(8).unwrap();
    assert!(adopted.upgrade().is_none());
}

#[test]
fn accept_and_open_return_adopted_descriptors() {
    let (reactor, mock) = reactor();
    let mut accept = reactor.accept(&Rc::new(5), &Unscoped);
    let how = crate::backend::OpenHow {
        flags: libc::O_RDONLY as u64,
        ..Default::default()
    };
    let mut open = reactor.open_at2(None, c"/x", how, &Unscoped);
    assert!(poll_noop(&mut accept).is_pending());
    assert!(poll_noop(&mut open).is_pending());
    reactor.poll(0).unwrap();
    let pending = mock.pending();
    mock.complete(pending[0].user_data, 7);
    mock.complete(pending[1].user_data, 8);
    reactor.poll(8).unwrap();
    let (fd, peer) = ready(poll_noop(&mut accept)).unwrap();
    assert_eq!(*fd, 7);
    assert!(!peer.is_empty());
    assert_eq!(*ready(poll_noop(&mut open)).unwrap(), 8);
}

#[test]
fn unknown_completion_stops_the_reactor_after_delivering_earlier_ones() {
    let (reactor, mock) = reactor();
    let counter = Arc::new(Counter::default());
    let waker = Waker::from(counter.clone());
    let mut first = reactor.nop(&Unscoped);
    let mut second = reactor.nop(&Unscoped);
    assert!(poll(&mut first, &waker).is_pending());
    assert!(poll(&mut second, &waker).is_pending());
    reactor.poll(0).unwrap();
    let pending = mock.pending();
    mock.complete(pending[0].user_data, 0);
    mock.complete(12345, 0);
    mock.complete(pending[0].user_data, 0);
    let error = reactor.poll(8).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::Unavailable);
    assert_eq!(counter.count(), 1);
    ready(poll(&mut first, &waker)).unwrap();
    assert!(reactor.is_closed());
    assert!(reactor.failure().is_some());
    // The second operation was canceled; later polls keep reaping.
    assert_eq!(mock.cancels().len(), 1);
    let mut late = reactor.nop(&Unscoped);
    let error = ready(poll_noop(&mut late)).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::Unavailable);
    // The stale duplicate completion is reported too.
    assert!(reactor.poll(8).is_err());
    mock.complete_cancel(pending[1].user_data, 0);
    mock.complete(pending[1].user_data, -libc::ECANCELED);
    reactor.poll(8).unwrap();
    assert!(ready(poll(&mut second, &waker)).unwrap_err().is_canceled());
}

#[test]
fn panicking_wakers_do_not_lose_other_completions() {
    struct Panicky;
    impl Wake for Panicky {
        fn wake(self: Arc<Self>) {
            panic!("waker panicked");
        }
    }
    let (reactor, mock) = reactor();
    let counter = Arc::new(Counter::default());
    let good = Waker::from(counter.clone());
    let bad = Waker::from(Arc::new(Panicky));
    let mut a = reactor.nop(&Unscoped);
    let mut b = reactor.nop(&Unscoped);
    let mut c = reactor.nop(&Unscoped);
    assert!(poll(&mut a, &bad).is_pending());
    assert!(poll(&mut b, &good).is_pending());
    assert!(poll(&mut c, &bad).is_pending());
    reactor.poll(0).unwrap();
    for op in mock.pending() {
        mock.complete(op.user_data, 0);
    }
    let result = std::panic::catch_unwind(AssertUnwindSafe(|| reactor.poll(8)));
    assert!(result.is_err());
    assert_eq!(counter.count(), 1);
    assert_eq!(reactor.in_flight(), 0);
    ready(poll_noop(&mut a)).unwrap();
    ready(poll_noop(&mut b)).unwrap();
    ready(poll_noop(&mut c)).unwrap();
    // The reactor is still usable.
    assert_eq!(reactor.poll(8).unwrap(), 0);
}

#[test]
fn panicking_scope_check_cancels_and_is_resumed() {
    #[derive(Clone)]
    struct Explosive(Rc<Cell<bool>>);
    impl Scope for Explosive {
        type Error = Error;
        fn check(&self) -> Result<()> {
            assert!(!self.0.get(), "scope check panicked");
            Ok(())
        }
    }
    let (reactor, mock) = reactor();
    let armed = Rc::new(Cell::new(false));
    let mut nop = reactor.nop(&Explosive(armed.clone()));
    assert!(poll_noop(&mut nop).is_pending());
    reactor.poll(0).unwrap();
    armed.set(true);
    let result = std::panic::catch_unwind(AssertUnwindSafe(|| reactor.poll(8)));
    assert!(result.is_err());
    armed.set(false);
    reactor.poll(0).unwrap();
    assert_eq!(mock.cancels().len(), 1);
    let op = mock.only_pending();
    mock.complete_cancel(op.user_data, 0);
    mock.complete(op.user_data, -libc::ECANCELED);
    reactor.poll(8).unwrap();
    assert!(ready(poll_noop(&mut nop)).unwrap_err().is_canceled());
}

#[test]
fn admission_is_bounded_and_released_at_the_fence() {
    let (reactor, mock) = reactor_with(ReactorConfig::default().with_max_in_flight(1));
    let rejections = Rc::new(RefCell::new(Vec::new()));
    let seen = rejections.clone();
    reactor.set_rejection_observer(move |rejection| {
        let scope = rejection.scope.downcast_ref::<Request>().unwrap();
        seen.borrow_mut().push((
            rejection.reason,
            rejection.kind,
            rejection.reserved,
            scope.id,
            rejection.error.kind(),
        ));
    });
    let mut first = reactor.nop(&Request::new(1));
    assert!(poll_noop(&mut first).is_pending());
    let (buf, dropped) = Tracked::new(2);
    let mut second = reactor.read_at(&Rc::new(1), 0, buf, &Request::new(2));
    let outcome = ready(poll_noop(&mut second));
    assert_eq!(outcome.result.unwrap_err().kind(), ErrorKind::Overloaded);
    assert!(!dropped.get(), "the buffer is returned");
    assert_eq!(
        *rejections.borrow(),
        [(
            RejectionReason::Capacity,
            OpKind::Read,
            false,
            2,
            ErrorKind::Overloaded
        )]
    );
    reactor.poll(0).unwrap();
    mock.complete(mock.only_pending().user_data, 0);
    reactor.poll(8).unwrap();
    // The slot is free at the fence, before the result is collected.
    let mut third = reactor.nop(&Request::new(3));
    assert!(poll_noop(&mut third).is_pending());
    ready(poll_noop(&mut first)).unwrap();
    reactor.clear_rejection_observer();
    let mut fourth = reactor.nop(&Request::new(4));
    assert!(ready(poll_noop(&mut fourth)).is_err());
    assert_eq!(rejections.borrow().len(), 1);
    drop(third);
    settle(&reactor, &mock);
}

#[test]
fn reservations_have_dedicated_capacity() {
    let (reactor, mock) = reactor_with(
        ReactorConfig::default()
            .with_max_in_flight(1)
            .with_max_reserved(2),
    );
    assert_eq!(
        reactor.reserve(0).unwrap_err().kind(),
        ErrorKind::InvalidInput
    );
    assert_eq!(
        reactor.reserve(3).unwrap_err().kind(),
        ErrorKind::Overloaded
    );
    let reservation = reactor.reserve(1).unwrap();
    assert_eq!(reactor.reserved_available(), 1);
    let rejections = Rc::new(RefCell::new(Vec::new()));
    let seen = rejections.clone();
    reactor.set_rejection_observer(move |r| seen.borrow_mut().push((r.reason, r.reserved)));

    let mut ordinary = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut ordinary).is_pending());
    let mut reserved = reactor.nop(&Unscoped).reserved(&reservation);
    assert!(
        poll_noop(&mut reserved).is_pending(),
        "admitted although ordinary is full"
    );
    assert_eq!(reservation.in_use(), 1);
    assert_eq!(reservation.available(), 0);
    let mut exhausted = reactor.nop(&Unscoped).reserved(&reservation);
    assert_eq!(
        ready(poll_noop(&mut exhausted)).unwrap_err().kind(),
        ErrorKind::Overloaded
    );
    assert_eq!(
        *rejections.borrow(),
        [(RejectionReason::ReservationExhausted, true)]
    );

    // Dropping the reservation returns its slots only after its operations fence.
    drop(reservation);
    assert_eq!(reactor.reserved_available(), 1);
    reactor.poll(0).unwrap();
    for op in mock.pending() {
        mock.complete(op.user_data, 0);
    }
    reactor.poll(8).unwrap();
    assert_eq!(reactor.reserved_available(), 2);
    ready(poll_noop(&mut reserved)).unwrap();
    ready(poll_noop(&mut ordinary)).unwrap();

    // A reservation from another reactor is refused.
    let (other, _) = reactor_with(ReactorConfig::default().with_max_reserved(1));
    let foreign = other.reserve(1).unwrap();
    let mut wrong = reactor.nop(&Unscoped).reserved(&foreign);
    assert_eq!(
        ready(poll_noop(&mut wrong)).unwrap_err().kind(),
        ErrorKind::InvalidInput
    );
    assert_eq!(
        rejections.borrow().last(),
        Some(&(RejectionReason::ForeignReservation, true))
    );
    assert!(format!("{foreign:?}").contains("slots"));
}

#[test]
fn rejection_observer_may_reenter_the_reactor() {
    let (backend, mock) = mock();
    let reactor = Rc::new(
        Reactor::new(
            backend,
            ReactorConfig::default()
                .with_max_in_flight(1)
                .with_max_reserved(1),
        )
        .unwrap(),
    );
    let weak = Rc::downgrade(&reactor);
    let calls = Rc::new(Cell::new(0));
    let seen = calls.clone();
    reactor.set_rejection_observer(move |_| {
        let reactor = weak.upgrade().unwrap();
        seen.set(seen.get() + reactor.in_flight());
        drop(reactor.reserve(1).unwrap());
        reactor.clear_rejection_observer();
    });
    let mut first = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut first).is_pending());
    let mut second = reactor.nop(&Unscoped);
    assert!(ready(poll_noop(&mut second)).is_err());
    assert_eq!(calls.get(), 1);
    drop((first, second));
    settle(&reactor, &mock);
}

#[test]
fn backend_rejections_return_inputs() {
    let (reactor, mock) = reactor();
    let rejections = Rc::new(RefCell::new(Vec::new()));
    let seen = rejections.clone();
    reactor.set_rejection_observer(move |r| seen.borrow_mut().push(r.reason));

    // One Full is absorbed by submitting and retrying.
    mock.state.borrow_mut().full = 1;
    let mut nop = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut nop).is_pending());
    assert_eq!(mock.state.borrow().submits, 1);

    mock.state.borrow_mut().full = 2;
    let (buf, dropped) = Tracked::new(1);
    let mut read = reactor.read_at(&Rc::new(1), 0, buf, &Unscoped);
    let outcome = ready(poll_noop(&mut read));
    assert_eq!(outcome.result.unwrap_err().kind(), ErrorKind::Overloaded);
    assert!(!dropped.get());

    mock.state.borrow_mut().unsupported = true;
    let mut unsupported = reactor.nop(&Unscoped);
    assert_eq!(
        ready(poll_noop(&mut unsupported)).unwrap_err().kind(),
        ErrorKind::Unsupported
    );
    assert_eq!(
        *rejections.borrow(),
        [RejectionReason::QueueFull, RejectionReason::Unsupported]
    );
    assert_eq!(reactor.in_flight(), 1);
    drop(nop);
    settle(&reactor, &mock);
}

#[test]
fn full_cancel_queue_is_retried() {
    let (reactor, mock) = reactor();
    let mut nop = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut nop).is_pending());
    reactor.poll(0).unwrap();
    mock.state.borrow_mut().cancel_full = 2;
    drop(nop);
    reactor.poll(0).unwrap();
    assert!(mock.cancels().is_empty());
    reactor.poll(0).unwrap();
    assert_eq!(mock.cancels().len(), 1);
    settle(&reactor, &mock);
}

#[test]
fn canceled_token_wakes_the_reactor_without_a_scan() {
    let (reactor, mock) = reactor_with(ReactorConfig::default().with_cancel_scan_budget(0));
    let token = CancelToken::new();
    let mut nop = reactor.nop(&token);
    assert!(poll_noop(&mut nop).is_pending());
    reactor.poll(0).unwrap();
    assert_eq!(token.registrations(), 1);
    let remote = token.clone();
    std::thread::spawn(move || remote.cancel()).join().unwrap();
    assert_eq!(
        mock.remote.load(Ordering::SeqCst),
        1,
        "the reactor was woken"
    );
    reactor.wait(Duration::from_secs(1)).unwrap();
    reactor.poll(0).unwrap();
    assert_eq!(mock.cancels().len(), 1);
    let op = mock.only_pending();
    mock.complete(op.user_data, -libc::ECANCELED);
    mock.complete_cancel(op.user_data, 0);
    reactor.poll(8).unwrap();
    assert_eq!(token.registrations(), 0, "deregistered at the fence");
    assert!(ready(poll_noop(&mut nop)).unwrap_err().is_canceled());
}

#[test]
fn deadline_hints_trigger_scope_checks() {
    #[derive(Clone)]
    struct Expiring(Instant);
    impl Scope for Expiring {
        type Error = Error;
        fn check(&self) -> Result<()> {
            if Instant::now() >= self.0 {
                Err(Error::new(ErrorKind::DeadlineExceeded))
            } else {
                Ok(())
            }
        }
        fn deadline(&self) -> Option<Instant> {
            Some(self.0)
        }
    }
    let (reactor, mock) = reactor_with(ReactorConfig::default().with_cancel_scan_budget(0));
    // Generous enough for slow interpreters (Miri) to submit first.
    let deadline = Instant::now() + Duration::from_millis(500);
    let mut nop = reactor.nop(&Expiring(deadline));
    assert!(poll_noop(&mut nop).is_pending());
    reactor.poll(0).unwrap();
    assert!(mock.cancels().is_empty());
    std::thread::sleep(deadline.saturating_duration_since(Instant::now()));
    reactor.poll(0).unwrap();
    assert_eq!(mock.cancels().len(), 1);
    let op = mock.only_pending();
    mock.complete(op.user_data, -libc::ECANCELED);
    mock.complete_cancel(op.user_data, 0);
    reactor.poll(8).unwrap();
    let error = ready(poll_noop(&mut nop)).unwrap_err();
    assert!(error.is_deadline_exceeded());
}

#[test]
fn scan_rotates_beyond_its_budget() {
    let (reactor, mock) = reactor_with(ReactorConfig::default().with_cancel_scan_budget(1));
    let scopes: Vec<Request> = (0..3).map(Request::new).collect();
    let mut ops: Vec<_> = scopes.iter().map(|scope| reactor.nop(scope)).collect();
    for op in &mut ops {
        assert!(poll_noop(op).is_pending());
    }
    reactor.poll(0).unwrap();
    for scope in &scopes {
        scope.failed.set(true);
    }
    for expected in 1..=3 {
        reactor.poll(0).unwrap();
        assert_eq!(mock.cancels().len(), expected);
    }
    let checks: usize = scopes.iter().map(|s| s.checks.get()).sum();
    assert!(checks <= 3 + 3 + 3, "{checks}");
    drop(ops);
    settle(&reactor, &mock);
}

#[test]
fn poll_cannot_reenter_from_a_scope_check() {
    #[derive(Clone)]
    struct Reentrant(
        Rc<RefCell<Option<Error>>>,
        Weak<Reactor<Mock>>,
        Rc<Cell<bool>>,
    );
    impl Scope for Reentrant {
        type Error = Error;
        fn check(&self) -> Result<()> {
            if !self.2.get() {
                return Ok(());
            }
            if let Some(reactor) = self.1.upgrade() {
                let poll = reactor.poll(1).unwrap_err();
                let fence = ready(poll_noop(&mut reactor.fence_matching(|_: &Unscoped| true)));
                assert_eq!(fence.unwrap_err().kind(), poll.kind());
                assert_eq!(reactor.in_flight(), 1, "other methods remain usable");
                *self.0.borrow_mut() = Some(poll);
            }
            Ok(())
        }
    }
    let (backend, mock) = mock();
    let reactor = Rc::new(Reactor::new(backend, ReactorConfig::default()).unwrap());
    let seen = Rc::new(RefCell::new(None));
    let armed = Rc::new(Cell::new(false));
    let scope = Reentrant(seen.clone(), Rc::downgrade(&reactor), armed.clone());
    let mut nop = reactor.nop(&scope);
    assert!(poll_noop(&mut nop).is_pending());
    // Arm the re-entrant check only after submission.
    armed.set(true);
    reactor.poll(0).unwrap();
    assert_eq!(
        seen.borrow().as_ref().unwrap().kind(),
        ErrorKind::Unavailable
    );
    armed.set(false);
    drop(nop);
    settle(&reactor, &mock);
}

thread_local! {
    static HOOK: RefCell<Option<Rc<dyn Fn()>>> = const { RefCell::new(None) };
}

fn run_hook() {
    let hook = HOOK.with(|hook| hook.borrow().clone());
    if let Some(hook) = hook {
        hook();
    }
}

/// A waker whose clone, wake and drop call back into the test.
fn reentrant_waker() -> Waker {
    const VTABLE: RawWakerVTable = RawWakerVTable::new(
        |_| {
            run_hook();
            RawWaker::new(std::ptr::null(), &VTABLE)
        },
        |_| run_hook(),
        |_| run_hook(),
        |_| run_hook(),
    );
    // SAFETY: the vtable functions ignore the data pointer.
    unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) }
}

#[test]
fn waker_clone_wake_and_drop_may_reenter() {
    let (backend, mock) = mock();
    let reactor = Rc::new(Reactor::new(backend, ReactorConfig::default()).unwrap());
    let weak = Rc::downgrade(&reactor);
    let calls = Rc::new(Cell::new(0));
    let seen = calls.clone();
    HOOK.with(|hook| {
        *hook.borrow_mut() = Some(Rc::new(move || {
            if let Some(reactor) = weak.upgrade() {
                let _ = reactor.in_flight();
                let _ = reactor.is_closed();
                seen.set(seen.get() + 1);
            }
        }))
    });
    let waker = reentrant_waker();
    let mut nop = reactor.nop(&Unscoped);
    assert!(poll(&mut nop, &waker).is_pending());
    let mut fence = reactor.fence_matching(|_: &Request| true);
    assert!(poll(&mut fence, &waker).is_ready(), "nothing matched");
    let mut drain = reactor.drain();
    assert!(poll(&mut drain, &waker).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    mock.complete(op.user_data, 0);
    mock.complete_cancel(op.user_data, 0);
    reactor.poll(8).unwrap();
    assert!(poll(&mut drain, &waker).is_ready());
    ready(poll(&mut nop, &waker)).unwrap();
    drop(waker);
    HOOK.with(|hook| hook.borrow_mut().take());
    assert!(calls.get() >= 3, "{}", calls.get());
}

#[test]
fn drain_closes_admission_and_fences_everything() {
    let (reactor, mock) = reactor();
    let counter = Arc::new(Counter::default());
    let waker = Waker::from(counter.clone());
    let mut a = reactor.nop(&Unscoped);
    let mut b = reactor.recv(&Rc::new(1), Vec::with_capacity(1), &Unscoped);
    assert!(poll_noop(&mut a).is_pending());
    assert!(poll_noop(&mut b).is_pending());
    reactor.poll(0).unwrap();
    let mut drain = reactor.drain();
    assert!(reactor.is_closed());
    assert_eq!(drain.remaining(), 2);
    assert!(poll(&mut drain, &waker).is_pending());
    let mut late = reactor.nop(&Unscoped);
    assert_eq!(
        ready(poll_noop(&mut late)).unwrap_err().kind(),
        ErrorKind::Unavailable
    );
    reactor.poll(0).unwrap();
    assert_eq!(mock.cancels().len(), 2);
    let pending = mock.pending();
    mock.complete(pending[0].user_data, 0);
    mock.complete_cancel(pending[0].user_data, -libc::ENOENT);
    reactor.poll(8).unwrap();
    assert_eq!(counter.count(), 1, "fence waiters are woken on progress");
    assert!(poll(&mut drain, &waker).is_pending());
    mock.complete_cancel(pending[1].user_data, 0);
    mock.complete(pending[1].user_data, -libc::ECANCELED);
    reactor.poll(8).unwrap();
    ready(poll(&mut drain, &waker)).unwrap();
    ready(poll_noop(&mut a)).unwrap();
    assert!(ready(poll_noop(&mut b)).result.unwrap_err().is_canceled());
    assert!(format!("{drain:?}").contains("remaining"));
}

/// A hook a reentrant waker runs, once, when it is cloned.
type CloneHook<'a> = RefCell<Option<Box<dyn FnOnce() + 'a>>>;

/// A waker whose clone runs `hook` once. The returned waker borrows `hook`.
fn clone_hook_waker(hook: &CloneHook<'_>) -> Waker {
    fn clone(data: *const ()) -> RawWaker {
        // SAFETY: `data` is the `CloneHook` the test keeps alive while the
        // waker and its clones exist.
        let hook = unsafe { &*data.cast::<CloneHook<'_>>() };
        let run = hook.borrow_mut().take();
        if let Some(run) = run {
            run();
        }
        RawWaker::new(data, &VTABLE)
    }
    fn noop(_: *const ()) {}
    static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, noop, noop, noop);
    let data = std::ptr::from_ref(hook).cast::<()>();
    // SAFETY: the vtable functions uphold the `RawWaker` contract and never
    // dereference `data` beyond the hook's lifetime in these tests.
    unsafe { Waker::from_raw(RawWaker::new(data, &VTABLE)) }
}

#[test]
fn fence_rechecks_after_a_reentrant_waker_clone_reaps_the_last_completion() {
    let (reactor, mock) = reactor();
    let mut op = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut op).is_pending());
    reactor.poll(0).unwrap();
    let pending = mock.only_pending();
    let mut drain = reactor.drain();
    reactor.poll(0).unwrap();
    mock.complete(pending.user_data, -libc::ECANCELED);
    mock.complete_cancel(pending.user_data, 0);
    let hook: CloneHook<'_> = RefCell::new(Some(Box::new(|| {
        reactor.poll(8).unwrap();
    })));
    let waker = clone_hook_waker(&hook);
    assert_eq!(drain.remaining(), 1);
    ready(poll(&mut drain, &waker)).unwrap();
    assert!(hook.borrow().is_none(), "the clone reentered the reactor");
    drop(waker);
    assert!(ready(poll_noop(&mut op)).unwrap_err().is_canceled());
}

#[test]
fn fence_matching_selects_by_scope_type_and_predicate() {
    let (reactor, mock) = reactor();
    let one = Request::new(1);
    let two = Request::new(2);
    let mut a = reactor.nop(&one);
    let mut b = reactor.nop(&two);
    let mut c = reactor.nop(&Unscoped);
    for op in [&mut a, &mut b] {
        assert!(poll_noop(op).is_pending());
    }
    assert!(poll_noop(&mut c).is_pending());
    reactor.poll(0).unwrap();
    let pending = mock.pending();
    let mut fence = reactor.fence_matching(|scope: &Request| scope.id == 1);
    assert_eq!(fence.remaining(), 1);
    reactor.poll(0).unwrap();
    assert_eq!(
        mock.cancels(),
        [(pending[0].user_data | CANCEL_BIT, pending[0].user_data)]
    );
    assert!(poll_noop(&mut fence).is_pending());
    mock.complete(pending[0].user_data, -libc::ECANCELED);
    mock.complete_cancel(pending[0].user_data, 0);
    reactor.poll(8).unwrap();
    ready(poll_noop(&mut fence)).unwrap();
    assert!(
        !reactor.is_closed(),
        "a selective fence keeps admission open"
    );
    assert!(ready(poll_noop(&mut a)).unwrap_err().is_canceled());
    assert_eq!(reactor.in_flight(), 2);
    drop((b, c));
    settle(&reactor, &mock);
}

#[test]
#[cfg_attr(miri, ignore = "deliberately leaks")]
fn fence_fails_when_the_reactor_fails() {
    let (reactor, mock) = reactor();
    let mut nop = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut nop).is_pending());
    let mut fence = reactor.fence_matching(|_: &Unscoped| true);
    mock.state.borrow_mut().submit_error = Some(Error::from_errno(libc::EBADF));
    let error = reactor.poll(0).unwrap_err();
    assert_eq!(error.raw_os_error(), Some(libc::EBADF));
    assert_eq!(ready(poll_noop(&mut fence)).unwrap_err(), error);
    drop(nop);
    // Leaks the never-completed operation; that is the point.
    std::mem::forget(reactor);
}

#[test]
fn shutdown_fences_or_times_out() {
    let (reactor, mock) = reactor();
    mock.state.borrow_mut().auto_cancel = true;
    let mut nop = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut nop).is_pending());
    reactor.shutdown(Duration::from_secs(5)).unwrap();
    assert!(ready(poll_noop(&mut nop)).unwrap_err().is_canceled());
    assert!(reactor.is_closed());

    let (reactor, mock) = reactor_with(ReactorConfig::default());
    let mut nop = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut nop).is_pending());
    let error = reactor.shutdown(Duration::from_millis(5)).unwrap_err();
    assert!(error.is_deadline_exceeded());
    assert!(mock.state.borrow().waits > 0);
    // Shutdown may be retried once the backend makes progress.
    mock.state.borrow_mut().auto_cancel = true;
    mock.state.borrow_mut().cancels.clear();
    let op = mock.only_pending();
    mock.complete(op.user_data, 0);
    mock.state
        .borrow_mut()
        .completions
        .push_back(CompletionEntry {
            user_data: op.user_data | CANCEL_BIT,
            result: 0,
            flags: 0,
        });
    reactor.shutdown(Duration::from_secs(5)).unwrap();
    ready(poll_noop(&mut nop)).unwrap();
}

#[test]
fn drop_wait_fences_before_dropping() {
    let (reactor, mock) =
        reactor_with(ReactorConfig::default().with_drop_wait(Duration::from_secs(5)));
    mock.state.borrow_mut().auto_cancel = true;
    let (buf, dropped) = Tracked::new(1);
    let mut read = reactor.read_at(&Rc::new(1), 0, buf, &Unscoped);
    assert!(poll_noop(&mut read).is_pending());
    drop(read);
    let leaked = leaked_operations();
    drop(reactor);
    assert!(dropped.get());
    assert_eq!(leaked_operations(), leaked);
}

#[test]
#[cfg_attr(miri, ignore = "deliberately leaks")]
fn drop_leaks_what_it_cannot_fence() {
    let (reactor, mock) = reactor();
    let (buf, dropped) = Tracked::new(1);
    let mut read = reactor.read_at(&Rc::new(1), 0, buf, &Unscoped);
    assert!(poll_noop(&mut read).is_pending());
    drop(read);
    let before = leaked_operations();
    drop(reactor);
    assert!(leaked_operations() > before);
    assert!(!dropped.get(), "memory the backend may use is never freed");
    assert_eq!(Rc::strong_count(&mock.state), 2, "the backend was leaked");
}

#[test]
fn leases_are_returned_or_released_after_the_fence() {
    let (reactor, mock) = reactor();
    let mut nop = reactor.nop(&Unscoped).lease(String::from("permit"));
    assert!(poll_noop(&mut nop).is_pending());
    reactor.poll(0).unwrap();
    mock.complete(mock.only_pending().user_data, 0);
    reactor.poll(8).unwrap();
    let (result, lease) = ready(poll_noop(&mut nop));
    result.unwrap();
    assert_eq!(lease, "permit");

    let (lease, dropped) = Tracked::new(0);
    let mut nop = reactor.nop(&Unscoped).lease(lease);
    assert!(poll_noop(&mut nop).is_pending());
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    drop(nop);
    reactor.poll(0).unwrap();
    mock.complete(op.user_data, 0);
    reactor.poll(8).unwrap();
    assert!(!dropped.get(), "held until the cancel fence");
    mock.complete_cancel(op.user_data, 0);
    reactor.poll(8).unwrap();
    assert!(dropped.get());

    // A rejected operation returns its lease too.
    let (reactor, mock) = reactor_with(ReactorConfig::default().with_max_in_flight(1));
    let mut first = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut first).is_pending());
    let mut second = reactor.nop(&Unscoped).lease(7u8);
    let (result, lease) = ready(poll_noop(&mut second));
    assert!(result.is_err());
    assert_eq!(lease, 7);
    assert!(format!("{first:?}").contains("submitted"));
    drop(first);
    settle(&reactor, &mock);
}

#[test]
fn options_must_be_set_before_the_first_poll() {
    let (reactor, mock) = reactor_with(ReactorConfig::default().with_max_reserved(1));
    let reservation = reactor.reserve(1).unwrap();
    let mut nop = reactor.nop(&Unscoped);
    let _ = poll_noop(&mut nop);
    let panic = std::panic::catch_unwind(AssertUnwindSafe(|| drop(nop.reserved(&reservation))));
    let message = panic.unwrap_err();
    assert!(
        message
            .downcast_ref::<&str>()
            .unwrap()
            .contains("after the first poll")
    );
    let mut nop = reactor.nop(&Unscoped);
    let _ = poll_noop(&mut nop);
    let panic = std::panic::catch_unwind(AssertUnwindSafe(|| drop(nop.lease(1))));
    assert!(panic.is_err());
    settle(&reactor, &mock);
}

#[test]
fn configuration_is_validated() {
    let (backend, _) = mock();
    let error = Reactor::new(backend, ReactorConfig::default().with_max_in_flight(0)).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);
    let (backend, _) = mock();
    let config = ReactorConfig::default().with_max_reserved(usize::MAX);
    assert!(Reactor::new(backend, config).is_err());
    let config = ReactorConfig::default()
        .with_max_wait(Duration::from_millis(1))
        .with_drop_wait(Duration::from_millis(2));
    assert_eq!(config.max_wait, Duration::from_millis(1));
    assert_eq!(config.drop_wait, Duration::from_millis(2));
    let (reactor, mock) = reactor_with(config.clone());
    assert_eq!(reactor.config(), &config);
    assert_eq!(reactor.backend().state.borrow().submits, 0);
    reactor.backend_mut().state.borrow_mut().waits = 0;
    let mut seed = [0u8; 4];
    reactor.entropy().fill(&mut seed).unwrap();
    let _ = reactor.clock().now();
    reactor.remote_waker().wake();
    assert_eq!(mock.remote.load(Ordering::SeqCst), 1);
    assert!(format!("{reactor:?}").contains("in_flight"));
}

#[test]
fn wait_does_not_block_with_pending_cancellation_work() {
    let (reactor, mock) = reactor();
    let mut nop = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut nop).is_pending());
    drop(nop);
    reactor.wait(Duration::from_secs(10)).unwrap();
    assert_eq!(
        mock.state.borrow().submits,
        1,
        "wait submits queued work first"
    );
    reactor.poll(0).unwrap();
    let op = mock.only_pending();
    mock.complete(op.user_data, 0);
    mock.complete_cancel(op.user_data, 0);
    reactor.poll(8).unwrap();
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn offload_hint_reaches_the_backend() {
    let (reactor, mock) = reactor();
    let fd: MockFd = Rc::new(3);
    let mut plain = reactor.read_at(&fd, 0, Vec::with_capacity(4), &Unscoped);
    let mut offloaded = reactor.write_at(&fd, 0, vec![1u8; 4], &Unscoped).offload();
    assert!(poll_noop(&mut plain).is_pending());
    assert!(poll_noop(&mut offloaded).is_pending());
    assert_eq!(
        mock.state.borrow().hints,
        [
            (OpKind::Read, PushHints::new()),
            (OpKind::Write, PushHints::new().with_offload(true)),
        ]
    );
    reactor.poll(usize::MAX).unwrap();
    for op in mock.pending() {
        mock.complete(op.user_data, 0);
    }
    reactor.poll(usize::MAX).unwrap();
    assert_eq!(ready(poll_noop(&mut plain)).result.unwrap(), 0);
    assert_eq!(ready(poll_noop(&mut offloaded)).result.unwrap(), 0);
}

#[test]
fn offload_after_poll_panics() {
    let (reactor, mock) = reactor();
    let mut op = reactor.nop(&Unscoped);
    assert!(poll_noop(&mut op).is_pending());
    let panic = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| drop(op.offload())));
    let message = panic.unwrap_err();
    let message = message.downcast_ref::<&str>().copied().unwrap_or_default();
    assert!(
        message.contains("offload called after the first poll"),
        "{message}"
    );
    settle(&reactor, &mock);
}
