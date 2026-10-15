# Racer bounded Flight admission: implementation and GPU results

Date: October 8, 2026. Branch: `racer-v2`.

## Outcome

The source fix is deployed. Ordinary plaintext acquisition now waits in a bounded FIFO instead of immediately failing on local Flight pressure. At the original six Flight credits per I/O worker, C8 completed with zero errors. At C16, the next observed **logical constraint was the 100 ms admission allowance**, not a proven backend or hardware ceiling.

No further load or tuning is authorized by this report. The last read-only audit confirmed C0/drained at 22:39:17 UTC. Configuration remained FLIGHTS64. There was no C24 run and no change from 100 ms to 250 ms.

| Provenance | Value |
|---|---|
| Source worktree commit | `60cc47593a560bfb5d24cb2e21d71259bd8189a2` |
| Integrated commit | `c4f24c5e55759342ee697c734f1757f91afaad5b` |
| Build | `37845543624` |
| Image | `ghcr.io/azure/racer-dataplane@sha256:4b6da894ae929ad70e92949796fdc014e8bab209072632a2aaebf29c8476f425` |
| Verified linux/amd64 manifest | `sha256:2755c5dbb85caa072aa79168141689f1be182af3e4c6cb79a2195b3062d254f6` |

## Fix and validation

The implementation is authoritative. In `cmd/racer-dataplane/src/read/flight.rs:987-988`, ordinary acquisition establishes a nonrenewable 100 ms cutoff, capped by the original scope and budget deadlines. Scope and budget checks precede allowance expiry (`:1016-1031`). Entry-cap and Flight-quota pressure are remembered as prior observations (`:1070-1099`), not fresh measurements at expiry. The terminal admission result is recorded once (`:1105-1107`).

FIFO tickets prevent new-entry barging. Existing-page joins can proceed without creating another Flight. Ticket-owned wake cleanup happens before ticket retirement and preserves reentrant successor notifications. Cancellation and shutdown drain queued tickets. Real Flight-credit return wakes the next waiter; dropping a caller does not release credit still owned by an in-flight private copy. Peer ciphertext acquisition, prefetched bootstrap, and new private-copy admission remain fail-fast. Original scope/deadline/attempt validation is not replaced by the local allowance.

Gate telemetry is schema2 with **58 fixed counters and a 64-event ring**. Indices0-41 retain their old meanings. Added Join/JoinCopy pairs distinguish QueueFull, AllowanceExpired, Queued, ScopeDeadline, BudgetDeadline, Cancelled, QueueAdmissionRejected and AdmissionFailed. Wait facts explicitly label their cause as `prior_observation_not_terminal_capacity` (`src/telemetry.rs:881-940,1161-1209`). Counter and ring snapshots are non-atomic; operation identity is not recorded.

The preserved `flight-wait-source-checkpoint.md` records:

- Fifteen focused admission gates passed, followed by one separate absent-page budget-precedence regression after review. This is **15 plus1**, not a claimed final combined16-test invocation.
- The read subset passed248 tests, client-response subset14, and runtime-adapter subset27 **before** the last narrow precedence fix.
- The later precedence regression, test-target Clippy, Rust formatting and diff check passed after that fix. The earlier15-second Clippy timeout was superseded by a completed bounded check.
- `GOTOOLCHAIN=go1.26.6 make fmt` passed before the source commit, with zero lint issues and no unrelated tracked edits.
- Tests cover cancellation/head advancement, queue fullness, same-page followers, private-copy completion wake, reentrant wake cleanup, mailbox progress, fail-fast bootstrap and reciprocal signed peer overload/fallback. Reciprocal fixtures are in-process Coordinators, not a network-socket or global distributed-deadlock proof.

The reporting phase ran the requested Go formatter again. No application source or benchmark policy was changed for this report.

## Fixed setup and safety protocol

Two nodes, `gpu-07-03` and `gpu-07-13`, retained the same LG instances and Zipf workload:512 objects of256 MiB,128 GiB total, exponent1.2, seed `gpu-zipf-20261008-128g-v1`, verify=false, diagnose=false. Origins stayed alive. The final worker plan was10 I/O and5 crypto workers with MAX_THREADS16.

FLIGHTS was returned from128 to64 by one UID/resourceVersion/full-old-data CAS. With ten I/O workers, integer partitioning gives six Flight credits per worker; the unchanged64 waiters-per-flight yields384 Waiter credits. The earlier128 setting yielded12 and768. See `src/app.rs:2347-2362`, `src/config.rs:224-225`, and `src/admission.rs:128-133`. No weighted-floor policy, other credit limit, worker count or memory budget was tuned.

Plaintext/ciphertext budgets stayed8 GiB each, registered2 GiB, dirty1 GiB, and request context256 MiB. DP memory limit64 GiB and LG4 GiB were unchanged. Self and visible ancestor `cpu.max` remained `max 100000`. This establishes no visible cgroup CPU quota, **not unlimited physical CPU or isolated hardware**.

