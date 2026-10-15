# Racer GPU-node concurrency curve with counted failures

## Result and scope

This continues the [progress-fix results](racer-gpu-progress-fix-results-20261007.md).
The user authorized a different acceptance policy: count selected failed responses
instead of stopping on the first one. C8 completed with low error rates. C16 hit
the initial absolute failure cap of 50, then completed after a separately reviewed
cap increase to 500. C24 crossed the 5% ratio limit during activation and stopped.
**No C32 ran. Both loadgens were confirmed at C0 with zero in-flight work.**

The highest completed concurrency in this continuation is **C16 under the 5%/500
counted-failure policy**, not an error-free or maximum sustainable point. Its
observed verified goodput was 6,213.981/7,094.167 MiB/s, totaling
**13,308.148 MiB/s, about 12.996 GiB/s** across the two nodes.

The workload remains a warm, shared **8 GiB catalog**: 128 blobs of 64 MiB,
seed `gpu-progress-fileslab-20261007-cold-01`, shuffle selection, UDS clients,
verification enabled. Ten file-backed 1 GiB slabs per node reside on ext4
`/dev/sda2`. **No NVMe cache disk was used. The goal of measuring all available
NVMe and RDMA hardware capacity is still unmet.** Client goodput is not RDMA wire
throughput, and hit-event counts do not establish byte fractions.

## Code contract and unchanged deployment

Loadgen credits verified bytes only when the complete operation succeeds with
verification enabled (`cmd/racer-loadgen/pull.go:251-282`). Failed partial-response
bytes therefore do not enter verified goodput. Received bytes are a separate
counter that may include failed operations. The metric definitions and latency
buckets are in `cmd/racer-loadgen/metrics.go:33-46`.

Failure labels distinguish `incomplete`, `http_status`, `size_mismatch`,
`digest_mismatch`, and other outcomes (`cmd/racer-loadgen/failures.go:32-49`). The
approved policy budgets only the first two. It does not assert that every failed
partial response is a clean overload or that failed data was verified.

The serving deployment did not change during this continuation:

