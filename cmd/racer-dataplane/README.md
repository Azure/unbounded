# Racer dataplane

One Rust package containing the node dataplane. `app.rs` composes worker-local
services; the binary enters through configuration and the application lifecycle.
The Go control plane is outside this package.

[Control API](CONTROL_API.md) defines the controller/dataplane contract, Kubernetes
membership inputs, enrollment, and projected credential rotation.

```sh
make racer-dataplane-test
# Optional test name filter and libtest options:
make racer-dataplane-test RACER_TEST_ARGS='topology:: --test-threads=1'
```

The standalone target runs library and binary unit tests with all features, using
the locked dependencies and `bin/racer-cargo` build cache. Override `RACER_CARGO`
or `RACER_CARGO_TARGET_DIR` as needed. It includes real Linux I/O tests; existing
hardware-only tests remain ignored. The test profile optimizes the dataplane at
level 1 and dependencies at level 3, retaining debug assertions, overflow checks,
and debug information. The first run compiles this profile; later runs reuse it.

For the complete Rust suite, including integration tests and doctests:

```sh
cargo fmt --manifest-path cmd/racer-dataplane/Cargo.toml --check
cargo check --locked --manifest-path cmd/racer-dataplane/Cargo.toml --target-dir bin/racer-cargo --all-targets --all-features
cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml --target-dir bin/racer-cargo --all-features
```

`make racer-test` also runs the Go server checks before the complete Rust suite.

## Generated datapath DST

Run both HTTP and connected-native generated-traffic oracles with the normal `dst`
test filter. The wrapper verifies a cgroup-v2 limit of at most 16 GiB and zero
swap for the entire build/test process tree before executing Cargo:

```sh
bash hack/scripts/memory-safe-run.sh -- make racer-dataplane-dst RACER_TEST_ARGS=--nocapture
# Replay a seed or an explicit comma-separated corpus:
RACER_DST_SEEDS=42 RACER_DST_STEPS=41 \
  bash hack/scripts/memory-safe-run.sh -- make racer-dataplane-dst RACER_TEST_ARGS=--nocapture
# Select only the connected native graph:
RACER_DST_SEEDS=1,7,42 \
  bash hack/scripts/memory-safe-run.sh -- make racer-dataplane-dst \
  RACER_DST_FILTER=dst_generated_native RACER_TEST_ARGS=--nocapture
```

`src/app_dst_tests.rs` defaults to seeds `1,7,42` and one full weighted action cycle
per seed (currently 41 actions). `RACER_DST_STEPS` accepts 1 through 512; the seed
list accepts at most 1024 unsigned decimal 64-bit values. Failures print seed,
action index, action name, cluster size, and observed coverage. Replaying requires
the same source revision: the seeded scheduler consumes random choices as the
production graph progresses.
Unknown `RACER_DST_*` parameters fail immediately. The supported parameters are
`RACER_DST_SEEDS`, `RACER_DST_STEPS`, and the Makefile's `RACER_DST_FILTER`.
The generator samples without replacement from `WEIGHTED_ACTIONS`, a list of named
enum actions whose repeated entries supply their weights. Its length determines
the refill cycle, default step count, and full-cycle coverage threshold. Four slots
select ordinary traffic; actions gated by node count or native mode also fall back
to ordinary traffic without resampling. Order, node/worker counts, victims, ranges,
fault subtypes, and polling quanta are seeded; there are no scenario scripts.
Runs shorter than a full cycle are useful for failure minimization but omit the
full-cycle coverage assertions. A custom corpus can fail its coverage obligations
even if its individual requests are correct.

