# Kernel CPU investigation, 2026-10-05

## 17:45 UTC: initial checkpoint

User requested all prior fixes committed, then definitive kernel CPU attribution. Original racer-v2 clean at d1095d7f0; ancestor checks confirmed a5ca6ea7f, f2decda59, d1095d7f0 and all three experiment records tracked. Prior work changed live benchmark configuration, not application code. Other existing worktrees are unrelated and untouched. New detached worktree tmp/racer-kernel-profile.

Load remains C8/nohash, Gantry quota4/P4, warm one-image catalog. No workload changes planned. Parent owns all profiling actions and cleanup. Each phase deadline5min, heartbeat at collection checkpoints (maximum60s); commands external TERM timeout with kill-after10s. Success: recover distinct symbols and validate dominant on-CPU stacks independently across hardware cohorts. No error.

Read-only research found raw Parca PPROF retains distinct kernel function IDs/SystemName, while Function.Name is empty. v0.28.0 TOP aggregates by Name alone (`https://github.com/parca-dev/parca/blob/v0.28.0/pkg/query/top.go#L108-L131`), collapsing all empty names. Zero addresses are not the aggregation key in this branch. Normalize exported copies only, preserving raw profiles. Existing privileged net-node pods provide host BCC/perf tools without installs or sysctl changes. Next bounded phase: raw profile export/analysis tool plus independent stdout-only BCC kernel-stack sampling on three nodes; retain artifacts only in this worktree.

## 17:49-17:54 UTC: collection completed

No load, workload configuration, host sysctl, or image changes. Independent BCC collection used existing `unbounded-net-node-4lxr9`, `unbounded-net-node-j56vl`, and `unbounded-net-node-7fcg8`, container `node`. These run on D8d_v5 `aks-ddv5-17198779-vmss0000ad`, D8ads_v5 `aks-adsv5-13731677-vmss00006u`, and D8ds_v6 `aks-ddsv6-84072342-vmss000000`, respectively. All have 8 logical CPUs and kernel 6.8.0-1067-azure.

Exact collection command, substituting POD:

```sh
timeout --signal=TERM --kill-after=10s 85s \
  kubectl --context joolshev-scale-test -n unbounded-system \
  exec POD -c node -- chroot /proc/1/root \
  timeout --signal=TERM --kill-after=10s 65s \
  env PYTHONDONTWRITEBYTECODE=1 python3 -B \
  /usr/share/bcc/tools/profile -K -F 49 30
```

The concurrent command envelopes were 17:49:00.860-17:49:37.026Z; actual 30-second sampling starts after compilation/attachment. All returned zero. Readiness/restarts unchanged. No profiler processes/new profiler BPF programs remained. Initial help inspection omitted Python's `-B`; incidental Python bytecode creation cannot be ruled out. Sampling itself disabled bytecode writes and used stdout, without host capture files.

`-K` disables user stack capture, not user-mode sampling. Empty stacks (15-18% node-wide) are likely user execution, not necessarily lost kernel samples. Source explicitly permits EFAULT when kernel stacks are unavailable in user context. Preserve empty, unresolved, and resolved buckets separately. BCC emitted 11/16/15 missing-stack warnings; weighted unresolved/missed fractions were 4.46%/6.19%/4.60% of retained node samples. Idle PID0 excluded.

### Independent BCC confirmation

Percentages below are leaf samples divided by all retained samples for that process, including empty and unresolved stacks. They are not percentages of kernel-only CPU or byte counts.

| Process / leaf | D8d_v5 | D8ads_v5 | D8ds_v6 |
|---|---:|---:|---:|
| Dataplane `_copy_from_iter` | 32.31% | 24.93% | 31.48% |
| Dataplane `clear_page_erms` | 18.25% | 11.54% | 14.59% |
| Loadgen `_copy_to_iter` | 33.66% | 20.47% | 32.83% |
| Gantry `_raw_spin_unlock_irqrestore` | 5.99% | 6.33% | 7.42% |

