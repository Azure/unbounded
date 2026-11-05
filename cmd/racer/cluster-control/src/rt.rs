//! The control thread's runtime: a `racer-io` reactor and local executor.
//!
//! All control futures run on one thread. Sockets, timers, and cross-thread
//! wakes are completions of one reactor, so a parked control thread blocks in
//! one `io_uring_enter` and never polls on an interval. The same code runs on
//! the simulated driver with a virtual clock: [`Executor::turn`] does one
//! bounded step and reports when it next needs to run, which a deterministic
//! test can drive itself.
//!
//! The reactor in use is thread-local while [`Executor::run`],
//! [`Executor::turn`], or [`Executor::block_on`] is on the stack.

use racer_io::{
    Driver, OpCtx, Reactor, WorkerCtx,
    exec::{LocalExecutor, Next},
    ring::{Config, Descriptor, SocketAddress, WakeHandle},
};
use std::{
    cell::{Cell, RefCell},
    future::Future,
    io,
    net::SocketAddr,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};

pub use crate::sync::{Signal, Slot, Slots};

/// Most futures polled per turn before the reactor is reaped again.
const BUDGET: usize = 256;

/// Largest reactor; one operation per connection fits within it.
const MAX_QUEUE: usize = 16 * 1024;

/// Smallest reactor.
const MIN_QUEUE: usize = 256;

struct Current {
    reactor: Reactor,
    executor: Rc<LocalExecutor>,
    tasks: Rc<Cell<usize>>,
    max_tasks: usize,
}

thread_local! {
    static CURRENT: RefCell<Option<Rc<Current>>> = const { RefCell::new(None) };
}

fn current() -> Rc<Current> {
    try_current().expect("control reactor is not running")
}

fn try_current() -> Option<Rc<Current>> {
    CURRENT.with(|c| c.borrow().clone())
}

/// Installs a runtime as current until dropped, restoring the previous one.
struct Enter(Option<Rc<Current>>);

impl Enter {
    fn new(current: &Rc<Current>) -> Self {
        Self(CURRENT.with(|c| c.borrow_mut().replace(current.clone())))
    }
}

impl Drop for Enter {
    fn drop(&mut self) {
        let previous = self.0.take();
        CURRENT.with(|c| *c.borrow_mut() = previous);
    }
}

/// Wakes the control thread from any thread.
#[derive(Clone)]
pub struct Notifier(Arc<WakeHandle>);

impl Notifier {
    /// Wake a parked control thread. Coalesced.
    pub fn notify(&self) {
        self.0.wake();
    }
}

/// Owner of the control reactor and its tasks. Not `Send`.
pub struct Executor {
    current: Rc<Current>,
}

impl Executor {
    /// A kernel (io_uring) executor with a fixed task limit.
    pub fn new(max_tasks: usize) -> io::Result<Self> {
        let entries = max_tasks.next_power_of_two().clamp(MIN_QUEUE, MAX_QUEUE);
        let reactor = Reactor::new(
            Driver::Uring,
            Config::new(entries as u32),
            WorkerCtx::host(),
        )
        .map_err(to_io)?;
        Ok(Self::with_reactor(reactor, max_tasks))
    }

    /// A simulated executor: `os` is the simulated disk and network, shared
    /// by every node of one world, and `clock` is this node's clock. Nothing
    /// blocks and time moves only through the clock.
    pub fn simulated(
        os: racer_io::sim::SimOs,
        clock: &racer_io::ctx::VirtualClock,
        seed: u64,
        max_tasks: usize,
    ) -> io::Result<Self> {
        let entries = max_tasks.next_power_of_two().clamp(MIN_QUEUE, MAX_QUEUE);
        let reactor = Reactor::new(
            Driver::Sim(os),
            Config::new(entries as u32),
            WorkerCtx::simulated(clock, seed, 0),
        )
        .map_err(to_io)?;
        Ok(Self::with_reactor(reactor, max_tasks))
    }

    /// An executor over an existing reactor, such as a simulated one.
    pub fn with_reactor(reactor: Reactor, max_tasks: usize) -> Self {
        let executor = Rc::new(LocalExecutor::new(Some(reactor.wake_handle())));
        Self {
            current: Rc::new(Current {
                reactor,
                executor,
                tasks: Rc::new(Cell::new(0)),
                max_tasks,
            }),
        }
    }