Each run chooses an initial 2-32-node cluster and generates legal concurrent
HEAD/bootstrap/pinned ranges, origin version mutations, retained old pins, node
addition/removal, checkpoint restart, in-flight process loss without checkpoint,
power loss during an accepted delayed disk write, established-link partitions,
peer listener outage/heal, client disconnect, memory eviction, short I/O,
connection rejection, delayed writes, and failed dirty writes. Membership can
also change while a client request is in progress. Scheduling uses bounded random
poll quanta and a virtual clock. Process incarnations have distinct deterministic
entropy streams. A 16 MiB-plus-tail object exercises page boundaries alongside
empty and short objects.
Nodes have one or two worker pairs and use production cross-worker mailboxes.
Additional generated actions retire page/credential keys, remove a cache and
recreate its name under a new UID, jump wall time, reject credentials with 401/403,
malform origin ETags and framing, truncate bodies, malform client requests, replay
signed peer handshakes, corrupt signatures, and corrupt stored record bytes.
Native actions cover Bind/Write/Invalidate crossed with synchronous rejection,
delayed completion, and failed completion.

Workers use `WorkerApplication::assemble` and production startup/recovery, client
listeners, dispatch, candidate selection, peer challenge/signature/replay checks,
credential encryption, origin HTTP, page crypto queues, encrypted storage,
checkpointing, and shutdown. Client and origin fixtures exchange raw bytes over
hostless `Descriptor::Sim` sockets. Canonical cache names differ per simulated
node while sharing a cache UID, isolating UDS paths inside the shared simulated OS.
Accepted certificates, key bundles, and publications are fixture inputs; this
harness does not enroll through a simulated controller.

The independent oracle retains immutable version bytes and checks every delivered
byte, ETag, range, total size, framing, and status. Healthy recovery must read every
current object successfully. Mandatory cache-only reads prove an encrypted disk
read with origin unavailable, followed by a memory hit without disk or origin I/O.
The native test requires successful native activation and completed DMA writes,
so HTTP fallback alone cannot pass it. Coverage reports relay-active polling turns,
successful responses, bytes, persisted records, OS completions, and consumed fault
rules. Every injected OS rule must be observed. Per-turn admission/queue/descriptor
bounds and final zero quota, descriptor, and native-resource usage are asserted.
Full cycles require both peer-security rejection modes, all five origin fault
types, all nine native fault combinations, secondary-worker data work, retirement,
recreation, and both crash actions. Corpus-wide obligations additionally require
actual relay activity, blocked sends on established streams, and reads after disk
corruption. Native rules must be consumed, not merely scheduled.

The crash model distinguishes volatile data and directory bindings from their
fsynced durable images. Provisioning is explicitly synced. Process loss first
rolls back the victim disk subtree, then discards queued/active worker tasks and
drops the application without invoking application drain/shutdown. Test-only
dispatch/driver hooks model process address-space loss where production Drop
deliberately retains undrained tasks. Only kernel cancellation fences are polled
after this cut; no acquisition, writer, or checkpoint producer is resumed.
Native destructors retain their normal QP/memory fencing. This is a deterministic
software crash model, not a hardware DMA or filesystem-provider guarantee.

Production checkpoint publication currently promises an atomic logical cut, not
fsync durability (`src/store/checkpoint.rs:3-4,170-210`). A power loss may therefore
discard an unsynced checkpoint; recovery must still serve correct bytes. The
durable model does not invent a persistence guarantee absent from production.
Control enrollment/TLS remain outside this harness: accepted control inputs are
injected, while retirement and cache-publication transitions run through the real
application lifecycle. Signed corruption/replay probes exercise real peer HTTP
ingress and security, rather than bypassing verification with fabricated tokens.

Constructors connect dependencies without opening files, accepting requests, or
spawning threads. Activation is explicit through the application lifecycle. See
[configuration](CONFIGURATION.md) for environment variables, resource limits, and
startup requirements. Linux io_uring and direct-I/O-capable storage are required.
See [deployment](DEPLOYMENT.md) for release artifacts, mounts, permissions, and
trusted local native-port configuration. The [Go controller](../racer-controller/README.md)
implements enrollment and publication serving; follow its installation procedure
to initialize durable state and provision serving TLS and bootstrap trust.

The optional `rdma` feature loads the separately built native libibverbs adapter.
See [native adapter](native/README.md) for installation and provider tests. Missing
devices or incompatible capabilities select HTTP. Native DMA and teardown behavior
must be verified on the deployment's provider; no usable type-2B device was available
in the implementation test environment.

## Implementation contracts

