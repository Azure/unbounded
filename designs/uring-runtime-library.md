# uring-runtime: a standalone io_uring runtime library

Status: in progress. This document is the working specification for extracting
`cmd/racer-dataplane/runtime` (package `uring-runtime`) into a reusable,
publishable library under `crates/`, and for migrating the Racer dataplane onto
it. Code is authoritative once written; update this document when a decision
changes.

## Goals

- A small, well documented, explicitly driven, thread-per-core io_uring runtime
  that any Linux application can embed.
- Memory safety that does not depend on callers remembering conventions: every
  pointer handed to the kernel is owned by the runtime until the kernel is
  provably done with it.
- Deterministic simulation as a separate crate that plugs in through a trait,
  with no cargo feature that changes production behavior.
- No application policy in the library: no retry loops, hedging, placement
  ratios, filesystem layout, checkpoint formats, or hardcoded tuning values.

## Non-goals

- A general purpose multi-threaded work-stealing executor.
- Portability beyond Linux.
- Hiding io_uring: the library is a thin, safe layer over it.

## Repository layout

```
crates/
  Cargo.toml            # standalone Cargo workspace (edition 2024, resolver 3)
  Cargo.lock
  uring-runtime/        # core library (lib name `uring_runtime`)
  uring-runtime-sim/    # deterministic simulation backend (lib name `uring_runtime_sim`)
cmd/racer-dataplane/
  platform/             # new internal crate `racer-platform`: Racer policy that used to live in the runtime
```

`crates/` is its own workspace so the library is built, linted and tested with
no knowledge of Racer. Racer depends on it with
`uring-runtime = { path = "../../crates/uring-runtime", version = "0.1.0" }`.

Workspace settings (`crates/Cargo.toml`):

- `edition = "2024"`, `rust-version = "1.88"`, `license = "Apache-2.0"`,
  `repository = "https://github.com/Azure/unbounded"`, shared through `[workspace.package]`.
- `[workspace.lints.rust]`: `missing_docs`, `unsafe_op_in_unsafe_fn`,
  `unreachable_pub`, `rust_2018_idioms` (`deny`/`warn` as appropriate).
- `[workspace.lints.clippy]`: `undocumented_unsafe_blocks = "deny"`,
  `missing_safety_doc = "deny"`.
- No `panic = "abort"`: the group relies on `catch_unwind`.

## Crate: `uring-runtime`

Dependencies are kept minimal: `io-uring`, `libc`, and `futures-core`/
`atomic-waker`-style helpers only if they pay for themselves. No `getrandom`
types in the public API.

### Modules

| Module | Contents |
|---|---|
| `error` | `Error`, `ErrorKind`, `Result` |
| `scope` | `Scope` trait, `Unscoped` |
| `cancel` | `CancelToken` (cross-thread), `CancelRegistration` |
| `clock` | `Clock` trait, `SystemClock`, `Entropy` trait, `SystemEntropy` |
| `buf` | `IoBuf`, `IoBufMut` unsafe traits and std impls |
| `backend` | `Backend` trait, `Op` descriptions, `CompletionEntry`, `Host` (synchronous resource operations) |
| `uring` | `Uring`: the real io_uring `Backend` |
| `reactor` | `Reactor<B>`, `ReactorConfig`, op futures, admission and reservations, fences, shutdown |
| `fd` | `Fd`: cheap-clone owned descriptor for the real backend |
| `net` | `SocketAddress` and sockaddr encoding |
| `task` | `LocalExecutor` (bounded, `!Send` tasks, real ready queue), `Spawner`, `JoinHandle`, `yield_now` |
| `time` | `Timers` (deadline queue driven explicitly), `Sleep` |
| `group` | pinned thread groups: `Group`, `Plan`, `Service`, `Factory`, `FailureReporter`, `GroupConfig`, `GroupError`, `FatalPolicy` |
| `affinity` | CPU topology discovery and thread pinning (no placement policy) |

### Errors

