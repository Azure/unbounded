# racer-vllm

A vLLM plugin that loads model weights from a Racer cache. It adds the load
format `racer`. It uses the `racer-client` package.

## Install

The packages are not on PyPI. Each GitHub release has their wheels and sdists
as assets. To install from a release:

```
pip install --no-index \
  --find-links https://github.com/Azure/unbounded/releases/expanded_assets/<tag> \
  racer-vllm
```

From a checkout:

```
pip install ./clients/python/racer ./clients/python/racer-vllm
```

To build sdists and wheels, run `make racer-python-dist` from
the repository root (output in `tmp/racer-python-dist`), or
`python -m build clients/python/racer` and
`python -m build clients/python/racer-vllm`. Both packages share one version;
`racer-vllm` requires the matching `racer-client` release.

vLLM finds the plugin through the `vllm.general_plugins` entry point. It
supports vLLM v0.30.0 and v0.33.0 or later; it uses newer vLLM features, such
as `is_unused_checkpoint_weight`, only where they exist.

## Model location

A model lives at `racer://<cache>/<bucket>/<prefix>`. The plugin reads
objects through `/run/racer/<cache>/client/socket`. Set `RACER_SOCKET` to use
another socket. Object keys follow racer-object, so `<bucket>/<prefix>/<file>`
names the S3 object.

## Use

vLLM reads the model config and tokenizer before it loads weights, so these
small files must be on local disk first. Fetch them, for example in an init
container. The admission webhook mounts the client socket into init
containers as well as regular containers (limited by
`racer.unbounded-cloud.io/client-containers` when set):

```
racer-vllm-fetch racer://models/my-bucket/llama-3-8b /models/llama-3-8b
```

This copies `config.json`, tokenizer files, `model.safetensors.index.json`
and similar files, plus small files named in the index metadata. It also
writes `racer-model.json`, which tells the loader where the weights are and
records the ETag of every fetched file and every weight file. The loader reads
the weights at those ETags, so they match the config and tokenizer. If the
model at the origin changes after the fetch, the load fails with
`ModelChangedError`; run `racer-vllm-fetch` again. The loader also refuses
any weight file that `racer-model.json` does not pin, so a layout change
(such as an index uploaded after a single-file fetch) fails the same way.
Without `racer-model.json`, nothing is pinned.

`racer-vllm-fetch` prints the names of the files it copied to standard output.
On standard error it then prints the weight layout (`regular` safetensors,
`sharded_state`, or `none`), the number of weight files and their total size,
for example `weights: regular layout, 4 files, 14.96 GiB`.

Then start vLLM on that directory:

```
vllm serve /models/llama-3-8b --load-format racer \
  --model-loader-extra-config '{"staging_bytes": 268435456, "concurrency": 4}'
```

Extra config keys, all optional:

