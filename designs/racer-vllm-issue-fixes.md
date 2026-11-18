# Racer vLLM loader issue fixes

## Summary

This change fixes the seven issues filed against the Racer vLLM loader and the
Racer dataplane after a DeepSeek-V4.1 validation run:

| Issue | Title | Root cause | Fix |
| --- | --- | --- | --- |
| #1153 | speculative decoding reloads the full checkpoint | vLLM hands the target load config to the drafter, so Racer streams the target checkpoint again | per-tensor selection for drafts |
| #1154 | regular checkpoints multiply reads by TP size | every rank reads every file; nothing warns or reports the layout | warning, fetch report, docs |
| #1155 | cuFile staging registration failure disables direct disk-to-GPU reads | the cuFile driver open fails (no nvidia-fs, P2PDMA off), and the status is discarded and mislabeled | status preservation, labels, hints, `RACER_GPU_PCI_P2PDMA=auto` |
| #1156 | 64 MiB segments waste a quarter of each cache device | upgraded clusters keep the old 64 MiB setting and the warning is not actionable | actionable warning, upgrade runbook |
| #1157 | benchmark host-read concurrency and staging settings | no reproducible benchmark | `hack/scripts/racer-vllm-bench.sh`, tuning docs |
| #1158 | add GPU-read latency metrics | latency events exist only for host reads | GPU-read latency series |
| #1159 | load pre-sharded checkpoints directly into GPU shard buffers | retained staging tensors force a second model-sized allocation, and unclosed `HostBuffer` memfds leak | `HostBuffer` finalizer, no clones for `sharded_state`, host-memory tensors |

## Supported vLLM baseline

`unbounded-stable` runs no vLLM 0.25.x workload. `deepseek-v4-1-flash` runs
`vllm/vllm-openai:v0.33.0` with TP=4, `--load-format racer` and a DSpark
speculative config; this deployment produced #1153, #1154, #1157 and #1159.
The other Racer-loaded models run `v0.30.0`.

The loader now supports vLLM 0.30.0 and 0.33.0+, and the `racer-vllm` image
moves to `vllm/vllm-openai:v0.33.0`. APIs that exist only in 0.33
(`draft_load_config`, `is_unused_checkpoint_weight`) are feature-detected.

## Verified root causes

### #1155: cuFile driver open, not staging registration

Probes on a GB200 node (`gb2002159d2424`, driver 580.95.05, cuFile 1.14.1)
reproduce the production failure:

- With the shipped `cufile.json` (compat mode off) and no nvidia-fs,
  `cuFileDriverOpen()` returns `5001 CU_FILE_DRIVER_NOT_INITIALIZED`. The
  per-process cuFile log (`/var/log/cufile/cufile_<pid>_<date>.log`) says
  `nvidia-fs.ko driver not loaded`.
- With `CUFILE_PARAM_USE_PCIP2PDMA` set, the driver opens and
  `cuFileDriverGetProperties` reports `CU_FILE_NVME_P2P_SUPPORTED`.
- With P2PDMA on, `cuFileBufRegister` succeeds but `cuFileHandleRegister`
  returns `5008 CU_FILE_IO_NOT_SUPPORTED`: cuFile requires the ext4 cache
  filesystem to be mounted with an explicit `data=ordered`, and the node mount
  has none. NVMe multipath is also on (`nvme_core.multipath=Y`), which P2PDMA
  does not support.

Racer opened the cuFile driver inside `StagingBuffer::register_cufile`, so a
driver-open failure during the first stage was counted as `staging_register`.
The native layer also collapsed every open failure into `Unavailable` and kept
the cuFile status in a thread-local on the opener thread, so the log never
showed it.

### #1156: the 64 MiB setting survives upgrades

`unbounded-system/racer-settings` still contains `RACER_SEGMENT_BYTES=67108864`.
The operator seeds that ConfigMap only when it is absent, so clusters installed
before the 1 GiB default keep the old geometry. Each 64 MiB segment fits three
page records and leaves 24.9 percent unused.

## Loader changes (#1153, #1154, #1159)

All in `clients/python/racer-vllm/src/racer_vllm.py` unless noted.

- `HostBuffer` (`clients/python/racer/src/racer/host.py`) closes its memfd
  with `weakref.finalize` when it is collected without `close()`, and unpins
  and unmaps once the last reference to its view goes, so an unclosed buffer
  no longer leaks shared memory until process exit. This was the cause of the
  node OOM that killed the dataplane. Unpinning and unmapping wait for the
  view because `torch.frombuffer` keeps the view without holding a buffer
  export, so a tensor may still read or copy from the mapping.
- `sharded_state` loads copy each tensor into its parameter immediately, so
  they no longer clone out of the staging buffer.
- `tensor_device: "cpu"` reads into host memory and yields CPU tensors. It is
  for vLLM model loaders that keep yielded tensors while they build fused
  parameters, which otherwise needs a second model-sized GPU allocation. It
  replaces the cluster's `/racer/patch_loader.py`.
- When exportable GPU memory cannot be allocated, the loader logs the error
  instead of falling back silently.
