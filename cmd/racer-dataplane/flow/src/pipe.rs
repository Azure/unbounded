//! Bounded, nonblocking, per-reader kernel pipes.
//!
//! Each lease owns both descriptors and its admission charge. Idle empty pipes
//! retain that charge in the worker-local pool. No descriptor or backing page is
//! recycled while a reader owns the lease. Writes copy into kernel
//! pipe pages before splice: socket acceptance is not a userspace page reuse fence.
use crate::{Charge, Error, Policy, Quotas, Result};
use std::{
    cell::RefCell,
    collections::VecDeque,
    future::poll_fn,
    io,
    os::fd::{AsFd, AsRawFd, FromRawFd},
    rc::{Rc, Weak},
    task::{Poll, Waker},
};
use uring_runtime::reactor::descriptor::Descriptor;

/// Maximum kernel buffer capacity per admitted reader. Pipe admission is in pipe
/// units, so total pipe capacity is bounded by the pipe quota * MAX_PIPE_BYTES.
/// Spliced bytes retained by sockets are subject to socket buffer limits instead.
pub const MAX_PIPE_BYTES: usize = 64 * 1024;

/// Worker-local pipe admission and a FIFO of bounded, byte-charged waiters.
pub struct PipePool<P: Policy> {
    quotas: Rc<Quotas<P>>,

    pipe_class: P::Class,

    waiter_class: P::Class,

    waiter_limit: usize,

    waiting: Waiters,

    idle: Rc<RefCell<Vec<PipeResources<P>>>>,
}

/// Shared local queue whose head alone attempts scheduled admission.
type Waiters = Rc<RefCell<VecDeque<Rc<RefCell<Option<Waker>>>>>>;

/// Wakes the queue after resource return; holds no reactor reference.
struct Notify(Waiters);

impl Drop for Notify {
    /// Notify the oldest queued acquisition after lease resources are released.
    fn drop(&mut self) {
        wake_front(&self.0);
    }
}

/// Consume the head's notification before calling it without queue or entry borrows.
fn wake_front(waiters: &Waiters) {
    let wake = waiters
        .borrow()
        .front()
        .and_then(|entry| entry.borrow_mut().take());
    if let Some(wake) = wake {
        wake.wake();
    }
}

/// Own a queue registration and its admission through cancellation or completion.
struct Waiting<P: Policy> {
    queue: Waiters,

    entry: Rc<RefCell<Option<Waker>>>,

    _reservation: Charge<P>,
}

impl<P: Policy> Drop for Waiting<P> {
    /// Remove exactly this waiter and notify its successor without self-polling.
    fn drop(&mut self) {
        {
            let mut queue = self.queue.borrow_mut();
            queue.retain(|entry| !Rc::ptr_eq(entry, &self.entry));
            if queue.is_empty() {
                // Do not retain queue storage after its admission charges leave.
                *queue = VecDeque::new();
            } else if queue.capacity() > queue.len().saturating_mul(2) {
                // Keep spare slots covered by live waiters' fixed allowances.
                // Shrink geometrically rather than reallocating on every departure.
                queue.shrink_to_fit();
            }
        }
        wake_front(&self.queue);
    }
}

/// Non-cloneable local ownership of both descriptors and their admission charge.
pub struct PipeLease<P: Policy> {
    resources: Option<PipeResources<P>>,

    pool: Weak<RefCell<Vec<PipeResources<P>>>>,

    quotas: Weak<Quotas<P>>,

    _notify: Notify,
}

/// Kernel pipe state; descriptors close before the trailing charge is released.
struct PipeResources<P: Policy> {
    read: Descriptor,

    write: Descriptor,

    capacity: usize,

    buffered: usize,

    // Declared after the descriptors so capacity is returned only after closing.
    _reservation: Charge<P>,
}

impl<P: Policy> Drop for PipeLease<P> {
    /// Recycle only empty pipes on a live authority; close partial payloads.
    fn drop(&mut self) {
        // Reactor ownership keeps the lease alive through every accepted CQE.
        // Never recycle canceled/partially drained payloads: close those pipes.
        if let Some(resources) = self.resources.take()
            && resources.buffered == 0
            && self.quotas.upgrade().is_some_and(|q| !q.is_stopped())
            && let Some(pool) = self.pool.upgrade()
        {
            pool.borrow_mut().push(resources);
        }
        // Notify drops after the pipe has been recycled or its charge released.
    }
}

impl<P: Policy> PipePool<P> {
    /// Charge each live or idle pipe by one unit of `pipe_class`. Queued waits
    /// charge bytes to `waiter_class`; `waiter_limit` bounds their count separately.
    /// A zero waiter limit permits immediate acquisition only.
    pub fn new(
        quotas: Rc<Quotas<P>>,
        pipe_class: P::Class,
        waiter_class: P::Class,
        waiter_limit: usize,
    ) -> Self {
        Self {
            quotas,
            pipe_class,
            waiter_class,
            waiter_limit,
            waiting: Rc::default(),
            idle: Rc::default(),
        }
    }

    /// Borrow the worker-local authority used by pipe and waiter admission.
    pub fn quotas(&self) -> &Rc<Quotas<P>> {
        &self.quotas
    }

    /// Byte charge retained for each queue entry, excluding the caller's future.
    /// The fixed allowance covers the queue slot, wake cell, and registration.
    pub fn waiter_bytes(&self) -> usize {
        std::mem::size_of::<Waiting<P>>() + 128
    }

    /// Empty retained pipes, still charged to the worker's fixed pipe budget.
    pub fn idle_count(&self) -> usize {
        self.idle.borrow().len()
    }

    /// Drain idle pipes on stop without releasing resources still owned by leases.
    fn check_running(&self) -> Result<()> {
        if self.quotas.is_stopped() {
            let idle = std::mem::take(&mut *self.idle.borrow_mut());
            // Charge destruction may reenter the pool; release the borrow first.
            drop(idle);
            return Err(Error::Unavailable);
        }
        Ok(())
    }

    /// FIFO scheduling above immediate raw admission. At most `waiter_limit` wait
    /// without pipes or new page acquisitions; each entry charges context bytes
    /// for its guard, queue slot, wake cell, and cancellation registration.
    /// `check` runs before acquisition and on every queued poll. `subscribe` runs
    /// only after queue capacity and its byte charge are secured, never on the
    /// immediate path. Its returned closure registers cancellation notification
    /// with each poll's waker and owns the registration until this wait ends.
    /// The caller must arrange polls for deadlines, quota stop, or charges held
    /// outside this pool; pipe returns and queue removal wake the FIFO head.
    /// No timer, polling loop, or self-wake is created here.
    pub async fn acquire_wait<E, Check, Subscribe, Register>(
        &self,
        mut check: Check,
        subscribe: Subscribe,
    ) -> std::result::Result<PipeLease<P>, E>
    where
        E: From<Error>,
        Check: FnMut() -> std::result::Result<(), E>,
        Subscribe: FnOnce() -> std::result::Result<Register, E>,
        Register: FnMut(&Waker),
    {
        check()?;
        if self.waiting.borrow().is_empty() {
            match self.acquire() {
                Err(Error::Overloaded) => {}
                result => return result.map_err(E::from),
            }
        }
        self.check_running()?;
        if self.waiting.borrow().len() >= self.waiter_limit {
            return Err(Error::Overloaded.into());
        }
        let reservation = self
            .quotas
            .reserve(None, self.waiter_class, self.waiter_bytes())?;
        // Reservation policy can synchronously admit another waiter.
        if self.waiting.borrow().len() >= self.waiter_limit {
            return Err(Error::Overloaded.into());
        }
        let entry = Rc::new(RefCell::new(None));
        self.waiting.borrow_mut().push_back(entry.clone());
        let waiting = Waiting {
            queue: self.waiting.clone(),
            entry,
            _reservation: reservation,
        };
        // Publish before reentry; Waiting rolls back subscription errors or panics.
        let mut register = subscribe()?;
        poll_fn(|cx| {
            register(cx.waker());
            check()?;
            self.check_running()?;
            // Clone and drop may reenter; publish before retiring the old waker.
            let wake = cx.waker().clone();
            let old = waiting.entry.borrow_mut().replace(wake);
            drop(old);
            if self
                .waiting
                .borrow()
                .front()
                .is_some_and(|entry| Rc::ptr_eq(entry, &waiting.entry))
            {
                match self.acquire() {
                    Err(Error::Overloaded) => {}
                    result => return Poll::Ready(result.map_err(E::from)),
                }
            }
            Poll::Pending
        })
        .await
    }

