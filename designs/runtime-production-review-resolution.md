# Runtime production review resolution

This disposition describes the integrated implementation, not a new runtime or
application model. Paths below are relative to `cmd/racer-dataplane/` unless stated
otherwise. Line citations identify implementation and executable assertions, not
just test names. Test presence is not a claim that every infrastructure test ran in
this validation task; execution evidence and remaining limits are recorded below.

## Ownership, unsafe storage, and reactor progress

1. **Delayed syscall backing: fixed and Miri-validated.** `SyscallArg` uses a private
   one-element Vec; pathname backing also uses Vec rather than moving Box-backed
   CString storage after pointer derivation (`runtime/src/reactor.rs:67`,
   `runtime/src/reactor/filesystem.rs:151`). The new pure delayed consumers move
   both owners through an entry and completion closure before reading/writing the
   original pointer (`runtime/src/reactor/filesystem/memory_tests.rs:6`, `:26`).
   They execute under pinned Miri without aliasing or isolation exemptions.
2. **Submission rollback and accepted-operation lifetime: fixed.** Ownership enters
   the table before SQ publication; rejection removes it outside the state borrow
   (`runtime/src/reactor.rs:682`). Original and cancel CQEs remain separate fences;
   cancellation, timeout, and abandoning delivery do not release accepted storage.
   Tests exercise actual published entries and arbitrary CQE orders rather than
   treating a cancellation notification as completion
   (`runtime/src/reactor/tests.rs:1767`, `:1835`, `:1907`, `:2054`).
3. **Reserved capacity and unread failures: fixed.** Reserved capacity is distinct
   from ordinary completed-reply slots. Failure replies, accepted descriptors, and
   readiness retries retain their partition until consumption/fencing. Public
   regressions cover failed replies and another reactor's independence
   (`runtime/tests/reserved_capacity.rs:40`, `:215`, `:296`); ordinary completion
   behavior remains explicitly different (`runtime/src/reactor/tests.rs:779`).
4. **Callback/reentrant destruction and panic batches: fixed.** Completion batches
   release the reactor borrow before finishing/waking, process unrelated completed
   entries even after a callback panic, then resume unwind
   (`runtime/src/reactor.rs:1188`). Scope checks and retired wakers also run outside
   state borrows (`runtime/src/reactor.rs:173`, `:1265`). Regression assertions cover
   raw-waker clone/drop/wake reentry, scope destruction, and preserved unrelated
   delivery (`runtime/src/reactor/tests.rs:1995`, `:2163`, `:2222`, `:2271`, `:2471`).
5. **Errno and transient data retries: fixed, narrowly scoped.** Raw unclassified
   errors survive as `Error::Os`; existing classifications and standard Error/
   Display remain (`runtime/src/lib.rs:46`, `:112`, `:129`). Data operations retain
   owners across at most eight retries for zero-progress EINTR/EAGAIN; EAGAIN adds
   a readiness fence, and positive partial success returns once
   (`runtime/src/reactor.rs:127`, `:739`). Tests assert exact owner lifetime,
   bounded exhaustion, no-spin readiness, cancellation, and unchanged offsets
   (`runtime/src/reactor/tests.rs:1538`, `:1593`, `:1671`, `:1699`, `:1951`). Connect
   and namespace mutations deliberately are not automatically replayed.
6. **Shutdown and selective fences: fixed, with explicit degraded behavior.**
   `shutdown(timeout)` bounds host waiting but errors never establish a fence;
   Drop can quarantine/leak the ring and unfenced owners. Selective cancellation
   covers the whole matching snapshot before waiting
   (`runtime/src/reactor.rs:1390`, `:1409`). Missing-original-CQE, fatal-driver,
   snapshot, exhausted-ID, and rotating-cancellation regressions are at
   `runtime/src/reactor/tests.rs:2318`, `:2342`, `:2373`, `:2404`, `:2434`.
   Deliberate nonchange: do not free memory or detach kernel owners to make teardown
   appear successful. Repeated degraded teardown can accumulate retained memory.
7. **Empty-submit and external wake progress: retained and verified.** Submission
   decisions inspect actual SQ occupancy and kernel flags, not a stale dirty bit;
   `wait` submits work queued by the service turn
   (`runtime/src/reactor.rs:1305`, `:1474`). Truth-table, partial/transient/fatal
   submission, external wake, cancel, and intervening-turn tests remain
   (`runtime/src/reactor/tests.rs:532`, `:575`, `:635`, `:729`, `:810`). The 10ms
   fallback is intentional for producers without eventfd attachment, not an
   implicit background executor or a reason to remove prompt eventfd wakes.