- `RacerModelLoader.load_model` detects a draft load
  (`model_config is speculative_config.draft_model_config`). A draft that
  inherits the target load config reads its own model path when it differs
  from the target, and only the tensors it loads: the model's
  `is_unused_checkpoint_weight` rule when present, a built-in rule for
  DeepSeek-V4 DSpark drafters (keep `mtp.` names), and the new
  `tensor_include` and `tensor_exclude` regex options. Tensor selection is
  per tensor, files with no selected tensor are not read, and the summary line
  reports skipped tensors and bytes. A draft with no rule that reads the
  target checkpoint logs a warning. `sharded_state` with an inherited draft
  config raises an error that names `draft_load_config`.
- A regular checkpoint loaded with TP > 1 logs one warning with the checkpoint
  size and the total bytes read by all ranks, and points to `sharded_state`.
  `racer-vllm-fetch` reports the layout, file count and size on stderr.

### Deferred: zero-copy binding

Binding Racer GPU allocations directly as model parameters (meta-device model
construction, as vLLM's `IpcModelLoader` does) was deferred because its
tradeoffs are negative for the current workloads:

- Non-persistent buffers, such as rotary caches, are not in `sharded_state`
  checkpoints, so a meta-device model would leave them uninitialized.
- Replacing `Parameter` objects drops the attributes vLLM sets on parameters
  when it creates them, which weight loaders and quantization methods rely on.
- Quantized models transform weights in `process_weights_after_loading`, so
  file-sized allocations would stay alive next to the transformed weights.
- vLLM sleep mode cannot manage memory it did not allocate.

The copy path with no clones and the host-memory option remove the second
model-sized allocation without these risks. Revisit if profiling shows the
one remaining copy matters.

## Dataplane changes

### cuFile diagnosis (#1155)

- `cmd/racer/gpu/native/gpu_cuda.c` records the cuFile driver-open status once
  for the process. Every later cuFile call on any thread reports the same
  status (native ABI version 4). The Rust error names cuFile statuses, for
  example `cuFile call failed with status 5001 (CU_FILE_DRIVER_NOT_INITIALIZED)`.
- `stage()` opens the cuFile driver before it registers the staging buffer, so
  `racer_gpu_cufile_disabled_total` labels the failing step correctly
  (`driver_open`, `staging_register` or `file_open`).
- The `state=cufile-disabled` and `state=cufile-unsupported` log lines name the
  step, the status and a fix: enable P2PDMA or install nvidia-fs for 5001,
  mount ext4 with `data=ordered` and avoid RAID0 and NVMe multipath for 5008,
  and the cuFile log directory.
- `RACER_GPU_PCI_P2PDMA` accepts `auto`, the new default: P2PDMA is requested
  when the device directory has no `nvidia-fs*` device. `true` and `false`
  keep their meaning.
- `docs/content/reference/racer.md` gains a cuFile troubleshooting section with
  the GB200 prerequisites and a status table.

Not done: a status gauge, a startup self-test, staging size validation and a
filesystem preflight. The corrected log line carries the same information when
the first stage runs.

### Segment tail (#1156)

The startup warning in `cmd/racer/src/app/devices.rs` now reports the record
size, the unused bytes per segment and in total, the percentage, what 1 GiB
would leave, that changing the size discards cached data, and that upgrades
keep the old value in `racer-settings`. The threshold stays at 10 percent, and
the dataplane does not round or reject small segments, because that would
change on-disk geometry. The Racer reference documents the arithmetic and an
upgrade runbook.

### GPU-read latency (#1158)

`racer_gpu_read_request_nanoseconds_{sum,count}` and
`racer_gpu_read_first_page_nanoseconds_{sum,count}` use the host-read
semantics: the first-page series covers every read that produced a first page,
and the request series covers successful reads only. Host and GPU series are
chosen by delivery mode, so a read is never counted in both.

## Benchmark (#1157)

`hack/scripts/racer-vllm-bench.sh` runs `vllm serve --load-format racer` over a
matrix of `concurrency`, `staging_bytes` and `use_vmm` (plus free-form extra
config), waits for each rank's load summary, and writes one CSV row per run
with load time, throughput, peak GPU and host memory and, when
`RACER_METRICS_URL` is set, dataplane counter deltas. The racer-vllm guide
documents how to run it and has a tuning table to fill from measured runs. The
validation baseline was 475.24 GiB at TP=4 with `concurrency=2` and
`use_vmm=false`: about 11.93 GiB/s aggregate and 3.01 to 3.48 GiB/s per rank.

## Cluster follow-ups after the release

- Bump the Racer images and wheels, and replace `/racer/patch_loader.py` with
  `"tensor_device": "cpu"` where loaders retain tensors.
- Remove `use_vmm=false` from the DeepSeek workload; VMM allocation and export
  work on the GB200 and B200 nodes.
- Mount the GB200 cache filesystem with `data=ordered` and turn off NVMe
  multipath, then confirm `racer_gpu_direct_disk_reads_total` increases.
- Schedule the `racer-settings` segment size change with a cold-cache window,
  or accept the warning.
- Run the benchmark matrix and fill in the tuning table.