    /// Cross-thread wake handle.
    pub fn notifier(&self) -> Notifier {
        Notifier(self.current.reactor.wake_handle())
    }

    /// Run `f` with this runtime current, for setup outside a turn.
    pub fn enter<T>(&self, f: impl FnOnce() -> T) -> T {
        let _enter = Enter::new(&self.current);
        f()
    }

    /// One bounded step: reap completions and poll ready tasks.
    pub fn turn(&mut self) -> io::Result<Next> {
        let _enter = Enter::new(&self.current);
        let c = &self.current;
        c.reactor.reap(BUDGET).map_err(to_io)?;
        c.executor.run_ready(BUDGET);
        Ok(self.next())
    }

    /// What the runtime needs next, without doing work.
    pub fn next(&self) -> Next {
        let c = &self.current;
        if c.executor.has_ready() || c.reactor.has_ready() {
            return Next::Ready;
        }
        match c.reactor.next_deadline() {
            Some(at) => Next::WaitUntil(at),
            None => Next::Idle,
        }
    }

    /// Block until the condition reported by `next`: a completion, a
    /// cross-thread wake, or the deadline.
    pub fn park(&mut self, next: Next) -> io::Result<()> {
        let until = match next {
            Next::Ready | Next::Done => return Ok(()),
            Next::WaitUntil(at) => Some(at),
            Next::Idle => None,
        };
        self.current.reactor.park(until).map_err(to_io)
    }

    /// Run until `done` returns true. `done` is checked after each turn.
    #[cfg(test)]
    pub fn run(&mut self, mut done: impl FnMut() -> bool) {
        loop {
            if self.turn().is_err() {
                return;
            }
            if self.enter(&mut done) {
                return;
            }
            // `done` may have spawned or woken tasks.
            if self.park(self.next()).is_err() {
                return;
            }
        }
    }

    /// Run a future to completion on this executor, for tests. The future
    /// is polled in place, so it may borrow from the caller.
    #[cfg(test)]
    pub fn block_on<F: Future>(&mut self, future: F) -> F::Output {
        struct Flag(std::sync::atomic::AtomicBool);
        impl std::task::Wake for Flag {
            fn wake(self: Arc<Self>) {
                self.0.store(true, std::sync::atomic::Ordering::SeqCst);
            }
        }
        let flag = Arc::new(Flag(std::sync::atomic::AtomicBool::new(true)));
        let waker = std::task::Waker::from(flag.clone());
        let mut future = std::pin::pin!(future);
        loop {
            self.turn().expect("reactor turn");
            if flag.0.swap(false, std::sync::atomic::Ordering::SeqCst) {
                let _enter = Enter::new(&self.current);
                let mut cx = std::task::Context::from_waker(&waker);
                if let std::task::Poll::Ready(v) = future.as_mut().poll(&mut cx) {
                    return v;
                }
            }
            if flag.0.load(std::sync::atomic::Ordering::SeqCst) {
                continue;
            }
            self.park(self.next()).expect("reactor park");
        }
    }
}

impl Drop for Executor {
    /// Drop tasks with this runtime current, so their operations cancel
    /// against the right reactor.
    fn drop(&mut self) {
        let _enter = Enter::new(&self.current);
        self.current.reactor.stop();
    }
}

/// Map a reactor error to an I/O error.
pub fn to_io(e: racer_io::Error) -> io::Error {
    match e {
        racer_io::Error::DeadlineExceeded => io::ErrorKind::TimedOut.into(),
        racer_io::Error::Cancelled => io::ErrorKind::Interrupted.into(),
        e => match e.errno() {
            Some(errno) => io::Error::from_raw_os_error(errno),
            None => io::Error::other(format!("{e:?}")),
        },
    }
}