8. **Descriptor and readiness contracts: fixed.** Direct nonblocking helpers bound
   EINTR retries, preserve flags while setting nonblocking/CLOEXEC, and actually
   disable TCP_NODELAY on false (`runtime/src/reactor/descriptor.rs:31`, `:65`,
   `:124`, `:147`, `:356`). ReadySet requires remove-before-close/reuse and drops
   cached events on removal; zero budget does no polling
   (`runtime/src/reactor/ready_set.rs:36`, `:61`, `:90`). Duplicate-descriptor,
   level-triggered, idle-fence, and failed-wait assertions are at `:162`, `:219`,
   `:251`, `:276`. Caller ownership of registered FDs is deliberate.

## Filesystem, publication, and resource accounting

9. **Bounded secret reads: fixed.** `ReadBuffer` retains the full output-limit charge,
   allocates once, exposes slices, and zeroizes before releasing its charge
   (`runtime/src/reactor/filesystem/operations.rs:46`, `:74`). Tests cover actual
   short CQEs, empty/exact/overflow limits, charge retention until output drop, and
   rejection before I/O (`runtime/src/reactor/filesystem/operations_tests.rs:27`,
   `:176`, `:255`, `:278`). Scratch charges remain separate; copying the output is
   caller-owned allocation policy, not a runtime escape hatch.
10. **Replacement freshness, aliases, names, and append: fixed.** Replacement validates
    distinct single components, empty regular-file size, exact link count, and
    matching inode/device after no-symlink nonappend reopening
    (`runtime/src/reactor/filesystem/operations.rs:5`, `:114`). Nested/same names,
    stale suffixes including empty input, mismatched stages, hard links, and append
    descriptors are covered (`runtime/src/reactor/filesystem/operations_tests.rs:125`,
    `runtime/src/reactor/filesystem/kernel_tests.rs:160`). Deliberate boundary:
    callers must exclusively control the pinned trusted directory and stage inode;
    checks cannot protect against an authorized concurrent directory writer.
11. **Publication phase and cause: fixed.** BeforeRename, RenameUncertain, and
    Published preserve the cause and distinguish prepublication failure from
    ambiguous accepted rename and post-rename fsync failure
    (`runtime/src/reactor/filesystem/operations.rs:18`, `:171`). Held cancellation,
    visible target, retained directory, and abandoned future assertions are at
    `runtime/src/reactor/filesystem/operations_tests.rs:209`, `:358`. Application
    translation preserves both dimensions (`src/error.rs:54`,
    `src/control.rs:2292`). No blind retry after uncertain/published outcomes.
12. **Durability and partial writes: fixed/explicit.** Both replacement paths loop
    over partial writes; Publish omits fsync while FileAndDirectory syncs before
    rename and after it (`runtime/src/reactor/filesystem/operations.rs:153`, `:171`,
    `runtime/src/reactor/filesystem/chunked.rs:9`). Tests distinguish durable crash
    results, zero-progress failure, and empty success
    (`runtime/src/reactor/filesystem/operations_tests.rs:87`, `:396`,
    `runtime/src/reactor/filesystem/chunked.rs:70`). Namespace-only publication
    intentionally does not promise crash durability.
13. **Secure traversal, stale-stage fencing, and permissions: fixed.** Caller access
    requirements distinguish missing stat fields from denied access; directory
    traversal and stage preparation use pinned no-symlink components
    (`runtime/src/reactor/filesystem/secure.rs:1`). Tests check parent traversal,
    symlinks, cleanup fsync before exclusive create, and real magic links/hardlinks
    (`runtime/src/reactor/filesystem/secure_tests.rs:19`, `:69`, `:100`,
    `runtime/src/reactor/filesystem/kernel_tests.rs:160`). Mode/UID/link policy
    remains explicit rather than a runtime-wide application policy.
14. **Blocking publication helper: contained, not mislabeled async.** `publish_new`
    validates all direct-child candidates before side effects, uses pinned host
    directory descriptors, exclusive private stages, and best-effort cleanup
    (`runtime/src/reactor/filesystem/publish.rs:183`). Host containment/umask and
    simulation permission tests are at `runtime/src/reactor/filesystem/kernel_tests.rs:67`,
    `:122`, `runtime/src/reactor/filesystem/chunked.rs:208`. It remains a blocking,
    namespace-only helper outside latency-sensitive workers. Shutdown checkpoint
    publication now invokes `publish_async` (`src/app/recovery.rs:87`,
    `src/store/checkpoint.rs:197`); no new universal filesystem service is introduced.