- `model` defines identity and immutable values; request context is a separate type.
- `runtime` owns explicit pinned workers, bounded handoffs, admission, and completion
  lifetimes. Worker-local futures and services do not require `Send`. Actual
  cross-worker commands must transfer resource ownership explicitly.
  Each logical worker has two separate threads: I/O and page crypto. The default
  cap is eight total userspace threads (up to four pairs), sized against allowed
  CPUs and cgroup quotas. Prefer separate NIC-local cores; on one allowed CPU both
  threads share it. Control and diagnostics fit within that budget. This explicitly
  supersedes `tmp/design.md`'s combined-role single-core description.
  `WorkerPair` represents both roles and local CPU/core/NIC/NUMA constraints;
  pure pair sizing floors odd caps/cores and effective quotas to complete pairs.
  Local discovery/planning validates accepted rail mappings without changing the
  cluster rail contract or deterministic page-to-rail selection.
  The `Sync` factory builds separate I/O and crypto services on their pinned
  threads. Group APIs specify start, concurrent drain, shutdown, and join,
   including partial-start rollback.
  `runtime::crypto` defines owned Send jobs/completions, generation/sequence IDs,
  key leases, original deadlines/cancellation, and a non-cloneable pair-bound
  permit reserving both job and completion space before enqueue. Rejection returns
  ownership. Completion consumption releases capacity, including for canceled or
  abandoned waiters. Wakeable polling and bounded quanta must permit single-CPU
   progress. Completion capacity is reserved before a crypto job is accepted.
  I/O-local `PageCrypto` selects a key lease and submits owned inputs via its
  runtime's `CryptoClient`; the paired `PageCryptoEngine` cannot access the Rc
  service graph. Output reservations move to jobs, rather than borrowing a fill's
  admission state. I/O alone owns flights, storage, metadata, and admission.
- `read` coordinates one per-page flight per worker. `client` and `peer` use that
  same coordinator; transport does not create another acquisition pipeline.
  Explicit version pins may use expired metadata; fresh/unpinned admission requires
  revalidation. Existing reads never switch ETags.
  `VersionMetadata` retains immutable version/total-length facts separately from
  the volatile `CurrentVersion` freshness pointer. Only page-zero revalidation
  publishes that pointer; pins and page hits publish immutable facts only.
  `MetadataService` shares the worker's live `Index`, `Fill`, and bounded worker
  directory. Bootstrap returns either metadata-only (empty) or a full `PageResult`.
  Missing pinned metadata can query retained descriptors across local page shards
  before a conditional page-zero probe. Fresh reads still require revalidation.
  `read::fill::PageResult` re-exports the cloneable memory-layer result containing
  metadata, verified plaintext, and original ciphertext for flight sharing.
  `Flights::join`/`AcquisitionWaiter::wait` elect one leader or return the complete
  result/failure. `join_copy` exposes only miss/wait/complete, with no ability to
  start acquisition or fund a retry. Publication validates the exact page and the
  bundle's metadata/plaintext/ciphertext association; page hits cannot refresh TTL.
  Acquisition waiters borrow non-cloneable request context and an exclusive
  remaining `AcquisitionBudget`. Neither headers nor credentials enter flight
  identity or completed entries. Different contexts share the same page flight.
  Membership and candidate authority belong to each elected acquisition; retry
  must resolve authority again under the selected caller's accepted membership.
  Credential-specific origin rejection fails only the supplying caller, then
  elects a remaining live acquisition caller under its original deadline,
  cancellation, attempt credits, and remaining link budget. It is never cached as
  page/version absence. Other terminal failures wake the current cohort.
  Cancellation/detach affects one waiter. Abandoned leadership enters `Draining`;
  accepted operations retain their owned I/O/crypto resources independently of
  futures. Only actual completions permit `RetryPending`, failure for copy-only
  survivors, or removal after the last waiter. Table identity, entry incarnation,
  and acquisition generation fence stale publication/failure/drain callbacks.
  The worker lifecycle retains the same flight table as Fill and exposes bounded
  polling/drain hooks for cleanup after request futures disappear.
   Registration, election, wakeups, Drop cleanup, and completion-driven drain are
   exercised by behavioral tests in `read`, including resource-pressure cases.
