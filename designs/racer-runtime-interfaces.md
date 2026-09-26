# Runtime integration interfaces

Runtime/memory interfaces below are implemented. Each worker continuously drives
the reactor and crypto client, including while application futures are draining.

- `Admission::new(Limits)`, `limits() -> &Limits`, `stop()`,
  `reserve(Option<&CacheId>, ResourceClass, usize) -> Result<Reservation>`,
  `reserve_fill(&CacheId, bool) -> Result<FillReservation>`.
  Reservation is non-cloneable, Send + Sync, releases on final owner drop, and has
  `amount()`, `class()`, `validate(ResourceClass, usize) -> Result<()>` and
  `cache() -> Option<&CacheId>`. Allocations must validate class and minimum amount.
- `RequestScope::check()`, `cancel()` preserve the original monotonic deadline.
  `Cancellation::register(&Waker)` registers cancellation notification; callers
  register before rechecking. `RequestScope::new(RequestId, Instant)` is provided.
- `Reactor::new(Rc<Admission>)` remains side-effect-free. The reactor owns every
  submitted FD/buffer/lease until its completion fence, independent of futures.
  `read_at`/`write_at` retain existing signatures and return `Completion<B,L>`.
  `recv`/`send` use the same arguments without offset. Partial byte counts are
  explicit; callers retain and resubmit completed owners as appropriate.
  `poll_budgeted(usize) -> Result<usize>`, `in_flight() -> usize`,
  `wait(Duration) -> Result<()>` drive completion and idle waits.
  `readiness(Rc<OwnedFd>, u32, &RequestScope) -> Operation<u32>` uses POLLIN/POLLOUT.
  `accept(Rc<OwnedFd>, &RequestScope) -> Operation<OwnedFd>` and
  `connect(Rc<OwnedFd>, SocketAddress, &RequestScope) -> Operation<()>` support
  owned `SocketAddress::Inet(SocketAddr)` / `Unix(PathBuf)` addresses.
  `connect_with_lease(fd, address, lease: L, scope) -> Operation<L>` retains and
  returns the entire connection/pool/admission owner across connect cancellation.
  HTTP checkout should use it rather than rely on FD weak-reference quarantine.
- `CryptoPort::poll_job` and `complete` retain existing owned-message signatures.
  The security engine destructures crate-visible `CryptoJob` and constructs
  `CryptoCompletion { permit, outcome, key, scope }` after all input access ends.
  Completion publication never needs additional admission. `CryptoClient` reaps
  abandoned completions; `outstanding() -> usize` includes accepted unreaped work.
  Once a job is accepted, `execute` returns cancellation/deadline failure only
  after its completion has been consumed. Read drivers may treat that return as
  their operation fence. Dropping the future separately abandons delivery while
  worker-owned state continues retaining and reaping the accepted job.
  Operational startup uses fallible `crypto::try_pair(worker, generation, capacity)`;
  legacy `pair` remains for validated composition. Both allocate fixed bounded queues.
- Runtime does not encrypt pages. Security's paired `PageCryptoEngine` owns AEAD.
  Only owned Send job/completion values cross threads; I/O-local futures stay local.

Memory has a focused delegate; reactor and affinity/worker each have focused
delegates. Runtime parent owns admission, channels, cancellation, and crypto handoff.
No runtime owner edits Cargo, app, config, error, HTTP, security, or storage files.

## Integration requirements
- Admission remains an I/O-local authority. Its reservations use atomic release
  counters and are Send + Sync. `Admission::owns(&Reservation)` validates pool
  provenance in addition to `Reservation::validate` checking class and size.
- Prefer `Cancellation::subscribe() -> Result<CancellationRegistration>` for
  operation-local waits; its `register(&Waker)` refreshes the waker and Drop removes
  the registration. This bounds concurrent registrations without accumulating
  sequential completed tasks. Legacy `register` remains for scope-long drivers.
- `CryptoClient::register_driver(&Waker)` registers the worker's completion wakeup;
  register before driving completions. Runtime deadline checks also require the
  worker's bounded timer tick. `CryptoPort::poll_job` registers the engine wakeup.
- Worker factory now exposes `limits() -> Limits`; override it in Application with
  the intended **per-worker partition** of validated node budgets. Returning full
  node byte limits for every worker multiplies memory consumption. WorkerGroup's
  default is bounded but not deployment configuration. Config/app owners must
  partition node-wide resources before service startup.
- `Admission::reserve_completion(cache, class, amount)` permits bounded allocations
  for already-admitted operations after `stop()`. Storage dirty-write staging during
  shutdown must use this rather than ordinary new-request `reserve`; it still
  enforces the same byte/count bounds. Prefer reserving staging before acquisition.

## Operational lifecycle and memory

- `WorkerGroup::run` runs worker-zero I/O on the caller thread and restores its
  affinity after join. `run_with_scope(factory, scope)` allows external cancellation.
  `start(Arc<dyn WorkerFactory + Send>, scope)` supports separately
  driven lifecycle methods but budgets its additional waiting caller thread.
  Startup errors close handoffs, fence accepted work, and join started roles.
  Borrowed factories are supported only by `run` and `run_with_scope`.
- `CryptoService::register_driver(&Waker)` must forward to its CryptoPort before
  each engine poll. I/O completion driver and operation wakers are independent.
  On engine loss, receiver destruction fences queue reclamation; accepted inputs
  remain retained until that fence. Deadline checks use bounded worker timer ticks.
- `Reactor::waker() -> Result<ReactorWake>` returns the cross-thread eventfd wake
  handle. Retain it through drain rather than trying to initialize a stopped ring.
  On an unrecoverable destructor driver error the reactor deliberately retains
  bounded kernel-visible owners; this is degraded teardown, not successful fencing.
- `Delivery::finish_to(reader, ConnectionLease, scope)` preserves HTTP framing and
  connection ownership. `finish_to_socket` handles owned unframed sockets. Copied
  pipe data can be spliced without retaining userspace page pointers; readiness
  and asynchronous fallback operations retain all required connection/page leases.
  Pipe admission pressure selects nonblocking copying instead of failing a body
  after its headers. Backpressured HTTP copies use at most 64 KiB of separately
  admitted request-context staging per pending send, retained through its fence.
- `MemoryCache::retire_key(cache, key)` and `remove_cache(cache)` block late
  publication. `evict_idle(bytes)` releases only idle bundles; admission pressure
  must call this from the read owner before retrying an allocation.

## Verification

Runtime tests cover real io_uring TCP/Unix connections, positional file I/O,
partial I/O, cancellation CQE ordering, abandonment, quota release, driver wakes,
affinity, one-CPU paired startup/drain and rollback. Memory tests cover real
copied-splice delivery, framing, stalls, disconnects and completion-owned release.
The host used for verification supports io_uring; those tests actually execute.