```rust
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum ErrorKind {
    Canceled, DeadlineExceeded, Overloaded, Unavailable, InvalidInput,
    InvalidConfiguration, NotFound, AlreadyExists, Unsupported, Os,
}

#[derive(Clone, Debug)]
pub struct Error { /* kind, errno: Option<i32>, context: Option<&'static str> */ }

impl Error {
    pub fn new(kind: ErrorKind) -> Self;
    pub fn with_context(self, context: &'static str) -> Self;
    pub fn from_errno(errno: i32) -> Self;        // maps ENOENT/EEXIST/...; ECANCELED stays Os
    pub fn kind(&self) -> ErrorKind;
    pub fn raw_os_error(&self) -> Option<i32>;
}
impl From<std::io::Error> for Error; impl From<Error> for std::io::Error;
```

`Error` is `Clone` but deliberately not `Copy`, so it can grow without a
breaking change. Kernel `ECANCELED` is reported as `Os(ECANCELED)`; only
cancellation requested through a `Scope` or the reactor is `Canceled`.
Every syscall failure keeps its errno (ring setup, eventfd, bind, epoll, ...).
`from_errno` accepts negated values (CQE results) and maps `ENOENT`, `EEXIST`,
`EINVAL`, `ENOSYS`/`EOPNOTSUPP` and `ETIMEDOUT` to dedicated kinds. Helpers:
`context()`, `is_canceled()`, `is_deadline_exceeded()`, `is_would_block()`,
`is_interrupted()`, `last_os_error()`; `ErrorKind::as_str()`.

### Scope

```rust
pub trait Scope: Clone + 'static {
    type Error: From<Error> + 'static;
    fn check(&self) -> Result<(), Self::Error>;
    fn cancellation(&self) -> Option<&CancelToken> { None }
    fn deadline(&self) -> Option<Instant> { None }
}
```

`Unscoped` (a unit struct, `Error = Error`) never cancels. `CancelToken`
itself implements `Scope` (`Error = Error`, fails with `Canceled`). The runtime
never reads a clock for a scope: `check()` must enforce the deadline itself;
`deadline()` is only a hint for arming a timer. The reactor uses
`check()` during its bounded cancellation scan, and also registers with
`cancellation()` so that canceling a token wakes the reactor instead of waiting
for the next scan.

### Buffers

```rust
pub unsafe trait IoBuf: 'static {
    fn stable_ptr(&self) -> *const u8;
    fn bytes_init(&self) -> usize;
}
pub unsafe trait IoBufMut: IoBuf {
    fn stable_mut_ptr(&mut self) -> *mut u8;
    fn bytes_total(&mut self) -> usize;
    unsafe fn set_init(&mut self, len: usize);
}
```

Safety contract stated in terms of moves: the pointer must remain valid and
point to the same allocation while the value is moved and while it is owned by
the reactor; `IoBufMut` must not be aliased. Impls: `Vec<u8>`, `&'static [u8]`,
`&'static str`. `Box<[u8]>` is not implemented: moving a `Box` retags its
allocation and invalidates previously derived pointers. `Read`/`Recv` write
into `[0, bytes_total)` and call `set_init(n)` (which never shrinks a `Vec`).
No `Slice<B>` view yet.

### Backend seam

The reactor owns an in-flight table and the original + cancel fence state
machine; it talks to a backend through a narrow trait. This is the seam the old
code already had (`simulation::Driver::{push, cancel, submit, pop}`), lifted
into a public trait.

```rust
pub trait Backend: 'static {
    type Fd: Clone + Debug + 'static;        // Uring: `Fd`, simulator: `SimFd`
    type Clock: Clock + Clone;
    type Entropy: Entropy;
    fn clock(&self) -> &Self::Clock;
    fn entropy(&self) -> &Self::Entropy;
    fn init(&mut self, config: &BackendConfig) -> Result<()>;
    unsafe fn push(&mut self, user_data: u64, op: &Op<'_, Self::Fd>) -> Result<(), PushError>;
    fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError>;
    unsafe fn adopt_fd(&mut self, raw: i32) -> Self::Fd;   // own an Accept/OpenAt2 result
    fn submit(&mut self) -> Result<usize>;
    fn pop(&mut self) -> Option<CompletionEntry>;   // { user_data, result: i32, flags }
    fn wait(&mut self, timeout: Duration) -> Result<()>;
    fn remote_waker(&self) -> Result<RemoteWaker>;   // cross-thread wake (eventfd for Uring)
    fn supports(&self, kind: OpKind) -> bool;
}
```

