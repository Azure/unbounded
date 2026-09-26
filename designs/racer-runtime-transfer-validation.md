# Runtime cancellation escaping peer acquisition

Base: `3ebec37f1150fb59a65d841e9aa0064e92b57d8f`.
Branch/worktree: `racer-fix-runtime-transfer`, `tmp/racer-fix-runtime-transfer`.
Affected artifact: **racer-dataplane only**.

## Exit classification before attribution

Read-only Kubernetes observations on September 26, 2026 used context
`joolshev-scale-test`, namespace `unbounded-system`, request timeout 20s and
kubectl process/subprocess timeout 30s. A bounded fleet snapshot found 1500 pods,
909 restarts, and 806 retained terminations: all `Error`, exit code 1. Of these,
792 had lifetimes at most 60 seconds and 14 exceeded 60 seconds. No retained
termination was OOMKilled. Kubernetes retains the last exit, not every exit.

All 14 long-lived containers' previous timestamped logs contained only
`racer-dataplane: Cancelled` at the corresponding termination time:

| Pod suffix | Lifetime seconds | Exit UTC |
| --- | ---: | --- |
| rgj9c | 103 | 17:03:44 |
| pjwwq | 109 | 17:04:34 |
| jhhgs | 115 | 17:04:36 |
| n6xbr | 155 | 17:04:46 |
| xnkg9 | 104 | 17:04:58 |
| kts4z | 205 | 17:05:26 |
| gxph9 | 199 | 17:05:57 |
| 5gmpq | 62 | 17:06:08 |
| 6z6zl | 236 | 17:06:08 |
| zjj27 | 257 | 17:06:18 |
| nk278 | 250 | 17:06:47 |
| v6qhv | 357 | 17:08:29 |
| rbdzk | 379 | 17:09:37 |
| 5psnk | 335 | 17:09:50 |

The sampled ingress pods pgxtl, c87nq, and 75twm instead had previous logs
`DeadlineExceeded`, at 17:03:02.849, 17:02:38.411, and 17:02:37.449 UTC.
These observations distinguish the newer runtime cancellation class from the
earlier startup-deadline class. They do not classify every historical restart.

Parent evidence remains in the deployment worktree's ignored `tmp/`:
`racer-3ebec3-{baseline,settled}-fleet.json`,
`racer-3ebec3-c1-settled.{json,md}`, and `racer-3ebec3-settled-summary.md`.
The summary records 800 -> 907 same-UID restarts, 13 retained lifetimes over 60s
at 17:09:03, and zero complete images of three with stable sampled pod identities
and restart counts during the probe (`:13-17,47-93`). Our later snapshot adds two
restarts and one additional retained long-lived exit. Previous logs alone do not
connect a particular page or request to any live runtime exit.

## Causal mechanism

Source references in this section are at the base revision; `src/` is relative
to `cmd/racer-dataplane`.

1. `PeerServer::serve_connection` clones the listener scope, replacing only the
   signed request ID and tightening its deadline (`src/peer/server.rs:258-273`).
   `request_scope` deliberately preserves that token (`src/peer.rs:92-112`).
2. `Coordinator::serve_peer` uses this scope for local Fill acquisition
   (`src/read/serve.rs:294-295,349-355`). Remote-worker dispatch has an independent
   token (`src/read/dispatch.rs:600-604`), so the local-owner case matters.
3. Fill retains a clone for its independent driver. When the leader expires or
   detaches, the driver cancels the clone (`src/read/fill.rs:203-235`;
   `src/read/flight.rs:122-132`). Cancellation clones share an Arc-backed state
   (`src/runtime/deadline.rs:16-23,125-150`). This cancels the listener itself.
4. The listener checks that scope and returns Cancelled
   (`src/peer/server.rs:74-81,394-401`). Application treats a running listener's
   terminal error as fatal (`src/app.rs:1057-1066`); the worker fails and drains
   the group (`src/runtime/worker.rs:764-781`). Main prints the error and exits 1
   (`src/main.rs:5-10`).

Thus one expired local-owner peer acquisition can stop unrelated peer work and
the running application. This mechanism is independently reproduced below.
It is consistent with the live Cancelled class, not proof that all 14 exits or
all remaining HTTP 503/EOF/stalls share this cause. No relay dependency cycle,
reactor-overload exit, or fleet-wide recovery is claimed.

## Regression and fix

`sdk_full_image_survives_expired_peer_fill` extends the actual Go SDK fixture,
production Coordinator/Fill, authenticated TCP transport, and four-peer graph.
After four SDK layer bootstraps, it sends a separate signed Acquire for layer 7,
page 3 to its first-ranked candidate. A retained plaintext reservation forces
that candidate's elected Fill to wait until the signed 100ms request deadline.
The test observes an actual Flight charge and expiry, then releases pressure.
It requires listener survival and completion of the original SDK image, warm and
cold, checking manifest/config plus all eight layer lengths and SHA-256 digests.
Final drain asserts zero plaintext, ciphertext, dirty, connection, relay, waiter,
and flight charges. Quotas and the original SDK deadlines are not increased.

