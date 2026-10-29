//! Single-threaded executor and `poll(2)` reactor for the control thread.
//!
//! All futures run on one thread. Sockets are nonblocking. A future waits for
//! readiness or a timer through this reactor. Other threads wake the loop
//! through [`Notifier`]. The current reactor is thread-local.

use futures::{
    executor::{LocalPool, LocalSpawner},
    task::LocalSpawnExt,
};
use std::{
    cell::{Cell, RefCell},
    collections::BinaryHeap,
    future::Future,
    io,
    net::{SocketAddr, TcpListener, TcpStream},
    os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd},
    rc::Rc,
    sync::Arc,
    task::{Poll, Waker},
    time::{Duration, Instant},
};

/// Readiness interest.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Interest {
    Read,
    Write,
}

struct Registration {
    fd: RawFd,
    interest: Interest,
    waker: Waker,
    ready: Rc<Cell<bool>>,
}

struct Timer {
    at: Instant,
    seq: u64,
    waker: Waker,
    fired: Rc<Cell<bool>>,
}

impl PartialEq for Timer {
    fn eq(&self, o: &Self) -> bool {
        (self.at, self.seq) == (o.at, o.seq)
    }
}
impl Eq for Timer {}
impl PartialOrd for Timer {
    fn partial_cmp(&self, o: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(o))
    }
}
impl Ord for Timer {
    // Reverse order turns the max-heap into a min-heap.
    fn cmp(&self, o: &Self) -> std::cmp::Ordering {
        (o.at, o.seq).cmp(&(self.at, self.seq))
    }
}

struct Reactor {
    registrations: RefCell<Vec<Registration>>,
    timers: RefCell<BinaryHeap<Timer>>,
    seq: Cell<u64>,
    spawner: LocalSpawner,
    tasks: Rc<Cell<usize>>,
    max_tasks: usize,
}

thread_local! {
    static CURRENT: RefCell<Option<Rc<Reactor>>> = const { RefCell::new(None) };
}

fn current() -> Rc<Reactor> {
    CURRENT.with(|c| c.borrow().clone().expect("control reactor is not running"))
}

/// Wakes the reactor loop from any thread.
#[derive(Clone)]
pub struct Notifier(Arc<OwnedFd>);

impl Notifier {
    /// Interrupt `poll(2)`. Errors are ignored: a full eventfd already wakes.
    pub fn notify(&self) {
        let one: u64 = 1;
        // SAFETY: writes 8 bytes from a valid u64 to an owned eventfd.
        let _ = unsafe { libc::write(self.0.as_raw_fd(), (&raw const one).cast(), 8) };
    }
}

/// Owner of the event loop. Not `Send`; create it on the control thread.
pub struct Executor {
    pool: LocalPool,
    reactor: Rc<Reactor>,
    event: Arc<OwnedFd>,
}

impl Executor {
    /// Create an executor with a fixed task limit.
    pub fn new(max_tasks: usize) -> io::Result<Self> {
        // SAFETY: plain syscall; the result is checked.
        let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: `fd` is a new descriptor owned here.
        let event = Arc::new(unsafe { OwnedFd::from_raw_fd(fd) });
        let pool = LocalPool::new();
        let reactor = Rc::new(Reactor {
            registrations: RefCell::new(Vec::new()),
            timers: RefCell::new(BinaryHeap::new()),
            seq: Cell::new(0),
            spawner: pool.spawner(),
            tasks: Rc::new(Cell::new(0)),
            max_tasks,
        });
        Ok(Self {
            pool,
            reactor,
            event,
        })
    }

    /// Cross-thread wake handle.
    pub fn notifier(&self) -> Notifier {
        Notifier(self.event.clone())
    }

    /// Run until `done` returns true. `done` is checked after each wake.
    pub fn run(&mut self, mut done: impl FnMut() -> bool) {
        CURRENT.with(|c| *c.borrow_mut() = Some(self.reactor.clone()));
        loop {
            self.pool.run_until_stalled();
            if done() {
                break;
            }
            self.turn();
        }
        CURRENT.with(|c| *c.borrow_mut() = None);
    }