    /// Reserve before creating descriptors. Exhaustion never waits for a reader.
    pub fn acquire(&self) -> Result<PipeLease<P>> {
        self.check_running()?;
        if let Some(resources) = self.idle.borrow_mut().pop() {
            return Ok(self.lease(resources));
        }
        let reservation = self.quotas.reserve(None, self.pipe_class, 1)?;
        reservation.validate(self.pipe_class, 1)?;
        Ok(self.lease(PipeResources::new(reservation)?))
    }

    /// Attach local recycling and notification to uniquely owned resources.
    fn lease(&self, resources: PipeResources<P>) -> PipeLease<P> {
        PipeLease {
            resources: Some(resources),
            pool: Rc::downgrade(&self.idle),
            quotas: Rc::downgrade(&self.quotas),
            _notify: Notify(self.waiting.clone()),
        }
    }
}

impl<P: Policy> PipeResources<P> {
    /// Create bounded descriptors, closing them before admission on any failure.
    fn new(reservation: Charge<P>) -> Result<Self> {
        #[cfg(feature = "simulation")]
        if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
            let (read, write) = sim.pipe(MAX_PIPE_BYTES);
            return Ok(Self {
                read,
                write,
                capacity: MAX_PIPE_BYTES,
                buffered: 0,
                _reservation: reservation,
            });
        }
        let mut fds = [-1; 2];
        // SAFETY: pipe2 initializes exactly two descriptors on success.
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_NONBLOCK | libc::O_CLOEXEC) } < 0 {
            return Err(Error::Io);
        }
        // SAFETY: both descriptors were newly created and have unique owners.
        let read = unsafe { Descriptor::from_raw_fd(fds[0]) };
        let write = unsafe { Descriptor::from_raw_fd(fds[1]) };
        // SAFETY: fcntl operates on a live descriptor and requires no pointer.
        let mut capacity = unsafe { libc::fcntl(write.as_raw_fd(), libc::F_GETPIPE_SZ) };
        if capacity < 0 {
            return Err(Error::Io);
        }
        if capacity as usize != MAX_PIPE_BYTES {
            // Request one bounded chunk once at creation, before pooling. Under
            // UID pipe pressure growth may fail; retain the smaller actual size.
            // SAFETY: the empty pipe can be resized without borrowing user memory.
            let resized = unsafe {
                libc::fcntl(write.as_raw_fd(), libc::F_SETPIPE_SZ, MAX_PIPE_BYTES as i32)
            };
            if resized > 0 {
                capacity = resized;
            }
        }
        if capacity <= 0 || capacity as usize > MAX_PIPE_BYTES {
            return Err(Error::Io);
        }
        Ok(Self {
            read,
            write,
            capacity: capacity as usize,
            buffered: 0,
            _reservation: reservation,
        })
    }
}

impl<P: Policy> PipeLease<P> {
    /// Transit benefits from a full bounded chunk even when the UID's default
    /// pipe size has shrunk. Failure to grow is harmless: use the actual capacity.
    pub fn prepare_transit(&mut self) {
        let pipe = self.resources.as_mut().unwrap();
        #[cfg(feature = "simulation")]
        if pipe.write.as_sim().is_some() {
            return;
        }
        if pipe.capacity < MAX_PIPE_BYTES && pipe.buffered == 0 {
            // SAFETY: live, empty pipe, bounded integer capacity, no user pointer.
            let capacity = unsafe {
                libc::fcntl(
                    pipe.write.as_raw_fd(),
                    libc::F_SETPIPE_SZ,
                    MAX_PIPE_BYTES as i32,
                )
            };
            if capacity > 0 {
                pipe.capacity = capacity as usize;
            }
        }
    }

    /// Receive opaque socket pages directly into an empty bounded pipe. No user
    /// buffer is borrowed or retained by this synchronous nonblocking syscall.
    /// The descriptor is validated as a nonblocking stream socket. Callers must
    /// not concurrently clear O_NONBLOCK through a duplicate descriptor.
    pub fn try_splice_from(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize> {
        #[cfg(feature = "simulation")]
        if socket.as_sim().is_some() || self.resources.as_ref().unwrap().write.as_sim().is_some() {
            return Err(io::Error::from_raw_os_error(libc::EOPNOTSUPP));
        }
        validate_socket(socket)?;
        let pipe = self.resources.as_mut().unwrap();
        let count = count.min(pipe.capacity - pipe.buffered);
        // SAFETY: socket is a validated nonblocking stream; this pipe owns
        // both ends, offsets are null, and no userspace pointer enters the kernel.
        let received = syscall_count(unsafe {
            libc::splice(
                socket.as_raw_fd(),
                std::ptr::null_mut(),
                pipe.write.as_raw_fd(),
                std::ptr::null_mut(),
                count,
                libc::SPLICE_F_NONBLOCK | libc::SPLICE_F_MOVE,
            )
        })?;
        pipe.buffered += received;
        Ok(received)
    }

    /// Return actual kernel capacity, which may be below the requested ceiling.
    pub fn capacity(&self) -> usize {
        self.resources.as_ref().unwrap().capacity
    }

    /// Return the exact suffix still retained in this pipe.
    pub fn buffered(&self) -> usize {
        self.resources.as_ref().unwrap().buffered
    }

    /// Copy at most the available capacity. WouldBlock and Interrupted are exposed
    /// to the caller; this method never waits or retains a borrowed buffer.
    pub fn try_write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        let pipe = self.resources.as_mut().unwrap();
        #[cfg(feature = "simulation")]
        if let Some(handle) = pipe.write.as_sim() {
            let written = handle.pipe_write(bytes)?;
            pipe.buffered += written;
            return Ok(written);
        }
        // SAFETY: the initialized slice stays live for this nonblocking syscall.
        // The read end is owned by this lease, so this cannot generate SIGPIPE.
        let written = unsafe {
            libc::write(
                pipe.write.as_raw_fd(),
                bytes.as_ptr().cast(),
                bytes.len().min(pipe.capacity),
            )
        };
        let written = syscall_count(written)?;
        pipe.buffered += written;
        Ok(written)
    }