`Op<'a, Fd>` is a `#[non_exhaustive]` enum of raw descriptions whose pointers
point into memory owned by the reactor's in-flight entry (stable for the whole
lifetime of the operation): `Nop`, `Read`, `Write`, `Recv`, `Send`, `PollAdd`,
`Accept`, `Connect`, `Timeout`, `OpenAt2`, `Statx`, `Fsync`, `RenameAt`,
`UnlinkAt`, `MkDirAt` (see `backend.rs` for fields). Read-only inputs are
references (`&'a Fd`, `&'a CStr`, `&'a SockAddr`, `&'a OpenHow`); buffers and
outputs are raw pointers (`*mut u8`, `*mut SockAddr`, `*mut libc::statx`).
Directory `None` means `AT_FDCWD`. `Timeout { deadline: Instant, timespec:
*mut KernelTimespec }`: the backend compares `deadline` with its clock (sim) or
writes the absolute `CLOCK_MONOTONIC` time into the reactor-owned `timespec`
(uring). Building an `Op` is safe; `push` is `unsafe`: every pointer and
referent must stay valid and unaliased until the completion for `user_data`
has been popped, so a backend may hand them to the kernel or keep them and
complete later. `OpKind` mirrors the variants plus `Cancel`.
`PushError { Full, Unsupported, Failed(Error) }` converts into `Error`
(`Full` -> `Overloaded`). `BackendConfig::new(capacity: u32)` is
`#[non_exhaustive]`. `CompletionEntry::into_result() -> Result<u32>`.
`RemoteWaker::new(impl Fn() + Send + Sync)` / `wake()`.

Synchronous resource creation is backend-neutral too, so applications never
need to branch on "am I simulated":

```rust
pub trait Host: Backend {
    fn socket(&self, domain: i32, ty: i32, protocol: i32) -> Result<Self::Fd>;
    fn bind(&self, fd: &Self::Fd, address: &SocketAddress) -> Result<()>;
    fn listen(&self, fd: &Self::Fd, backlog: i32) -> Result<()>;
    fn local_addr / peer_addr, shutdown, set_nodelay, try_send, try_recv(flags),
    fn poll_now(&self, fd: &Self::Fd, events: i16) -> Result<i16>,
    fn pipe(&self) -> Result<(Self::Fd, Self::Fd)>, ...
}
```

Implemented set: `socket`, `bind`, `listen`, `local_addr`, `peer_addr`,
`shutdown(fd, std::net::Shutdown)`, `set_nodelay(fd, bool)`,
`try_send(fd, &[u8], flags)`, `try_recv(fd, &mut [u8], flags)`,
`poll_now(fd, i16) -> i16`, `pipe() -> (read, write)`. Sockets and pipes are
nonblocking and close-on-exec. Extend as the Racer migration needs.

`SocketAddress` (`#[non_exhaustive]`: `Inet(SocketAddr)`, `Unix(PathBuf)`;
unnamed Unix = empty path, abstract Unix unsupported) is encoded by the public
`SockAddr` (`encode`, `empty`, `decode`, `as_ptr`, `as_mut_ptr`, `len`,
`len_mut_ptr`, `set_len`); both the address and its length live in one
Vec-backed allocation, stable across moves.

### Reactor

Module `reactor` (`reactor/{mod,ops,table,admission,fence}.rs`).