Initial test against unmodified production code failed in the graph driver with
`Cancelled`. The strengthened test uses production `PeerServer::listen` rather
than the fixture's custom peer accept loop. Restoring only the base shared-token
decision in the candidate made that exact test fail with
`peer server: Err(Cancelled)` (2.92s after compilation). Restoring isolation made
both warm/cold images pass. No baseline toggle remains.

The fix creates one exchange-local cancellation token and subscribes to parent
cancellation. Parent cancellation wakes the exchange and propagates inward on
poll; acquisition cleanup cannot propagate outward. Listener and signed deadlines
are preserved without resetting them. Deadline expiry retains DeadlineExceeded;
the bridge propagates explicit cancellation rather than reclassifying expiry.
Existing I/O/crypto owners still retain resources through their completion fences.

The existing header timeout/cancellation/drop test additionally checks a real
waker notification before reactor polling. Its silent/partial/trickle/idle,
shorter-listener deadline, cancellation, and abandonment assertions still pass,
including quota retention before fencing and release afterward.

The prior integration prose explicitly specified a shared listener cancellation
token while promising that connection errors do not stop other connections
(`src/peer/INTEGRATION.md:38,57` at base). The user explicitly approved exchange
isolation after this conflict and the old-fail/new-pass result were reported.
The integration contract is updated to describe one-way propagation.

## Checks

All commands were explicitly bounded. No test/build command exceeded 300s.
Commands below are relative to this worktree.

- `timeout 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib -- --test-threads=1`:
  **570 passed, 14 ignored**, including the cancellation wake/fence assertions.
- `timeout 120s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib sdk_ -- --ignored --test-threads=1`:
  **7 passed**, all six existing variants plus the new expiry regression, each
  warm/cold. Every image verifies 542,950,400 layer bytes.
- `timeout 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib full_image_ -- --ignored --test-threads=1 --skip sdk_`:
  **5 passed**, including the real Fill/election and signed relay transfer graphs.
- `timeout 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --test production_dataplane -- --test-threads=1`:
  **10 passed**, including the two-worker ownership/continuation case.
- Scoped `timeout 180s env GOTOOLCHAIN=go1.26.6 make fmt GO_PACKAGE_DIRS=cmd/racer-dataplane/tests/conformance/production_stream_fixture_test.go.txt GO_PACKAGE_PATTERNS=./pkg/racersdk`:
  **passed, 0 issues**.
- All-target release Clippy in warning mode completed. Its initial run identified
  one new too-many-arguments warning in the expanded fixture; scenario arguments
  were grouped into a test-only struct. Existing repository warnings are outside
  this fix; strict warning-free repository lint is not claimed.
- Final all-target release Clippy: **completed successfully**, 133 existing
  warning diagnostics across target builds. No diagnostics point to added code;
  the only diagnostic in edited files is unchanged `src/peer/tests.rs:411`.
- Final release binary build, crate rustfmt check, and `git diff --check`:
  **passed**. After the test-only argument grouping, the new SDK expiry case was
  rerun and **passed warm/cold**; the unchanged broader checks were not repeated.

## Scope and limits

Exactly one read-only CLI-delegated subagent inspected runtime exit paths and
production graph coverage; no further agents were spawned. Main concurrent work
was untouched. Existing kind clusters were enumerated but not modified; the
one-dataplane cluster cannot reproduce this peer-listener path and the reported
multi-dataplane host-inotify limitation was not bypassed. No host sysctl changes,
AKS configuration mutations, durable deletion, push, or rollout occurred.
No bpftrace process was started, so none requires cleanup.

The new regression exercises the local-owner path and production listener, not
the complete multi-worker Application lifecycle. Existing two-worker integration
and relay graphs pass, but a sustained many-puller/multiple-relay dependency cycle
is not reproduced. Synthetic pressure establishes the cancellation mechanism,
not the ownership mix that caused every live deadline. Existing SDK fixture
builds and child executions retain their bounded 280s build/90s test watchdogs.

Parent rollout should build **racer-dataplane** from this commit and validate
runtime-exit classes plus complete verified pulls of
`loadgen.invalid/benchmark/image@sha256:2aa1e8745512f4f76499b8b9a5543642630d3f1120668d8636278537d9aa53a6`.
The supplied live image includes 542,952,560 manifest/config/layer bytes; the
local fixture uses the exact layer sizes with synthetic manifest/config and
layer content. Local success does not establish fleet recovery.
