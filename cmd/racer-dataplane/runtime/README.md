# uring-runtime

A worker-local, explicitly driven io_uring runtime. It does not create an
executor or choose application placement and admission policies.

- `reactor::Reactor<S, B>` owns submissions through their completion fences.
  `Scope` supplies cancellation/deadline policy; `Budget` supplies retained byte
  charges. Callers drive `poll_budgeted` and `wait` explicitly.
- `group` runs pinned local services and optional shared helper services with
  explicit startup, drain, fence, shutdown, and join phases. The caller supplies
  the plan, service factory, and teardown scope.
- `affinity` discovers effective CPU/cgroup/NUMA/NIC constraints. It does not
  choose lane-to-helper ratios or device placement.
- `channel`, `deadline`, and `environment` provide bounded SPSC handoff,
  cancellation registration, and time/entropy sources.
- `mailbox` provides bounded owned commands and completion receipts. Cancellation
  notifies the owner; it does not release accepted payloads or their budget.
- `offload` provides reserved submission/completion ports, a bounded admission FIFO,
  and waiter routing. Callers supply ticket identity, payloads, execution, scope
  checks, and result classification. A dropped waiter does not release job credit.
- `yield_now`, `poll_scoped`, and `Busy` provide cooperative turns, cancellation
  registration around caller-owned polling, and worker-local exclusion. Scoped
  polling requires the owner to arrange deadline polling; it does not add a timer.
- `thread_waker` and `drive_local_with` support explicit local drivers. Backend
  failures are reported without abandoning the operation's completion fence.
- `deadline::unix_millis` and `Deadline::{to_unix_millis,from_unix_millis}` use the
  stable environment clock anchor with checked, millisecond-truncating conversion.
- `Reactor::sleep_until` takes an explicit `SleepMode`; compiling simulation never
  silently selects the clock-poll backend for a production caller.
- `deadline_registry::Registry` tracks externally admitted deadline registrations
  with explicit time and bounded wakeup polling. `clock_observer::Observer` detects
  wall/monotonic drift using caller-supplied samples and a threshold. Neither chooses
  request deadlines, metadata invalidation, or a background polling schedule.
- `Reactor::file_read_bounded` probes EOF with caller-selected limits and chunks.
  Callers select file type, initial size, and permission requirements before reading.
- `reactor::filesystem::secure` validates path/component bytes, regular-file size,
  and caller-selected owner/mode/link requirements. `AccessError` distinguishes
  missing stat fields from permission failures for application-level translation.
  `Reactor::file_directory` walks pinned directories without following symlinks and
  optionally creates owner-only children with parent fsync fences. Callers check
  final-directory access separately and select path limits.
- `Reactor::file_stage` removes and fences a stale stage before exclusive owner-only,
  no-symlink creation beneath a pinned directory. `file_remove_synced` also fences
  absent names. Callers own validated removal names, stage naming, serialization,
  and failure cleanup; token/projection symlink policy is not imposed by these APIs.
- `Reactor::file_replace` writes a caller-prepared stage and renames it with explicit
  `Durability`. `file_replace_chunked` uses caller-selected bounded scratch for larger
  byte slices. Both handle partial writes; `Publish` intentionally performs no fsync,
  while `FileAndDirectory` syncs the file before rename and the directory afterward.
  Neither operation prepares or cleans up its stage.
- `reactor::filesystem::publish_new` is synchronous namespace-only replacement using
  caller-supplied stage candidates. It skips existing stages, cleans failed attempts,
  and performs no fsync; it does not promise crash durability.
- The opt-in `test-util` feature exposes `test_util::WakeCounter` without simulation.
- `drivers::poll_task` polls an optional pinned task and clears a completed slot
  before returning its output. Scheduling and error handling remain with the caller.

## Deliberate application adapters

Racer keeps RequestScope narrowing and candidate checks together: narrowing clones
the full policy state, takes the earlier hard deadline, and does not bypass checks
through a generic deadline wrapper. Application affinity, endpoints, backoff,
publication readiness, checkpoint cuts, and crypto/reactor fencing remain policy.

Generic owned task reservation/spawning already lives in `drivers::{reserve,spawn}`
and `Permit::{submit,submit_detached}`. Group startup/teardown barriers already live
in `group`. Extracting Racer publication/checkpoint barriers or service graph owners
would require another service-level design, not a small primitive substitution.

Checkpoint publication uses chunked replacement with namespace-only durability;
identity persistence uses synced stage preparation and file/directory durability.
Those choices are intentionally different. Likewise, token path reads allow ordinary
symlinks while excluding magic links, but identity directory traversal and relative
identity reads reject all symlinks. The control adapter retains these security
requirements, effective-UID selection, limits, and error translation.

Cooperative yielding, ingress deadline registration, clock drift observation, and
offload queue mechanics are generic APIs now. Their caller adapters still own
scheduling, admission classes, metadata invalidation, crypto execution, completion
classification, and abandonment fences.

I/O buffers implement unsafe stable-storage contracts: accepted operations can
outlive their futures, and storage must remain valid until the kernel fence.
Dropping an operation is abandonment, not proof that its resources are reusable.

The nondefault `simulation` feature provides deterministic clock, entropy, and
I/O backends. Racer enables it only as a development dependency. Its entropy
domain is retained for compatibility with existing replay seeds.

Racer-specific request IDs, candidate deadlines, admission classes, crypto payloads,
page ownership hashing, TLS time conversion, and placement policy stay in application
adapters rather than the generic runtime.

From the dataplane directory, run
`timeout --signal=TERM --kill-after=10s 300s cargo test -p uring-runtime --features simulation -j 2`
to test this crate, including the public reserved-capacity lifecycle scenario and
the simulated short-write request/reply workflow. Simulation scenarios need no
io_uring permissions; real-kernel regression tests remain separate and may require
io_uring support. Split workspace coverage into bounded package-level commands.
Build the production binary with:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo build --release --bin racer-dataplane --no-default-features -j 2
```