    /// Run a future to completion on this executor, for tests.
    #[cfg(test)]
    pub fn block_on<F: Future>(&mut self, future: F) -> F::Output {
        CURRENT.with(|c| *c.borrow_mut() = Some(self.reactor.clone()));
        let mut future = Box::pin(future);
        let output = loop {
            let waker = futures::task::noop_waker();
            let mut cx = std::task::Context::from_waker(&waker);
            if let Poll::Ready(v) = future.as_mut().poll(&mut cx) {
                break v;
            }
            self.pool.run_until_stalled();
            if let Poll::Ready(v) = future.as_mut().poll(&mut cx) {
                break v;
            }
            self.turn();
        };
        CURRENT.with(|c| *c.borrow_mut() = None);
        output
    }

    /// One `poll(2)` call followed by readiness and timer wakes.
    fn turn(&self) {
        let now = Instant::now();
        let timeout = self
            .reactor
            .timers
            .borrow()
            .peek()
            .map(|t| t.at.saturating_duration_since(now))
            .unwrap_or(Duration::from_secs(1))
            .min(Duration::from_secs(1));
        let registrations = std::mem::take(&mut *self.reactor.registrations.borrow_mut());
        let mut fds: Vec<libc::pollfd> = Vec::with_capacity(registrations.len() + 1);
        fds.push(libc::pollfd {
            fd: self.event.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        });
        for r in &registrations {
            fds.push(libc::pollfd {
                fd: r.fd,
                events: match r.interest {
                    Interest::Read => libc::POLLIN,
                    Interest::Write => libc::POLLOUT,
                },
                revents: 0,
            });
        }
        let ms = timeout.as_millis().min(1000) as libc::c_int;
        let ms = if ms == 0 && !timeout.is_zero() { 1 } else { ms };
        // SAFETY: `fds` is a valid array of the given length.
        let n = unsafe { libc::poll(fds.as_mut_ptr(), fds.len() as libc::nfds_t, ms) };
        if n > 0 && fds[0].revents != 0 {
            let mut buf = [0u8; 8];
            // SAFETY: reads at most 8 bytes into a valid buffer.
            let _ = unsafe { libc::read(self.event.as_raw_fd(), buf.as_mut_ptr().cast(), 8) };
        }
        let mut keep = Vec::new();
        for (r, p) in registrations.into_iter().zip(&fds[1..]) {
            if n > 0 && p.revents != 0 {
                r.ready.set(true);
                r.waker.wake();
            } else if r.ready.get() || Rc::strong_count(&r.ready) == 1 {
                // The waiting future was dropped.
            } else {
                keep.push(r);
            }
        }
        self.reactor.registrations.borrow_mut().extend(keep);
        let now = Instant::now();
        let mut timers = self.reactor.timers.borrow_mut();
        while timers
            .peek()
            .is_some_and(|t| t.at <= now || Rc::strong_count(&t.fired) == 1)
        {
            let t = timers.pop().expect("peeked");
            t.fired.set(true);
            t.waker.wake();
        }
    }
}

/// Spawn a task. Fails when the fixed task limit is reached.
pub fn spawn(future: impl Future<Output = ()> + 'static) -> Result<(), crate::Error> {
    let reactor = current();
    if reactor.tasks.get() >= reactor.max_tasks {
        return Err(crate::Error::Unavailable);
    }
    let tasks = reactor.tasks.clone();
    tasks.set(tasks.get() + 1);
    struct Guard(Rc<Cell<usize>>);
    impl Drop for Guard {
        fn drop(&mut self) {
            self.0.set(self.0.get() - 1);
        }
    }
    let guard = Guard(tasks);
    reactor
        .spawner
        .spawn_local(async move {
            let _guard = guard;
            future.await;
        })
        .map_err(|_| crate::Error::Stopped)
}

