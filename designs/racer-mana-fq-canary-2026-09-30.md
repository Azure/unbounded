# Racer MANA FQ canary: no verified-throughput recovery

## Decision

Do not deploy FQ as a remedy for the observed zero-goodput nodes. A one-node,
eight-leaf FQ canary changed queue accounting and reduced some queue pressure,
but did not restore complete verified image delivery. The original scheduler
configuration was restored on September 30, 2026 at **23:12:19 UTC**. No production
tuning or load resumption is part of this result.

## Experiment and measurements

The 1,500-node Gantry/Racer campaign used runtime images built at `3235e717`.
The canary and controls were Standard_D8ds_v6 VMs with MANA, eight TX queues,
TX ring setting 256, and kernel package `6.8.0-1067.75`.

- Canary: Ddsv6 node suffix `0000bl`, `ens1`, eight FQ leaves under `mq 1:`.
- Failing control: suffix `00005f`, unchanged default `mq`/`fq_codel`.
- Healthy control: suffix `000023`, unchanged default `mq`/`fq_codel`.
- Applied load: eight concurrent verified image pulls per node.
- Install: 23:00:59 UTC, while all 1,500 load generators were paused and drained.
- Direct observations: 23:03:12-23:03:32 and 23:04:22-23:05:02 UTC.

Results for the later 40-second window:

| Metric | FQ canary | Failing control | Healthy control |
| --- | ---: | ---: | ---: |
| Complete verified images | 0 | 0 | 27 |
| Failed pulls | 12 | 9 | 0 |
| Verified bytes added | 0 | 0 | 14,451,754,960 |
| VF TX, decimal Gb/s | 4.220 | 4.214 | 9.913 |
| Median established peer TCP RTT, ms | 13.554 | 13.877 | 1.567 |
| MANA TX queue-stop increments | 251,501 | 552,879 | 0 |
| Qdisc drop increments | 0 | 37 | 0 |
| Mean aggregate pending TX SKBs | 1,073 | 1,168 | 8 |

Across the two noncontiguous measured windows, totaling 60 seconds, the canary
completed zero images and recorded 20 failed pulls. The healthy control completed
41 images. The canary's lifetime success count remained 34 between the first
direct observation at 23:02:40 and the final check at 23:05:41; lifetime successes
must not be credited to this measurement window.

Verified bytes, not NIC traffic, measure usable completion:
[`cmd/racer-loadgen/pull.go:177-204`](../cmd/racer-loadgen/pull.go) credits an image
only after full successful verification. Its error-path tests require zero
verified credit in
[`cmd/racer-loadgen/pull_test.go:239-247`](../cmd/racer-loadgen/pull_test.go).
These observations do not establish full VM bandwidth saturation.

### Qualifications

The fleet also rolled opaque relay back to its default false setting before this
run. The concurrent failing control shared that change but not the FQ change.
Earlier relay-false/concurrency-eight observations had the same broad shape:
zero verified goodput on the failing nodes, roughly 4 Gb/s VF TX, and a healthy
control near 10 Gb/s VF TX. This was not a randomized crossover, and cache state,
placement, projection timing, and offered peer traffic can differ between nodes.

The absent `RACER_OPAQUE_RELAY` variable defaults to false in
[`config.rs:168-172`](../cmd/racer-dataplane/src/config.rs); the parser test at
[`config.rs:1173-1190`](../cmd/racer-dataplane/src/config.rs) checks both explicit
values and the default. Do not infer configuration solely from a rollout label.

The fleet's concurrency-file projection was gradual. Direct metrics confirmed
all three sampled hosts at concurrency eight before each measured window;
Prometheus confirmed all 1,500 at eight by 23:05:41. Short rate windows with too
few scrape samples returned empty vectors, not proof of zero failures.

Qdisc counters have different meanings across FQ and fq_codel. Zero FQ drops or
fewer queue-stop events are not sufficient evidence of application improvement.

## Queue mechanism and remaining uncertainty

Read-only one-second debugfs samples showed continuously advancing completion
queues, not a seconds-long CQ hang. Failing hosts had persistently occupied MANA
send queues and substantial software backlog. Healthy queues were usually nearly
empty. Earlier matched captures located substantial ACK delay after the failing
host's egress capture point. This leaves driver/NIC service and provider egress
behavior unresolved; it does not prove a physical-network reordering defect.

Useful numeric fields are under
`/sys/kernel/debug/mana/<PCI-device>/vport0/TX-*`:
`sq_head`, `sq_tail`, `sq_pend_skb_qlen`, and `cq_head`. These are independent
reads, not an atomic snapshot. Reject implausible head/tail differences caused by
concurrent progress; do not interpret unsigned underflow as enormous occupancy.
Raw `*_dump` files are binary, not text diagnostics.

The installed Azure header includes `work_done_since_doorbell`. Thus the kernel's
`6.8` version alone does not establish that the upstream MANA doorbell fix is
missing. Check exact distribution backports before recommending a kernel repair.
Increasing a TX ring may only add buffering and can recreate driver queues;
it is not an authorized or demonstrated fix from this experiment.

