# Racer read tail latency: October 12, 2026

## Result

Added and deployed four panels to **Racer Direct Zipf** (dashboard UID
`racer-direct-zipf`): whole-read percentiles, fractions above latency thresholds,
completion outcomes, and in-flight/completion/scrape coverage. All 21 dashboard
queries returned data; Grafana returned the expected ten-panel dashboard.
The original six panels remain unchanged. Dashboard tests passed (10), as did
the Zipf render tests (5).

`make fmt` completed gofumpt but failed in golangci-lint: the installed linter
was built with Go 1.26 and encountered source requiring Go 1.27. No application
code was changed. This is not a successful broad lint result.

No service instrumentation, images, concurrency, or node settings changed.
No code optimization had both a demonstrated connection to the tail and a
low-risk implementation. This is a baseline and diagnosis, not a before/after
speedup claim.

## Scope and measurement

Context: `joolshev-nvme-test`, namespace `unbounded-system`, 100 loadgens at
concurrency 16 each, reading 1 GiB blobs through the SDK and local dataplane.
Each blob contains 64 pages of 16 MiB. Dataplane/controller image tag:
`f9a088a22a29cf8549e1945af833f508a14199d1`; loadgen:
`ee7aa7d2ab336da93106cc0d7f10874f71fb8207`. Source inspection used this branch's
worktree based on `8ba9adfe4`; it does not prove exact deployed-source identity.

The percentile panel uses this query, substituting 0.50, 0.95, 0.99, and 0.999:

```promql
histogram_quantile(0.99,
  sum by (le) (
    rate(racer_loadgen_pull_duration_seconds_bucket{
      job="kubernetes-pods", namespace="${namespace}",
      app_kubernetes_io_name="racer-loadgen",
      racer_unbounded_cloud_io_benchmark="zipf-16t-v3",
      result="success"
    }[5m])
  )
)
```

This measures completed whole-blob operations, including SDK admission and
body consumption, validation, and close, not page latency or time to first byte
(`cmd/racer-loadgen/pull.go:205-218,251-311,346-425`). Digest verification was
disabled; length and metadata validation remain. Failures and in-progress reads
are excluded from success quantiles and shown separately.

Buckets include 5, 10, and 30 seconds with no boundaries between them
(`cmd/racer-loadgen/metrics.go:33-46`). Quantiles are coarse interpolations.
The directly bucketed **fraction exceeding 10 seconds** is a more reliable tail
comparison. Live boundary labels are `5.0`, `10.0`, and `30.0`.

| UTC window | Successful reads/s | Above 10s | Estimated p99 | Estimated p99.9 | Loadgen errors/s |
| --- | ---: | ---: | ---: | ---: | ---: |
| 20:46-21:01 | 618.42 | 0.1900% | 9.783s | 19.474s | 0 |
| 21:06:15-21:11:15 | 620.08 | 0.1834% | 9.776s | 19.098s | 0 |

Six of the first window's ten worst nodes remained in the second window's top
ten. The original worst-ten group still had 2.9 times the above-10s share of
the other nodes. These are Prometheus rate estimates, not exact boundary counts.

## Findings and limits

### Receive-side pressure is real, but tail causation is not established

A synchronized 40-second sample at 21:15:45.895-21:16:25.895Z measured host
MANA NIC counters and TCP counters from each actual dataplane network namespace:

| Node suffix | Receive no-WQE discards/s | TCP retransmits / outgoing segments | Softnet budget/time-limit exits/s |
| --- | ---: | ---: | ---: |
| `00000g` | 569.7 | 0.1506% | 97.6 |
| `00001y` | 458.6 | 0.1197% | 95.0 |
| `000020` | 341.1 | 0.1099% | 61.4 |
| `000021` | 352.7 | 0.1267% | 47.1 |

The first two nodes are persistent tail outliers. At 21:17:15Z their five-minute
above-10s shares were 0.6785% and 0.5800%; `000021` had zero but still had drops
and retransmissions. Local receive loss generally causes remote retransmissions;
these unpaired counters cannot identify recovery delays on a slow read.

