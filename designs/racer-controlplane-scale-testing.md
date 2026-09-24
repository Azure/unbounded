# Control-plane distinct-client TLS scale testing

## Lanes and capacity qualification

`cmd/racer-controlplane/tests/scale/distinct.rs` owns both lanes:

- `subscription::distinct_scale::distinct_tls_representative` runs by default:
  24 distinct clients, three universes, 512 slots, four runtime workers, a
  12-second scenario deadline and independent 20-second process watchdog.
- `subscription::distinct_scale::distinct_tls_fanout` is an ignored release test.
  It defaults to 10,000 clients, one universe, full slot geometry and sixteen
  runtime workers. Selecting it requires `RACER_SCALE_CAPACITY_QUALIFIED=1`;
  missing qualification or a debug build fails rather than returning success.
  Qualification is an operator attestation, not automatic proof of capacity.

Reserve CPU capacity for both endpoints and snapshot builders; avoid concurrent
builds and resource-intensive suites. Record CPU affinity, cgroup quota/throttling,
memory limits and pressure, and host load. The historical host exposed 48 logical
CPUs / 24 cores. Historical process peaks were about 1.3 GiB, excluding kernel
socket memory; allocate headroom beyond that observation. At 10,000 clients,
provide more than 20,000 available file descriptors plus reconnect headroom, and
enough ephemeral ports for both waves and TIME_WAIT. These are capacity inputs,
not newly measured minimum requirements or guarantees of passing.

Use separate processes for geometries to keep peak RSS meaningful. Select the
exact test rather than every ignored test. Keep an external deadline. Serialize
builds and execution through the verification owner, especially with limited disk.

## Stateless fixture contract

The fixture uses versioned signed process claims (`SignedClaims` in the Rust
security API), role-specific EKUs, and the current Pod namespace/name/UID and boot
identity. It loads a version-5 CA image with no participant shards and checks that
fleet certificate creation leaves its bounded metadata and issuer expiry watermark
unchanged. Leaf expiry is bounded by the preallocated watermark. This is synthetic
credential setup, not an enrollment persistence or rotation throughput measurement
(`cmd/racer-controlplane/tests/scale/distinct.rs:342-406,719-791`).

Topology uses universe/Node-UID highest-random-weight placement, with statistical
balance, adjacent repeats, and valid zero-slot idle members. Per-universe
`Publication::publish` calls use distinct synthetic reserved revisions and a
range-sized gap before the successor publication. The clients check the exact
expected revisions and snapshot/envelope agreement; they do not assume a fresh
process starts at revision 1 or that updates are consecutive. No old RecordStore,
store gate, participant ledger, or historical topology payload is part of this
router fixture. Revision checkpoint CAS and leadership authority remain runtime
test responsibilities (`cmd/racer-controlplane/src/publication.rs:50-68`,
`cmd/racer-controlplane/tests/scale/distinct.rs:72-79,809-842,1003-1010`).

Rotation in the production security manager is gated by confirmed publication
overlap plus skew, then the old issuer's maximum issued expiry plus skew. Fleet
acknowledgments do not gate progress (`cmd/racer-controlplane/src/security/state.rs:522-583`).
The scale fixture holds one root throughout; expiry/rotation scenarios remain in
the security, service, and native campaign suites. Historical measurements below
predate the stateless HRW and signed-claims cutover and are not current capacity
evidence.

## Structural diagnosis and evidence

The original `tmp/racer-test-audit/controlplane-scale-release.log:7-18` records a
35-second response deadline during publish/reconnect, 4,120 coordinator events,
then secondary `SendError` panics. `REPORT.md:33,85-88` also records a debug failure
and shared-host contention. This is a bounded failure, not evidence of a deadlock.
Contention's exact contribution is unproven.

The harness deliberately overlaps work:

1. Early clients start unchanged long polls while initial delivery is still
   draining. Publication follows the initial fleet barrier and hold.
2. The 35-second read deadline starts before the response status line, so measured
   publication latency includes existing held-request time. The field called
   `first_byte` actually waits for the complete HTTP status line.
3. Each successor-publication recipient immediately reconnects while others await
   that publication.
   Previously both event types shared a completion count: 4,120 events does not
   mean 4,120 updated nodes.
4. Production permits four snapshot builders and sixteen responses
   (`cmd/racer-controlplane/src/subscription.rs:85-87`). Response permits precede
   snapshot work (`:526-536`) and remain in the body (`:575-586`). The 28-second
   unchanged-poll hold does not cap changed-snapshot admission/build time.