15. **Configuration versus ABI/path limits: clarified and tested.** Policy-selected
    read/chunk/queue/budget limits do not lift signed-offset, SQE-length, Linux
    pathname, or filesystem component limits (`runtime/src/reactor.rs:803`,
    `runtime/src/reactor/filesystem.rs:147`, `:170`). O_PATH flags and creation mode
    handling are tested independently of application limits
    (`runtime/src/reactor/filesystem/kernel_tests.rs:36`,
    `runtime/src/reactor/tests.rs:2091`).

## Handoffs, scheduling, lifecycle, and environment

16. **Channel reentrant orphan cleanup: fixed and Miri-validated.** Advance the head
    before payload destruction, rereading it on reentry; publish ownership before
    waking consumers/producers (`runtime/src/channel.rs:103`, `:193`). Reentrant
    and panicking destructors, non-power-of-two wrap, capacity/close, and threaded
    FIFO tests execute in Miri (`runtime/src/channel.rs:230`, `:266`, `:280`, `:308`,
    `:338`). No broad unsafe-test skip was introduced.
17. **Offload admission/registry callback hazards: fixed and Miri-validated.**
    Snapshot callbacks outside RefCell borrows; preserve unique endpoint ownership
    without a RefCell around reentrant receive; reject duplicate registration and
    preserve the first completion (`runtime/src/offload.rs:204`, `:294`, `:321`,
    `:353`, `:438`, `:499`, `:523`). Raw-waker, payload/scope-drop, and mutating
    completion reentry assertions are at `:653`, `:697`, `:729`, `:759`, `:799`.
18. **Accepted offload credit, stale IDs, and worker loss: fixed.** Permits survive
    execution and unread completion; abandonment only changes delivery. Sequence
    overflow never wraps, stale registrations cannot remove a newer generation,
    and worker-loss inspection is budgeted (`runtime/src/offload.rs:61`, `:155`,
    `:413`, `:550`). Tests cover FIFO cancellation, overflow, exact drop counts,
    and executing versus queued owners (`:779`, `:829`, `:898`, `:946`, `:995`,
    `:1079`, `:1193`). Fenced cleanup is not inferred from worker EOF alone.
19. **Mailbox cancellation and poisoned callback paths: fixed.** Cancellation
    notifies the owner without releasing accepted payloads, completion capacity,
    or retained budget; callbacks/destruction occur outside locks
    (`runtime/src/mailbox.rs:1`, `:20`). Assertions cover detached receipt,
    stale/duplicate completion, completion-only waiting, and reentrant/panicking
    disposal (`runtime/src/mailbox.rs:68`, `:114`, `:173`, `:332`).
20. **Local scheduler accounting and wake ownership: fixed and Miri-validated.**
    Queue polling releases capacity before destroying completed/panicking tasks,
    prevents nested poll from replacing its outer owner, and schedules backlog
    without repolling blocked tasks (`runtime/src/drivers.rs:164`). Regressions at
    `:345`, `:373`, `:397`, `:420`, `:472`, `:531`, `:649` run in Miri. Worker-local
    non-Send scheduling and caller-selected work budgets remain deliberate.
21. **Cancellation and deadline registration: fixed/configurable.** Subscriptions
    own their wake lifetime independently even with shared executor wakers; the
    live-registration ceiling is caller-selected (`runtime/src/deadline.rs:76`,
    `:102`, `:127`; assertions `:168`, `:244`, `:263`, `:283`). Deadline ordering is
    by time then registration, bounded polling handles zero budget and overflow,
    and retired wakers drop outside borrows (`runtime/src/deadline_registry.rs:35`,
    `:57`, `:84`, `:110`, `:149`). Registry admission remains caller-owned.
22. **Cooperative driver and hedge fences: fixed.** Local driving wakes its parent
    after bounded wait and reports backend errors without abandoning the owned
    operation (`runtime/src/cooperative.rs:83`; tests `:125`, `:282`). Hedge parent
    cancellation requests each child once and drains launched contenders before
    returning (`runtime/src/hedge.rs:26`, `:51`; assertions `:270`, `:363`). A caller
    dropping the enclosing future still must arrange an owner to finish its fences.
23. **Listener retry policy: configurable, compatible.** `RetryOptions` adds explicit
    interval/count bounds, rejects zero/overflow, and never scope-truncates an
    already accepted operation (`runtime/src/retry.rs:4`, `:36`; assertions `:103`,
    `:147`, `:185`). The compatibility default remains scope-bounded at 10ms rather
    than imposing a new application retry budget.
