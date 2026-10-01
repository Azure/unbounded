# Racer tail-latency controls: code-only evaluation

Evaluated at `3255648690259da3ac95ec2c5c8586c7a802d576`, 2026-10-01.
Implementation and test assertions were read before related prose. No cluster
inspection, rollout, load change, or performance measurement was performed.
This document changes no defaults. Below, `D/` means
`cmd/racer-dataplane/src/`; other paths are repository-relative.

## 1. Node-shared adaptive admission is already active

| Setting | Compiled default | Meaning |
| --- | --- | --- |
| `RACER_PEER_INFLIGHT_MAX` | 256 | Outstanding outbound peer exchanges per dataplane node owner |
| `RACER_PEER_PER_NEIGHBOR_MAX` | 32 | Outstanding exchanges to one immediate neighbor, across workers |
| `RACER_PEER_ATTEMPT_TIMEOUT_MS` | 30000 | Nonrenewable local peer-exchange cap |
| `RACER_PAGE_HEDGE_SLOTS` | 0 | Hedging disabled; opt-in maximum 32 slots |
| `RACER_PAGE_HEDGE_DELAY_MS` | 100 | Positive delayed-launch threshold, maximum 30000 ms |
| `RACER_PAGE_HEDGE_BYTES` | 33554448 | One maximum duplicate plaintext/ciphertext pair: 32 MiB + 16 bytes |

Sources: `D/config.rs:204-228`, `D/read/hedge.rs:25-51`. Admission requires
`1 <= per_neighbor <= total <= 65536` (`D/peer/adaptive.rs:32-38`). These are
actual parser defaults, not assertions about deployed configuration.

Production constructs one adaptive owner after worker sizing and shares it with
worker routing/requesters (`D/app.rs:201-211,766-823`). It is not multiplied by
worker count and is not opt-in. It adds no waiting queue or memory-budget bypass.
Local pressure halves the node limit, at most once per 250 ms, without revoking
accepted work or marking a neighbor unhealthy. Verified completions restore one
node slot per second. Attributable immediate-link failures halve that peer's
limit and open a 250 ms backoff; one exclusive verified probe can recover the
circuit. Old-generation success cannot erase a newer peer failure. Peer state is
bounded to 256 entries with guarded retirement (`D/peer/adaptive.rs:41-43,112-260`).

Blame is deliberately narrow: selected observed connect errors and live-scope EOF provide
link evidence; generic I/O errors, local deadlines/cancellation and downstream
overload are not peer-failure evidence. Signed downstream overload is also not
adaptive recovery (`D/http/pool.rs:329-365`, `D/peer/transfer.rs:61-77`,
`D/peer/requester.rs:315-343`). Permits follow accepted transport ownership;
native teardown failure retains/quarantines its permit rather than pretending
timeout means DMA completion (`D/rdma/lifecycle.rs:55-67,480-505`).

Assertions cover shared caps and retained permits, local-pressure reduction,
exclusive recovery, and production worker pointer identity
(`D/peer/adaptive.rs:341-462`, `D/app_integration_tests.rs:65-93`).

## 2. Attempt cap and body ETA are distinct from signed authority

Each candidate request installs a local cap at
`now + min(peer_attempt_timeout, remaining overall time)`. Progress can refresh
the idle alarm but cannot renew that total cap or the original signed ceiling.
Attempts and route links are debited from the original acquisition budget before
sending; retries receive no fresh overall budget (`D/read/candidates.rs:655-705`,
`D/read/flight.rs:214-256,271-289`).

For known-length bodies, ETA uses bytes delivered since the first body sample,
not checkout/header wait. After an observation interval of
`min(candidate idle share, attempt duration / 3)`, it rejects a rate that cannot
finish by the local completion boundary. When another route is affordable, that
boundary also reserves fallback time. Even the last affordable candidate gets
an ETA check. Unknown-length progress has no rate prediction. Completion clears
the body projection, not the total cap; expiration is sticky
(`D/runtime/deadline.rs:147-238,248-275`).

An expired exchange is canceled and polled through its completion fence before
fallback; a late success cannot override expiry (`D/read/candidates.rs:733-795`).
Tests assert healthy progress beyond the idle share, slow-body rejection even
without another route, unchanged signed deadlines, conserved credits, and
fenced late-success rejection (`D/read/candidate_timeout_tests.rs:277-432,436-489`).
Thus the cap is not a promise that all resource teardown finishes at that instant.

## 3. Opt-in hedging is deliberately narrower than general request racing

