# Racer model weights

Status: in progress. This note records the design of the model-weight path
for vLLM: the `racer-vllm` package, local retention, and the capacity plan.
User-facing documentation is in `docs/content/guides/racer-vllm.md`.

## Goals

- Start many vLLM replicas of large models (hundreds of GB) with each page
  fetched from the origin only a few times.
- Put bytes in GPU memory with no extra host copies where hardware allows.
- Keep working on sites without RDMA.

## Manifest

`racer-vllm import` writes `racer-manifest.json` at the model prefix. It
holds, for every file: path, size, and SHA-256; and for each safetensors file,
the parsed header (tensor names, dtypes, shapes, offsets). Blobs are uploaded
first and the manifest last, so the manifest's presence means the upload is
complete.

The loader reads the manifest instead of listing or probing the origin for
part files. It knows every tensor's location before the first read, which lets
the planner build reads without fetching headers, and lets `verify` check
sizes or hashes. `materialize` copies the manifest with the small files and
pins each file's ETag.

## Planner and zero-copy staging

The loader asks vLLM which weights this rank needs, reusing vLLM's own
filters (expert-parallel `local_expert_ids`, `secondary_weights`, name
prefixes, encoder-only and unused-weight filters). The planner turns the
needed tensors into byte ranges from the manifest, merges neighbors, and
issues up to `concurrency` reads.

Each read lands in a staging buffer of `staging_bytes`, with total staging
capped by `max_staging_bytes`. With `delivery: gpu` or `auto`, the dataplane
writes straight into GPU staging (`gpu-read`); otherwise into host memory
(`host-read`). Tensors are handed to vLLM as views into staging when they can
be (`clone: auto`) and copied out only when the buffer must be reused. A draft
model for speculative decoding uses the same plan and never re-reads the full
checkpoint.

## Sharded artifacts

`racer-vllm prepare` runs vLLM once and captures each rank's weights after
loading and before `process_weights_after_loading`. Capturing at that point
keeps the artifacts independent of kernel-specific repacking, which vLLM
redoes on every load. Artifacts live under `sharded/<fingerprint>/` next to
the model.

The fingerprint hashes the inputs that decide each rank's tensors: the
manifest, the vLLM version, the parallel layout (tensor, pipeline, and expert
parallel sizes), quantization, and dtype. A loader computes its own
fingerprint and uses only a matching directory. `sharded: auto` falls back to
the full checkpoint on a miss; `require` fails; `off` never looks.

## Local retention

Second-sight keeps peer pages only after a second read. Replicas on one node
read the same pages once each, close together, and without RDMA the pages
arrive over HTTP and are not kept, so the next replica fetches them again.
`Racer-Retain: local` (Python `retain=True`, prefetch `retain_local`) makes a
peer page eligible on first sight, under `RACER_RETAINED_SEGMENT_PERCENT`.
`racer-vllm warm` uses it to fill a node before replicas start.

## Capacity plan

- Raw NVMe device mode, selected per node with
  `racer.unbounded-cloud.io/block-devices`, gives the cache whole devices. The
  first and last 1 MiB of each device must already be zero; Racer never clears
  them. Device mode ignores `RACER_SLAB_BYTES_PER_WORKER` for capacity.
- New-install defaults are `RACER_SEGMENT_BYTES` 1 GiB and
  `RACER_SLAB_BYTES_PER_WORKER` 4 GiB (file mode).
- Model caches get `priority: 900` so other caches yield space under pressure.
- Operators check `racer_effective_payload_bytes` against the models a node
  should hold.

## Open questions

- Whether `prepare` should upload artifacts itself or leave that to the
  operator.
- Garbage collection of stale `sharded/<fingerprint>/` directories.