Upstream Linux maps `hc_rx_discards_no_wqe` to GDMA receive discards without
available receive work-queue entries
([v6.8 mana_en.c:2381-2432](https://github.com/torvalds/linux/blob/v6.8/drivers/net/ethernet/microsoft/mana/mana_en.c#L2381-L2432)).
The installed Azure kernel has backports; exact distribution source was not
available. Burst traffic or delayed receive processing are hypotheses, not
established causes. Softnet budget exits are not measured scheduling delay.

RX rings were 1,024 entries, maximum 8,192, with 32 channels. Increasing rings
is not a harmless service patch: upstream MANA detaches and reattaches the
device when changing ring parameters
([v6.17 mana_ethtool.c:466-484](https://github.com/torvalds/linux/blob/v6.17/drivers/net/ethernet/microsoft/mana/mana_ethtool.c#L466-L484)).

### Ordered two-page reads can amplify a slow page

The SDK grants two page credits and requests ordered delivery
(`pkg/racersdk/client.go:53-54,370-379`). The server polls acquisitions
concurrently, but delivers the front page first
(`cmd/racer-dataplane/src/read/range_stream.rs:686-710,745-805,1002-1025`).
When page N stalls and N+1 is ready, both credits remain occupied and N+2 cannot
start. The test explicitly asserts this ordering and start limit
(`cmd/racer-dataplane/src/read/tests/hot_reads.rs:986-1017`). This is a verified
mechanism, not a measured attribution of the multi-second tail.

Raising the server window alone cannot override SDK credits and can increase
memory floors enough to reduce the worker count
(`cmd/racer-dataplane/src/app.rs:2428-2438,2515-2553`). Flight admission can also
wait up to 100ms without rejection, so zero rejection counters do not exclude
queueing (`cmd/racer-dataplane/src/read/flight.rs:1106-1139,1182-1280`).

### CPU and disk costs do not identify the tail's critical path

The first window's worst-ten nodes averaged about 44 idle cores out of 64;
their hottest two cores averaged 61% busy. In the second window, the newly
ranked worst ten averaged 18.61 Gbps/node outbound versus 18.88 elsewhere;
the fleet maximum was 21.03 Gbps. Only six nodes overlap between those groups. The
[SKU limit](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/ddsv6-series)
is 30 Gbps outbound, not RX plus TX. This does not show sustained host-wide CPU
or egress saturation and does not exclude bursts or worker-local limits.

Dataplane file descriptors identify three raw NVMe devices as data backing.
Disk queueing and multi-millisecond block-read means were observed, but did not
track tail rank consistently. One zero-tail control had a 35ms block-read mean
while a persistent outlier had 3-5ms. Asynchronous I/O can queue despite low
CPU iowait. Device means are not page or whole-read tail measurements.

A validated Parca export for 21:15:15-21:15:30Z covered 100 node/pod groups.
Raw pprof and query-range CPU sums matched exactly: 21,922,789,079,074ns,
equivalent to 1,461.52 fleet cores. Flat CPU costs included ChaCha20 15.21%,
Poly1305 9.53%, kernel copy-from 6.87%, CRC64 4.62%, and page clearing 2.79%.
These are CPU costs, not evidence of off-CPU tail causation. Longer historical
Parca queries had incomplete coverage for an unresolved reason.

Crypto queue metrics exclude permit admission and completion-reap time
(`cmd/racer-dataplane/src/security.rs:1071-1138,2739-2785,2925-2939`). Peer timing
means sample selected successful HTTP Page exchanges, not every read path
(`cmd/racer-dataplane/src/peer.rs:38-103`). The receive gate appears disabled
from current-source defaults, absent deployment overrides, and zero activity;
effective runtime configuration was not verified. Its zero counters provide
no wait-time evidence (`src/config.rs:181-184` and
`src/peer/receive.rs:94-97`, relative to `cmd/racer-dataplane`).

### Rejected low-risk candidate: disk decode buffer checkout

Disk decode allocates and copies ciphertext with `to_vec`
(`cmd/racer-dataplane/src/store.rs:257-262`). In the validated profile, 3.80% of
CPU samples were unresolved libc leaves under this disk-copy path. Its output
already returns to the wiped-buffer recycler on final release
(`cmd/racer-dataplane/src/memory.rs:128-170`; `flow/src/lib.rs:743-750,854-857`,
relative to `cmd/racer-dataplane`).

Adding recycler checkout could save allocation on hits, but retains the copy.
The recycler has only two exact-capacity slots per quota authority, shared
across classes; pressure can clear it before checkout. Its standard miss path
also zero-fills fresh storage (`cmd/racer-dataplane/flow/src/lib.rs:407-456,674-707`).
A simple substitution could therefore regress misses. Hit availability and
tail benefit are unmeasured. No change was made; secure erasure must remain.

## Options for the next decision

1. **Recommended: sampled slow-read timelines.** Correlate Flight admission,
   disk completion, crypto permit/queue/execute/reap, ordered-head blocking,
   and SDK release/delivery delays. Use bounded sampling and avoid payloads or
   sensitive keys. Existing aggregate means cannot resolve these alternatives.
2. **Controlled receive-path experiment.** First verify the installed driver's
   behavior and correlate local drops with remote flow recovery. Then consider
   ring depth or IRQ/RSS settings on a controlled group, with a drain/restore
   plan. This is an operational experiment requiring review, not a low-risk
   Racer code fix.
3. **Conditional larger-credit experiment.** Only if ordered-head gaps dominate,
   test a bounded SDK credit increase with memory and admission guardrails.
   More in-flight work can worsen the underlying resource pressure.

Both live DaemonSets allow 100% unavailability and have zero termination grace.
Any later service rollout must first stop new load, verify all loadgens report
applied concurrency zero and in-flight zero, resolve the operator's authoritative
override, and retain parent-owned recovery before resuming load. No drain or
rollout was performed in this investigation.

Older prose in `designs/racer-tail-latency-controls.md:175-205` describes an SDK
PageWindow option and rollout tooling absent from this worktree. Current
`pkg/racersdk/types.go:137-154` has no such option. Do not use that prose as an
implementation or deployment guide without reconciling the drift.

Raw query responses, profile exports, scripts, and command checkpoints are
preserved locally in `tmp/racer-tail-evidence-20261012.tar.gz` (not committed).
Key files inside are `racer-tail-live-diagnostics.json`,
`racer-tail-live-batch2.json`, `racer-tail-live-batch3.json`,
`parca-final-validation.json`, `parca-final-aligned-merge.pprof`, and
`dashboard-live-queries.json`. Dashboard source and usage are under
`deploy/racer/grafana-direct-zipf*`.
