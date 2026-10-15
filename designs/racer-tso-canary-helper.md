# One-host TSO aggregation experiment helper

`hack/scripts/racer-tso-canary.py` implements one operational experiment, not a
recommended fleet setting. It was implemented without executing a cluster change.
The parent owns load, integrity recovery, and any decision to run it.

## Hypothesis and limits

The October 1 matched C2 window showed similar TX bytes but 669,244 versus 419,460
TSO submissions on extreme `0000bl` and healthy `00002b`. Mean TSO payload was
36,050 versus 55,821 bytes. Most additional submissions were non-TSO, so larger
TSO cannot eliminate all additional work. Pending SKBs averaged 456 versus 3,
while every CQ advanced. This is not proof of a faulty driver or link saturation.

The only proposed setting is `net.ipv4.tcp_min_tso_segs=2 -> 32 -> 2` on `0000bl`.
Keep offloads, rings, scheduler, application settings, and load unchanged. At MSS
1448, 32 segments target about 46 KiB, below the observed GSO limit. TCP windows,
available data, and congestion control still constrain the actual segmentation.

Upstream Linux v6.8 `net/ipv4/tcp_output.c:2005-2035,2743-2778` reads this setting
during segmentation selection; `net/ipv4/sysctl_net_ipv4.c:1269-1276` uses a plain
runtime sysctl handler. This does not resize/recreate a NIC ring, purge a qdisc,
or require an application restart. Therefore this experiment deliberately uses
a live baseline/change/restore sequence instead of the earlier conservative C0
proposal. It is not disruption-free: larger bursts can worsen loss and latency
for **all host-network TCP**, including control services. Exact Azure source
backports were not obtained; functional measurements are mandatory.

FQ and 32 KiB TCP Small Queues experiments already failed to establish recovery.
Do not combine or repeat them as part of this tool. Keep CRC, AEAD, digest checks,
deadlines, and connection poisoning enabled. Existing AEAD rejects are not waived:
new local rejects abort even when the baseline cumulative counter is nonzero.

## Invocation and authorization

Run from the assigned worktree. Commands create only local output/checkpoints in
`tmp/`; collect mode makes no host writes, including no rollback writes.

```sh
timeout --signal=TERM --kill-after=10s 300s \
  python3 -u -B hack/scripts/racer-tso-canary.py collect --concurrency 4 \
  > tmp/racer-tso-collect.jsonl 2>&1
```

Only after parent authorization, stable images, stable fleet C4, exclusive
ownership of this sysctl, and an independent recovery operator/path:

```sh
timeout --signal=TERM --kill-after=10s 300s \
  python3 -u -B hack/scripts/racer-tso-canary.py canary --concurrency 4 \
  --authorize '0000bl:tcp_min_tso_segs:2->32->2' \
  > tmp/racer-tso-canary.jsonl 2>&1
```

Use a fresh output filename for every run; do not overwrite evidence. Default
checkpoint is `tmp/racer-tso-canary-checkpoint.md`, appended and flushed with UTC,
commands, stage/next action, and errors. Remote events stream every sample; parent
heartbeats occur every five seconds. Raw JSONL contains numeric NIC counters,
qdisc accounting, named debugfs queue fields, aggregate TCP memory/RTT, host TCP
counters, CPU counters, and allowlisted application metrics. No packet payload,
socket endpoints, environment, credentials, or binary debugfs dump is collected.

### Fixed identities and stages

- Context `joolshev-scale-test`, namespace `unbounded-system`, container `node`.
- Extreme: `aks-ddsv6-84072342-vmss0000bl`, pod `unbounded-net-node-57j4m`, IP
  `10.224.3.206`.
- Control: `aks-ddsv6-84072342-vmss00002b`, pod `unbounded-net-node-2kssd`, IP
  `10.224.2.121`.
- Resolve current loadgen pod IPs through the API, never assume old pod IPs.
- Fleet Prometheus applied concurrency must equal the requested value on 1500
  distinct nodes. Both original sysctls must be exactly 2. Remote hostname is
  independently checked. The parent must separately establish ready fleet,
  stable image IDs, no concurrent rollout, and any stricter fleet integrity guard.