/// Current task count.
pub fn task_count() -> usize {
    current().tasks.get()
}

/// Wait until `fd` may be ready. Spurious wakes are possible.
pub fn ready(fd: RawFd, interest: Interest) -> impl Future<Output = ()> {
    let mut state: Option<Rc<Cell<bool>>> = None;
    std::future::poll_fn(move |cx| {
        if let Some(s) = &state
            && s.get()
        {
            return Poll::Ready(());
        }
        let flag = Rc::new(Cell::new(false));
        current().registrations.borrow_mut().push(Registration {
            fd,
            interest,
            waker: cx.waker().clone(),
            ready: flag.clone(),
        });
        state = Some(flag);
        Poll::Pending
    })
}

/// Sleep until an instant.
pub fn sleep_until(at: Instant) -> impl Future<Output = ()> {
    let mut fired: Option<Rc<Cell<bool>>> = None;
    std::future::poll_fn(move |cx| {
        if Instant::now() >= at || fired.as_ref().is_some_and(|f| f.get()) {
            return Poll::Ready(());
        }
        let reactor = current();
        let seq = reactor.seq.get();
        reactor.seq.set(seq + 1);
        let flag = Rc::new(Cell::new(false));
        reactor.timers.borrow_mut().push(Timer {
            at,
            seq,
            waker: cx.waker().clone(),
            fired: flag.clone(),
        });
        fired = Some(flag);
        Poll::Pending
    })
}

/// Sleep for a duration.
pub fn sleep(duration: Duration) -> impl Future<Output = ()> {
    sleep_until(Instant::now() + duration)
}

/// Run `future` until `deadline`. Returns `Err(Timeout)` when it passes first.
pub async fn deadline<T>(at: Instant, future: impl Future<Output = T>) -> Result<T, crate::Error> {
    let mut future = std::pin::pin!(future);
    let mut timer = std::pin::pin!(sleep_until(at));
    std::future::poll_fn(|cx| {
        if let Poll::Ready(v) = future.as_mut().poll(cx) {
            return Poll::Ready(Ok(v));
        }
        if timer.as_mut().poll(cx).is_ready() {
            return Poll::Ready(Err(crate::Error::Timeout));
        }
        Poll::Pending
    })
    .await
}

/// Run `future` for at most `duration`.
pub async fn timeout<T>(
    duration: Duration,
    future: impl Future<Output = T>,
) -> Result<T, crate::Error> {
    deadline(Instant::now() + duration, future).await
}

/// Edge-free broadcast: waiters wake on every [`Signal::notify`].
///
/// Each [`Signal::changed`] future owns one entry in a slab of wakers, so
/// registering, re-registering, and dropping a waiter are O(1) and a notify
/// with W waiters is O(W).
#[derive(Clone, Default)]
pub struct Signal(Rc<SignalInner>);

#[derive(Default)]
struct SignalInner {
    generation: Cell<u64>,
    wakers: RefCell<WakerSlab>,
}

#[derive(Default)]
struct WakerSlab {
    entries: Vec<Option<Waker>>,
    free: Vec<usize>,
    live: usize,
}

impl WakerSlab {
    fn insert(&mut self, waker: Waker) -> usize {
        self.live += 1;
        match self.free.pop() {
            Some(i) => {
                self.entries[i] = Some(waker);
                i
            }
            None => {
                self.entries.push(Some(waker));
                self.entries.len() - 1
            }
        }
    }

    fn remove(&mut self, i: usize) {
        if let Some(e) = self.entries.get_mut(i)
            && e.take().is_some()
        {
            self.live -= 1;
            self.free.push(i);
        }
    }
}

impl Signal {
    /// Wake all current waiters.
    pub fn notify(&self) {
        self.0.generation.set(self.0.generation.get() + 1);
        // Take the slab first: a waker may run code that registers again.
        let slab = std::mem::take(&mut *self.0.wakers.borrow_mut());
        for w in slab.entries.into_iter().flatten() {
            w.wake();
        }
    }

