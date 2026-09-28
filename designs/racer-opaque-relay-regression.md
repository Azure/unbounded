# Opaque relay stage-9 regression investigation

Base: `bc412276bc129f879be7e87aab984a77eff0d243`. No production changes,
configuration changes, cluster queries/mutations, load changes, push, or deployment.
The one commit adds regression coverage and improves the existing local benchmark.

## Decision

Keep the operational stage-8 rollback. **A pipe-count increase is not a sufficient
fix, and this investigation does not establish a production source defect.** Do not
replace fail-fast relay admission with pipe waiting, release relay admission early,
increase synchronous work, or change TCP/splice flags from this evidence alone.
The operational parent owns further baseline tuning; this task does not duplicate it.

The demonstrated failure mechanisms are shared pipe exhaustion at pipes128 and
remaining relay-capacity/deadline pressure at pipes512. The exact contribution of
each mechanism to the remaining fleet throughput gap is not identifiable from
these on-CPU profiles and finite failure rings.

## Verified capacity and lifetime behavior

Paths below are relative to `cmd/racer-dataplane/src/` and refer to this base.

1. `peer/server.rs:313-337` reserves Relay before downstream exchange and acquires
   a pipe only after a verified, nonempty downstream HTTP response. Pipe acquisition
   is immediate, not queued; failure signs Overloaded before sending any success
   head (`:367-369`). The acquired downstream body is discarded, not cached.
2. One pipe costs **one admission unit and two descriptors**, not two pipe units
   (`memory/pipe.rs:184-233`). Empty pipes keep their admission charge in the pool
   and are reusable (`:92-105,189-191`). A full gauge alone is not proof of active
   saturation. Partially buffered pipes close instead of reentering the pool.
3. Delivery shares the same worker-local pipe pool (`app.rs:586-588,805`).
   It queues before acquiring new pages and holds a pipe during acquisition
   (`read/range_stream.rs:279-342`). Relay uses immediate `acquire`, so it does not
   honor delivery FIFO order. Replacing it with `acquire_wait` after receiving a
   body would hold downstream connections while waiting for capacity potentially
   reserved by local acquisitions. This changes the dependency graph and is not
   justified as a harmless scheduling fix.
4. Opaque transit retains both connections and Relay admission until the reverse
   body has drained (`http/relay.rs:14-20,174-216`). Backpressure stops downstream
   receipt until the buffered chunk drains (`:79-136`). The materialized path can
   finish/pool downstream before reverse sending (`peer/transfer.rs:361-418`);
   its Relay reservation ends when `Relay::forward` returns
   (`peer/relay.rs:62-65,83-86,124-136`). Therefore the same relay limit does **not**
   represent the same occupancy/service time between the two paths. Increasing
   pipes cannot remove the separate relay or outbound-connection ceilings.
5. Waiting for TCP readiness retains ownership through original/cancel CQEs
   (`runtime/reactor.rs:741-768,280-294`). The 10 ms scope in `http/relay.rs:179-194`
   checks reverse disconnect while the source is silent; it is not a mandatory
   10 ms sleep per chunk. Readiness can complete immediately. Worker scheduling
   uses FuturesUnordered and a wakeable reactor wait (`app.rs:1353-1361`,
   `runtime/worker.rs:764-776`, `runtime/reactor.rs:930-985`). No lost wake or
   recurring forced pipe-wait delay was reproduced.

## Saved operational evidence

All artifact paths below are relative to the original workspace, not this worktree.

- `tmp/racer-stage9-results.md:7-26`: stage8 307.539822 GB/s / 98.483048%;
  stage9 pipes128 218.008699 GB/s / 52.372136%; pipes512 245.725731 GB/s /
  94.160521%. Stage9 also includes metrics sharding `29a08798`; this is not a
  relay-only image comparison or repeated crossover. Historical stage8 is not a
  new measurement of the restored deployment.
- Raw stage9 `*-opaque-summary.json:912-917`: 27,478 retained Pipe rejection
  records, 9,714 adjacent correlated PeerRelay/Pipe pairs. Raw
  `*-pipes512-summary.json:874-914`: 3,235 PeerReceiveBody deadlines, 3,983 Relay
  rejection records, 1,994 correlated PeerRelay/Relay pairs, and no Pipe entry.
  These are sampled retained records, not exhaustive terminal-failure counts.
  The complete filename prefix is `tmp/racer-relay-sweep-20260928-stage9`.