24. **Group lifecycle and fail-closed fences: fixed.** Startup rollback closes
    never-built lanes; teardown uses fresh caller policy, fences helper-independent
    resources, and never treats thread exit as proof of a fence
    (`runtime/src/group.rs:185`, `:417`, `:448`, `:480`). Tests assert SIGABRT before
    service drop for lane/helper fence errors/panics, rollback, helper deadlines,
    and cooperative independent fences (`:1029`, `:1040`, `:1167`, `:1295`, `:1631`).
    IoService forwards inner lifecycle/failure-reporting ownership
    (`src/worker.rs:720`; pending-drain regression `:2648`). Deliberate nonchange:
    a never-ready fence can block join; forced resource destruction is unsafe.
25. **Affinity/cgroup coverage: fixed.** Discover only covering mount hierarchies,
    tighten ancestor quotas, and fail closed for hidden applicable CPU/cpuset
    memberships (`runtime/src/affinity.rs:208`, `:222`). Coverage/ancestor,
    constrained-mask, and calling-thread affinity assertions are at `:381`, `:402`,
    `:427`, `:444`, `:455`. Placement ratios and device policy remain application-owned.
26. **Clock/entropy/environment boundaries: fixed and generic.** Checked advancement
    is atomic on overflow; scope poll/drop restores prior environment; cloned role
    environments share deterministic cursor state. The runtime default entropy
    domain is neutral, while applications can explicitly version a historical
    replay domain (`runtime/src/environment.rs:95`, `:156`, `:242`, `:280`; assertions
    `:314`, `:330`, `:352`, `:365`, `:432`, `:464`). Racer explicitly opts into its
    historical domain (`src/app/tests/dst.rs:37`), not a runtime app-model default.

## Simulator fidelity, boundedness, and deliberate nonchanges

27. **Filesystem model parity: fixed within the exposed subset.** Resolver behavior
    separates parent traversal from final-component following; O_EXCL/dangling
    links and O_PATH descriptors match tested Linux behavior. Short and zero-byte
    writes, append offsets, repeat locks, nonrecursive mkdir, and invalid extent
    handling are covered (`runtime/src/reactor/simulation/disk.rs:466`, `:602`,
    `:926`, `:973`, `:1014`; assertions in
    `runtime/src/reactor/simulation/review_tests.rs:13`, `:65`, `:260`, `:344` and
    `runtime/src/reactor/simulation/followup_tests.rs:23`, `:134`). This is not a
    claim of complete Linux filesystem emulation.
28. **Crash history isolation and bounded storage: fixed.** Live pending crash
    registrations replace truncated historical inference; alias opens watch the
    resolved target disk. Repeated unrelated crashes cannot invalidate another
    disk's pending owners (`runtime/src/reactor/simulation/disk.rs:70`, `:225`,
    `runtime/src/reactor/simulation/driver.rs:45`). Assertions cover resolved aliases, more than the
    previous history limit, sparse holes, durable snapshots, and atomic rejection
    (`runtime/src/reactor/simulation/review_tests.rs:119`, `:438`, `:460`,
    `runtime/src/reactor/simulation/disk_tests.rs:35`, `:612`).
29. **Socket/pipe readiness and shutdown parity: fixed against Linux.** Half-close
    directions remain distinct; pipe EOF/EPIPE, atomic small writes, empty splice,
    family validation, and renamed listener descendants are modeled
    (`runtime/src/reactor/simulation/network.rs:224`, `:432`; assertions
    `runtime/src/reactor/simulation/review_tests.rs:150`, `:191`, `:399`). Differential tests compare
    regular-file and socket shutdown readiness against host poll and real io_uring
    (`runtime/src/reactor/simulation/followup_tests.rs:204`, `:247`). A full Unix
    socket does not invent POLLERR/POLLOUT after peer SHUT_RD. Earlier handoff prose
    proposing that extension is superseded by exact Linux parity in integrated code.
30. **Fault hooks and ownership permutations: fixed/test-scoped.** Arbitrary CQE
    injection/reordering hooks are cfg(test), while normal simulated execution uses
    immutable slices for write/send and exclusive slices for read/recv
    (`runtime/src/reactor/simulation/driver.rs:113`, `:295`). Actual reactor entries
    exercise permutations, not merely standalone driver events
    (`runtime/src/reactor/simulation/followup_tests.rs:354`). Setters reject invalid
    capacities/faults atomically (`:475`); traces and sparse allocations are bounded.
31. **Unsupported abstract sockets: deliberate nonchange.** Shared SocketAddress
    exposes pathname Unix sockets, rejecting empty, embedded-NUL, and oversized
    names; simulation uses that encoder before side effects
    (`runtime/src/reactor.rs:115`, `:1625`; tests `runtime/src/reactor/tests.rs:1397`,
    `runtime/src/reactor/simulation/followup_tests.rs:537`). No new abstract-address
    API, generic executor, cross-package service redesign, hardware backend, or
    broad unsupported-feature expansion is necessary to resolve this review.