```rust
pub struct Reactor<B: Backend> { /* RefCell<State<B>>, !Send */ }

#[non_exhaustive] #[derive(Clone, Debug, Eq, PartialEq)]   // impl Default
pub struct ReactorConfig {
    pub max_in_flight: usize,       // ordinary partition, default 256, must be > 0
    pub max_reserved: usize,        // pool for reserve(), default 0
    pub max_wait: Duration,         // cap per wait, default 10 ms
    pub drop_wait: Duration,        // bounded fence on Drop, default 0 (never block)
    pub cancel_scan_budget: usize,  // slots scanned per poll, default 64
    pub cancel_wins: bool,          // default false: kernel result wins
}   // + with_* builders; backend capacity = 2 * (max_in_flight + max_reserved)

impl<B: Backend> Reactor<B> {
    pub fn new(backend: B, config: ReactorConfig) -> Result<Self>;  // calls backend.init
    pub fn config(&self) -> &ReactorConfig;
    pub fn backend(&self) -> Ref<'_, B>; pub fn backend_mut(&self) -> RefMut<'_, B>;
    pub fn clock(&self) -> &B::Clock; pub fn entropy(&self) -> Ref<'_, B::Entropy>;
    pub fn remote_waker(&self) -> RemoteWaker;
    pub fn in_flight(&self) -> usize; pub fn is_closed(&self) -> bool;
    pub fn failure(&self) -> Option<Error>;

    // Operations: lazy futures, submitted on first poll.
    pub fn read_at<Buf: IoBufMut, S: Scope>(&self, fd: &B::Fd, offset: u64, buf: Buf, scope: &S)
        -> Operation<'_, B, ReadAt<B, Buf>, S>;          // BufResult<usize, Buf, S::Error>
    pub fn write_at<Buf: IoBuf, S>(..) ;                 // BufResult<usize, Buf, S::Error>
    pub fn recv<Buf: IoBufMut, S>(&self, fd, buf, scope);  // BufResult<usize, Buf, S::Error>
    pub fn send<Buf: IoBuf, S>(&self, fd, buf, scope);     // MSG_NOSIGNAL; BufResult<usize, ..>
    pub fn readiness<S>(&self, fd, events: u32, scope);    // Result<u32 revents, S::Error>
    pub fn accept<S>(&self, fd, scope);                    // Result<(B::Fd, SockAddr), S::Error>
    pub fn connect<S>(&self, fd, address: &SocketAddress, scope); // Result<(), S::Error>
    pub fn sleep_until<S>(&self, deadline: Instant, scope);       // Result<(), S::Error>, -ETIME = Ok
    pub fn open_at2<S>(&self, dir: Option<&B::Fd>, path: &CStr, how: OpenHow, scope); // Result<B::Fd, _>
    pub fn statx<S>(&self, dir, path, flags: i32, mask: u32, scope); // Result<libc::statx, _>
    pub fn fsync<S>(&self, fd, datasync: bool, scope);
    pub fn rename_at<S>(&self, old_dir, old_path, new_dir, new_path, flags: u32, scope);
    pub fn unlink_at<S>(&self, dir, path, flags: i32, scope);
    pub fn mkdir_at<S>(&self, dir, path, mode: u32, scope);
    pub fn nop<S>(&self, scope);

    pub fn reserve(&self, slots: usize) -> Result<Reservation>;
    pub fn reserved_available(&self) -> usize;
    pub fn set_rejection_observer(&self, f: impl Fn(&Rejection<'_>) + 'static);
    pub fn clear_rejection_observer(&self);
    pub fn poll(&self, max_completions: usize) -> Result<usize>;
    pub fn wait(&self, timeout: Duration) -> Result<()>;
    pub fn drain(&self) -> Fence<'_, B>;                                  // close + cancel all
    pub fn fence_matching<S: Scope>(&self, f: impl FnMut(&S) -> bool) -> Fence<'_, B>;
    pub fn shutdown(&self, timeout: Duration) -> Result<()>;
}
pub fn leaked_operations() -> usize;   // process-wide count of leaked operations

// Operation<'r, B, O: OpSpec<B>, S: Scope, L = ()>: Future + Unpin
impl Operation<..> {
    pub fn reserved(self, r: &Reservation) -> Self;               // before first poll
    pub fn lease<L>(self, lease: L) -> Operation<.., Leased<L>>;  // output becomes (output, L)
    pub fn is_submitted(&self) -> bool;
}
```

Op spec types (`ReadAt`, `WriteAt`, `SocketRecv`, `SocketSend`, `Readiness`,
`Accept`, `Connect`, `SleepUntil`, `OpenAt2`, `Statx`, `Fsync`, `RenameAt`,
`UnlinkAt`, `MkDirAt`, `Nop`) implement the sealed `OpSpec<B>` trait; the
single generic future `Operation` avoids boxing.