/// Spawn a task. Fails when the fixed task limit is reached.
pub fn spawn(future: impl Future<Output = ()> + 'static) -> Result<(), crate::Error> {
    let c = current();
    if c.tasks.get() >= c.max_tasks {
        return Err(crate::Error::Unavailable);
    }
    struct Guard(Rc<Cell<usize>>);
    impl Drop for Guard {
        fn drop(&mut self) {
            self.0.set(self.0.get() - 1);
        }
    }
    c.tasks.set(c.tasks.get() + 1);
    let guard = Guard(c.tasks.clone());
    c.executor.spawn_detached(async move {
        let _guard = guard;
        future.await;
    });
    Ok(())
}

/// Current task count.
pub fn task_count() -> usize {
    current().tasks.get()
}

/// Current time on the reactor clock, or the host clock outside a runtime.
pub fn now() -> Instant {
    try_current().map_or_else(Instant::now, |c| c.reactor.now())
}

/// Current wall time on the reactor clock, or the host clock outside a
/// runtime. Lease timestamps and certificate validity use it, so a
/// simulated authority stamps virtual wall time.
pub fn wall_now() -> std::time::SystemTime {
    try_current().map_or_else(std::time::SystemTime::now, |c| {
        c.reactor.ctx().clock.wall_now()
    })
}

/// Certificate validity time for rustls: the wall clock of the runtime
/// current when this is called, or the host clock outside a runtime. A
/// simulated authority then checks peer certificates against virtual time.
pub fn time_provider() -> Arc<dyn rustls::time_provider::TimeProvider> {
    let clock = try_current().map_or_else(racer_io::Clock::host, |c| c.reactor.ctx().clock.clone());
    Arc::new(WallTime(clock))
}

#[derive(Debug)]
struct WallTime(racer_io::Clock);

impl rustls::time_provider::TimeProvider for WallTime {
    fn current_time(&self) -> Option<rustls::pki_types::UnixTime> {
        let since = self
            .0
            .wall_now()
            .duration_since(std::time::UNIX_EPOCH)
            .ok()?;
        Some(rustls::pki_types::UnixTime::since_unix_epoch(since))
    }
}

/// The simulated OS of the current runtime, if it is simulated.
#[cfg(test)]
pub fn sim_os() -> Option<racer_io::sim::SimOs> {
    try_current().and_then(|c| c.reactor.sim_os())
}

/// Sleep until an instant on the reactor clock.
pub fn sleep_until(at: Instant) -> impl Future<Output = ()> {
    current().reactor.sleep_until(at)
}

/// Sleep for a duration.
pub fn sleep(duration: Duration) -> impl Future<Output = ()> {
    sleep_until(now() + duration)
}

/// Run `future` until `deadline`. Returns `Err(Timeout)` when it passes first.
pub async fn deadline<T>(at: Instant, future: impl Future<Output = T>) -> Result<T, crate::Error> {
    let mut future = std::pin::pin!(future);
    let mut timer = std::pin::pin!(sleep_until(at));
    std::future::poll_fn(|cx| {
        if let std::task::Poll::Ready(v) = future.as_mut().poll(cx) {
            return std::task::Poll::Ready(Ok(v));
        }
        if timer.as_mut().poll(cx).is_ready() {
            return std::task::Poll::Ready(Err(crate::Error::Timeout));
        }
        std::task::Poll::Pending
    })
    .await
}

/// Run `future` for at most `duration`.
pub async fn timeout<T>(
    duration: Duration,
    future: impl Future<Output = T>,
) -> Result<T, crate::Error> {
    deadline(now() + duration, future).await
}

/// Read a small file (such as a projected token or CA bundle) on the
/// reactor, so the control thread never blocks on the filesystem.
/// Ordinary symlinks are followed; files over `limit` bytes fail.
pub async fn read_file(path: &std::path::Path, limit: usize) -> io::Result<Vec<u8>> {
    let c = current();
    let bytes = c
        .reactor
        .read_path(path, limit, &OpCtx::none())
        .await
        .map_err(to_io)?;
    Ok(bytes.to_vec())
}

/// Size of one socket receive.
const RECV_BYTES: usize = 16 * 1024;

/// A connected stream socket on the reactor.
pub struct Tcp {
    fd: Rc<Descriptor>,
    /// Receive buffer, reused across reads. `None` after a cancelled read
    /// kept it in the reactor until its fence.
    buf: Option<Vec<u8>>,
}

