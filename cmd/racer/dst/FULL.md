# Full fidelity: control plane

Full fidelity runs real application code on the simulated driver
(`Driver::Sim`) inside one racer-dst `World`, stepped on one thread. This
page covers the control plane: real `racer_authority::Authority`
instances against the in-memory Kubernetes API that the crate's own
simulated tests use. The code is `src/authority.rs`; scenarios are in
`src/scenarios.rs` (`authority-*`) and run from `seeds/ci.txt` by
`make racer-dst`.

## World

| Node | Role | What it is |
|---|---|---|
| 0 | `kube-api` (`KubeRole`) | `simulation::FakeKube`: the fake API server on its own executor, seeded with the installation objects (state Secret, 16 seat Leases, primary Lease, DaemonSet, ServiceAccount, Nodes and Pods) |
| `i` = 1..=N | `authority` (`AuthorityRole`) | `Authority::simulated(pod_config(i), os, clock, seed)` for Pod `p{i}` on Node `n{i}` |

- One simulated OS (`Scenario::shared_network`): every node shares the
  network and the disk. Sockets are labeled with the node's endpoint
  (`SimOs::enter_endpoint`) while a role steps: `10.0.0.1:443` for the API
  server, `10.0.1.{i}:8443` for Pod `i`. Partitions cut these labels.
- One `VirtualClock` per node, all anchored at the same world origin. Wall
  time starts at a fixed instant (Unix 1_800_000_000), not at host time;
  control tokens expire at Unix 4_000_000_000 so they stay valid. Lease
  timestamps and certificate validity use the virtual wall clock
  (`rt::wall_now`, `rt::time_provider`).
- A role step is one `Authority::turn` (or one API executor turn). The
  scheduler wakes a role when its last `Next` is due, or when `next()`
  reports something earlier than its last step did (a completion arrived or
  a nearer timer appeared), so I/O completions caused by other roles are
  never missed.
- Each authority's seed (SplitMix64: key ids, nonces, jitter, claim delays)
  and the API server's executor seed are drawn from the world `Choices`.
- The world board (`world::Board`) holds a `ControlPlane`: the API view and
  every live authority, plus the invariant observers' state, per run.

## Fault model

All faults are drawn from the world `Choices` stream, so they shrink and
replay with it. Node 0 is never crashed, slowed or skewed
(`FaultMix::first_node = 1`).

| Fault | Effect |
|---|---|
| `Crash { node }` | The role is dropped: the `Authority` and every task, socket and listener it owns go away without releasing Leases (a killed process). |
| `Restart { node }` | A new `Authority` with the same identity (Pod name, UID, Node, addresses, token) and a fresh seed. |
| `Partition { a, b }` / `Heal` | `SimOs::partition` between the two endpoints: Pod and API server, or Pod and Pod (peer image copies). A blackhole: streams stall until healed or their own deadlines fire. |
| `ClockRate { node, ppm }` | The authority's monotonic and wall clocks run up to +-10% fast or slow from then on (`max_rate_ppm = 100_000`). |
| API write conflict | `ApiFaults::conflict_ppm`: on an API server step, the next write (PUT, POST, PATCH, DELETE) answers 409 without applying (a spurious resourceVersion conflict). |
| Watch drop | `ApiFaults::watch_drop_ppm`: on an API server step, every open watch stream ends cleanly; clients watch again. |