- `tmp/racer-stage9-results.md:113-123`: changing only pipes eliminated sampled
  pipe failures but left relay exhaustion at 64 per worker. A longer-lived relay
  slot under reverse backpressure is consistent with this shift, not proof of
  every failure's cause.
- Reactor-2 CPU profiles: stage8 `skb_segment` cumulative **5.37 s / 9.24%**;
  stage9 pipes128 **11.53 s / 19.85%**; pipes512 **12.74 s / 22.00%**.
  At pipes512, segmentation contains **5.21 s** in `pskb_expand_head`, **5.37 s**
  in `memset_orig`, and **2.79 s** in `__memcpy`. These overlap; do not add them.
  Extract with `go tool pprof -top -focus=skb_segment` from
  `tmp/parca-recovery-{stage8-SMT,stage9-opaque,stage9-pipes512}-racer-io-2.pprof`.
  A cumulative focused export shows Geneve tunnel segmentation/bridge forwarding
  under the splice send stack. This is concrete kernel networking work absent
  from plain loopback, not time sleeping in splice. It supports a target-node
  bottleneck, not a fleet-wide proof or a specific safe syscall-flag fix.
- No samples matched `Metrics|PipePool.*acquire_wait` in the pipes512 reactor-2
  export. Inlining and sampling prevent treating absence as exoneration. Metrics
  records only update counters (`telemetry/metrics.rs:223-235`); there is no
  demonstrated causal link warranting a metrics revert/local fork. The body A/B
  below holds the metrics implementation fixed in the same binary.

## Tests and local speed

Added `http::relay::tests::tcp_backpressure_recovers_and_wakes_queued_pipe_owner_without_losing_frame`:
real loopback TCP, 16 MiB + 16 bytes, bounded destination send buffer, stalled
reader, queued pipe acquisition, and resumed consumer. It checks retained
admission during the stall, exact body, recovered pipe capacity, reusable reverse
connection, next-frame bytes, zero live connection/relay charges, and empty pipe
reuse. The resumed transfer runs with FuturesUnordered, eventfd wakeups, and
`Reactor::wait`, not a continuously polled noop-waker future. Existing tests cover
cancel/deadline/drop fences, truncation, fallback, signed metadata/AEAD, and FIFO
queue admission/cancellation. No deadline, crypto, or completion bounds changed.

The existing ignored body benchmark now uses the same wake-driven harness. Eight
16 MiB + 16-byte bodies per sample; same binary, materialized/opaque/opaque/
materialized ordering, producer and consumer on separate threads:

| Path | Wall ms | Relay thread CPU ms |
| --- | ---: | ---: |
| Materialized | 179.909 | 88.236 |
| Opaque | 120.381 | 46.901 |
| Opaque | 116.412 | 48.492 |
| Materialized | 148.700 | 67.951 |

This demonstrates local progress/speed, not production NIC saturation. It does
not model multi-node backpressure, physical networking/Geneve, or fleet routing.
No speed threshold is asserted in the correctness test.

Validation: all-feature library **810 passed, 10 explicit ignores**; production
dataplane integration **15 passed, 2 explicit ignores**; explicit body benchmark
passed; Rust formatting passed; scoped `make fmt` for `internal/racer/wire` passed
with zero issues and no Go changes. Initial test development incorrectly expected
zero RequestContext despite HttpIo's retained buffers; corrected to check live
resource release and bounded retained buffers. A nonexistent SDK formatter path
was corrected to the existing wire package. All commands used external TERM,
kill-after-10s bounds no greater than 300 seconds. No command timed out.

## Actionable next gate

Do not promote opaque relay based on its loopback gain or the pipes512 relief.
A future source change needs either a deterministic failing lifecycle/scheduling
test or an isolated measurement of **relay slot duration, reverse-send blocked
duration, downstream checkout occupancy, and per-worker runnable delay**, alongside
the physical-network segmentation cost. That separates queue latency from active
relay service capacity. A controlled network-realistic materialized/opaque A/B
must hold metrics, routing/load, capacity limits, and security behavior fixed.
Until then, preserve the better observed baseline and the existing fail-closed
limits rather than masking pressure with speculative queues or early releases.