Only a singleflight leader's plaintext fixed-page acquisition at a noncandidate
can try a pair. The first two ranked candidates must be distinct, healthy direct
neighbors, with at least four attempt and eight link credits available. Direct
neighbor eligibility is checked against the topology, not inferred from distinct
final destinations (`D/read/candidates.rs:60-109`, `D/peer.rs:61-75`).

The primary uses `Acquire`; at most one delayed secondary uses `CopyOnly`, so
the duplicate does not request another origin acquisition. Both are pinned to
their own direct first hop and forced to HTTP. Metadata, bootstrap, subscription
selection, ciphertext relay, and native transfers do not race through this hook.
There is no whole-GET duplication; an eligible fixed-page fallback within a read
can still reach the hook (`D/read/candidates.rs:110-174`,
`D/peer/requester.rs:144-168,276-293`, `D/read/fill.rs:999-1048`).

Slots and duplicate-byte capacity are shared by all workers of one node owner
(`D/app.rs:610-617`). Each slot charges a full 33554448-byte pair, including during
the delay. Before scheduling, worker/cache plaintext and ciphertext escrow must
also fit; actual receive/decrypt allocations still pay their normal quotas.
Launch rechecks adaptive and local headroom. Adaptive reduction, peer circuit
pressure, missing route independence, insufficient original credits, exhausted
slots, or memory pressure suppress speculation
(`D/read/hedge.rs:89-108`, `D/read/candidates.rs:87-174`,
`D/peer/adaptive.rs:77-87`). Escrow is intentionally conservative extra accounting,
not a measurement of allocated memory or transmitted bytes.

The original credits are partitioned, with only unused child credits reunited.
Either response must pass binding/page validation and plaintext decryption before
it can win. The winner cancels the loser but waits for accepted exchange/crypto work
to fence before return/publication. This is **not early publication** and slow
loser teardown can erase any latency benefit. On recoverable pair failure, serial
resolution can revisit candidates using only remaining original credits; this
acquisition does not launch another pair (`D/read/candidates.rs:176-200`,
`D/read/hedge.rs:159-264`, `D/read/fill.rs:1003-1044,1149-1196`). Tests assert real
AEAD validation, two-reader singleflight, exact credit/deadline conservation,
suppression, and retention after caller detach until the loser fence
(`D/read/fill_peer_tests.rs:152-237,243-314`, `D/read/hedge.rs:334-432`).

**No fleet-global distributed hedge budget is implemented.** The owner is local
`Arc`/`Mutex` state, not a distributed coordinator (`D/read/hedge.rs:58-108`,
`D/app.rs:610-617`). Operators must account for enabled node/process count,
including overlapping replicas during rollout. For homogeneous settings, the
capacity ceiling is `N * min(slots, floor(bytes / 33554448))` concurrent pairs;
this is not a bytes-per-second or experiment-total traffic limit. More slots
alone cannot exceed the byte cap, and actual memory admission may allow fewer.

## 4. Window 1 versus 2: use the existing staged guard, not a default increase

Compiled `RACER_RANGE_WINDOW_PAGES` defaults to **2** (`D/config.rs:252`). Gantry
passes `racer_page_window` to SDK `PageWindow`; absent credits select two, with
byte credits derived from page credits (`cmd/gantry/agent_racer.go:243-253`,
`pkg/racersdk/subscription.go:92-104`). Gantry also accepts
`GANTRY_RACER_PAGE_WINDOW` and `--racer-page-window`; the guard rejects precedence
that shadows the reviewed config (`internal/gantry/config/config.go:679,782`,
`hack/scripts/racer-rollout/page_window.py:118-130`). The ordered dataplane window is the minimum
of configured window and client page credits; SDK resident buffers are capped at
two, further reduced by page/byte credits (`D/read/range_stream.rs:259-266`,
`pkg/racersdk/ordered.go:27-30`). A window-1 contract is tested for resident storage
and reuse, not a universal prohibition on window 2
(`pkg/racersdk/ordered_test.go:159-204`).
At 16 MiB per page, the two-buffer allowance is 32 MiB per active bulk stream,
before other allocations; window 2 is not a free concurrency increase.

The existing `da7a6bae` guard is sufficient for the reviewed opt-in experiment;
no new tuning code or regression is needed here. Use the exact bounded commands
in `hack/scripts/racer-rollout/page-window.md:45-107`: fresh repository-local
state, `plan --stage gantry`, review artifacts/hash, `verify` (server dry-run),
then separately approved `apply`. Gate rollout, effective credits, and memory
before separately planning/verifying/applying `--stage racer`. Roll back Racer
first, then Gantry, from the original forward plans with fresh review.