The shared disk is not crashed on `Crash` (it is every node's disk); the
authority keeps no durable local state beyond its token file.

## Invariants

All are `Class::Safety` and run after every step.

| Name | Holds |
|---|---|
| `single-primary` | At most one authority returns `primary_term()` at any world instant, and each term `(lease UID, transitions)` is led by one node only. |
| `term-monotonic` | Per node (observer), terms of the same Lease UID led over time never go back. |
| `committed-monotonic` | The committed record in the racer-state Secret never regresses: sequence and keyring generation never decrease, term transitions never decrease for one primary Lease. |
| `seat-serves-only-while-fresh` | A ready (serving) authority on seat `i` is the holder of seat Lease `i` in the API, and no two ready authorities serve one seat. |
| `authority-has-wake` | Every live authority reports `Ready` or `WaitUntil`: it always has a timer (renewals), never `Idle` or `Done`. With the world's built-in `pending-without-wake`, no authority holds work with no wake source. |
| `eventual-primary` | Bounded liveness: once every node is up, nothing is partitioned and no fault happened for `PRIMARY_BOUND` (120 s), a primary was seen within the last `PRIMARY_BOUND`. |

New terms and commits are also folded into the world trace, so a divergence
in what the control plane decided fails the determinism double-run even when
the schedule matches.

## Determinism

The authority and the fake API use `rcgen`/`ring` for keys and TLS, which
draw from the host OS. Key bytes, certificate signatures and handshake
randomness therefore differ between runs, and ECDSA P-256 signatures vary in
DER length (70 to 72 bytes). What is kept deterministic:

- Behavior never branches on key bytes, and every authority seed comes from
  `Choices`.
- Message sizes must not move time. Authority scenarios set
  `Scenario::charge_bytes(false)`: OS operations are charged by kind only, so
  a one-byte longer signature does not shift a completion by a nanosecond.
  The simulated network moves whole buffers (chunking is fixed, not drawn),
  so sizes do not change how many steps a transfer takes either.
- The trace hashes the schedule (role, wake time) and observations (terms,
  committed sequence and generation), never key or certificate bytes.

If an authority scenario ever reports `nondeterministic`, first suspect a new
size-dependent path (a read buffer boundary crossed by a variable-length
record) or a new `HashMap` iteration on a behavior path.

## Bugs found

- `authority-crash-restart` seed 1: a dropped `Authority` leaked every task,
  listener and connection (futures held the runtime `Rc` across awaits, a
  cycle), so a restarted Pod failed to bind its addresses. Fixed in
  `authority/src/rt.rs`; regression test
  `dropped_authority_restarts_with_the_same_identity`.

# Full fidelity: application nodes

`dst/src/app.rs` runs real Racer application nodes. Each `app-<name>` scenario
is one world role that runs `racer::app::dst::run(name, seed, 0)`, the
whole-application harness in `src/app/dst.rs`. The
harness is compiled only for tests and for the root crate's `dst` feature,
which `racer-dst` enables; it is `#[doc(hidden)]` and is not a public API.

## World

- Every node is assembled through `WorkerApplication::assemble` and started
  through the production lifecycle on `racer_io::Env::sim` (`Driver::Sim`,
  `SimOs`, `SimClock`). Page crypto and the RDMA owner are stepped on the
  harness thread (`SteppedCrypto`); the RDMA fabric is
  `rdma_verbs::simulation`.
- Nodes talk over one simulated network. Control is supplied by the harness:
  it signs node identities under its own CA, publishes publications and
  keyring bundles directly and parks the live control and keyring feeds. It
  does not use the `racer-authority` world (see "Remaining work").
- Clients are simulated UDS readers; origins are fake UDS adapters serving an
  immutable versioned catalog with injected faults (credential reject and
  forbidden, slow and failed origins).
- The harness owns its own `SimOs` and `SimClock` and advances time itself
  (1 ms per turn), so to the world the role finishes in one step at time 0.
  The role draws its seed from the choice stream and folds the run's replay
  digest into the world trace.
- All polls use a recording waker (`wake` in `src/app/dst.rs`), not a no-op
  waker.

## Invariants

All are harness assertions; a panic becomes the world violation
`role-assert` with the panic message.

- Oracle: every successful response equals the origin version's bytes;
  pinned reads whose version changed get the 412 contract, origin failures
  get 502.
- Resource bounds per worker on every turn (admission classes, writer queue,
  client connections, descriptor bound).
- Release at quiescence: after teardown no live handles, no live fabric
  resources, all checkpoint shards released.
- Stranded work: `settle` fails when work is pending for `STRANDED_TURNS`
  consecutive turns with no completion, no in-flight reactor or crypto
  operation and no recorded wake.
- Liveness after heal: `verify_recovery` reads every object after faults
  are cleared.
- Corpus coverage: `app-corpus` and `app-corpus-native` replay seeds 1, 7
  and 42 and fail when a coverage obligation (actions, origin faults, native
  faults, byte and response counts) is never met.

## Determinism

`make racer-dst` runs every `app-*` entry twice and compares the world trace,
which holds the app replay digest. The corpus does not replay each seed a
second time internally; a divergence is reported as a trace hash mismatch
without the first differing checkpoint.

## Seeds

`DST_SEEDS`, `DST_STEPS` and `DST_FILTER` have their `racer-dst` meanings;
seed file entries such as `app-generated seed=N` pick app seeds.

## Remaining work

- Run app nodes as world roles on the world's `SimOs` and `VirtualClock`
  (one role per worker lane), so world faults (partition, crash/restart,
  clock skew) and the world's wake-based scheduler apply to them directly.
- Drive enrollment, publication and keyring from the `racer-authority`
  world instead of harness-signed documents.
- Crash during a write with torn `SimOs` disk faults, then restart from the
  checkpoint and assert hits never return wrong bytes.