    /// Read currently buffered bytes, or return WouldBlock for an empty pipe.
    pub fn try_read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        let pipe = self.resources.as_mut().unwrap();
        #[cfg(feature = "simulation")]
        if let Some(handle) = pipe.read.as_sim() {
            let read = handle.pipe_read(bytes)?;
            pipe.buffered -= read;
            return Ok(read);
        }
        // SAFETY: the destination is exclusively borrowed until read returns.
        let read = unsafe {
            libc::read(
                pipe.read.as_raw_fd(),
                bytes.as_mut_ptr().cast(),
                bytes.len(),
            )
        };
        let read = syscall_count(read)?;
        pipe.buffered -= read;
        Ok(read)
    }

    /// Splice a bounded suffix using production or simulated runtime descriptors.
    pub fn try_splice_descriptor(
        &mut self,
        socket: &Descriptor,
        count: usize,
    ) -> io::Result<usize> {
        #[cfg(feature = "simulation")]
        {
            let pipe = self.resources.as_mut().unwrap();
            match (pipe.read.as_sim(), socket.as_sim()) {
                (Some(read), Some(socket)) => {
                    let sent = read.splice(socket, count.min(pipe.buffered))?;
                    pipe.buffered -= sent;
                    return Ok(sent);
                }
                (None, None) => {}
                _ => return Err(io::Error::from_raw_os_error(libc::EOPNOTSUPP)),
            }
        }
        self.try_splice_to(socket, count)
    }

    /// Transfer copied kernel pipe bytes to a nonblocking stream socket. The
    /// caller retains both owners through this synchronous syscall. Unsupported
    /// splice errors leave the bytes in the pipe for an independent copy fallback.
    /// Callers must not concurrently clear O_NONBLOCK through a duplicate FD.
    pub fn try_splice_to(&mut self, socket: &impl AsFd, count: usize) -> io::Result<usize> {
        validate_socket(socket)?;
        self.splice_to_fd(socket.as_fd().as_raw_fd(), count)
    }

    /// Drain to a stream socket. Like `try_splice_to`, this safe API validates
    /// O_NONBLOCK and SO_TYPE; a Descriptor alone does not prove either property.
    /// Callers must not concurrently clear O_NONBLOCK through a duplicate FD.
    pub fn try_splice_connection(&mut self, socket: &Descriptor) -> io::Result<usize> {
        self.try_splice_descriptor(socket, self.buffered())
    }

    /// Drain a validated socket while masking only this thread's generated SIGPIPE.
    fn splice_to_fd(&mut self, fd: libc::c_int, count: usize) -> io::Result<usize> {
        let pipe = self.resources.as_mut().unwrap();
        if count == 0 || pipe.buffered == 0 {
            return Ok(0);
        }
        // Unlike send, splice has no MSG_NOSIGNAL. Mask only on this worker and
        // only across the syscall, consuming our own EPIPE signal before restore.
        let signal = SigpipeGuard::block()?;
        // SAFETY: owned live FDs, null offsets for pipe/socket, no userspace page
        // pointers, and both ends are nonblocking. No vmsplice/GIFT is involved.
        let result = syscall_count(unsafe {
            libc::splice(
                pipe.read.as_raw_fd(),
                std::ptr::null_mut(),
                fd,
                std::ptr::null_mut(),
                count.min(pipe.buffered),
                libc::SPLICE_F_NONBLOCK,
            )
        });
        if result
            .as_ref()
            .is_err_and(|e| e.raw_os_error() == Some(libc::EPIPE))
        {
            signal.consume_generated();
        }
        drop(signal);
        if let Ok(sent) = result {
            pipe.buffered -= sent;
        }
        result
    }
}

/// Require a live nonblocking stream socket before synchronous splice.
fn validate_socket(socket: &impl AsFd) -> io::Result<()> {
    let fd = socket.as_fd().as_raw_fd();
    // SPLICE_F_NONBLOCK controls only the pipe side. O_NONBLOCK alone does not
    // prevent regular-file I/O from blocking, so require a stream socket too.
    // SAFETY: AsFd borrows a live descriptor for this synchronous query.
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
    if flags < 0 {
        return Err(io::Error::last_os_error());
    }
    if flags & libc::O_NONBLOCK == 0 {
        return Err(io::Error::from_raw_os_error(libc::EINVAL));
    }
    let mut kind: libc::c_int = 0;
    let mut length = std::mem::size_of_val(&kind) as libc::socklen_t;
    // SAFETY: both output pointers reference correctly sized local values.
    if unsafe {
        libc::getsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_TYPE,
            (&mut kind as *mut libc::c_int).cast(),
            &mut length,
        )
    } < 0
    {
        return Err(io::Error::last_os_error());
    }
    if kind != libc::SOCK_STREAM {
        return Err(io::Error::from_raw_os_error(libc::EINVAL));
    }
    Ok(())
}

/// Temporarily mask SIGPIPE while preserving the thread's prior signal state.
struct SigpipeGuard {
    previous: libc::sigset_t,

    set: libc::sigset_t,

    was_pending: bool,
}

impl SigpipeGuard {
    /// Save the thread mask, block SIGPIPE, and remember preexisting signals.
    fn block() -> io::Result<Self> {
        // SAFETY: all signal set pointers refer to initialized local storage.
        unsafe {
            let mut previous = std::mem::zeroed();
            let mut set = std::mem::zeroed();
            libc::sigemptyset(&mut set);
            libc::sigaddset(&mut set, libc::SIGPIPE);
            let error = libc::pthread_sigmask(libc::SIG_BLOCK, &set, &mut previous);
            if error != 0 {
                return Err(io::Error::from_raw_os_error(error));
            }
            // Only a successfully installed mask creates a restoring owner.
            let mut guard = Self {
                previous,
                set,
                was_pending: true,
            };
            let mut pending = std::mem::zeroed();
            if libc::sigpending(&mut pending) < 0 {
                return Err(io::Error::last_os_error());
            }
            guard.was_pending = libc::sigismember(&pending, libc::SIGPIPE) == 1;
            Ok(guard)
        }
    }

    /// Consume only a newly generated signal without waiting.
    fn consume_generated(&self) {
        if self.was_pending {
            return;
        }
        let timeout = libc::timespec {
            tv_sec: 0,
            tv_nsec: 0,
        };
        // SAFETY: zero timeout never waits; only our thread's blocked SIGPIPE is
        // consumed. Preserve a signal that was pending before entering the guard.
        while unsafe { libc::sigtimedwait(&self.set, std::ptr::null_mut(), &timeout) } < 0 {
            if io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
                break;
            }
        }
    }
}

impl Drop for SigpipeGuard {
    /// Restore the exact mask saved after successful installation.
    fn drop(&mut self) {
        // SAFETY: restore the exact thread mask saved by successful pthread_sigmask.
        unsafe { libc::pthread_sigmask(libc::SIG_SETMASK, &self.previous, std::ptr::null_mut()) };
    }
}

/// Convert a syscall byte count while preserving its OS error.
fn syscall_count(value: isize) -> io::Result<usize> {
    if value < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(value as usize)
    }
}

/// Unsupported splice leaves buffered bytes available to a copy fallback.
pub fn splice_unsupported(error: &io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
    )
}