- Per-cache UDS paths are exactly `/run/racer/<cache name>/client/socket` and
  `/run/racer/<cache name>/origin/socket`. Racer owns the client listener; the
  application adapter owns the origin listener. Separate endpoint directories let
  pods mount only the UDS directory authorized by a future admission controller.
  Cache names must be safe single path components, and complete paths must fit the
  platform UDS limit. Cache reconciliation must reject noncanonical supplied paths.
- `security` uses separate page and ephemeral-credential encryption domains.
  Pass the exact cache key, `Racer-Metadata`, and optional Authorization to origin.
  Metadata stays signed plaintext across peers; Authorization is encrypted across
  peers and decrypted for the local origin socket. Credentials are opaque upstream
  fetch context, not Racer authorization, and never enter caches, disk, or logs.
  `CredentialCrypto::seal` synchronously borrows the request's `OriginContext` and
  original `RequestScope`, returning an independent owned `PeerOriginContext` per
  attempt. Retry/fanout reseals without cloning raw secrets. Each encryption must
  generate a fresh cryptographic nonce and bind the credential domain, key ID,
  request/attempt, object, and exact opaque metadata in canonical AAD. The facade
  shares worker admission; each envelope owns its request-context reservation and
   original cancellation/deadline through transport completion. Allocation is
   admitted before sealing, and secret-bearing buffers are zeroized on release.
  `SignedRequest` and `SignedResponse` own the logical message plus its original
  signed head and ordered forwarding heads. `Forwarding` signs/verifies complete
  envelopes and appends request/response hops without replacing the original.
  Its opaque `VerifiedRequest` is required by local service and relay; its
  `VerifiedResponse` is returned to logical requesters. Both retain the full chain
  and expose signed fields read-only. Local `PeerResponse` results are unsigned.
  `RequestBinding`, minted by request signing/verification, retains the exact
  original request head/signature and is required for response signing/verification.
  `PeerTransport` exchanges owned signed envelopes, including through relays.
   Canonical field agreement, original/hop signatures, replay/identity checks,
   response correlation, and route consumption are checked before verified types
   can be constructed. The versioned profile is documented in
   `designs/racer-peer-security.md` at the repository root.
- `store` accepts only encrypted pages. Slabs require `O_DIRECT` with discovered
  address/offset/length alignment. Aligned padded record lengths differ from
  authenticated ciphertext lengths. Padding is initialized and never delivered.
  Memory entries and dirty `CiphertextCopy` bundles retain metadata with each page;
  completed index entries and record headers retain `VersionMetadata`. Total object
  length is never inferred from a page length. Per-page descriptors survive bounded
  page-zero catalog eviction, so old cached pages remain usable by explicit pins.
  HEAD-only and empty-object descriptors have a bounded standalone catalog and
  checkpoint representation without allocating an encrypted page. Checkpoints
  include descriptors atomically with completed page mappings, omit dirty pages and
  current-version pointers, and reject conflicting lengths for one version.
  Recovery is connected to the same live index/segments and restores one consistent
  cut before admission; recovered descriptors carry no freshness claim. Request
  context, Authorization, and opaque adapter metadata are absent from these types.
- `memory`, `runtime`, and `rdma` retain leases until all applicable kernel/NIC
  fences finish, including cancellation. Disk persistence is bounded and async;
  failed dirty writes may be discarded.
  `IoBuffer` is sealed and requires `'static`, exclusively owned, address-stable
  backing plus its quota reservation: fixed boxed staging bytes or an aligned
  allocation, never borrowed slices or inline arrays. Before submitting I/O, the
  reactor must put buffers, owned FD references, and connection/segment leases in
  its in-flight table, independently of the waiting future. Future drop abandons
  the result; release/reuse requires original and cancellation completion fences,
  including during shutdown. `Completion<B, L>` returns the buffer and lease after
  fencing. HTTP heads transfer/return connection ownership and require owned byte
  staging; bodies consume owned buffers and return them with the connection lease.
  Shared/borrowed body slices require explicit bounded staging. Slab operations
   consume a segment lease alongside the aligned buffer. Cancellation requests do
   not release those resources before the corresponding completion fences.
