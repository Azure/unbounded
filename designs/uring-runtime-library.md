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
  `repository = "https://github.com/project-unbounded/unbounded"` (copy the URL
  used by other manifests if one exists), shared through `[workspace.package]`.
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
| `group` | pinned thread groups: `Group`, `Plan`, `Service`, `Factory`, `FailureReporter`, `FatalPolicy` |
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

### Scope

```rust
pub trait Scope: Clone + 'static {
    type Error: From<Error> + 'static;
    fn check(&self) -> Result<(), Self::Error>;
    fn cancellation(&self) -> Option<&CancelToken> { None }
    fn deadline(&self) -> Option<Instant> { None }
}
```

`Unscoped` (a unit struct, `Error = Error`) never cancels. The reactor uses
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
`&'static str`, `Box<[u8]>` only if Miri passes for it. `Read`/`Recv` write into
`[0, bytes_total)` and call `set_init(n)`.

### Backend seam

The reactor owns an in-flight table and the original + cancel fence state
machine; it talks to a backend through a narrow trait. This is the seam the old
code already had (`simulation::Driver::{push, cancel, submit, pop}`), lifted
into a public trait.

```rust
pub trait Backend: 'static {
    type Fd: Clone + Debug + 'static;        // Uring: `Fd`, simulator: `SimFd`
    type Clock: Clock;
    type Entropy: Entropy;
    fn clock(&self) -> &Self::Clock;
    fn entropy(&self) -> &Self::Entropy;
    fn init(&mut self, config: &BackendConfig) -> Result<()>;
    fn push(&mut self, user_data: u64, op: &Op<'_, Self::Fd>) -> Result<(), PushError>;
    fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError>;
    fn submit(&mut self) -> Result<usize>;
    fn pop(&mut self) -> Option<CompletionEntry>;   // { user_data, result: i32, flags }
    fn wait(&mut self, timeout: Duration) -> Result<()>;
    fn waker(&self) -> Result<RemoteWaker>;          // cross-thread wake (eventfd for Uring)
    fn supports(&self, kind: OpKind) -> bool;
}
```

`Op<'a, Fd>` is a `#[non_exhaustive]` enum of raw descriptions whose pointers
point into memory owned by the reactor's in-flight entry (stable for the whole
lifetime of the operation): `Read`, `Write`, `Recv`, `Send`, `PollAdd`,
`Accept`, `Connect`, `Timeout`, `OpenAt2`, `Statx`, `Fsync`, `RenameAt`,
`UnlinkAt`, `MkDirAt`. Building an `Op` is safe; `push` is the only place that
may hand pointers to the kernel, and the reactor guarantees the backing memory
outlives both completions.

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

(The exact `Host` method set is whatever the migrated Racer code needs, kept
POSIX-shaped and documented.)

### Reactor

```rust
pub struct Reactor<B: Backend> { /* RefCell<State<B>> */ }
pub struct ReactorConfig {
    pub max_in_flight: usize,          // ordinary partition; ring = 2x for cancels
    pub max_wait: Duration,            // cap on a single wait (default 10 ms)
    pub drop_wait: Duration,           // bounded drain on Drop (default 0: never block)
    pub cancel_scan_budget: usize,     // entries scanned per poll
    pub cancel_wins: bool,             // default false: kernel result is returned
}
impl<B: Backend> Reactor<B> {
    pub fn new(backend: B, config: ReactorConfig) -> Result<Self>;
    pub fn backend(&self) -> Ref<'_, B>; pub fn clock(&self) -> B::Clock;
    pub fn read_at<Buf: IoBufMut, S: Scope>(&self, fd: &B::Fd, offset: u64, buf: Buf, scope: &S) -> Read<'_, B, Buf, S>;
    // write_at, recv, send, poll (readiness), accept, connect, sleep_until,
    // open_at2, statx, fsync, rename_at, unlink_at, mkdir_at
    pub fn reserve(&self, slots: usize) -> Result<Reservation>;   // dedicated capacity
    pub fn poll(&self, max_completions: usize) -> Result<usize>;  // reap + scan + submit
    pub fn wait(&self, timeout: Duration) -> Result<()>;
    pub fn remote_waker(&self) -> Result<RemoteWaker>;
    pub fn in_flight(&self) -> usize;
    pub async fn drain(&self) -> Result<()>;           // stop admission, fence everything
    pub async fn fence_matching(&self, f: impl Fn(&dyn Any) -> bool);  // or typed by scope
    pub fn shutdown(&self, timeout: Duration) -> Result<()>;
}
```