The guard requires the explicit window-1/relay-false baseline, approved hashes,
unchanged identities/configuration, Gantry's reviewed 2Gi limit, and known config
precedence. It uses UID/resourceVersion/data tests and preserves unrelated YAML
bytes (`hack/scripts/racer-rollout/page_window.py:26-96,101-201,222-250`). It is
context-specific, not a general fleet rollout or cross-resource transaction.
It does **not** certify pod convergence, current memory headroom, or workload
safety; those remain parent/operator gates. Relevant success/failure assertions
are `test_page_window.py:100-223` in the same directory.

Prose reconciliation: `page-window.md:28-36` records historical deployed values
of 1, not today's state or compiled defaults. Its abbreviated `D/config.rs` and
`D/app.rs` line references have drifted; current acquisition memory floors are
at `D/config.rs:405-430`, and startup sizing is at `D/app.rs:203-205`. This is not
evidence to raise anything. Its warning that 2Gi is not proof of safety remains
consistent with the two-buffer SDK implementation (`page-window.md:19-26`).

## 5. Controlled comparison, not a performance conclusion

Future operator-approved evaluation should hold image/build, workload/catalog,
concurrency, warming, topology/node count, qdisc configuration, request/attempt
timeouts, adaptive caps and memory budgets fixed. Exclude rollout/warm-up windows.
Declare sampling duration, minimum completion count, abort thresholds, phase
deadlines and recovery owner before changing configuration.

1. **Deadline-only versus hedge:** at one fixed effective window, compare the
   same build with slots 0 against an explicitly budgeted small opt-in hedge
   capacity. Keep phase 1/2 behavior identical. Record route eligibility and
   suppression: no launches means no evidence of hedge effectiveness.
2. **Window 1 versus 2:** keep hedging off; use the staged guard above. Measure
   the Gantry-only control separately: buffer allowance changes even while
   effective dataplane acquisition remains 1. Only compare fully converged
   two-setting endpoints as window 1 versus 2. A combined window-2/hedge arm is
   optional later, not a substitute for these isolated comparisons.

For every arm collect the same fleet coverage and interval:

| Measure | Required interpretation |
| --- | --- |
| p99 and completions | Page and end-to-end fully verified completion latency, sample counts, goodput, and per-node tails; do not hide failed requests in a success-only p99 |
| Errors | Absolute counts and rates, timeouts/cancellations, integrity/version failures, zero-goodput nodes, admission rejection/pressure |
| Duplicate bytes | Measure actual extra peer/wire traffic where attribution exists; otherwise mark unavailable and report aggregate traffic separately |
| qdisc deltas | Per-node/interface before/after drops, overlimits, requeues and backlog samples, normalized by interval/work; handle counter resets explicitly |
| Memory | Gantry and Racer RSS/working set/peak, GC headroom, restarts/OOM, PSI and admission pressure; include SDK buffers and hedge escrow plus actual allocations |

`racer_page_hedges_started_total`, `racer_page_hedges_won_total`, and
`racer_page_hedges_suppressed_total` explain participation. Crucially,
`racer_page_hedge_duplicate_reserved_bytes_total` increments by one full pair per
secondary launch, **not measured duplicate wire bytes** (`D/read/hedge.rs:140-148`,
`D/telemetry/metrics.rs:184-196`). A "win" likewise does not prove earlier user
completion because return waits for the loser fence. Require repeatable p99
improvement with acceptable errors, traffic, queue and memory deltas before any
promotion; reject or roll back regressions. This phase establishes no such result.

## Focused validation performed

All commands ran in the assigned worktree, with external
`timeout --signal=TERM --kill-after=10s`; no cluster commands were run.

- `180s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_page_window.py'`:
  **13 passed**, Kubernetes calls mocked.
- `cargo test --locked --offline --manifest-path cmd/racer-dataplane/Cargo.toml --lib FILTER`:
  `hedge` **14 passed** and `adaptive` **11 passed** (120s bounds);
  `read::candidates::timeout_tests` **14 passed**, `runtime::deadline::tests`
  **7 passed**, `peer::tests::body_progress` **4 passed**, and
  `worker_requesters_share_configured_admission_and_production_metrics`
  **1 passed** (90s bounds). Filters overlap; these are not additive unique counts.
- An initial `body_progress_tests` filter matched zero tests; the actual module
  name was verified and the corrected four-test filter above passed.

These are contract/unit/local-socket tests, not a load benchmark, full suite, or
native-hardware validation. Go tests were not run. The already reported
`make fmt` Go-toolchain mismatch was not retried unchanged; no Go or Rust source
was edited. Defaults and existing guard behavior remain unchanged.