| Key | Default | Meaning |
|---|---|---|
| `uri` | from `racer-model.json` | The `racer://` model location. |
| `staging_bytes` | 256 MiB | Size of each staging buffer. |
| `concurrency` | 4 | Staging buffers, and reads in flight. |
| `clone` | true | Copy each tensor out of staging before handing it to vLLM. `sharded_state` loads never clone, because they copy each tensor into the model at once. |
| `socket` | none | Client socket path. |
| `use_vmm` | true | Allow exportable `cuMemCreate` staging, needed for `gpu-read`. |
| `mode_retry_seconds` | 300 | Retry a delivery mode the dataplane refused after this many seconds. `null` retries only after a dataplane restart. |
| `sharded_state` | false | Read a vLLM `sharded_state` checkpoint; each rank reads only its files. |
| `pattern` | as vLLM | File name pattern for `sharded_state`, such as `model-rank-{rank}-part-{part}.safetensors`. |
| `prefetch` | true | Ask the dataplane to fill the weight pages in the background before the loader reads them. |
| `prefetch_ttl_seconds` | 3600 | Ticket lifetime. `0` uses the dataplane's default. Each load renews the ticket. |
| `tensor_device` | `cuda` | Where staging buffers and the tensors handed to vLLM live: `cuda` (GPU) or `cpu` (host memory). See [Host memory tensors](#host-memory-tensors). |
| `tensor_include` | none | Regular expressions (a list, or one string). With any set, read only the tensors whose name matches one (`re.search`). Regular checkpoints only. |
| `tensor_exclude` | none | Regular expressions. Skip the tensors whose name matches one. Regular checkpoints only. |

With `prefetch` on, the loader submits a ticket for every weight file once it
has stat'ed them. A read that needs a page being prefetched joins that fill,
so a cold load uses the dataplane's prefetch concurrency as well as the
loader's `concurrency`. A dataplane without prefetch, or a passthrough cache,
rejects the ticket and the load continues. More reads in flight can also help
on a cold cache, but each one costs a staging buffer. On a fast local disk
they may not help at all. With a [tensor selection](#tensor-selection), the
ticket is submitted after the headers are read, and only for the files that
hold selected tensors; a ticket covers whole files.

## How it loads

1. Find the safetensors files from `model.safetensors.index.json`, or use
   `model.safetensors`.
2. Stat every file before reading any, at the ETag from `racer-model.json`
   when there is one. Every later read sends `If-Match` with that ETag, so the
   whole model is read at one version. A file that changes during the load
   fails it with `ModelChangedError`. Unless `prefetch` is off, submit a
   prefetch ticket for every file at those ETags, so the dataplane starts
   filling the cache while the loader runs.
3. For each file, read the 8-byte header length and the JSON header. All
   headers are read before any tensor data. Drop the tensors that the
   [tensor selection](#tensor-selection) skips; a file with no tensor left is
   not read at all.
4. Group consecutive tensors into ranges that fit a staging buffer. Each range
   ends on a Racer page boundary (16 MiB), so no page is read twice. A tensor
   that crosses the boundary is split across two ranges and joined again
   before vLLM sees it. Skipped tensors end a range, so their bytes are never
   read. Read each range into
   GPU memory with `gpu-read`. If the dataplane cannot do that, use
   `host-read` into pinned memory and copy, else the normal stream. With
   `tensor_device` set to `cpu`, read into host memory with `host-read`.
5. Give each tensor (right dtype and shape) to `model.load_weights`. While
   vLLM uses one buffer, the next range is read into another.

A tensor bigger than a staging buffer gets its own allocation.

After each load the loader logs one line (logger `vllm.racer_vllm.loader`,
so it uses vLLM's log format) such as:

```
racer: loaded 4 files, 14.96 GiB in 9.12s (1.64 GiB/s) from racer://models/llama/Llama-3.1-8B-Instruct; reads by delivery mode: gpu=60 (14.96 GiB) host=0 (0.00 GiB) stream=0 (0.00 GiB)
```

The counts cover tensor data reads. Safetensors header reads use the stream
and are not counted. When the tensor selection skips tensors, the line ends
with, for example, `; skipped 12 tensors (1.50 GiB) and 1 whole files by
tensor selection`, and the file count leaves out the skipped files.

If exportable GPU memory (`cuMemCreate`) is not available, the loader logs a
warning with the CUDA error and stages in torch memory, which rules out
`gpu-read` for the rest of the load.

When the dataplane refuses a mode, the client logs one warning (logger
`racer.client`) with the status, for example
`racer: gpu-read is not available from /run/racer/models/client/socket
(status 501: ...); falling back to host mode ...`. It warns again only after the
mode is retried and refused again.

## Tensor parallelism

With a regular checkpoint, every tensor-parallel rank reads every weight file,
and vLLM's `weight_loader` keeps the rank's slice. vLLM decides the slice
inside each model's loader, so the plugin cannot read only the needed bytes in
general. Ranks on one node share the node's Racer dataplane, so only the first
read of each page goes to a peer or the origin; repeat reads by the other ranks
come from the dataplane's memory or disk cache. They are cache hits, but they
still cost client, dataplane and PCIe work. For example, a 475.24 GiB
checkpoint at `tensor_parallel_size=4` read 1.9 TiB on the node, while each
rank kept only 80.77 GiB of it.

When it loads a regular checkpoint with a tensor parallel size above 1, the
loader logs one warning per process with the tensor parallel size, the bytes
each rank reads, and the total for the node.

To have each rank read only its own data, save a `sharded_state` checkpoint
with vLLM (`examples/features/sharded_state/save_sharded_state_offline.py`,
which uses `ShardedStateLoader.save_model`), upload it, and load with
`{"sharded_state": true}`. The loader probes this rank's parts
(`part-0`, `part-1`, ... until one is missing) and copies each tensor straight
into the model's state dict, as vLLM's `sharded_state` loader does. The
checkpoint must be saved with the same tensor and pipeline parallel sizes.
`racer-vllm-fetch` finds no safetensors weights for such a checkpoint, so it
probes vLLM's default patterns (or `--pattern`) and records every rank's
parts. Each rank then loads only the parts pinned there; a new, missing, or
replaced part fails with `ModelChangedError`. Without `racer-model.json`, each
rank pins its own files at the ETags seen when its load starts.

With `clone` set to false, tensors are views into staging memory. They stay
valid only until vLLM asks for the next tensors after the buffer's last one.
Only use this when the model copies each weight at once. `sharded_state`
loads always work this way, since they copy each tensor into the model before
reading the next.

## Host memory tensors

By default, tensors are read into GPU staging buffers and handed to vLLM as
GPU tensors. Some vLLM models keep references to the tensors they are given
while they build fused parameters, for example DeepSeek-V4 and GLM. A kept
tensor keeps its staging memory (or its clone) on the GPU, so the load can
need a second model-sized GPU allocation and run out of memory.

For such models, set `tensor_device` to `cpu`. The loader then stages in host
memory with `host-read` and hands vLLM CPU tensors that share no staging
buffer, as vLLM's default loader does; the model copies each one to the GPU.
This costs a host copy per tensor and does not use `gpu-read`, so keep the
default for models that copy each weight at once. It is the supported
replacement for patching the loader to stage in host memory.

## Tensor selection

The loader can skip single tensors of a regular checkpoint, because it plans
its reads per tensor. It reads a tensor only when all of these allow it:

- `tensor_include`: when set, the name matches one of the expressions.
- `tensor_exclude`: the name matches none of the expressions.
- The model's `is_unused_checkpoint_weight(name)` (vLLM 0.33 and later),
  when the model defines it, does not return true.
- For a speculative decoding draft whose model class name starts with
  `DSparkDeepseekV4` (the DeepSeek-V4 DSpark drafters), the name starts with `mtp.`.
  These drafters load only the `mtp.*` tensors of the target checkpoint but
  do not define `is_unused_checkpoint_weight` yet, so the loader has this
  one rule built in.

Files with no selected tensor are not read, and the load summary reports the
skipped tensors, bytes and files. A selection that skips every tensor fails
the load. Selection does not apply to `sharded_state` checkpoints.

## Speculative decoding

Unless `--speculative-config` sets `draft_load_config`, vLLM loads the draft
model with the target's load config, so with `--load-format racer` the
drafter uses this loader and the target's `model_loader_extra_config`. The
loader tells the draft load apart from the target load:

- The tensor selection above applies, so a DSpark drafter that shares the
  target checkpoint reads only its `mtp.*` tensors instead of the whole
  checkpoint again.
- If the draft model path differs from the target's, the inherited `uri` is
  ignored and the URI comes from the draft model directory (its
  `racer-model.json`, or a `racer://` model name).
- If no rule or filter applies and the draft reads the target's URI, the
  loader warns with the bytes it reads again. Set `tensor_include` or
  `tensor_exclude`, or set `draft_load_config` in `--speculative-config` to
  give the draft its own options.
- An inherited config with `sharded_state` fails the draft load, because the
  draft would read the target's rank files. Set `draft_load_config`, for
  example `{"load_format": "racer", "model_loader_extra_config":
  {"sharded_state": false}}`, or point it at a `sharded_state` checkpoint of
  the draft model.

## Tests

```
make racer-python-test
```