## Reusable scheduler safety findings

An anonymous network namespace with an eight-queue, nonpersistent TAP reproduced
the default `mq 0:` plus eight `fq_codel 0:` leaves on local kernel 7.0.0-30.
The fixture established:

1. `parent :3` cannot address a leaf under the automatic handle-zero root for a
   normal `tc qdisc replace`. The kernel rejects parent lookup.
2. `tc qdisc change ... root handle 1: mq` is not an in-place root rename.
3. Replacing the root with named `mq 1:` permits leaf replacements under `1:1`
   through `1:8`.
4. Deleting the named root recreates the automatic root and default leaves on
   an UP device. The fixture's original and restored configurations were equal.
5. This restoration was also verified on the Azure canary, including all nine
   kinds, handles, parents, and options. Counters and queued packets are not
   restored.

Upstream Linux v6.8 provides the mechanism:

- [`sch_api.c:300-305,1628-1634`](https://github.com/torvalds/linux/blob/v6.8/net/sched/sch_api.c#L300): handle-zero lookup fails.
- [`sch_api.c:1136-1180`](https://github.com/torvalds/linux/blob/v6.8/net/sched/sch_api.c#L1136): root graft deactivates and reactivates the scheduler.
- [`sch_mq.c:175-189`](https://github.com/torvalds/linux/blob/v6.8/net/sched/sch_mq.c#L175): even a single MQ leaf graft deactivates the device scheduler.
- [`sch_generic.c:1280-1297,1341-1366`](https://github.com/torvalds/linux/blob/v6.8/net/sched/sch_generic.c#L1280): deactivation resets all TX queues.
- [`sch_generic.c:1172-1190,1230-1240`](https://github.com/torvalds/linux/blob/v6.8/net/sched/sch_generic.c#L1172): activation recreates default qdiscs when the root is absent.

**A leaf-only change can purge other leaves' queued packets.** Sequential changes
are not atomic, and a `tc` batch does not make them transactional. Pause and drain
all application work, all software queues, and all MANA send queues before both
installation and planned rollback. Background host traffic still prevents a
guarantee of zero packet loss. Scheduler quiescing is not a physical NIC restart.

### Guarded procedure used, not a tuning recommendation

The coordinator owns workload pause/resume and explicit single-host authorization.
Every command needs an external TERM timeout and a cleanup allowance. Before any
change, snapshot all nine configurations and counters; pin node and interface
identity; verify carrier, MTU, queue count, readiness, zero application inflight,
zero software backlog, and zero MANA pending work. Verify
`net.core.default_qdisc=fq_codel` and that the original leaves use default options.

With those gates satisfied, the authorized installation used:

```text
tc qdisc replace dev ens1 root handle 1: mq
tc qdisc replace dev ens1 parent 1:N handle 10N: fq \
  limit 10240 flow_limit 1024 quantum 1514 initial_quantum 15140 pacing
```

Expand `N` to each of 1 through 8 as explicit arguments. Verify the named root,
all eight leaves, and health before the coordinator resumes workload. Any partial
installation failure must enter rollback; leave no mixed configuration behind.

After the coordinator pauses and drains again, reversal is:

```text
tc qdisc del dev ens1 root
```

Delete only the installed named root. If a read shows the exact original automatic
configuration already present, verify it without deleting again. Do not attempt
`replace ... handle 0: mq`: creation allocates a nonzero handle.

The restored leaves in this experiment had limit 10240, flows 1024, quantum 1514,
target 5 ms, interval 100 ms, memory limit 33554432, ECN enabled, and drop batch 64.
The observed JSON encoded target/interval as 4999/99999. Compare saved JSON values
by kind/handle/parent/options, ignoring order and reset counters. Automatic rollback
depends on the current default qdisc and cannot recreate arbitrary custom leaves.

## Completion and follow-up

Installation was verified at 23:00:59 UTC. Rollback at **23:12:19.614 UTC** verified
all nine original configurations, health `ok`, readiness `ready`, carrier up,
MTU 1500, and local concurrency/inflight zero. Fleet guards immediately before
rollback confirmed 1,500 applied-zero targets and zero total inflight. Load was
not resumed by the canary worker.

Do not retain or expand FQ based on this result. The next independent read-only
step is exact Azure driver/backport and provider-egress investigation, using the
matched SQ/CQ evidence rather than another unchanged application restart or
packet trace. Any ring or driver experiment requires separate authorization and
a reviewed recovery plan. Keep verification, deadlines, and circuit breakers.

Raw, uncommitted campaign artifacts were recorded under the assigned operational
worktree's `tmp/`: `fq-all8-install-evidence.jsonl`, `mana-queue-fq-ramp.jsonl`,
`mana-queue-fq-late.jsonl`, and `fq-all8-rollback-evidence.jsonl`. This document
retains the representative findings; those temporary files are not a public API
or a permanent reproducibility dependency.