Dataplane sample denominators: 3847/3293/3837; Gantry 3589/4047/3748; loadgen 2641/2873/2562. Fully resolved/nonempty counts respectively: dataplane 2976/2367/2896, Gantry 2949/3332/3171, loadgen 2197/2363/2239. Empty counts respectively: dataplane 784/792/753, Gantry 395/362/375, loadgen 305/354/244. Remaining samples are unresolved/missed.

### Raw Parca confirmation

Fixed window 17:48:30-17:50:00Z, selector includes `node="aks-ddv5-17198779-vmss0000ad"` and each of `container="gantry"`, `"dataplane"`, `"racer-loadgen"`. Query-range coverage verified one comm/container/pod series per export with six 15-second bins, exactly matching profile CPU sums.

API: `/api/v1/namespaces/parca/services/parca-profile-store:7070/proxy/api/profiles/query`. GET parameters: `mode=MODE_MERGE`, `report_type=REPORT_TYPE_PPROF`, `merge.query=parca_agent:samples:count:cpu:nanoseconds:delta{...}`, `merge.start`/`merge.end` RFC3339. Base64 decode `pprof`, then analyze with `hack/cmd/racer-profile-summary`. Raw function identity/mapping preserved, empty Name filled from SystemName in exported copies only.

| Process | Total sampled CPU seconds | Kernel leaf CPU seconds | Kernel share |
|---|---:|---:|---:|
| Gantry | 212.000 | 186.000 | 87.74% |
| Dataplane | 231.842 | 185.526 | 80.02% |
| Loadgen | 158.947 | 139.579 | 87.81% |

Definitive leaf costs: dataplane `_copy_from_iter` 77.632 CPU-s (**33.48% total CPU**), separate `clear_page_erms`/zeroing 43.789 CPU-s (**18.89%**); loadgen `_copy_to_iter` 58.842 CPU-s (**37.02%**). Copying plus zeroing in these two processes alone accounts for roughly 30% of the combined three-process CPU. This is not a claim about achievable speedup.

Gantry has 171.842 CPU-s (**81.06% total**) on stacks containing splice functions and 132.368 CPU-s (**62.44%**) on stacks containing TCP functions. These cumulative unions overlap and must not be added. No single leaf dominates: network processing, allocation/refcount, socket release, and wakeups are distributed costs. `nft_do_chain` is 4.421 CPU-s (**2.09%**) flat, 14.158 CPU-s (**6.68%**) cumulative, so firewall evaluation is observable but not the dominant total cost. Gantry copy leaves total 3.316 CPU-s (**1.56%**); checksum leaves 0.368 CPU-s (**0.17%**).

Representative original stacks, leaf to root:

```text
dataplane (one stack, 42.211 CPU-s):
_copy_from_iter <- copy_page_from_iter <- skb_copy_datagram_from_iter
  <- unix_stream_sendmsg <- __sys_sendto <- ... <- __send <- try_send

dataplane (one stack, 26.105 CPU-s):
clear_page_erms <- get_page_from_freelist <- __alloc_pages <- alloc_pages_mpol
  <- alloc_pages <- alloc_skb_with_frags <- sock_alloc_send_pskb <- unix_stream_sendmsg

loadgen (one stack, 50.737 CPU-s):
_copy_to_iter <- simple_copy_to_iter <- __skb_datagram_iter <- skb_copy_datagram_iter
  <- tcp_recvmsg_locked <- tcp_recvmsg <- inet_recvmsg <- sock_recvmsg
  <- sock_read_iter <- vfs_read

gantry (one stack, 3.158 CPU-s):
_raw_spin_unlock_irqrestore <- __wake_up_sync_key <- unix_write_space <- sock_wfree
  <- unix_destruct_scm <- skb_release_head_state <- consume_skb
  <- unix_stream_read_generic <- unix_stream_splice_read <- sock_splice_read
  <- ... <- syscall.Splice
```

### Meaning and limits

The large kernel cost is now attributable to actual data movement: copying and allocating/clearing kernel pages on the dataplane's Unix-socket send, Gantry's splice/TCP forwarding and buffer bookkeeping, and copying TCP receives into loadgen userspace. This is not a cache-miss or origin-fetch explanation. Prometheus at 17:50Z over two minutes: all 1500 loadgens scraped and C8, 9.24208 TiB/s received, 18715.92 successful pulls/s, only success result series, zero plaintext misses and peer acquisitions. All three DaemonSets remained 1500/1500 Ready.