/// Kernel behavior, admission, cancellation, and simulation ownership contracts.
#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        future::Future,
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
        task::{Context, Wake},
    };

    /// Independent pipe, waiter-context, and payload fixture resources.
    #[derive(Clone, Copy)]
    enum ResourceClass {
        Pipe,

        RequestContext,

        Plaintext,
    }

    impl crate::Class for ResourceClass {
        const COUNT: usize = 3;

        /// Select the fixture's stable per-class counter.
        fn index(self) -> usize {
            self as usize
        }
    }

    /// Independent pipe and context limits with pipe-release notifications.
    struct TestPolicy {
        pipes: usize,

        context: usize,

        on_context_limit: RefCell<Option<Box<dyn FnOnce()>>>,
    }

    impl Policy for TestPolicy {
        type Class = ResourceClass;

        type Key = ();

        /// Give pipes their count ceiling and other classes a byte ceiling.
        fn limit(&self, class: ResourceClass) -> usize {
            if matches!(class, ResourceClass::RequestContext) {
                let callback = self.on_context_limit.borrow_mut().take();
                if let Some(callback) = callback {
                    callback();
                }
            }
            match class {
                ResourceClass::Pipe => self.pipes,
                _ => self.context,
            }
        }

        /// All pipe fixture admission is unkeyed.
        fn max_keys(&self) -> usize {
            0
        }

        /// Pipe release can notify an explicitly registered quota waiter.
        fn wakes(class: ResourceClass) -> bool {
            matches!(class, ResourceClass::Pipe)
        }

        /// Pipe counts do not authorize userspace page backing.
        fn covers(_: ResourceClass) -> bool {
            false
        }

        /// Rejection facts are not needed by these pipe assertions.
        fn rejected(&self, _: crate::Rejection<ResourceClass>) {}
    }

    /// Build a local authority with ample waiter-context capacity.
    fn admission(pipes: usize) -> Rc<Quotas<TestPolicy>> {
        Rc::new(Quotas::new(TestPolicy {
            pipes,
            context: 32 * 1024 * 1024,
            on_context_limit: RefCell::default(),
        }))
    }

    /// Build an eight-waiter pool using distinct pipe and context classes.
    fn new_pool(quotas: Rc<Quotas<TestPolicy>>) -> PipePool<TestPolicy> {
        PipePool::new(
            quotas,
            ResourceClass::Pipe,
            ResourceClass::RequestContext,
            8,
        )
    }

    /// Create an acquisition with no cancellation or deadline source.
    fn acquire_wait(
        pool: &PipePool<TestPolicy>,
    ) -> std::pin::Pin<Box<impl Future<Output = Result<PipeLease<TestPolicy>>> + '_>> {
        Box::pin(pool.acquire_wait(|| Ok::<_, Error>(()), || Ok(|_: &Waker| {})))
    }

    /// Count progress notifications without requiring an executor.
    #[derive(Default)]
    struct WakeCounter(AtomicUsize);

    impl Wake for WakeCounter {
        /// Count an owned wake notification.
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }

        /// Count a borrowed wake notification.
        fn wake_by_ref(self: &Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    impl WakeCounter {
        /// Read the number of observed progress notifications.
        fn count(&self) -> usize {
            self.0.load(Ordering::Relaxed)
        }
    }

    /// Waker callbacks use thread-local hooks without sharing worker-local state.
    mod waker_callbacks {
        use super::*;
        use std::task::{RawWaker, RawWakerVTable};

        type Hook = Box<dyn FnOnce(&str)>;

        thread_local! {
            static HOOK: RefCell<Option<Hook>> = const { RefCell::new(None) };
        }

        /// Remove the hook before calling it so nested callbacks are harmless.
        fn invoke(event: &str) {
            let hook = HOOK.with(|slot| slot.borrow_mut().take());
            if let Some(hook) = hook {
                hook(event);
            }
        }

        /// Install one callback on this test thread.
        pub fn on_callback(hook: impl FnOnce(&str) + 'static) {
            HOOK.with(|slot| *slot.borrow_mut() = Some(Box::new(hook)));
        }

        /// Drop an unused hook before thread-local teardown, without a slot borrow.
        pub fn clear() {
            drop(HOOK.with(|slot| slot.borrow_mut().take()));
        }

        /// No data pointer is owned; callbacks only access the calling thread.
        fn raw() -> RawWaker {
            RawWaker::new(
                std::ptr::null(),
                &RawWakerVTable::new(
                    |_| {
                        invoke("clone");
                        raw()
                    },
                    |_| invoke("wake"),
                    |_| invoke("wake_by_ref"),
                    |_| invoke("drop"),
                ),
            )
        }

        /// Build a transferable waker with no shared mutable data.
        pub fn waker() -> Waker {
            // SAFETY: the vtable never dereferences data and owns no allocation.
            // All mutable hooks are thread-local, including on other threads.
            unsafe { Waker::from_raw(raw()) }
        }
    }

    /// Consume the head before callbacks can borrow or notify the same queue.
    #[test]
    fn review_regression_pipe_waker_front_reentry() {
        let entry = Rc::new(RefCell::new(Some(waker_callbacks::waker())));
        let queue: Waiters = Rc::new(RefCell::new(VecDeque::from([entry.clone()])));
        let callback_queue = queue.clone();
        let callback_entry = entry.clone();
        let called = Rc::new(std::cell::Cell::new(false));
        let callback_called = called.clone();
        waker_callbacks::on_callback(move |event| {
            let queue = callback_queue.borrow_mut();
            let entry = callback_entry.borrow_mut();
            assert_eq!(event, "wake", "notification must not clone its target");
            assert!(entry.is_none());
            assert_eq!(queue.len(), 1);
            drop(entry);
            drop(queue);
            wake_front(&callback_queue);
            callback_called.set(true);
        });
        wake_front(&queue);
        assert!(called.get());
        assert!(entry.borrow().is_none());
        assert_eq!(queue.borrow().len(), 1);
        wake_front(&queue);
    }

    /// Dropping a replaced registration may synchronously notify the new target.
    #[test]
    fn review_regression_pipe_waker_replacement_reentry() {
        let pool = new_pool(admission(1));
        let held = pool.acquire().unwrap();
        let mut wait = acquire_wait(&pool);
        let old = waker_callbacks::waker();
        assert!(
            wait.as_mut()
                .poll(&mut Context::from_waker(&old))
                .is_pending()
        );
        let queue = pool.waiting.clone();
        waker_callbacks::on_callback(move |event| {
            assert_eq!(event, "drop");
            wake_front(&queue);
        });
        let counter = Arc::new(WakeCounter::default());
        let new = Waker::from(counter.clone());
        let mut cx = Context::from_waker(&new);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        assert_eq!(counter.count(), 1);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        drop(held);
        assert_eq!(counter.count(), 2);
        assert!(matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        assert!(pool.waiting.borrow().is_empty());
    }

    /// Consumed notifications rearm on each poll and preserve FIFO progress.
    #[test]
    fn review_regression_pipe_waker_rearms_and_advances_fifo() {
        let quotas = admission(1);
        let pool = new_pool(quotas.clone());
        let held = pool.acquire().unwrap();
        let registrations = std::cell::Cell::new(0);
        let mut first = Box::pin(pool.acquire_wait(
            || Ok::<_, Error>(()),
            || Ok(|_: &Waker| registrations.set(registrations.get() + 1)),
        ));
        let mut second = acquire_wait(&pool);
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(second.as_mut().poll(&mut cx).is_pending());
        wake_front(&pool.waiting);
        wake_front(&pool.waiting);
        assert_eq!(count.count(), 1);
        let queue = pool.waiting.clone();
        waker_callbacks::on_callback(move |event| {
            assert_eq!(event, "clone");
            wake_front(&queue);
        });
        let reentrant = waker_callbacks::waker();
        assert!(
            first
                .as_mut()
                .poll(&mut Context::from_waker(&reentrant))
                .is_pending()
        );
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert_eq!(registrations.get(), 3);
        assert_eq!(count.count(), 1, "pending polls must not self-wake");
        drop(held);
        assert_eq!(count.count(), 2);
        assert!(second.as_mut().poll(&mut cx).is_pending());
        let Poll::Ready(Ok(first_lease)) = first.as_mut().poll(&mut cx) else {
            panic!("head did not acquire returned pipe");
        };
        assert_eq!(registrations.get(), 4);
        assert_eq!(count.count(), 3, "head completion must notify successor");
        assert!(second.as_mut().poll(&mut cx).is_pending());
        drop(first_lease);
        assert_eq!(count.count(), 4);
        assert!(matches!(second.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        assert!(pool.waiting.borrow().is_empty());
        assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
    }

    /// Distinguish application gate rejection from flow-control exhaustion.
    #[derive(Debug, PartialEq)]
    enum GateError {
        Rejected,

        Flow(Error),
    }

    impl From<Error> for GateError {
        /// Preserve the underlying flow-control failure.
        fn from(error: Error) -> Self {
            Self::Flow(error)
        }
    }

    /// Unsupported operation errors never include backpressure or disconnects.
    #[test]
    fn unsupported_splice_is_distinct_from_backpressure_and_disconnect() {
        for code in [libc::EINVAL, libc::ENOSYS, libc::EOPNOTSUPP] {
            assert!(splice_unsupported(&io::Error::from_raw_os_error(code)));
        }
        for code in [libc::EAGAIN, libc::EINTR, libc::EPIPE, libc::ECONNRESET] {
            assert!(!splice_unsupported(&io::Error::from_raw_os_error(code)));
        }
        assert!(!splice_unsupported(&io::Error::other("custom")));
    }

    /// Immediate acquisition skips subscription and failed waits roll back fully.
    #[test]
    fn lazy_subscription_and_failure_rollback() {
        use std::cell::Cell;
        let quotas = admission(1);
        let pool = new_pool(quotas.clone());
        let subscribed = Cell::new(0);
        let subscribe = || {
            subscribed.set(subscribed.get() + 1);
            assert_eq!(pool.waiting.borrow().len(), 1);
            Err::<fn(&Waker), _>(GateError::Rejected)
        };
        let mut cx = Context::from_waker(Waker::noop());
        let mut immediate = Box::pin(pool.acquire_wait(|| Ok(()), subscribe));
        let Poll::Ready(Ok(held)) = immediate.as_mut().poll(&mut cx) else {
            panic!("immediate acquisition failed")
        };
        assert_eq!(subscribed.get(), 0);
        assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
        let mut wait = Box::pin(pool.acquire_wait(|| Ok(()), subscribe));
        assert!(matches!(
            wait.as_mut().poll(&mut cx),
            Poll::Ready(Err(GateError::Rejected))
        ));
        assert_eq!(subscribed.get(), 1);
        assert!(pool.waiting.borrow().is_empty());
        assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
        assert_eq!(quotas.used(ResourceClass::Pipe), 1);
        drop(held);

        let mut rejected = Box::pin(pool.acquire_wait(|| Err(GateError::Rejected), subscribe));
        assert!(matches!(
            rejected.as_mut().poll(&mut cx),
            Poll::Ready(Err(GateError::Rejected))
        ));
        assert_eq!(subscribed.get(), 1);
        assert_eq!(pool.idle_count(), 1);

        for (context, waiter_limit) in [(0, 8), (usize::MAX, 0)] {
            let quotas = Rc::new(Quotas::new(TestPolicy {
                pipes: 1,
                context,
                on_context_limit: RefCell::default(),
            }));
            let pool = PipePool::new(
                quotas.clone(),
                ResourceClass::Pipe,
                ResourceClass::RequestContext,
                waiter_limit,
            );
            let _held = pool.acquire().unwrap();
            let mut wait = Box::pin(pool.acquire_wait(|| Ok(()), subscribe));
            assert!(matches!(
                wait.as_mut().poll(&mut cx),
                Poll::Ready(Err(GateError::Flow(Error::Overloaded)))
            ));
            assert_eq!(subscribed.get(), 1, "rejected queue must not subscribe");
            assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
            assert!(pool.waiting.borrow().is_empty());
        }
    }

    /// Reservation callbacks cannot let an outer wait overfill a reentered queue.
    #[test]
    fn review_regression_waiter_limit_after_reserve_reentry() {
        let quotas = admission(1);
        let pool = Rc::new(PipePool::new(
            quotas.clone(),
            ResourceClass::Pipe,
            ResourceClass::RequestContext,
            1,
        ));
        let held = pool.acquire().unwrap();
        let nested_pool = pool.clone();
        let nested = Rc::new(RefCell::new(Box::pin(async move {
            acquire_wait(&nested_pool).await
        })));
        let callback_wait = nested.clone();
        *quotas.policy.on_context_limit.borrow_mut() = Some(Box::new(move || {
            let mut cx = Context::from_waker(Waker::noop());
            assert!(
                callback_wait
                    .borrow_mut()
                    .as_mut()
                    .poll(&mut cx)
                    .is_pending()
            );
        }));
        let mut outer = Box::pin(pool.acquire_wait(
            || Ok::<_, Error>(()),
            || -> Result<fn(&Waker)> {
                panic!("overloaded outer wait must not subscribe");
            },
        ));
        let mut cx = Context::from_waker(Waker::noop());
        assert!(matches!(
            outer.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(pool.waiting.borrow().len(), 1);
        assert_eq!(
            quotas.used(ResourceClass::RequestContext),
            pool.waiter_bytes()
        );
        drop(held);
        assert!(matches!(
            nested.borrow_mut().as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        assert!(pool.waiting.borrow().is_empty());
        assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
    }

    /// Subscription sees its own queue slot before it tries a nested acquisition.
    #[test]
    fn review_regression_waiter_limit_during_subscribe_reentry() {
        let quotas = admission(1);
        let pool = PipePool::new(
            quotas.clone(),
            ResourceClass::Pipe,
            ResourceClass::RequestContext,
            1,
        );
        let held = pool.acquire().unwrap();
        let mut nested = acquire_wait(&pool);
        let mut outer = Box::pin(pool.acquire_wait(
            || Ok::<_, Error>(()),
            || {
                let mut cx = Context::from_waker(Waker::noop());
                assert!(matches!(
                    nested.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Overloaded))
                ));
                assert_eq!(pool.waiting.borrow().len(), 1);
                assert_eq!(
                    quotas.used(ResourceClass::RequestContext),
                    pool.waiter_bytes()
                );
                Ok(|_: &Waker| {})
            },
        ));
        let mut cx = Context::from_waker(Waker::noop());
        assert!(outer.as_mut().poll(&mut cx).is_pending());
        assert_eq!(pool.waiting.borrow().len(), 1);
        drop(held);
        assert!(matches!(outer.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        assert!(pool.waiting.borrow().is_empty());
        assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
    }

    /// Unwinding a subscription removes its published slot and releases its charge.
    #[test]
    fn review_regression_subscription_panic_rolls_back_waiting() {
        let quotas = admission(1);
        let pool = new_pool(quotas.clone());
        let _held = pool.acquire().unwrap();
        let published = std::cell::Cell::new(false);
        let mut wait = Box::pin(pool.acquire_wait(
            || Ok::<_, Error>(()),
            || -> Result<fn(&Waker)> {
                published.set(pool.waiting.borrow().len() == 1);
                panic!("subscription failed");
            },
        ));
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            wait.as_mut().poll(&mut Context::from_waker(Waker::noop()))
        }));
        assert!(result.is_err());
        assert!(published.get());
        assert!(pool.waiting.borrow().is_empty());
        assert_eq!(pool.waiting.borrow().capacity(), 0);
        assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
        let mut replacement = acquire_wait(&pool);
        assert!(
            replacement
                .as_mut()
                .poll(&mut Context::from_waker(Waker::noop()))
                .is_pending()
        );
    }

    /// Gate failure, stop, and abandonment release registration and exact charges.
    #[test]
    fn gate_stop_and_abandonment_drop_registration_and_exact_charge() {
        use std::cell::Cell;
        /// Count a caller-owned cancellation registration until its closure drops.
        struct Registration(Rc<Cell<usize>>);

        impl Drop for Registration {
            /// Release exactly this registration's live count.
            fn drop(&mut self) {
                self.0.set(self.0.get() - 1);
            }
        }
        for failure in 0..3 {
            let quotas = admission(1);
            let pool = new_pool(quotas.clone());
            let held = pool.acquire().unwrap();
            let reject = Cell::new(false);
            let registrations = Rc::new(Cell::new(0));
            let registered = Cell::new(0);
            let mut wait = Box::pin(pool.acquire_wait(
                || {
                    if reject.get() {
                        Err(GateError::Rejected)
                    } else {
                        Ok(())
                    }
                },
                || {
                    registrations.set(registrations.get() + 1);
                    let registration = Registration(registrations.clone());
                    let registered = &registered;
                    Ok(move |_: &Waker| {
                        let _keep = &registration;
                        registered.set(registered.get() + 1);
                    })
                },
            ));
            let counter = Arc::new(WakeCounter::default());
            let waker = Waker::from(counter.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert_eq!(registered.get(), 2);
            assert_eq!(registrations.get(), 1);
            assert_eq!(counter.count(), 0);
            assert_eq!(
                quotas.used(ResourceClass::RequestContext),
                pool.waiter_bytes()
            );
            let mut second = acquire_wait(&pool);
            assert!(second.as_mut().poll(&mut cx).is_pending());
            match failure {
                0 => {
                    reject.set(true);
                    assert!(matches!(
                        wait.as_mut().poll(&mut cx),
                        Poll::Ready(Err(GateError::Rejected))
                    ));
                }
                1 => {
                    quotas.stop();
                    assert!(matches!(
                        wait.as_mut().poll(&mut cx),
                        Poll::Ready(Err(GateError::Flow(Error::Unavailable)))
                    ));
                }
                _ => {}
            }
            drop(wait);
            assert_eq!(registrations.get(), 0);
            assert_eq!(
                quotas.used(ResourceClass::RequestContext),
                pool.waiter_bytes()
            );
            assert_eq!(pool.waiting.borrow().len(), 1);
            assert!(counter.count() > 0, "head removal must wake successor");
            drop(held);
            if failure == 1 {
                assert!(matches!(
                    second.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Unavailable))
                ));
                assert_eq!(quotas.used(ResourceClass::Pipe), 0);
            } else {
                assert!(matches!(second.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
            }
            drop(second);
            assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
            assert!(pool.waiting.borrow().is_empty());
        }
    }

    /// Both descriptor splice directions reject blocking sockets without data loss.
    #[test]
    fn public_descriptor_splice_validates_both_directions() {
        use std::{
            io::{Read, Write},
            os::unix::net::UnixStream,
        };
        let pool = new_pool(admission(1));
        let mut pipe = pool.acquire().unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let socket = Descriptor::from(std::os::fd::OwnedFd::from(socket));
        pipe.try_write(b"out").unwrap();
        assert_eq!(
            pipe.try_splice_connection(&socket)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(
            pipe.try_splice_from(&socket, 1).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(pipe.buffered(), 3);
        socket.set_nonblocking().unwrap();
        assert_eq!(pipe.try_splice_connection(&socket).unwrap(), 3);
        let mut bytes = [0; 3];
        peer.read_exact(&mut bytes).unwrap();
        assert_eq!(&bytes, b"out");
        peer.write_all(b"in!").unwrap();
        pipe.prepare_transit();
        assert_eq!(pipe.try_splice_from(&socket, 3).unwrap(), 3);
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 3);
        assert_eq!(&bytes, b"in!");
    }

    /// Simulated descriptors preserve byte counts, pooling, and partial close.
    #[cfg(feature = "simulation")]
    #[test]
    fn simulation_uses_runtime_descriptors_and_preserves_accounting() {
        let sim = uring_runtime::reactor::simulation::Simulation::new();
        let _environment = sim.enter();
        let quotas = admission(1);
        let pool = new_pool(quotas.clone());
        let mut pipe = pool.acquire().unwrap();
        assert!(pipe.resources.as_ref().unwrap().read.as_sim().is_some());
        assert_eq!(pipe.capacity(), MAX_PIPE_BYTES);
        pipe.prepare_transit();
        assert_eq!(pipe.try_write(b"simulated").unwrap(), 9);
        let (socket, peer) = sim.socket_pair();
        assert_eq!(
            pipe.try_splice_from(&socket, 1).unwrap_err().raw_os_error(),
            Some(libc::EOPNOTSUPP)
        );
        assert_eq!(pipe.try_splice_descriptor(&socket, 3).unwrap(), 3);
        assert_eq!(pipe.buffered(), 6);
        let mut bytes = [0; 9];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 3);
        assert_eq!(&bytes[..3], b"sim");
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 6);
        assert_eq!(&bytes[..6], b"ulated");
        drop(pipe);
        assert_eq!(pool.idle_count(), 1);
        assert_eq!(quotas.used(ResourceClass::Pipe), 1);
        let mut pipe = pool.acquire().unwrap();
        pipe.try_write(b"discard").unwrap();
        drop(pipe);
        assert_eq!(pool.idle_count(), 0);
        assert_eq!(quotas.used(ResourceClass::Pipe), 0);
    }

    /// Mixed descriptor backends reject splice without consuming buffered bytes.
    #[cfg(feature = "simulation")]
    #[test]
    fn mixed_splice_backends_preserve_buffered_suffix() {
        use std::os::unix::net::UnixStream;

        let pool = new_pool(admission(2));
        let mut real_pipe = pool.acquire().unwrap();
        let (real_socket, _peer) = UnixStream::pair().unwrap();
        real_socket.set_nonblocking(true).unwrap();
        let real_socket = Descriptor::from(std::os::fd::OwnedFd::from(real_socket));
        let sim = uring_runtime::reactor::simulation::Simulation::new();
        let _environment = sim.enter();
        let mut simulated_pipe = pool.acquire().unwrap();
        let (simulated_socket, _peer) = sim.socket_pair();

        for (pipe, socket) in [
            (&mut real_pipe, &simulated_socket),
            (&mut simulated_pipe, &real_socket),
        ] {
            assert_eq!(pipe.try_write(b"pending").unwrap(), 7);
            assert_eq!(
                pipe.try_splice_descriptor(socket, 3)
                    .unwrap_err()
                    .raw_os_error(),
                Some(libc::EOPNOTSUPP)
            );
            assert_eq!(pipe.buffered(), 7);
            let mut bytes = [0; 7];
            assert_eq!(pipe.try_read(&mut bytes).unwrap(), 7);
            assert_eq!(&bytes, b"pending");
        }
    }

    /// Empty pipes reuse both descriptors while partial payloads are closed.
    #[test]
    fn empty_pipe_reuses_descriptors_and_partial_pipe_is_closed() {
        const CHILD: &str = "FLOW_PIPE_DESCRIPTOR_CLOSE_CHILD";
        if std::env::var_os(CHILD).is_none() {
            // Descriptor numbers are process-global. Other parallel tests may
            // reuse a closed number before fcntl, so assert closure in isolation.
            let status = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "pipe::tests::empty_pipe_reuses_descriptors_and_partial_pipe_is_closed",
                    "--test-threads=1",
                ])
                .env(CHILD, "1")
                .status()
                .unwrap();
            assert!(status.success(), "isolated descriptor assertions failed");
            return;
        }
        let admission = admission(1);
        let pool = new_pool(admission.clone());
        let mut pipe = pool.acquire().unwrap();
        let read = pipe.resources.as_ref().unwrap().read.as_raw_fd();
        let write = pipe.resources.as_ref().unwrap().write.as_raw_fd();
        pipe.try_write(b"secret").unwrap();
        let mut bytes = [0; 6];
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 6);
        drop(pipe);
        assert_eq!(admission.used(ResourceClass::Pipe), 1);
        let mut pipe = pool.acquire().unwrap();
        assert_eq!(pipe.resources.as_ref().unwrap().read.as_raw_fd(), read);
        assert_eq!(pipe.resources.as_ref().unwrap().write.as_raw_fd(), write);
        assert_eq!(
            pipe.try_read(&mut bytes).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        pipe.try_write(b"discard").unwrap();
        drop(pipe);
        assert!(pool.idle.borrow().is_empty());
        assert_eq!(admission.used(ResourceClass::Pipe), 0);
        // Dropped resources close their owned descriptors. Do not probe the old
        // numbers: parallel tests may already have reused them for other files.
        let mut replacement = pool.acquire().unwrap();
        assert_eq!(admission.used(ResourceClass::Pipe), 1);
        assert_eq!(replacement.buffered(), 0);
        assert_eq!(
            replacement.try_read(&mut bytes).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
    }

    /// Stop observation drains idle owners but leaves active work and waiters owned.
    #[test]
    fn review_regression_stop_drains_idle_on_all_acquisition_paths() {
        use std::io::Read;

        for path in 0..3 {
            let quotas = admission(3);
            let pool = new_pool(quotas.clone());
            let idle_read = pool.acquire().unwrap();
            let idle_write = pool.acquire().unwrap();
            let mut held = pool.acquire().unwrap();
            held.try_write(b"live").unwrap();
            let mut read = std::fs::File::from(
                idle_read
                    .resources
                    .as_ref()
                    .unwrap()
                    .read
                    .as_fd()
                    .try_clone_to_owned()
                    .unwrap(),
            );
            let write = idle_write
                .resources
                .as_ref()
                .unwrap()
                .write
                .as_fd()
                .try_clone_to_owned()
                .unwrap();
            let mut first = acquire_wait(&pool);
            let mut second = acquire_wait(&pool);
            let mut cx = Context::from_waker(Waker::noop());
            assert!(first.as_mut().poll(&mut cx).is_pending());
            assert!(second.as_mut().poll(&mut cx).is_pending());
            drop((idle_read, idle_write));
            assert_eq!(pool.idle_count(), 2);
            assert_eq!(quotas.used(ResourceClass::Pipe), 3);
            quotas.stop();
            match path {
                0 => assert!(matches!(pool.acquire(), Err(Error::Unavailable))),
                1 => assert!(matches!(
                    second.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Unavailable))
                )),
                _ => assert!(matches!(
                    acquire_wait(&pool).as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Unavailable))
                )),
            }
            assert_eq!(pool.idle_count(), 0, "path {path} retained idle pipes");
            assert_eq!(quotas.used(ResourceClass::Pipe), 1);
            let pending = if path == 1 { 1 } else { 2 };
            assert_eq!(pool.waiting.borrow().len(), pending);
            assert_eq!(
                quotas.used(ResourceClass::RequestContext),
                pending * pool.waiter_bytes()
            );
            // Owned duplicates observe peer closure, never a reused descriptor number.
            assert_eq!(read.read(&mut [0]).unwrap(), 0);
            let mut poll = libc::pollfd {
                fd: write.as_raw_fd(),
                events: libc::POLLOUT,
                revents: 0,
            };
            // SAFETY: poll borrows one initialized entry and a live owned descriptor.
            assert_eq!(unsafe { libc::poll(&mut poll, 1, 0) }, 1);
            assert_ne!(poll.revents & libc::POLLERR, 0);
            assert!(matches!(
                first.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            if path != 1 {
                assert!(matches!(
                    second.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Unavailable))
                ));
            }
            assert!(pool.waiting.borrow().is_empty());
            assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
            let mut bytes = [0; 4];
            assert_eq!(held.try_read(&mut bytes).unwrap(), 4);
            assert_eq!(&bytes, b"live");
            drop(held);
            assert_eq!(quotas.used(ResourceClass::Pipe), 0);
            assert_eq!(pool.idle_count(), 0);
            assert!(matches!(pool.acquire(), Err(Error::Unavailable)));
        }
    }

    /// Charge destruction may reenter a stopped pool after its idle list is detached.
    #[test]
    fn review_regression_stop_drain_charge_destructor_reentry() {
        let quotas = admission(2);
        let pool = Rc::new(new_pool(quotas.clone()));
        let first = pool.acquire().unwrap();
        let second = pool.acquire().unwrap();
        drop((first, second));
        quotas.stop();
        let called = Rc::new(std::cell::Cell::new(false));
        let callback_called = called.clone();
        let callback_pool = pool.clone();
        quotas.totals.wake.register(&waker_callbacks::waker());
        waker_callbacks::on_callback(move |event| {
            assert_eq!(event, "wake");
            assert_eq!(callback_pool.idle_count(), 0);
            assert!(matches!(callback_pool.acquire(), Err(Error::Unavailable)));
            callback_called.set(true);
        });
        assert!(matches!(pool.acquire(), Err(Error::Unavailable)));
        waker_callbacks::clear();
        assert!(called.get());
        assert_eq!(quotas.used(ResourceClass::Pipe), 0);
        assert_eq!(pool.idle_count(), 0);
    }

    /// A lease outliving its pool continues to hold capacity until drop.
    #[test]
    fn exhaustion_and_drop_return_capacity_even_after_pool_drop() {
        let admission = admission(1);
        let pool = new_pool(admission.clone());
        let lease = pool.acquire().unwrap();
        assert!(matches!(pool.acquire(), Err(Error::Overloaded)));
        drop(pool);
        let pool = new_pool(admission);
        assert!(matches!(pool.acquire(), Err(Error::Overloaded)));
        drop(lease);
        assert!(pool.acquire().is_ok());
    }

    /// Head and tail cancellation keep allocated queue storage within live charges.
    #[test]
    fn review_regression_canceled_waiter_storage_remains_charged() {
        const WAITERS: usize = 1024;
        for keep_tail in [false, true] {
            let quotas = admission(1);
            let pool = PipePool::new(
                quotas.clone(),
                ResourceClass::Pipe,
                ResourceClass::RequestContext,
                WAITERS,
            );
            let held = pool.acquire().unwrap();
            let mut cx = Context::from_waker(Waker::noop());
            let mut waiting: Vec<_> = (0..WAITERS).map(|_| acquire_wait(&pool)).collect();
            for wait in &mut waiting {
                assert!(wait.as_mut().poll(&mut cx).is_pending());
            }
            if keep_tail {
                waiting.reverse();
            }
            while waiting.len() > 1 {
                drop(waiting.pop());
                let queue = pool.waiting.borrow();
                assert_eq!(queue.len(), waiting.len());
                let slots = queue.capacity() * std::mem::size_of::<Rc<RefCell<Option<Waker>>>>();
                let owners = queue.len()
                    * (std::mem::size_of::<Waiting<TestPolicy>>()
                        + std::mem::size_of::<RefCell<Option<Waker>>>()
                        + 2 * std::mem::size_of::<usize>());
                assert!(
                    slots + owners <= quotas.used(ResourceClass::RequestContext),
                    "{} live waiters retain {} queue slots without admission",
                    queue.len(),
                    queue.capacity(),
                );
            }
            drop(held);
            assert!(matches!(
                waiting[0].as_mut().poll(&mut cx),
                Poll::Ready(Ok(_))
            ));
            drop(waiting);
            assert_eq!(pool.waiting.borrow().capacity(), 0);
            assert_eq!(quotas.used(ResourceClass::RequestContext), 0);
        }
    }

    /// Scheduled acquisition is bounded and FIFO without polling itself awake.
    #[test]
    fn scheduled_acquisition_is_bounded_fifo_and_wakes_only_for_progress() {
        let admission = admission(2);
        let pool = new_pool(admission.clone());
        let held = [pool.acquire().unwrap(), pool.acquire().unwrap()];
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut waiting: Vec<_> = (0..8).map(|_| acquire_wait(&pool)).collect();
        for wait in &mut waiting {
            assert!(wait.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(count.count(), 0, "waiting does not spin/self-wake");
        assert!(matches!(
            acquire_wait(&pool).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(pool.waiting.borrow().len(), 8);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            8 * pool.waiter_bytes()
        );
        assert_eq!(admission.used(ResourceClass::Pipe), 2);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        drop(held);
        assert!(count.count() > 0);
        // Reverse polling cannot let new arrivals jump the queue.
        for wait in waiting.iter_mut().skip(1).rev() {
            assert!(wait.as_mut().poll(&mut cx).is_pending());
        }
        let mut leases = VecDeque::new();
        for mut wait in waiting {
            if leases.len() == 2 {
                leases.pop_front();
            }
            let Poll::Ready(Ok(pipe)) = wait.as_mut().poll(&mut cx) else {
                panic!("FIFO waiter did not progress")
            };
            leases.push_back(pipe);
            assert!(admission.used(ResourceClass::Pipe) <= 2);
        }
        assert!(pool.waiting.borrow().is_empty());
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        drop(leases);
        assert_eq!(
            admission.used(ResourceClass::Pipe),
            2,
            "idle pipes remain admitted"
        );
        drop(pool);
        assert_eq!(admission.used(ResourceClass::Pipe), 0);
    }

    /// Kernel descriptors have bounded capacity and independent nonblocking data.
    #[test]
    fn pipes_are_nonblocking_bounded_cloexec_and_independent() {
        let admission = admission(2);
        let pool = new_pool(admission);
        let mut first = pool.acquire().unwrap();
        let mut second = pool.acquire().unwrap();
        let resources = first.resources.as_ref().unwrap();
        for fd in [&resources.read, &resources.write] {
            // SAFETY: these descriptors are owned for the duration of the query.
            assert_ne!(
                unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) } & libc::O_NONBLOCK,
                0
            );
            assert_ne!(
                unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
                0
            );
        }
        assert!(first.capacity() <= MAX_PIPE_BYTES);
        assert!(first.capacity() > 0);
        // Account the actual kernel capacity, including denied best-effort growth.
        assert_eq!(first.capacity(), unsafe {
            libc::fcntl(
                first.resources.as_ref().unwrap().write.as_raw_fd(),
                libc::F_GETPIPE_SZ,
            )
        } as usize);
        let bytes = vec![0x5a; first.capacity()];
        assert_eq!(first.try_write(&bytes).unwrap(), bytes.len());
        assert_eq!(
            first.try_write(b"x").unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        let mut out = vec![0; bytes.len()];
        assert_eq!(
            second.try_read(&mut out).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(first.try_read(&mut out).unwrap(), bytes.len());
        assert_eq!(out, bytes);
        assert_eq!(
            first.try_read(&mut out).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(second.try_write(b"second").unwrap(), 6);
        assert_eq!(second.try_read(&mut out).unwrap(), 6);
        assert_eq!(&out[..6], b"second");
    }

    /// Copied pages remain intact after source reuse, partial drain, and pipe reuse.
    #[test]
    fn copied_splice_survives_source_reuse_and_partial_drain() {
        use std::{io::Read, os::unix::net::UnixStream};
        let admission = admission(1);
        let pool = new_pool(admission);
        let mut pipe = pool.acquire().unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        socket.set_nonblocking(true).unwrap();
        let mut source = b"copied kernel pages".to_vec();
        assert_eq!(pipe.try_write(&source).unwrap(), source.len());
        source.fill(0);
        assert_eq!(pipe.try_splice_to(&socket, 6).unwrap(), 6);
        assert_eq!(pipe.buffered(), 13);
        assert_eq!(pipe.try_splice_to(&socket, usize::MAX).unwrap(), 13);
        assert_eq!(pipe.buffered(), 0);
        // Socket-owned kernel pages survive both pipe closure and quota reuse.
        drop(pipe);
        let mut reused = pool.acquire().unwrap();
        reused.try_write(b"replacement data").unwrap();
        let mut received = [0; 19];
        peer.read_exact(&mut received).unwrap();
        assert_eq!(&received, b"copied kernel pages");
    }

    /// Blocking sockets and disconnects preserve the full buffered suffix.
    #[test]
    fn splice_rejects_blocking_socket_and_handles_disconnect_without_losing_bytes() {
        use std::os::unix::net::UnixStream;
        let admission = admission(1);
        let pool = new_pool(admission);
        let mut pipe = pool.acquire().unwrap();
        pipe.try_write(b"abc").unwrap();
        let (socket, peer) = UnixStream::pair().unwrap();
        assert_eq!(
            pipe.try_splice_to(&socket, 3).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(pipe.buffered(), 3);
        socket.set_nonblocking(true).unwrap();
        drop(peer);
        assert_eq!(
            pipe.try_splice_to(&socket, 3).unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        assert_eq!(pipe.buffered(), 3);
        let mut bytes = [0; 3];
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 3);
        assert_eq!(&bytes, b"abc");
    }

    /// Backpressure and invalid socket types leave fallback bytes untouched.
    #[test]
    fn splice_backpressure_preserves_buffered_bytes_and_socket_validation() {
        use std::{io::Write, os::unix::net::UnixStream};
        let admission = admission(1);
        let pool = new_pool(admission);
        let mut pipe = pool.acquire().unwrap();
        pipe.try_write(b"pending").unwrap();
        let (mut socket, _peer) = UnixStream::pair().unwrap();
        socket.set_nonblocking(true).unwrap();
        let start = std::time::Instant::now();
        loop {
            assert!(start.elapsed() < std::time::Duration::from_secs(5));
            match socket.write(&[1; 8192]) {
                Ok(count) => assert_ne!(count, 0),
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                Err(error) => panic!("{error}"),
            }
        }
        assert_eq!(
            pipe.try_splice_to(&socket, 7).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(pipe.buffered(), 7);
        let (datagram, _peer) = std::os::unix::net::UnixDatagram::pair().unwrap();
        datagram.set_nonblocking(true).unwrap();
        assert_eq!(
            pipe.try_splice_to(&datagram, 7).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        let file = std::fs::OpenOptions::new()
            .write(true)
            .open("/dev/null")
            .unwrap();
        // SAFETY: change flags on this exclusively owned test descriptor.
        assert_eq!(
            unsafe { libc::fcntl(file.as_raw_fd(), libc::F_SETFL, libc::O_NONBLOCK) },
            0
        );
        assert_eq!(
            pipe.try_splice_to(&file, 7).unwrap_err().raw_os_error(),
            Some(libc::ENOTSOCK)
        );
        let mut bytes = [0; 7];
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 7);
        assert_eq!(&bytes, b"pending");
    }
}