    /// Current generation, to pass to [`Signal::changed`].
    pub fn generation(&self) -> u64 {
        self.0.generation.get()
    }

    /// Wakers registered and not yet woken or dropped.
    #[cfg(test)]
    pub fn waiters(&self) -> usize {
        self.0.wakers.borrow().live
    }

    /// Wait until the generation differs from `seen`.
    pub fn changed(&self, seen: u64) -> Changed<'_> {
        Changed {
            signal: self,
            seen,
            slot: None,
        }
    }
}

/// Future of [`Signal::changed`].
pub struct Changed<'a> {
    signal: &'a Signal,
    seen: u64,
    /// Slab index, valid while the generation still equals `seen` (a
    /// notify empties the slab and bumps the generation together).
    slot: Option<usize>,
}

impl Future for Changed<'_> {
    type Output = ();

    fn poll(mut self: std::pin::Pin<&mut Self>, cx: &mut std::task::Context<'_>) -> Poll<()> {
        let signal = self.signal;
        let inner = &signal.0;
        if inner.generation.get() != self.seen {
            self.slot = None;
            return Poll::Ready(());
        }
        let mut slab = inner.wakers.borrow_mut();
        match self.slot {
            Some(i) => {
                if let Some(Some(w)) = slab.entries.get_mut(i)
                    && !w.will_wake(cx.waker())
                {
                    w.clone_from(cx.waker());
                }
            }
            None => {
                let i = slab.insert(cx.waker().clone());
                drop(slab);
                self.slot = Some(i);
            }
        }
        Poll::Pending
    }
}

impl Drop for Changed<'_> {
    fn drop(&mut self) {
        if let Some(i) = self.slot
            && self.signal.0.generation.get() == self.seen
        {
            self.signal.0.wakers.borrow_mut().remove(i);
        }
    }
}

/// A counting semaphore with an optional bounded FIFO wait.
/// [`Slots::try_acquire`] fails when full; [`Slots::acquire`] queues until a
/// deadline. The rules are [`crate::step::admission::WriteQueue`], shared
/// with the simulator.
#[derive(Clone)]
pub struct Slots(Rc<RefCell<SlotQueue>>);

type SlotQueue = crate::step::admission::WriteQueue<Rc<SlotWaiter>>;

/// A queued [`Slots::acquire`].
#[derive(Default)]
pub struct SlotWaiter {
    /// The slot was handed to this waiter.
    granted: Cell<bool>,
    /// The waiter gave up; skip it.
    gone: Cell<bool>,
    waker: RefCell<Option<Waker>>,
}

fn waiting(w: &Rc<SlotWaiter>) -> bool {
    !w.gone.get() && !w.granted.get()
}

/// A held slot.
pub struct Slot(Rc<RefCell<SlotQueue>>);

fn release(queue: &Rc<RefCell<SlotQueue>>) {
    let next = queue.borrow_mut().release(waiting);
    if let Some(w) = next {
        w.granted.set(true);
        if let Some(waker) = w.waker.borrow_mut().take() {
            waker.wake();
        }
    }
}

impl Drop for Slot {
    fn drop(&mut self) {
        release(&self.0);
    }
}

impl Slots {
    /// `capacity` concurrent holders; nobody may wait.
    pub fn new(capacity: usize) -> Self {
        Self::queued(capacity, 0)
    }

    /// `capacity` concurrent holders and up to `max_waiting` queued
    /// [`Slots::acquire`] calls.
    pub fn queued(capacity: usize, max_waiting: usize) -> Self {
        Self(Rc::new(RefCell::new(SlotQueue::new(capacity, max_waiting))))
    }

    /// Take a slot now. Fails when full or when others are queued.
    pub fn try_acquire(&self) -> Option<Slot> {
        self.0
            .borrow_mut()
            .try_take(waiting)
            .then(|| Slot(self.0.clone()))
    }

    /// Slots held.
    #[cfg(test)]
    pub fn used(&self) -> usize {
        self.0.borrow().used
    }