impl Tcp {
    fn new(fd: Descriptor) -> Self {
        let _ = fd.enable_tcp_nodelay(true);
        Self {
            fd: Rc::new(fd),
            buf: None,
        }
    }

    /// Connect on the reactor.
    pub async fn connect(address: SocketAddr) -> io::Result<Self> {
        let c = current();
        let domain = if address.is_ipv4() {
            libc::AF_INET
        } else {
            libc::AF_INET6
        };
        let fd = match c.reactor.sim_os() {
            Some(os) => os.socket(domain)?,
            None => Descriptor::socket(domain).map_err(to_io)?,
        };
        let fd = Rc::new(fd);
        c.reactor
            .connect(fd.clone(), SocketAddress::Inet(address), (), &OpCtx::none())
            .await
            .map_err(to_io)?;
        let fd = Rc::try_unwrap(fd).map_err(|_| io::Error::other("descriptor still shared"))?;
        Ok(Self::new(fd))
    }

    /// Receive and append to `out`. Returns 0 at end of stream.
    pub async fn recv_into(&mut self, out: &mut Vec<u8>) -> io::Result<usize> {
        let buf = self.buf.take().unwrap_or_else(|| vec![0; RECV_BYTES]);
        let done = current()
            .reactor
            .recv(self.fd.clone(), buf, (), &OpCtx::none())
            .await
            .map_err(to_io)?;
        out.extend_from_slice(&done.buffer[..done.bytes]);
        self.buf = Some(done.buffer);
        Ok(done.bytes)
    }

    /// Send all of `data`.
    pub async fn send_all(&mut self, data: Vec<u8>) -> io::Result<()> {
        let mut data = data;
        loop {
            let done = current()
                .reactor
                .send(self.fd.clone(), data, (), &OpCtx::none())
                .await
                .map_err(to_io)?;
            if done.bytes == 0 {
                return Err(io::ErrorKind::WriteZero.into());
            }
            if done.bytes >= done.buffer.len() {
                return Ok(());
            }
            data = done.buffer;
            data.drain(..done.bytes);
        }
    }
}

/// A stream listener on the reactor.
pub struct Listener {
    fd: Rc<Descriptor>,
    address: SocketAddr,
}

impl Listener {
    /// Bind a listener. Uses the simulated network when the current runtime
    /// is simulated.
    pub fn bind(address: SocketAddr) -> io::Result<Self> {
        if let Some(os) = try_current().and_then(|c| c.reactor.sim_os()) {
            let fd = os.listen(SocketAddress::Inet(address))?;
            return Ok(Self {
                fd: Rc::new(fd),
                address,
            });
        }
        let listener = std::net::TcpListener::bind(address)?;
        listener.set_nonblocking(true)?;
        let address = listener.local_addr()?;
        Ok(Self {
            fd: Rc::new(Descriptor::from(listener)),
            address,
        })
    }

    pub fn local_addr(&self) -> io::Result<SocketAddr> {
        Ok(self.address)
    }

    /// Accept one connection.
    pub async fn accept(&self) -> io::Result<(Tcp, SocketAddr)> {
        let fd = current()
            .reactor
            .accept(self.fd.clone(), &OpCtx::none())
            .await
            .map_err(to_io)?;
        let peer = fd
            .tcp_tuple()
            .map_or(SocketAddr::from(([0, 0, 0, 0], 0)), |(_, peer)| peer);
        Ok((Tcp::new(fd), peer))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        sync::atomic::{AtomicUsize, Ordering::SeqCst},
        task::{Poll, Waker},
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
        assert!(signal.slab_len() <= 1);
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
            assert!(signal.slab_len() == 2000);
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
            client.send_all(b"hi".to_vec()).await.unwrap();
            let mut buf = Vec::new();
            while buf.len() < 2 {
                assert!(server.recv_into(&mut buf).await.unwrap() > 0);
            }
            assert_eq!(&buf[..], b"hi");
            assert!(Tcp::connect("127.0.0.1:1".parse().unwrap()).await.is_err());
        });
        let mut ex2 = ex;
        let f = flag.clone();
        ex2.run(move || f.load(SeqCst));
    }
}