| Item | Value |
| --- | --- |
| Dataplane build source | `b04d28f91d775519c820027a73291b8c3cbd4fd4` |
| Successful build run | [37694995366](https://github.com/Azure/unbounded/actions/runs/37694995366) |
| Image | `ghcr.io/azure/racer-dataplane@sha256:466ea41053da31dc11fb0857bad75aea82dbb24958f6e77f8822fe73b6f8c316` |
| Node plaintext / ciphertext / registered budgets | 8 GiB / 8 GiB / 2 GiB |
| Node queue entries / flights / configured threads | 256 / 64 / 16 |
| Actual workers | 10 I/O + 5 crypto per node |
| Dataplane limits | 16 CPU / 64 GiB per node |
| Loadgen limits | 8 CPU / 4 GiB per node |

Controllers, operator, and loadgen retained the earlier images. No image build,
budget adjustment, seed change, raw-cache migration, or disk repair accompanied
these runs. Per-worker quota division remains as implemented in
`cmd/racer-dataplane/src/app.rs:2331-2357`; possible local limits are not proof of
which resource rejected a particular request.

## Acceptance policy and safeguards

The previous report used a zero-error gate. For this continuation the parent
explicitly allowed counted `incomplete` and `http_status` failures, per node:

1. Stop at a cumulative failure ratio **greater than or equal to 5%** once at
   least 32 operations have completed. Denominator is success plus failure.
2. Initially stop at **50 failures**, independent of the minimum count or ratio.
3. After the C16/50 abort, a separately reviewed `--max-failures` option retained
   default 50 and allowed 1-500. Raising it above 50 requires both explicit
   counted-failure opt-in and a positive ratio budget. Only the later C16 and C24
   runs used 500. The ratio rule was not loosened.

The runner distinguishes `completed_with_counted_unverified_response_failures`
from zero-error success. Integrity/size/digest/unknown failure categories remain
unconditional stops. Other hard gates cover 30 seconds without per-node verified
progress, pod readiness/restarts/identity, exact-container memory reaching 75% of
a finite limit, memory-limit/OOM events, raw-device FDs, old checkpoint changes,
and NVMe write/discard changes.

Every run was separately authorized, with a 60-second target, external 300-second
TERM bound, absolute internal deadline, 15-second command heartbeats, and
finally-C0/drain. The parent retained an independent SSH fallback. These are
**sampled limits, not instantaneous guarantees**: failures can accumulate between
samples and during control projection and drain. Counts and overshoot are retained.

The reviewed runner and tests are local evidence, not application source changes.
Review corrected three operational issues before counted-failure execution:
reading cgroup pseudofile contents rather than size, validating successive
counter monotonicity including established zero-valued series, and keeping
standalone recovery independent of plan-file loading. Later final accounting was
changed to compute both nodes from the raw drained snapshot even if policy
validation stops on the first node. Recorded focused tests are preserved; no
unchanged source suite was rerun for this report.

## Completed observation windows

Rates use each node's own monotonic scrape-midpoint interval. Actual observation
spans include collection overhead: 64.356s for C8 and 64.684s for C16/500.
Percentiles are histogram estimates, not exact samples.

| Phase / node | Metric interval s | Verified MiB | Verified MiB/s | Success / failure | Window error ratio | Success p50/p95/p99 ms |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| C8/50 node03 | 64.169 | 320,384 | 4,992.785 | 5,006 / 7 | 0.1396% | 69 / 445 / 744 |
| C8/50 node13 | 64.163 | 405,568 | 6,320.907 | 6,337 / 4 | 0.0631% | 67 / 413 / 491 |
| C16/500 node03 | 64.783 | 402,560 | 6,213.981 | 6,290 / 161 | 2.4957% | 73 / 613 / 935 |
| C16/500 node13 | 64.765 | 459,456 | 7,094.167 | 7,179 / 141 | 1.9262% | 72 / 477 / 869 |

C8 aggregate is **11,313.692 MiB/s, about 11.049 GiB/s**. C16's higher observed
goodput came with substantially more counted failures, not a free scaling gain.
These short, sequential warm continuations do not establish sustainable capacity.

| Phase / node | Window failure labels | Failure p50/p95/p99 ms |
| --- | --- | --- |
| C8 node03 | 3 HTTP-status + 4 incomplete | 33 / 825 / 965 |
| C8 node13 | 4 incomplete | 233 / 473 / 495 |
| C16/500 node03 | 33 HTTP-status + 128 incomplete | 45 / 354 / 471 |
| C16/500 node13 | 29 HTTP-status + 112 incomplete | 52 / 444 / 765 |

### Activation and stopping tails are not steady-window measurements

| Phase / node | Activation success/failure | Observation success/failure | Stopping tail success/failure | Total error ratio |
| --- | ---: | ---: | ---: | ---: |
| C8 node03 | 127 / 0 | 5,006 / 7 | 728 / 1 | 0.1363% |
| C8 node13 | 314 / 0 | 6,337 / 4 | 872 / 0 | 0.0531% |
| C16/500 node03 | 127 / 0 | 6,290 / 161 | 951 / 16 | 2.3459% |
| C16/500 node13 | 329 / 7 | 7,179 / 141 | 909 / 13 | 1.8769% |

Activation includes baseline and projection time. The stopping tail includes C0
API/projection latency and admitted work, not only time after applied C0. Entire
run totals are not labeled steady throughput. Maximum sampled cumulative ratios
were 0.1363%/0.0879% for C8 and 2.5240%/2.0833% for C16/500; neither had cap
overshoot under its selected cap. No digest/size/unknown failure increment or
dataplane corruption/CRC/AEAD failure was observed in the audited phases.

## Resource and transport observations

| Window / node | DP process cores | LG process cores | File payload published/read MiB/s | Whole-port TX/RX bytes |
| --- | ---: | ---: | --- | --- |
| C8 node03 | 1.973 | 2.993 | 841.10 / 365.99 | 1,984,755,392 / 3,010,863,152 |
| C8 node13 | 2.447 | 3.846 | 1,132.29 / 433.58 | 3,010,863,152 / 1,984,755,392 |
| C16/500 node03 | 2.576 | 3.907 | 848.63 / 469.18 | 2,823,099,936 / 4,105,734,636 |
| C16/500 node13 | 2.956 | 4.487 | 1,048.51 / 562.30 | 4,105,734,636 / 2,823,099,936 |

File payload counters describe application slab payload, not backing-device I/O.
Whole-port data counters cover physical-name mlx5_00 through mlx5_07, excluding
bond aliases, with four-byte units converted to bytes. They include any traffic
on those ports. C16/500 approximate TX/RX rates were 41.63/60.55 MiB/s on node03
and 60.47/41.58 on node13; RX write-request deltas were 983,280/676,005, RX read
requests and TX discards zero. This is not RDMA saturation or exclusive Racer
wire accounting.

Memory-hit events dominated: C8 memory/disk/peer counts were 17,935/1,469/260 and
22,919/1,741/214; C16/500 counts were 22,183/1,898/331 and 25,348/2,279/264.
Counters are not disjoint request classes or byte shares. They support a mostly
local-cache workload, not an exact memory-served byte percentage.

Exact nested DP and LG cgroups were collected using current pod/container
identities. Maximum sampled DP memory was 19.549/19.204 GB at C8 and
19.533/19.124 GB at C16/500; LG memory stayed below 18 MB. All samples stayed below
75%, with zero memory-limit/OOM event increments. DP CPU throttling stayed zero.
**Loadgen throttling was nonzero at C16/500:** node03 had 9 periods/36,793 microseconds,
node13 12 periods/16,624 microseconds. These small observations are reported,
not attributed to the dataplanes or hidden by aggregate CPU averages.

DP process CPU uses utime+stime, measured CLK_TCK, and process-adjacent uptime.
Loadgen process CPU comes from its exported process counter. Cgroup reads are
slightly staggered and include exec-helper activity. Root-cgroup memory events
are not assigned to containers. No backing-device before/after window baseline
was collected, so physical `/dev/sda2` throughput/utilization is unavailable.

## Aborted points and boundary interpretation

### C16 with the original absolute cap of 50

The partial observation lasted about 19.5s. The stop sample had 49 cumulative
failures on node03 and 51 on node13; node13 crossed the absolute cap. Both ratios
were below 5%. After C0 propagation/drain, raw totals were 63/68 failures,
overshoot 13/18 relative to 50. No completed 60s C16/50 point exists.

That run exposed stale second-node reporting when policy checking raised on the
first node. Its raw snapshots establish the corrected totals; its original
`result.policy` is explicitly labeled unreliable for final node03 counts. The
subsequent reviewed accounting fix preserves both nodes and recomputes final
totals from the raw drain snapshot. Old artifacts were not rewritten.

### C24 with cap 500

The first activation sample had **10 failures among 167 completed operations on
node03: 5.9880%**. Node13 had 9/433, or 2.0785%. The ratio guard stopped the run
before an observation window began. This is a small-sample activation failure,
**not a measured sustained C24 ceiling or a hardware plateau**.

| C24 phase | node03 success/failure | node13 success/failure |
| --- | ---: | ---: |
| Activation-to-abort | 157 / 10 | 424 / 9 |
| Abort-to-drained tail | 1,036 / 45 | 894 / 46 |
| Total | 1,193 / 55 | 1,318 / 55 |

Final ratios fell to **4.4071%/4.0058%**. Later successful completions do not erase
the earlier 5.9880% breach. Node03 sampled ratio excess was 0.9880 percentage
points; no cap-500 overshoot. Final failures were 12 HTTP-status/43 incomplete
and 16 HTTP-status/39 incomplete, with zero digest/size/unknown increments.

C24 before-to-drained CPU/resource averages include idle activation and stopping
time, not steady C24 load. Exact-container memory events stayed zero; DP throttling
was zero, while LG throttling increased 235,611/200,252 microseconds. Storage
invariants held and C0/drain was confirmed. There was no retry or C32 run.

An independent audit found that the first offline memory-maximum calculation
omitted propagation samples. Including every available measurement phase gives
C24 loadgen maxima of **15,962,112 bytes on node03 and 17,256,448 bytes on node13**,
both from the first propagation snapshot. The earlier values 15,757,312 and
16,011,264 were drain samples, not maxima. All four runs were recomputed; no other
sampled maxima or existing headline counts, rates, ratios, or latencies changed.
Raw snapshots and superseded derivations remain preserved; `analysis-audited`
files record the corrected maxima and their source samples.

## Attribution remains unresolved

Post-C0 journals retain `Overloaded` at first-slice and partial-delivery boundaries.
General admission tails show pipe, ciphertext, dirty-ciphertext and occasionally
ingress-connection pressure. Request IDs are absent from many admission records;
terminal resource facts are unknown; bounded rings overwrite entries. Empty
fill-final journals have fill-only coverage and are not proof of no pressure.

The deployed client path can propagate auxiliary readiness errors while a slice
is pending (`cmd/racer-dataplane/src/client.rs:280-295,369-398`), but first-slice
acquisition follows a different path (`:328-342`). The native retry alarms use
the ordinary worker reactor (`src/rdma/retry.rs:40-56`). **No captured evidence
proves that reactor admission, retry alarms, or any one resource caused these
specific live failures.** Counts and low average CPU do not settle causality.

Request-correlated protected diagnostics remain the useful next step before
claiming an exact limiting gate. Do not infer an automatic budget increase or
permission for further load from these results.

## Final state and preservation

At the last phase's 23:45 UTC confirmation, both loadgens were applied C0 with
zero in-flight work; both dataplanes remained Ready with stable identities and
matching membership. The approved image, plaintext/ciphertext/registered budgets,
catalog, file-slab isolation, repaired-disk fence, model PV, and ClusterVolume
identities were unchanged. Raw FDs stayed zero; original raw-cache checkpoint
hashes and NVMe write/discard counters were unchanged. This report phase did not
repeat load, preflight, or configuration actions.

Restricted evidence is preserved under
`tmp/racer-gpu-concurrency-20261007-artifacts/` in the original workspace:

- All new worktree `tmp/ops-*` scripts, reviews, tests, checkpoints, raw snapshots,
  journals, analysis, reports, and formatting output.
- The committed report and a source snapshot of the final runner.
- A verified dependency subset from
  `tmp/racer-gpu-progress-fix-20261007-artifacts/`, with its original manifest and
  exact hashes. The runner's read-only archive import and expected original path
  are documented in the preservation metadata. The original archive is unchanged.
- `SHA256SUMS` and its digest, with copied-file hash verification. Directories are
  0700 and files 0600. No Secret object, private identity contents, token contents,
  or whole kubeconfig is included. Build targets and unrelated directories are
  excluded rather than recursively copied.

Primary run prefixes are `ops-curve-c8-01`, `ops-curve-c16-01` (cap-50 abort),
`ops-curve-c16-cap500-01`, and `ops-curve-c24-cap500-01`. Each includes activation,
available observations, drain proof, final control/config, and journals. Analysis
labels an activation abort separately rather than inventing a steady window.
`ops-checkpoint.md` contains exact commands, CAS results, review decisions,
deadlines, errors, recovery, and parent ownership. No cleanup or worktree removal
occurred before the parent audit.