5. Both TLS endpoints, client protobuf decoding/digest verification, and server
   snapshot work share one process and CPU budget. Large fleet bytes and
   overlapping reconnects make this a capacity test rather than a protocol smoke
   test.

Deadlines and workload overlap are retained. Separating reconnects or resetting
response deadlines at publication would change what is measured. Historical
completed runs in the source header predate tightened deadlines and do not prove
current 10,000-client first-byte compliance. Reduced runs do not supersede the
audit failure.

## Diagnostics and cancellation

`SCALE_GEOMETRY` records dimensions, process limits and ephemeral port range.
`SCALE_PROGRESS` appears at stage changes, every five seconds on a separate thread,
at client deadlines, and at failure/exit. It records:

- Separate initial deliveries, successor deliveries and reconnect request writes.
  The latter is not server registration; the waiter barrier checks that.
- Client counts in admission, TCP, TLS, write, status-line read, headers, body,
  validation and finished states, split by initial/publication/reconnect phase;
  total validated payload bytes. Deadline failures identify the client index,
  phase and operation.
- Server waiters, response occupancy, builder permits and cache usage.
- Process CPU ticks, RSS/peak RSS, threads, open descriptors, affinity and available
  parallelism; host load, available memory, CPU/memory pressure; visible cgroup-root
  CPU/memory limits and throttling statistics.

Procfs/cgroup samples are best effort, with null for unavailable values. The
visible cgroup root can differ from the process's nested cgroup; the log includes
`/proc/self/cgroup` to expose that limitation. Sampling survives executor
starvation. Server lock snapshots are nonblocking and report null when busy or
poisoned, so diagnostics cannot block cancellation on those locks. The independent
hard watchdog remains authoritative. `SCALE_RESULT` appears only after successful
assertions and teardown.

On worker loss or stage expiry, the coordinator reports state, aborts and reaps
siblings within two seconds, then fails. Closed event receivers cause senders to
return rather than panic. Dropping the finish sender cancels clients in any phase.
The listener owns server connection tasks in a JoinSet, so listener cancellation
also aborts its children. Cleanup does not turn failure into a skip/pass.

## Bounded verification and reproduction

Run from the repository root. Compile separately when the verification owner has
disk capacity, using the selected existing `CARGO_TARGET_DIR`.

```sh
export CARGO_INCREMENTAL=0

# Schedule compilation separately from execution.
cargo test --locked --manifest-path cmd/racer-controlplane/Cargo.toml --lib --no-run

# Coordinator cleanup and closed-receiver regressions, without TLS load.
timeout --kill-after=5s 20s cargo test --locked --manifest-path cmd/racer-controlplane/Cargo.toml --lib subscription::distinct_scale::scale_ -- --nocapture --test-threads=1

# Existing representative scenario: 24 clients / three universes / 512 slots.
timeout --kill-after=5s 30s cargo test --locked --manifest-path cmd/racer-controlplane/Cargo.toml --lib subscription::distinct_scale::distinct_tls_representative -- --exact --nocapture --test-threads=1

cargo test --locked --release --manifest-path cmd/racer-controlplane/Cargo.toml --lib --no-run

# Smaller reproduction: 96 clients / three universes / full slot geometry.
# Same 28s hold and 35s response deadline; not 10k capacity coverage.
RACER_SCALE_CAPACITY_QUALIFIED=1 RACER_SCALE_NODES=96 RACER_SCALE_UNIVERSES=3 timeout --kill-after=5s 160s cargo test --locked --release --manifest-path cmd/racer-controlplane/Cargo.toml --lib subscription::distinct_scale::distinct_tls_fanout -- --exact --ignored --nocapture --test-threads=1

# Dedicated lane after reserving capacity.
RACER_SCALE_CAPACITY_QUALIFIED=1 RACER_SCALE_NODES=10000 RACER_SCALE_UNIVERSES=1 timeout --kill-after=5s 160s cargo test --locked --release --manifest-path cmd/racer-controlplane/Cargo.toml --lib subscription::distinct_scale::distinct_tls_fanout -- --exact --ignored --nocapture --test-threads=1
```

Capture complete stdout/stderr and exit status. Timeout, panic, failed qualification
or unavailable prerequisites are not passing scale coverage. Do not retry until
green or enlarge deadlines to conceal an unresolved bottleneck.