Ownership: on first poll the spec (owning the buffer, a descriptor clone, the
encoded `SockAddr`, `CString` paths, `OpenHow`, a one-element `Vec<statx>`,
the timeout's `KernelTimespec`), the scope and the lease are moved into one
heap node (`Box::into_raw`, addressed only through raw pointers), and the
`Op` is derived from the node in place. The node is freed after the fence by
the future (inputs returned to the caller) or by the reactor (future
dropped). Otherwise it is leaked and counted. Accepted/opened descriptors are
adopted (`Backend::adopt_fd`) when the original completion arrives.

In-flight table: a generation-tagged slab; `user_data` = generation (31 bits)
<< 32 | slot, bit 63 marks the cancel request. Fence state per entry:
`InFlight(None | Queued | Pushed | Completed) -> AwaitingCancel(result) ->
Done(result)`; unknown, stale or duplicate completions fail the reactor
(admission closed, everything canceled, `poll` returns the error, `failure()`
reports it, later polls keep reaping).

Cancellation is requested by: the scope's `CancelToken` (registered per
operation; canceling wakes the reactor through the backend's `RemoteWaker`),
an expired `Scope::deadline` hint followed by a failing `check` (`wait`
never sleeps past the earliest hint), the bounded round-robin `check` scan, a
dropped future, `drain`, `fence_matching`, `shutdown`, or a reactor failure.
Precedence: a successful completion is returned as `Ok` (kernel wins; the
caller can consult its scope); a failed completion of a canceled operation
is the scope's error, or `Canceled` if the scope still passes. With
`cancel_wins`, every canceled operation reports the cancellation error and
drops the result (closing accepted descriptors). Kernel errors convert with
`S::Error: From<Error>`; keep `Error` in the application error type to
observe errno (for example `ECONNREFUSED` from `connect`). No retries.

Admission: ordinary slots (`max_in_flight`) or a `Reservation` (`Rc`-shared,
`slots()/in_use()/available()`, pool returned when the last clone drops and
its operations are fenced). Slots are held until the fence, not until the
result is collected. Rejections (`RejectionReason::{Capacity,
ReservationExhausted, ForeignReservation, Closed, Failed, QueueFull,
Unsupported, Backend}`) return all inputs and the lease, and are reported
to the observer as `Rejection { reason, kind: OpKind, reserved, scope: &dyn
Any, error: &Error }` after the state borrow is released. Scope failures
before submission are not rejections.

Re-entrancy: no user code runs under the state borrow (waker clones are made
outside it). `poll` and `fence_matching` fail with `Unavailable` when called
from a scope check or fence predicate (the busy guard protects in-flight node
pointers). Wakers, scope-check panics and destructors of released inputs are
isolated with `catch_unwind`; the first panic is resumed after the batch.

Drop: if work is in flight, `shutdown(drop_wait)` when `drop_wait > 0`
and not panicking; anything still in flight is leaked together with the
backend (`ManuallyDrop`), and counted by `leaked_operations()`.

Kernel requirements: Linux 5.6 for the core opcodes, 5.15 for `MkDirAt`.

### Uring backend

`uring::Uring` (`!Send`; `Fd = crate::Fd`, `SystemClock`, `SystemEntropy`;
`Uring::new()`, `Uring::is_available()`, `single_issuer()`, `submit_all()`).

- `init`: SQ = capacity, CQ = 2 * capacity; setup flags tried in order
  `SUBMIT_ALL | SINGLE_ISSUER`, `SUBMIT_ALL`, none, falling back only on
  `EINVAL`; requires `IORING_FEAT_NODROP` (else `Unsupported`); probes
  opcodes with `register_probe` (`supports`, and `push` refuses unsupported
  opcodes with `PushError::Unsupported`); creates the wake eventfd. Setup
  flags are recorded from what was requested, not read from the
  `Parameters` layout.
- `submit`: skips `io_uring_enter` when the SQ is empty and neither
  `CQ_OVERFLOW` nor `TASKRUN` is flagged; `EINTR`/`EAGAIN`/`EBUSY` count as
  zero submitted.
- `wait`: does not submit (the reactor does); returns at once on a zero
  timeout, a pending same-thread wake, or ready completions; otherwise
  `ppoll` on the ring fd and the eventfd, draining the eventfd afterwards.
- `RemoteWaker`: same-thread wakes set a flag; remote wakes are coalesced
  with an atomic and write the eventfd once.
- `Timeout`: absolute `CLOCK_MONOTONIC` (`TimeoutFlags::ABS`), converted
  from `Instant` via `Instant::now()` and `clock_gettime` read back to back.
- `Accept` uses `SOCK_CLOEXEC | SOCK_NONBLOCK`; the reactor's `send` sets
  `MSG_NOSIGNAL`.
- `Host`: libc calls; sockets and pipes are nonblocking and close-on-exec.

### Task, time, cancel

- `LocalExecutor` (`task` module): bounded set of `!Send` tasks with a
  wake-driven ready queue.
  - API: `new(capacity)`, `spawn(fut) -> Result<JoinHandle<T>>` and
    `spawn_detached(fut) -> Result<()>` (both `Overloaded` at capacity;
    there is no separate `try_spawn`), `reserve() -> Result<Permit>` then
    `Permit::{spawn, spawn_detached}` (infallible), `spawner() -> Spawner`
    (`Weak`, `!Send`; `Unavailable` once the executor is dropped),
    `run_ready(budget) -> usize`, `poll(&mut Context, budget) -> usize`,
    `abort_all()`, `len()` (tasks), `reserved()` (unused permits),
    `capacity()`, `is_empty()`, `has_ready()`. `yield_now() -> YieldNow`.
  - Scheduling: task wakers are `Send + Sync` and carry only a
    `(slot, generation)` key; a wake pushes the key into an
    `Arc<Mutex<VecDeque>>` once (atomic `queued` flag) and wakes the owner
    waker (the one last passed to `poll`; taken on wake like `AtomicWaker`).
    A turn polls at most `budget` tasks and only those queued before it
    started, so yielding or newly spawned tasks run next turn. Idle tasks
    cost nothing. `poll` self-wakes the owner if ready work remains and
    `budget > 0`. Nested `poll`/`run_ready` from a task returns `0` and does
    not replace the owner.
  - `JoinHandle<T>: Future<Output = Result<T>>`: `Ok` after the future was
    dropped; `Canceled` ("task aborted") on `abort()`, `abort_all()` or
    executor drop; `Unavailable` ("task panicked") on panic. Dropping it
    detaches. `is_finished()`.
  - Panics: the task is removed, capacity released, the task dropped, then
    the panic is resumed in the caller of `run_ready`/`poll`.
  - Drop ordering (from the old `Detached`): capacity is released before a
    finished, aborted or panicked task's future is dropped. Async blocks drop
    their own locals inside their final poll, so only state still owned by
    the future (for example fields of a hand-written future) observes it.
  - No internal borrow or lock is held while user code (polls, waker clone,
    wake and drop, destructors) runs.
  - Not ported (Racer keeps them): `Busy`, `poll_scoped`, `thread_waker`,
    `drive_local_with`, `poll_task`, the thread-local "current queue"
    (`enter`, `scope`, free functions `reserve`/`poll`/`pending`).
- `Timers` (`time` module): `Rc` handle over a `BTreeMap<(Instant, id),
  Option<Waker>>`. `sleep_until(deadline) -> Sleep` (infallible, registers
  immediately; drop removes), `timeout(deadline, fut) -> Timeout<F>`
  (`Result<T>`, `DeadlineExceeded`, the output wins ties),
  `poll(now, budget) -> usize` (wakes outside the borrow),
  `next_deadline()`, `len()`, `is_empty()`. `Sleep::{deadline, is_elapsed}`.
  It never reads a clock.
- `CancelToken`: `Clone + Send + Sync`, infallible `new`/`cancel`,
  `is_canceled`, `child()`, `canceled()` future (`Canceled`, owned),
  `register(&Waker) -> CancelRegistration` (`update(&Waker)` to re-arm,
  deregisters on drop), `registrations()` for diagnostics. Unbounded slab with
  O(1) removal and no registration limit (registrations are bounded by the
  reactor's in-flight capacity), so `register` is infallible. Wakers run
  outside the lock.
- `Clock`/`Entropy` are implemented for `&C`/`&E`; handles must be cheap (ZST,
  `Rc` or borrow), never an `Arc` clone per read. `SystemEntropy` uses
  `libc::getrandom` with EINTR retry and short-read handling.
- `Fd`: `Rc<OwnedFd>`, `!Send`, `into_owned() -> Result<OwnedFd, Fd>`,
  `ptr_eq`, `handle_count`, `From<OwnedFd>` and std socket/file types.

### Group and affinity

`group` is the lifecycle engine. It does not depend on the reactor or task
modules.

```rust
let plan = Plan::builder()                     // validates; InvalidConfiguration + context
    .lane("lane-0", 0).lane("lane-1", None)    // cpu: usize or None (unpinned)
    .helper("helper-0", 2, [0, 1])             // one shard per served lane
    .max_threads(4)                            // default unlimited
    .build()?;
pub trait Factory<S: Scope>: Sync {
    fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<S>>, S::Error>;
    fn build_helper(&self, lane: usize) -> ...;          // default InvalidConfiguration
    fn abandon_lane(&self, lane: usize) -> Result<(), S::Error>;  // default Ok
    fn teardown_scope(&self, startup: &S) -> S;          // default clone
}
pub type LifecycleFuture<'a, E> = Pin<Box<dyn Future<Output = Result<(), E>> + 'a>>;
pub trait Service<S: Scope> {          // only `poll` is required
    fn poll(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<(), S::Error>;
    // defaults: set_failure_reporter, waker (thread_waker(None)), register_driver,
    // start, wait_timeout, stop_admission, drain, close, fence, shutdown
}
impl<S: Scope + Send> Group<S> where S::Error: Clone + Send {
    pub fn new(plan: Plan, config: GroupConfig) -> Result<Self>;
    pub fn start<F: Factory<S> + Send + ?Sized + 'static>(&mut self, Arc<F>, &S) -> Result<(), GroupError<S::Error>>;
    pub fn run / run_scoped(&mut self, &F, &S) -> Result<(), GroupError<S::Error>>;
    pub fn drain(&self, &S); pub fn shutdown(&self, &S); pub fn join(&mut self); pub fn stats(&self) -> Stats;
}
```

- Lane 0 is the coordinator: under `start` it is a new thread that spawns and
  joins the others; under `run`/`run_scoped` it is the caller (whose affinity
  is restored afterwards). `start` counts the caller against `max_threads`.
- Per lane: build, `start`, steady state (`poll(work_budget)`, then park for
  at most `wait_timeout(idle_wait)`), `stop_admission`, `drain`, `close`,
  first `fence` (drained), wait for shutdown, `shutdown`, second `fence`,
  drop on the owner thread. A helper shard starts before its lane is built,
  is polled until its lane closes and its lane's first fence is confirmed,
  then drains and fences; shards are polled round-robin.
- `GroupConfig` (`#[non_exhaustive]`, `Default`, `with_*`): `idle_wait` 1 ms,
  `work_budget` 64, `helper_budget` 1, `helper_passes_per_park` 64,
  `wait_interval` 1 ms, `fatal`.
- Every error and panic is caught and attributed:
  `GroupError<E> { Failed { role, stage, error }, Panicked { role, stage, message }, Fatal { event, error } }`
  with `Role { Group, Lane(i), Helper { helper, lane } }` and `Stage`.
  `into_error()` maps to `E` (panics and error-less fatals become
  `Unavailable` with context). A fatal error replaces an earlier ordinary one.
- `FatalPolicy { LeakAndReport (default), Abort, Custom(Arc<dyn Fn(&FatalEvent)>) }`
  applies when a fence or `abandon_lane` fails or panics, or a service would be
  dropped before both fences. The service (and a panicking fence future) is
  `mem::forget`-ed; a helper shard whose lane failed its fence is leaked too.
- `FailureReporter<E>` (`Clone`, no `Copy` bound) records
  `Failed { stage: Report }` and moves the group to drain.
- `thread_waker(Option<RemoteWaker>)` unparks the creating thread and wakes the
  optional reactor waker.

`affinity` has no placement policy:

- `CpuSet` (ascending, `Display`/`FromStr` in Linux list format,
  `MAX_CPU = 2^20 - 1`), `current_cpus()`, `set_cpus(&CpuSet)`, `pin_cpu(cpu)`.
- `Topology { cpus: Vec<CpuLocation>, quota: Option<CpuQuota>, devices: Vec<DeviceLocality> }`
  from `Topology::discover()` or `discover_at(root, &allowed)` (fake trees in
  tests). `CpuLocation { cpu, package, die, core, numa_node }` uses `None` for
  absent ids and `-1`. Cgroup v1 and v2 limits are read from every visible
  ancestor; an applicable hierarchy that is not visible fails closed.
  `DeviceLocality { class: "net" | "infiniband", name, numa_node }`; devices
  that vanish mid-scan are skipped.
- `place`/`place_exact` move to Racer.

## Crate: `uring-runtime-sim`

- `SimBackend` implements `Backend + Host` with `Fd = SimFd`,
  `Clock = SimClock`, `Entropy = SimEntropy`.
- `World` (shared `Rc` handle): network (streams, listeners, datagrams, pipes,
  partitions), disk (volatile and durable images, `crash`, `crash_under`,
  `corrupt`), typed fault injection (`Fault` + `OpKind` instead of strings),
  bounded trace.
- `SimClock`: deterministic monotonic and wall time from a seed; single-thread
  counters, no `Arc<Mutex>` on the read path. `SimEntropy`: SHA-256 counter
  stream; the domain string is a constructor argument (Racer passes the old
  `"uring-runtime.entropy.v1\0"` to keep replay corpora stable).
- The Linux differential tests live in `tests/` and are Linux-only.

## What moves to Racer

New internal crate `cmd/racer-dataplane/platform` (`racer-platform`) holds the
policy that multiple Racer crates share; root-only pieces go to the root crate.

| Old location | New home |
|---|---|
| `reactor::filesystem::{secure, operations}` | `racer-platform::fs` (built on core `open_at2`/`statx`/... ops) |
| `reactor::filesystem::checkpoint` | root `src/store/` |
| `channel`, `mailbox`, `mailbox::executor`, `offload` | `racer-platform` |
| `drivers::{retry_listener, race, Hedge*}` | `racer-platform::retry` |
| `environment::{now, wall_now, fill_random, Environment, require_simulated}` (ambient TLS) | `racer-platform::env` (Racer's choice of ambient context, built on core `Clock`/`Entropy` and the sim crate's `SimClock`) |
| `environment::{Deadline, unix_millis, Observer, Registry}` | `racer-platform::time` |
| `group::affinity::{place, place_exact}` | root `src/worker.rs` area |
| `reactor::ready_set::ReadySet` | root `src/client/` |
| `Descriptor::{idle_healthy, peer_*, tcp_tuple, ...}` | `racer-platform::socket` |
| `Reactor<S, B: Budget>` admission accounting, `SUBMISSION_BYTES` | `racer-platform::reactor` adapter over `uring_runtime::Reactor<RacerBackend>` |

`RacerBackend` is Racer's own `Backend` (an enum dispatching to `Uring` or, in
simulation builds and tests, `SimBackend`). The simulation choice is Racer
policy and stays behind Racer's own `simulation` feature.

## Validation

- Reactor state-machine tests use a scripted backend (`reactor/tests.rs`)
  and run under Miri (Stacked and Tree Borrows); real-kernel tests live in
  `uring-runtime/tests/uring.rs`.
- `crates/`: `cargo fmt --check`, `cargo clippy --all-targets -D warnings`,
  `cargo test` (io_uring tests fail, not skip, when `URING_RUNTIME_REQUIRE_KERNEL=1`),
  `cargo doc -D warnings`, Miri on the pure state-machine, buffer and executor
  tests, MSRV check.
- `cmd/racer-dataplane`: existing CI jobs unchanged in intent.