    /// Take a slot, waiting in FIFO order until `at`. `None` when the queue
    /// is full or the deadline passes first.
    pub async fn acquire(&self, at: Instant) -> Option<Slot> {
        let waiter = Rc::new(SlotWaiter::default());
        let admit = self.0.borrow_mut().admit(waiter.clone(), waiting);
        match admit {
            crate::step::admission::Admit::Take => return Some(Slot(self.0.clone())),
            crate::step::admission::Admit::Full => return None,
            crate::step::admission::Admit::Wait => {}
        }
        // From here the waiter is queued: `Pending` marks it gone on any
        // exit, and hands a granted but unclaimed slot to the next waiter.
        let pending = Pending {
            queue: self.0.clone(),
            waiter,
        };
        let granted = std::future::poll_fn(|cx| {
            if pending.waiter.granted.get() {
                return Poll::Ready(());
            }
            *pending.waiter.waker.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        });
        if deadline(at, granted).await.is_err() && !pending.waiter.granted.get() {
            return None;
        }
        pending.waiter.gone.set(true);
        Some(Slot(self.0.clone()))
    }

    /// Queued entries, including ones that gave up and are not yet trimmed.
    #[cfg(test)]
    pub fn waiting(&self) -> usize {
        self.0.borrow().waiting()
    }
}

struct Pending {
    queue: Rc<RefCell<SlotQueue>>,
    waiter: Rc<SlotWaiter>,
}

impl Drop for Pending {
    fn drop(&mut self) {
        if self.waiter.gone.replace(true) {
            // Claimed: the slot moved into a `Slot`.
            return;
        }
        if self.waiter.granted.get() {
            // Granted but never claimed: pass it on.
            release(&self.queue);
        }
    }
}

/// Nonblocking TCP connection.
pub struct Tcp(TcpStream);

impl Tcp {
    /// Wrap an accepted or connected stream.
    pub fn from_std(stream: TcpStream) -> io::Result<Self> {
        stream.set_nonblocking(true)?;
        stream.set_nodelay(true)?;
        Ok(Self(stream))
    }

    /// Connect without blocking the thread.
    pub async fn connect(address: SocketAddr) -> io::Result<Self> {
        let domain = if address.is_ipv4() {
            libc::AF_INET
        } else {
            libc::AF_INET6
        };
        // SAFETY: plain syscall; the result is checked.
        let fd = unsafe {
            libc::socket(
                domain,
                libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                0,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: `fd` is a new descriptor owned by the stream.
        let stream = unsafe { TcpStream::from_raw_fd(fd) };
        let (storage, length) = sockaddr(address);
        // SAFETY: `storage` holds a valid address of `length` bytes.
        let rc = unsafe { libc::connect(fd, (&raw const storage).cast(), length) };
        if rc < 0 {
            let error = io::Error::last_os_error();
            if error.raw_os_error() != Some(libc::EINPROGRESS) {
                return Err(error);
            }
            ready(fd, Interest::Write).await;
            if let Some(error) = stream.take_error()? {
                return Err(error);
            }
            // A spurious wake can arrive before the connection completes.
            loop {
                match stream.peer_addr() {
                    Ok(_) => break,
                    Err(e) if e.kind() == io::ErrorKind::NotConnected => {
                        ready(fd, Interest::Write).await;
                        if let Some(error) = stream.take_error()? {
                            return Err(error);
                        }
                    }
                    Err(e) => return Err(e),
                }
            }
        }
        Self::from_std(stream)
    }

    pub fn fd(&self) -> RawFd {
        self.0.as_raw_fd()
    }

    pub fn std(&mut self) -> &mut TcpStream {
        &mut self.0
    }
}

fn sockaddr(address: SocketAddr) -> (libc::sockaddr_storage, libc::socklen_t) {
    // SAFETY: an all-zero sockaddr_storage is valid.
    let mut storage: libc::sockaddr_storage = unsafe { std::mem::zeroed() };
    match address {
        SocketAddr::V4(a) => {
            let sin = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: a.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from_ne_bytes(a.ip().octets()),
                },
                sin_zero: [0; 8],
            };
            // SAFETY: sockaddr_storage is larger than sockaddr_in.
            unsafe { std::ptr::write((&raw mut storage).cast(), sin) };
            (storage, size_of::<libc::sockaddr_in>() as libc::socklen_t)
        }
        SocketAddr::V6(a) => {
            let sin6 = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as libc::sa_family_t,
                sin6_port: a.port().to_be(),
                sin6_flowinfo: a.flowinfo(),
                sin6_addr: libc::in6_addr {
                    s6_addr: a.ip().octets(),
                },
                sin6_scope_id: a.scope_id(),
            };
            // SAFETY: sockaddr_storage is larger than sockaddr_in6.
            unsafe { std::ptr::write((&raw mut storage).cast(), sin6) };
            (storage, size_of::<libc::sockaddr_in6>() as libc::socklen_t)
        }
    }
}