Each point used a fresh two-phase receipt:60-second TTL,20-second quick gate,110-second active reserve,150-second mandatory C0/drain reserve,290-second total and external TERM300. C8/C16 required explicit counted5% allowance after32 completions per node and failure cap500. Unknown, deadline, transport and integrity labels remained hard stops. Cumulative ratio at or above5% and count at or above500 stop the run; sampled overshoot remains reportable.

Each run returned both LGs to applied0/inflight0 before the separate full post-audit. Original raw16-device inventory,7+8 approved FD identity,30 zero edge hashes, protected PV/write-discard counters, frozen old checkpoint hashes, resources and operation-scoped shared health were retained. An operation's fresh shared census was fixed across preflight/run/resume; no drift was silently rebased during load.

## Results

Success means a completed **unverified**256 MiB pull. Goodput is successes times256 MiB divided by each pod's actual measurement interval, summed across pods. Partial received bytes are excluded. Verified bytes stayed0. Error fractions use failures divided by successes plus failures.

| Attempt | Node03 success / failure | Node13 success / failure | Total success / failure | Total error fraction | Window goodput, GiB/s |
|---|---:|---:|---:|---:|---:|
| C8, FLIGHTS64 | 3232 /0 | 3251 /0 | 6483 /0 | 0 /6483 =0% | 19.977 |
| C16 | 3957 /7 | 3700 /11 | 7657 /18 | 18 /7675 =0.2345% | 23.849 |
| C16, thread endpoints | 3984 /12 | 3973 /7 | 7957 /19 | 19 /7976 =0.2382% | 23.937 |

C16 node error fractions were7/3964=0.1766% and11/3711=0.2964%. The thread run fractions were12/3996=0.3003% and7/3980=0.1759%. These were counted-error completions, not zero-error passes.

| Window | Node03 success / failure | Node13 success / failure | Actual LG intervals03 /13, seconds |
|---|---:|---:|---:|
| C8 | 2460 /0 | 2440 /0 | 61.335 /61.308 |
| C16 | 2993 /7 | 2832 /8 | 61.042 /61.084 |
| Thread C16 | 3049 /9 | 2993 /5 | See saved per-pod window records |

The first C16 had17 incomplete responses and1 HTTP-status failure; the thread run had17 incomplete and2 HTTP-status failures. Full activation, window and drain-phase accounting is preserved. The first C16 received2496 MiB beyond successful-completion bytes over the fully drained attempt. Interval-only differences can also reflect outstanding transfers. Coarse histogram p50/p95/p99 estimates are not exact request quantiles, especially with few failures.

C8 at Flight6 is a useful functional regression result but does **not prove the new wait path was exercised**. Successful waits and same-page joins have no dedicated exported count. The earlier FLIGHTS128 zero-error point is not a controlled speedup comparison: RAM warmth, durable index coverage, Zipf selections and measurement intervals differed.

## Observed logical constraint, not backend proof

In the first C16, all18 distinct primary terminal requests matched18 new `AllowanceExpired/Join` events by DP request ID. Seventeen carried prior EntryCap and one had no prior cause. Two other Queued/JoinCopy events had no primary terminal failure and were not counted as additional LG failures.

The thread C16 had19 new expiry events matching19 primary terminal requests, all with prior EntryCap. An extra FlightQuota/JoinCopy event on node13 worker7 had no primary terminal failure. Historical rows were excluded using the baseline sequence totals. Across each point's seven captures, all58 counters were parsed and all new gate/terminal sequences were captured: no overwritten or uncaptured events. ClientWrite boundaries were deduplicated from primary FirstSlice/NextSlice failures.

Matching request IDs, request-relative gate pages, timing and the direct code path strongly associate the failures with the **100 ms admission cutoff**. They do not show fresh entry occupancy at expiry. Terminal pages are absent, and there is no operation-global identity or matching LG trace ID. Per-node LG counts agree with DP primary outcomes; this is not a fabricated end-to-end trace join.

Crypto queue means increased from about13 ms at C8 to25 ms at C16 while decrypt execution stayed about13 ms. Physical-disk means and whole-port RDMA counters were collected, but neither proves backend saturation. Kernel disk busy time on parallel NVMe is not a capacity percentage, guard reads are included, and whole-port counters may include other traffic or aliases.

## Per-thread evidence

All five crypto and ten I/O lanes matched across endpoints, including worker0 on caller TID1. Actual startup placement maps IO0/5 to crypto CPU11, IO1/6 to12, IO2/7 to13, IO3/8 to14, and IO4/9 to15. This agrees with `src/worker.rs:509-528` and caller-lane execution in `runtime/src/group.rs:786-818`.