- Concurrent remote hosts: 30-second baseline, 60-second changed/observe phase,
  then 30-second restored phase. Five-second sampling, plus command time, means
  boundaries are not perfectly synchronized. Use recorded timestamps, not nominal
  seconds. Baseline failures prevent the write. Control is never changed.
- Local health/readiness, fixed applied load, missing diagnostic metrics, counter
  resets, new rejection/corruption counters, or new failed pulls fail closed.
  Fleet-wide integrity is the parent's responsibility, not covered by two nodes.
- New image with unchanged counters is not conclusively detected; never roll
  during capture. No automatic continuation across a failed stage.

## Restoration and failure handling

Three layers: Python `finally` attempts restoration even after a lost mutation
acknowledgment; outer host shell traps EXIT/TERM/INT/HUP and restores; parent uses
a **new exec** to independently restore/read back after remote completion.
Remote execution has a 180-second TERM timeout and 10-second kill allowance;
local exec has a 200-second timeout. On ambiguous transport failure the parent
waits until 210 seconds after launch before independent rollback, avoiding a
late write from a remote still finishing baseline. Preflight is capped at 30
seconds; the entire invocation must have the external 300-second bound.

An unreachable host, SIGKILL, host failure, or timeout-killed cleanup can defeat
in-process guarantees. The tool returns failure, never a cleanup-success claim,
if independent restoration fails. Parent must retain this exact emergency path:

```sh
timeout --signal=TERM --kill-after=10s 25s \
  kubectl --context joolshev-scale-test --request-timeout=15s \
  -n unbounded-system exec unbounded-net-node-57j4m -c node -- \
  nsenter -t 1 -m -n -- \
  timeout --signal=TERM --kill-after=10s 10s sh -c '
    test "$(cat /proc/sys/kernel/hostname | tr A-Z a-z)" = aks-ddsv6-84072342-vmss0000bl || exit 41
    sysctl -w net.ipv4.tcp_min_tso_segs=2
    test "$(cat /proc/sys/net/ipv4/tcp_min_tso_segs)" = 2
  '
```

If issuing emergency rollback before remote completion, first stop/wait out the
remote runner or retain supervision through its deadline so it cannot later
enter changed phase. Never launch overlapping canaries. Independent helper
rollback refuses a value other than 2 or 32 rather than silently overwrite an
unknown concurrent experiment. Parent emergency rollback assumes exclusive
ownership. Separately verify health/readiness and control progress afterward.

## Interpretation gates (parent enforced, no auto-promotion)

`stage_summary` reports actual intervals, mean TSO payload, CQ completions per GB,
pending occupancy, NIC deltas, body sample count/mean, and verified bytes/goodput.
Raw samples retain the evidence needed for additional analysis. Success gates:

1. Functional: TSO payload grows and CQ completions/GB falls at least 15%.
2. Pressure: mean pending SKBs falls at least 30%, stop/wake per GB declines, and
   all CQs keep progressing. Five-second samples cannot exclude subsecond stalls.
3. Utility: sustained verified-image progress and at least 50% lower successful
   page-body mean, without worse errors, CPU, loss, or control performance.
4. Integrity: no new rejection or digest mismatch; no weakened validation.

These are diagnostic thresholds, not statistical proof. Compare same-image
baseline/control/after; useful change must reverse on restore. Low traffic or
zero completed body samples is inconclusive, not successful. Socket lifetime
retransmission aggregates are not interval retransmission counters; use SNMP
deltas. Do not add qdisc root and leaf accounting together. Body time includes
staging admission, and reports only eligible successful operations.

If functional/utility gates fail, restore and return the evidence; do not try a
larger value automatically. Parent can stop early for fleet integrity, health,
CPU, loss, or deadline regression. No host setting is retained after the run.

## Offline validation

```sh
timeout --signal=TERM --kill-after=10s 60s \
  python3 -B hack/scripts/racer-tso-canary_test.py
```

Tests use fake settings and shell functions only, including trap execution on
normal failure and TERM. No tests invoke Kubernetes or write real sysctls.