## CI, documentation, and actual validation

32. **Strict kernel CI: wired through the real environment boundary.** Repository
    `.github/workflows/ci.yaml:192` explicitly passes `RUNTIME_REQUIRE_IO_URING=1` to
    the Racer Rust systemd service, not just its calling shell. The kernel helper
    rejects absent/denied io_uring in strict mode; unexpected failures always fail
    (`runtime/src/reactor/tests.rs:1499`). Simulation and Miri are separate evidence,
    never a replacement for that lane.
33. **Strict lint/default build and pinned Miri: implemented and locally executed.**
    CI installs rustfmt and clippy with Rust 1.96.0 (`.github/workflows/ci.yaml:162`). Repository `Makefile:660` target
    `runtime-check` runs bounded all-target/all-feature clippy with `-D warnings`
    and no-default-feature compilation. A separate four-group lane pins
    nightly-2025-11-21 (`.github/workflows/ci.yaml:226`). Repository `hack/scripts/runtime-miri.py:15` lists exact test
    names, uses `--exact`, checks the nonzero expected count, and preserves default
    Miri safety checking. No test-removal, cfg(miri) exclusion, or lint suppression.
34. **Public contract documentation: prepared; concurrent deletion preserved.**
    The task branch's `runtime/README.md` describes
    generic production ownership, ReadBuffer charging, trusted exclusive stages,
    publication phase/cause, degraded leaks versus fail-closed abort, bounded data
    retry versus configurable overload retry, explicit app entropy domains,
    ABI/path constraints, and the intentional 10ms fallback. Application policy
    remains with applications; stale application-model/domain wording is removed.
    Final working-tree integration preserves the user's concurrent deletion of
    that README instead of restoring it. The replacement remains available at
    `fix/runtime-integration` commit `a283ee840`; this disposition is integrated.

### Final concurrent integration

The original checkout advanced while this review was implemented. Integration
preserves its newer allocator, topology, control-wire, identity, and fixture
changes. The control fixture return type and strict kernel check were applied at
their new `src/test_support.rs` locations rather than restoring duplicate helpers
in `src/control.rs`. Both allocator and runtime strict-kernel CI flags remain.
The newer native wrapper already forwards the lifecycle hooks and centralizes
native close/fence ownership; its implementation and tests remain, with the
review's distinct failure-reporter regression added. Earlier line citations refer
to the reviewed task tree and may shift in the concurrently integrated tree.

### Local evidence (2026-10-05)

- Existing nightly-2025-11-21 lacked cargo-miri. The actual initial
  `cargo +nightly-2025-11-21 miri --version` failed, rather than being assumed present.
- Installed the pinned toolchain/components into this worktree's
  `tmp/runtime-rustup` and `tmp/runtime-cargo`, with sysroot in
  `tmp/runtime-miri-sysroot`. Installer reported missing rustup in isolated
  CARGO_HOME after installing components; subsequent version and setup commands
  proved the installed toolchain usable: `miri 0.1.0 (53732d5e07 2025-11-20)`.
- Exact allowlist results: **channel 7/7, offload 8/8, scheduler 7/7, memory 3/3**,
  zero failures/ignores. Channel includes the full 10,000-item threaded FIFO test.
  Memory includes delayed mutable SyscallArg and immutable Vec-backed pathname
  consumers. The new test's initial missing CStr import was corrected before the
  successful run; no runtime bug or Miri accommodation was needed.
- `make runtime-check VERSION=validation` passed on cargo/clippy 1.96.0.
  Both new memory tests also passed natively. Scoped rustfmt checks and pinned
  actionlint v1.7.12 against CI passed. Tools/caches installed for this task stayed
  project-local; every command had external TERM/KILL bounds.
- Parent integration checkpoint separately reports strict runtime **240 unit +
  5 integration + 6 doctests**, full workspace compilation, application regression
  validation, and exact-replay DST recovery. Those are inherited evidence, not
  reruns claimed by this CI/docs task.

### Not executed here

GitHub dispatch/systemd service execution on a hosted runner, privileged restart
infrastructure, full-stack Kubernetes/Docker tests, RDMA hardware, throughput
campaigns, and physical power-loss/filesystem durability tests were not run by this
task. The unavailable-kernel strict failure branch was inspected, not reproduced
by disabling this host's io_uring. Miri proves only its selected executed paths;
simulation crash tests do not establish real-device durability. These limitations
are explicit validation boundaries, not silent skips or claims of completion.