Every I/O result returns ownership of everything the caller passed in, on
success and on failure:

```rust
pub struct BufResult<T, Buf> { pub result: Result<T, E>, pub buf: Buf }
```

Option-style combinators replace the `_with_lease`, `_reserved` and
`_with_observation` method families: a `Submit` options struct (or builder) on
each operation carries an optional `Reservation` and an optional
caller-owned "lease" value that is held until both fences and returned.

Cancellation precedence: by default the kernel result wins. If an operation
completed successfully after its scope was canceled, the caller still gets the
bytes or the accepted descriptor plus a `canceled` flag, so stream state is
never silently lost. `cancel_wins = true` restores the old behavior.

Retries are not done by the reactor. Short reads/writes and `EAGAIN` are
returned to the caller.

Drop: never blocks unless `drop_wait > 0`. Anything the kernel may still touch
is leaked (`mem::forget`) and counted (`Reactor::leaked()`), never freed.

Kernel requirements: Linux 5.6 for the core opcodes, 5.15 for `MkDirAt`.
`Uring::init` probes opcodes with `io_uring::Probe`, requires
`IORING_FEAT_NODROP`, and falls back from `SINGLE_ISSUER`/`SUBMIT_ALL` on
`EINVAL`. `sleep_until` uses absolute `CLOCK_MONOTONIC` timeouts.

Fixes carried over: IPv6 `sin6_flowinfo` is passed through without byte
swapping, matching std.

### Task, time, cancel

- `LocalExecutor`: bounded set of `!Send` tasks with a wake-driven ready queue
  (a woken task is pushed once; `poll(budget)` polls at most `budget` ready
  tasks; idle tasks cost nothing). `spawn` returns a `JoinHandle<T>`;
  `try_spawn` fails with `Overloaded` at capacity. `clear()` drops all tasks.
  Preserve the old drop-ordering guarantees: capacity is released only after
  the task future is dropped; waking from inside a waker callback is safe.
- `Timers`: deadline queue keyed by `(Instant, id)`; `register(deadline)`
  returns a `Sleep` future; `poll(now, budget)` fires expired entries; it never
  reads a clock itself.
- `CancelToken`: `Clone + Send + Sync`, infallible `new`/`cancel`,
  `is_canceled`, `child()`, `canceled()` future, `register(&Waker)`.
  Registration limit is a constructor argument (default unbounded slab).

### Group and affinity

- `group` keeps the lifecycle engine (Running -> Drain -> Shutdown, two fences
  per service, panic capture with payload kept in the error) with configurable
  `GroupConfig { idle_wait, work_budget, helper_budget, fatal: FatalPolicy }`.
  `FatalPolicy::LeakAndReport` is the default; `Abort` is opt-in.
- `affinity` keeps topology discovery (`Topology::discover()`, injectable sysfs
  root for tests), `CpuSet`, `current_cpus`, `set_cpus`, `pin_cpu`. Placement
  (`place`, `place_exact`) moves to Racer.

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

- `crates/`: `cargo fmt --check`, `cargo clippy --all-targets -D warnings`,
  `cargo test` (io_uring tests fail, not skip, when `URING_RUNTIME_REQUIRE_KERNEL=1`),
  `cargo doc -D warnings`, Miri on the pure state-machine, buffer and executor
  tests, MSRV check.
- `cmd/racer-dataplane`: existing CI jobs unchanged in intent.