This is definitive path attribution, not complete instruction-level accounting or proof of one removable bottleneck. Parca unresolved leaves: 7.77%/5.22%/2.85% of total Gantry/dataplane/loadgen CPU; some dataplane unresolved leaves have no kernel mapping. Software clock interrupts can be delayed during IRQ-disabled sections, biasing samples toward IRQ restoration. Therefore an irqrestore leaf is not proof of expensive unlock instructions or lock contention. Neither memory-bandwidth saturation nor physical NIC saturation has been measured. Three nodes establish cross-cohort corroboration, not fleet-wide profiles.

Raw Parca duration is zero and one header timestamp reports year 2184. Saved API requests and query-range coverage establish the real window; do not use the malformed header. The TOP collapse is separately proven: raw Gantry profiles contain hundreds of distinct kernel SystemNames, while TOP aggregates by empty Function.Name and retains an arbitrary symbol as metadata. No server change was made.

### Implementation cross-check and next work

Current checkout, not established identical to deployed images: loadgen uses a 32 KiB buffer (`cmd/racer-loadgen/pull.go:148-151`) and repeated body.Read even with verify=false (`:412-453`). Gantry preserves UnixConn plus ReaderFrom forwarding (`pkg/racersdk/http_stream.go:279-295`); enabling splice is not a new fix. Dataplane delivery initially copies to a pipe then splices, and can fall back to ordinary sends (`cmd/racer-dataplane/http/src/delivery.rs:146-212`, `flow/src/pipe.rs:337-358,420-449`). The deployed stacks directly establish ordinary Unix-socket send copying independently of source-version equivalence.

Highest-value next experiments: quantify deployed dataplane pipe/copy fallback and backpressure; test loadgen read-buffer amortization separately. Removing the initial copy/zeroing requires a safe buffer/page lifetime design, not simply enabling Gantry splice or increasing concurrency. Larger read buffers may reduce syscall overhead, not eliminate payload copies. No transport redesign was made in this investigation.

## Evidence retention and integration checkpoint

Raw profiles, BCC outputs, detailed per-process tables, guards, exact commands and checkpoints are retained in the local project archive `tmp/racer-kernel-evidence-20261005.tar.gz` (not committed). The committed record contains the core numbers and original stack excerpts. Raw Parca SHA-256:

- Gantry: `882d171e07ba0ff464852a99b0e45eaa51026b7e55718bc21ce0f8dfe3383a03`
- Dataplane: `2a5b53a2466610e2c62eadbeb48385ff1bc81073994992dcb3aa7e1c8432f69b`
- Loadgen: `fa179b8f5e8dc343c2dce23ee1445de0c23de1e2aaf63e268f831d7a908b5f23`

New offline analyzer is an isolated Go module pinned to the pprof version already in root go.sum, without changing application dependencies. Tests cover distinct zero-address kernel symbols, mapping identity, recursive cumulative deduplication, sample-column selection/errors, normalized export, and preservation of existing files including link aliases. Focused tests and project-config lint passed with Go1.26.6 for lint compatibility. Next phase: parent review/test, required make fmt, two small commits, cherry-pick, worktree cleanup. Deadline5min, checkpoint per command; load stays running unchanged.

18:01:54Z checkpoint: parent focused `go test -mod=readonly -timeout=5m ./...` and isolated project-config lint both passed. First root `make fmt` overlapped isolated lint and failed its global process lock; after that process completed, sequential `make fmt` passed (exit0), with no tracked-source changes outside this task. Archive created and hashed: `9738d82ebedc71b8335be0f88a8e35563c652d5c58bccb619a2d97930d26b4c6`. Review complete, no unresolved command errors. Next exact operations: stage only analyzer subtree and commit; stage this record and commit; cherry-pick both onto racer-v2 and remove this clean worktree. Evidence archive remains outside the removed worktree, inside project tmp.