- `control` publishes immutable accepted snapshots and coherent key bundles.
  Bootstrap returns the local node certificate directly; common Secret bundles
  contain only peer trust and cache keys. Token-authenticated bootstrap also renews
  certificates; subsequent snapshot polls use mTLS, not control HTTP signatures.
  One selected worker owns control enrollment; worker handles share node-wide
  snapshot, key-epoch, and replay roots. Bounded dispatch routes work to page owners.
- Live key retirement and cache removal use a node-wide quiescent cut. Admission
  pauses while accepted read, write, crypto, kernel, and native operations finish;
  recoverable checkpoints are invalidated before omitted keys are destroyed.
  The control worker alone restores client, peer, and diagnostic listeners.
  This conservative path temporarily interrupts unrelated caches and diagnostics.
- `test_support` is test-only. Inline test sections identify the owning contracts;
  implement behavioral tests with each feature, rather than tests of placeholders.

## Protocol and verification references

- `CLIENT_ORIGIN_API.md`: approved SDK and origin-adapter wire contract.
- `src/topology/ALGORITHM_V1.md`: deterministic placement and routing profile.
- `designs/racer-peer-security.md`: authenticated peer and encryption profile.
- `designs/racer-store-protocol.md`: record/checkpoint formats and recovery rules.
- `designs/racer-sdk-conformance.md`: independent wire and Go SDK checks.

The `designs/` paths above are relative to the repository root. Component
`INTEGRATION.md` files describe ownership and lifecycle APIs. Passing component
tests does not establish deployment interoperability or hardware DMA guarantees;
run the combined suite and applicable native-provider tests for a release.
`designs/racer-production-validation.md` records real multi-page streaming,
zero-TTL, cancellation, disk-hit, and memory-pressure validation.

## Hotpath bottleneck benchmark

This ignored, release-only test reuses the production-component fixture with the
host's production-default number and placement of pinned I/O/crypto worker pairs,
io_uring, HTTP over Unix socket pairs, and encrypted O_DIRECT slabs. It parses
`Config` defaults without ambient overrides and calls `AffinityPlan::from_topology`
on discovered CPU affinity/cpuset, physical cores, quota, and NIC locality.
`DEFAULT_MAX_THREADS=8` caps this at four pairs, not half the host logical CPUs
(`src/config.rs:26`, `src/runtime/affinity.rs:91-112,125-191`). The default non-RDMA
node budgets satisfy the builder's per-worker progress floors at four pairs
(`src/app.rs:179-197,229-268`). The owned test driver needs one additional thread
slot (`src/runtime/worker.rs:194-197`); it preserves the selected pairs and CPUs.
Client and fixture-origin threads are additional benchmark machinery.

It runs 1, 32, and 128 concurrent readers sequentially:

- `memory`: repeated reads of one warmed, distinct 16 MiB page per owner.
- `disk`: 128 distinct persisted pages per sweep, evicting plaintext between
  sweeps. Origin is offline, so a memory/origin fallback cannot mask disk work.
- `fill`: 128 distinct cold pages from the streaming fixture origin, including
  encryption and asynchronous persistence.

Memory and disk each run four sweeps of 128 total requests; fill runs one. Keys
are selected with the production `WorkerMap` hash to balance page-zero/metadata
owners. Every service installs its own worker ID in one shared, bounded
`WorkerDirectory`, used by real dispatch, metadata, fill, and range streams.
Client sockets enter round-robin workers; requests rotate to the next owner to
exercise cross-worker coordination (with four pairs, all c32/c128 requests enter
a different worker than their owner). c1 remains one sequential client and visits
all owners. Warmup asserts the per-owner persisted record counts. Each client
issues one request at a time. Memory now spans multiple keys, and aggregate
admission grows with the selected pair count: results are not an apples-to-apples
speedup comparison with the old single-key, single-pair benchmark.