| Crypto CPU | Node03 total duty | Node13 total duty |
|---|---:|---:|
| 11 | 81.59% | 83.31% |
| 12 | 76.46% | 73.15% |
| 13 | 85.60% | 79.01% |
| 14 | 74.74% | 78.77% |
| 15 | 73.86% | 72.90% |

CPU time was mostly user time. Crypto runnable scheduler-wait fractions were about0.026-0.060%. The hottest I/O lane was worker0 at about45.9% duty on both nodes. Worker7 mapped to crypto13; its duty was about43.7%/43.2%. The exact per-thread user/system/runtime/wait table and per-owner expiry counts are in the private report.

Thread intervals were62.755/62.730 seconds, shifted14.5-14.8 seconds from DP metric snapshots. They are not exactly aligned or event-local. Three other nonpersistent TIDs per node were explicitly unavailable across endpoints, not treated as zero; all crypto/I/O lanes were available. These measurements do **not** demonstrate sustained100% crypto saturation or scheduler starvation. Bursts, serialization, queue fairness and Flight-credit lifetime remain possible explanations, not established causes.

## Incidents and limits retained

- Shared monitoring collectors with512 MiB limits repeatedly reported OOMKilled. During rollout preparation/observation, f87cn advanced172->173 at21:30:35; bqhrs201->202 at21:31:47 and202->203 at21:37:14. Earlier monitoring OOMs preceded load. These observations are not a Racer causal claim. Racer safety acceptance was conditional: shared rollout history remains INCIDENT and unqualified rollout validity remains false.
- An unrelated NCCL Job appeared during deployment. One rank ran with1 GPU/2 CPU/8 GiB requested on node13; the other stayed Pending. No explicit RDMA exposure appeared in its pod spec, but actual traffic was unknown. Both pods and Job later disappeared; successful completion was not established. They were never killed or modified by this work. Fresh tested operation scopes required their accepted absence and rejected reappearance. Shared health remained stable during the reported benchmark points.
- The protected node03 NVMe is excluded from Racer raw FDs and payload guard reads. Several observations showed increments of8 reads/36 sectors,18432 bytes. A further such increment occurred during the thread run. Their source remains unknown. Protected writes/discards stayed unchanged against original baselines. **Do not claim all protected counters stayed unchanged.** No disks were wiped, repaired or restored.
- The earlier checkpoint-recovery incident, including an indexed-cache drop, remains unresolved. The last image/config restarts preserved the full recorded index; that is not a resolution of the earlier incident. Four older private checkpoint backups were retained without recopying or restoring them.
- Final indexed payload was128 GiB on node03 and127.75 GiB on node13 after natural benchmark fills. Startup preservation required exact pre-restart totals; run-time growth was measured, not rejected as a fixed-gauge mismatch.

## Evidence and reproduction

Private evidence is preserved at `tmp/racer-gpu-flight-wait-20261008-artifacts` in the original workspace, not committed. Directories are0700 and files0600. `PRESERVATION.json` records original and preserved paths, SHA256 hashes, exclusions and external immutable binary-backup references. `SHA256SUMS` hashes the preserved files; `SEAL.json` records its seal and file count. Original archives are unchanged. Build targets are excluded.

Key files under `worktree-tmp/`:

- `flight-wait-source-checkpoint.md`, `ops-flight-wait-checkpoint.md`, `ops-checkpoint.md`.
- `ops-flight-c8-report-2219.md`, `ops-flight-c8-analysis-2218.json` (SHA256 `db1bbe43646defdf179e6c4580d5b4220e2f07883ee8fe528fac0b11e607c12c`).
- `ops-flight-c16-report-2229.md`, `ops-flight-c16-analysis-2227.json` (SHA256 `0bfa6d883cde45f9979702f72077cdc4ae78728c2cdadd2a16bfb93d58156dfe`).
- `ops-thread-c16-report-2241.md`, `ops-thread-c16-analysis-2240.json` (SHA256 `95760b28036095f939770e002594a22348fa144750c8de9c4f6d56c7e475820e`).
- The run prefixes `ops-flight-c8-run-2214`, `ops-flight-c16-run-2224`, and `ops-thread-c16-run-2237`: receipts, per-pod samples, all58 counters, raw gate/terminal sequences and C0 evidence.
- Separate post-audits `ops-flight-c8-post-2216`, `ops-flight-c16-post-2226`, and `ops-thread-c16-post-2239`.
- Source/image/config receipts, conditional incident acceptance, failed/partial operation evidence, schema/parser tests, concrete bounded CLIs, thread collectors and startup mappings.

Imported sealed scripts and their referenced JSON baselines are preserved with original path mappings. Absolute paths in scripts are historical execution provenance, not permission to replay an old apply or consumed run. Reproduction starts with offline parsing and hash verification. Any new live operation requires separate authorization, a fresh scoped preflight and unchanged safety guards. No benchmark is authorized by restoring this archive.