/// Nonblocking listener.
pub struct Listener(TcpListener);

impl Listener {
    pub fn bind(address: SocketAddr) -> io::Result<Self> {
        let listener = TcpListener::bind(address)?;
        listener.set_nonblocking(true)?;
        Ok(Self(listener))
    }

    pub fn local_addr(&self) -> io::Result<SocketAddr> {
        self.0.local_addr()
    }

    /// Accept one connection.
    pub async fn accept(&self) -> io::Result<(Tcp, SocketAddr)> {
        loop {
            match self.0.accept() {
                Ok((s, a)) => return Ok((Tcp::from_std(s)?, a)),
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    ready(self.0.as_raw_fd(), Interest::Read).await
                }
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(e) => return Err(e),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        io::{Read, Write},
        sync::atomic::{AtomicUsize, Ordering::SeqCst},
    };

    #[test]
    fn timers_fire_in_order_and_deadline_cancels() {
        let mut ex = Executor::new(16).unwrap();
        let order = Rc::new(RefCell::new(Vec::new()));
        let o = order.clone();
        ex.block_on(async move {
            let a = o.clone();
            spawn(async move {
                sleep(Duration::from_millis(30)).await;
                a.borrow_mut().push(2);
            })
            .unwrap();
            let b = o.clone();
            spawn(async move {
                sleep(Duration::from_millis(5)).await;
                b.borrow_mut().push(1);
            })
            .unwrap();
            assert_eq!(
                timeout(Duration::from_millis(10), sleep(Duration::from_secs(5))).await,
                Err(crate::Error::Timeout)
            );
            sleep(Duration::from_millis(40)).await;
        });
        assert_eq!(*order.borrow(), vec![1, 2]);
    }

    /// Polls `f` once with `waker`.
    fn poll_with<F: Future>(f: std::pin::Pin<&mut F>, waker: &Waker) -> Poll<F::Output> {
        f.poll(&mut std::task::Context::from_waker(waker))
    }

    fn counting_waker() -> (Waker, Arc<AtomicUsize>) {
        struct W(Arc<AtomicUsize>);
        impl std::task::Wake for W {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, SeqCst);
            }
        }
        let n = Arc::new(AtomicUsize::new(0));
        (Waker::from(Arc::new(W(n.clone()))), n)
    }

    #[test]
    fn signal_registration_is_one_entry_per_waiter() {
        let signal = Signal::default();
        let g = signal.generation();
        let (w1, n1) = counting_waker();
        let (w2, n2) = counting_waker();
        let mut a = std::pin::pin!(signal.changed(g));
        let mut b = std::pin::pin!(signal.changed(g));
        for _ in 0..100 {
            assert!(poll_with(a.as_mut(), &w1).is_pending());
        }
        // Re-polling with another waker replaces the entry, not appends.
        assert!(poll_with(a.as_mut(), &w2).is_pending());
        assert!(poll_with(b.as_mut(), &w1).is_pending());
        assert_eq!(signal.waiters(), 2);
        signal.notify();
        assert_eq!(signal.waiters(), 0);
        // `a` last registered `w2`, `b` registered `w1`: one wake each.
        assert_eq!(n1.load(SeqCst), 1);
        assert_eq!(n2.load(SeqCst), 1);
        assert!(poll_with(a.as_mut(), &w1).is_ready());
        assert!(poll_with(b.as_mut(), &w1).is_ready());
        // Already changed: ready without registering.
        let mut c = std::pin::pin!(signal.changed(g));
        assert!(poll_with(c.as_mut(), &w1).is_ready());
        assert_eq!(signal.waiters(), 0);
    }

    #[test]
    fn signal_dropped_waiters_free_their_entries() {
        let signal = Signal::default();
        let g = signal.generation();
        let (w, n) = counting_waker();
        for _ in 0..1000 {
            let mut f = std::pin::pin!(signal.changed(g));
            assert!(poll_with(f.as_mut(), &w).is_pending());
        }
        // Timed out waiters do not accumulate, and their slots are reused.
        assert_eq!(signal.waiters(), 0);
        assert!(signal.0.wakers.borrow().entries.len() <= 1);
        signal.notify();
        assert_eq!(n.load(SeqCst), 0);
        // A waiter dropped after a notify does not touch the new slab.
        let g = signal.generation();
        let mut old = Box::pin(signal.changed(g));
        assert!(poll_with(old.as_mut(), &w).is_pending());
        signal.notify();
        let mut new = std::pin::pin!(signal.changed(signal.generation()));
        assert!(poll_with(new.as_mut(), &w).is_pending());
        drop(old);
        assert_eq!(signal.waiters(), 1);
    }

    #[test]
    fn signal_wakes_many_waiters_in_linear_work() {
        let mut ex = Executor::new(4096).unwrap();
        let signal = Signal::default();
        let woken = Rc::new(Cell::new(0usize));
        ex.block_on(async {
            for _ in 0..2000 {
                let s = signal.clone();
                let w = woken.clone();
                spawn(async move {
                    // Wait twice: the second wait re-registers after a wake.
                    for _ in 0..2 {
                        let g = s.generation();
                        s.changed(g).await;
                        w.set(w.get() + 1);
                    }
                })
                .unwrap();
            }
            sleep(Duration::from_millis(5)).await;
            assert_eq!(signal.waiters(), 2000);
            signal.notify();
            sleep(Duration::from_millis(5)).await;
            assert_eq!(signal.waiters(), 2000);
            assert!(signal.0.wakers.borrow().entries.len() == 2000);
            signal.notify();
            sleep(Duration::from_millis(5)).await;
        });
        assert_eq!(woken.get(), 4000);
        assert_eq!(signal.waiters(), 0);
    }

    #[test]
    fn slots_queue_in_order_and_hand_over() {
        let mut ex = Executor::new(64).unwrap();
        ex.block_on(async {
            let slots = Slots::queued(1, 2);
            let held = slots.try_acquire().unwrap();
            let order = Rc::new(RefCell::new(Vec::new()));
            let far = Instant::now() + Duration::from_secs(5);
            for i in 0..2 {
                let (s, o) = (slots.clone(), order.clone());
                spawn(async move {
                    let slot = s.acquire(far).await.unwrap();
                    o.borrow_mut().push(i);
                    sleep(Duration::from_millis(5)).await;
                    drop(slot);
                })
                .unwrap();
                sleep(Duration::from_millis(1)).await;
            }
            assert_eq!(slots.waiting(), 2);
            // Queue full: real overload fails at once.
            assert!(slots.acquire(far).await.is_none());
            // No queue jumping.
            assert!(slots.try_acquire().is_none());
            drop(held);
            assert_eq!(slots.used(), 1);
            sleep(Duration::from_millis(30)).await;
            assert_eq!(*order.borrow(), vec![0, 1]);
            assert_eq!(slots.used(), 0);
            assert!(slots.try_acquire().is_some());
        });
    }

    #[test]
    fn slots_wait_times_out_and_cancel_passes_slot_on() {
        let mut ex = Executor::new(64).unwrap();
        ex.block_on(async {
            let slots = Slots::queued(1, 8);
            let held = slots.try_acquire().unwrap();
            // Deadline passes while queued.
            let soon = Instant::now() + Duration::from_millis(10);
            assert!(slots.acquire(soon).await.is_none());
            // A cancelled waiter (timeout around acquire) is skipped.
            let far = Instant::now() + Duration::from_secs(5);
            assert!(
                timeout(Duration::from_millis(5), slots.acquire(far))
                    .await
                    .is_err()
            );
            let got = Rc::new(Cell::new(false));
            let (s, g) = (slots.clone(), got.clone());
            spawn(async move {
                let _slot = s.acquire(far).await.unwrap();
                g.set(true);
            })
            .unwrap();
            sleep(Duration::from_millis(2)).await;
            drop(held);
            sleep(Duration::from_millis(5)).await;
            assert!(got.get());
            assert_eq!(slots.used(), 0);
            // Free slot and empty queue: acquire does not wait.
            assert!(slots.acquire(Instant::now()).await.is_some());
            // Plain `new` never queues.
            let plain = Slots::new(1);
            let _h = plain.try_acquire().unwrap();
            assert!(plain.acquire(far).await.is_none());
        });
    }

    #[test]
    fn slots_granted_but_unclaimed_slot_moves_on() {
        let slots = Slots::queued(1, 8);
        let held = slots.try_acquire().unwrap();
        let far = Instant::now() + Duration::from_secs(5);
        let (w, _) = counting_waker();
        let mut ex = Executor::new(8).unwrap();
        ex.block_on(async {
            let mut first = Box::pin(slots.acquire(far));
            let mut second = Box::pin(slots.acquire(far));
            assert!(poll_with(first.as_mut(), &w).is_pending());
            assert!(poll_with(second.as_mut(), &w).is_pending());
            // The slot is handed to `first`, which is dropped before it runs.
            drop(held);
            drop(first);
            assert_eq!(slots.used(), 1);
            match poll_with(second.as_mut(), &w) {
                Poll::Ready(Some(s)) => drop(s),
                _ => panic!("second waiter did not get the slot"),
            }
            assert_eq!(slots.used(), 0);
        });
    }

    #[test]
    fn task_limit_is_enforced() {
        let mut ex = Executor::new(1).unwrap();
        ex.block_on(async {
            spawn(sleep(Duration::from_millis(5))).unwrap();
            assert_eq!(spawn(async {}), Err(crate::Error::Unavailable));
            sleep(Duration::from_millis(20)).await;
            assert_eq!(task_count(), 0);
            spawn(async {}).unwrap();
        });
    }

    #[test]
    fn tcp_connect_accept_and_notify() {
        let mut ex = Executor::new(8).unwrap();
        let notifier = ex.notifier();
        let flag = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let f = flag.clone();
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(20));
            f.store(true, std::sync::atomic::Ordering::SeqCst);
            notifier.notify();
        });
        ex.block_on(async {
            let listener = Listener::bind("127.0.0.1:0".parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            let (client, server) = futures::join!(Tcp::connect(address), listener.accept());
            let mut client = client.unwrap();
            let (mut server, _) = server.unwrap();
            client.std().write_all(b"hi").unwrap();
            let mut buf = [0u8; 2];
            loop {
                match server.std().read(&mut buf) {
                    Ok(2) => break,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        ready(server.fd(), Interest::Read).await
                    }
                    other => panic!("{other:?}"),
                }
            }
            assert_eq!(&buf, b"hi");
            assert!(Tcp::connect("127.0.0.1:1".parse().unwrap()).await.is_err());
        });
        let mut ex2 = ex;
        let f = flag.clone();
        ex2.run(move || f.load(SeqCst));
    }
}