This is a bounded closed-loop component probe, not a full-node capacity estimate.
Socket creation replaces listener acceptance; peer transports are outside this
fixture. The disk case measures software overhead on RAM-backed ext4, not
physical-storage performance.
Fill timings also include the fixture origin's byte generation and streaming.

```sh
sudo -v
CARGO_BUILD_JOBS=2 cargo test --locked --release --manifest-path cmd/racer-dataplane/Cargo.toml \
  --test production_dataplane hotpath::hotpath -- \
  --ignored --exact --nocapture --test-threads=1
```

Requires Linux with io_uring, loadable `brd`, ext4 tools, and working `sudo -n`.
The test owns a 4 GiB RAM block device, formats/mounts it under the crate's
`target/`, and unmounts/unloads it on success or panic. It refuses an already
loaded `brd` module. Abrupt process termination cannot run Rust teardown.
`RACER_BENCH_FAIL_SETUP=1` deliberately panics after mounting to check cleanup.
Before allocating the device and before every case, it requires available host
and visible ancestor cgroup-v2 memory of at least the larger of 16 GiB and twice
the conservative memory envelope. Unsupported/unresolvable cgroup layouts fail
closed. Per-worker **fixture limits**, not partitioned production defaults, remain
256 MiB plaintext, 512 MiB ciphertext, 128 MiB dirty, 16 MiB request context,
16 delivery pipes, and 256 queue entries/client connections. Production defaults
instead divide node-wide 256/256/128/16 MiB and 16 pipes among workers
(`src/config.rs:173-185`, `src/app.rs:234-249`). No limits are raised on overload.

At four pairs the envelope is 12,416 MiB (plus 64 bytes): the full 4,096 MiB device,
3,712 MiB admitted dimensions (including a conservative unused registered-memory
allowance), 2,048 MiB aggregate worker auxiliary allowance (512 MiB each),
2,048 MiB allocator allowance, and 512 MiB clients/kernel buffers.
Thus preflight requires just over 24,832 MiB
available, leaving an equal amount of headroom. Auxiliary allowances cover origin
streaming, crypto scratch, stacks, indexes, queues, rings, sockets, and pipes;
these are conservative planning allowances, not an OS-enforced RSS cap. Clients
use two 64 KiB buffers each and origin streams 64 KiB chunks rather than retaining
payloads. Plaintext/resident cache and ciphertext/slab staging are admission-bound.

Disk preload is always 128 pages total (2 GiB payload), not per worker. Fill adds
128 pages beyond one warm page per owner. Slab capacity is sized for three encrypted
records per 64 MiB segment plus a free segment, bounded to 3 GiB aggregate
(768 MiB per worker at four pairs), even if sparse files become fully allocated.
All workers share one 4 GiB brd/ext4 filesystem with unique scratch/slab/UDS paths.
Cases run sequentially and delete their slabs after joined shutdown. Standard
output includes worker count, CPU layout, memory totals, and per-worker counters.

Read the failure counts alongside throughput and latency. Only complete 206
bodies count as successful requests/bytes; HTTP errors and truncated bodies are
reported separately, along with server-side error categories. Percentiles cover
successful requests only. Overload is an observation, not a benchmark assertion
failure. Warmup validates every byte; timed reads validate headers, lengths, and
sampled bytes. `read_s` includes all attempts; `drained_s` additionally waits for
the background writer. Draining does not imply fsync durability or that every
response was persisted: inspect `fresh_pages_persisted` and `dirty_discards`.
Cache-only cases assert that origin call counts stay unchanged.

For CPU profiles, build with `CARGO_PROFILE_RELEASE_DEBUG=1` added to the command
above. While it runs, attach from another terminal to the test process:

```sh
sudo perf record -F 99 -g --call-graph dwarf -p <test-process-pid> -- sleep 10
sudo perf report
```

Correlate the capture with the printed case/sweep. Separate I/O and crypto worker
stacks from client/origin threads and untimed warmup before attributing a
bottleneck. Start with admission failures, incomplete responses, and persistence
shortfalls; a higher successful-read rate alone can hide rejected work.
