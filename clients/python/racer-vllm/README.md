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

vLLM finds the plugin through the `vllm.general_plugins` entry point. It was
written for vLLM v0.25.x.

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
`ModelChangedError`; run `racer-vllm-fetch` again.

Then start vLLM on that directory:

```
vllm serve /models/llama-3-8b --load-format racer \
  --model-loader-extra-config '{"staging_bytes": 268435456, "concurrency": 4}'
```

Extra config keys, all optional:

| Key | Default | Meaning |
|---|---|---|
| `uri` | from `racer-model.json` | The `racer://` model location. |
| `staging_bytes` | 256 MiB | Size of each GPU staging buffer. |
| `concurrency` | 4 | Staging buffers, and reads in flight. |
| `clone` | true | Copy each tensor out of staging before handing it to vLLM. |
| `socket` | none | Client socket path. |
| `use_vmm` | true | Allow exportable `cuMemCreate` staging, needed for `gpu-read`. |
| `mode_retry_seconds` | 300 | Retry a delivery mode the dataplane refused after this many seconds. `null` retries only after a dataplane restart. |
| `sharded_state` | false | Read a vLLM `sharded_state` checkpoint; each rank reads only its files. |
| `pattern` | as vLLM | File name pattern for `sharded_state`, such as `model-rank-{rank}-part-{part}.safetensors`. |

On fast local disks, more reads in flight help only when the dataplane also
fetches further ahead. Set `concurrency` to 8 together with a larger
`RACER_RANGE_WINDOW_PAGES` and `RACER_GPU_STAGING_BYTES` on the dataplane. See
the GPU reads section of the Racer reference.

## How it loads

1. Find the safetensors files from `model.safetensors.index.json`, or use
   `model.safetensors`.
2. Stat every file before reading any, at the ETag from `racer-model.json`
   when there is one. Every later read sends `If-Match` with that ETag, so the
   whole model is read at one version. A file that changes during the load
   fails it with `ModelChangedError`.
3. For each file, read the 8-byte header length and the JSON header. All
   headers are read before any tensor data.
4. Group tensors into ranges that fit a staging buffer. Each range ends on a
   Racer page boundary (16 MiB), so no page is read twice. A tensor that
   crosses the boundary is split across two ranges and joined again before vLLM
   sees it. Read each range into
   GPU memory with `gpu-read`. If the dataplane cannot do that, use
   `host-read` into pinned memory and copy, else the normal stream.
5. Give each tensor (right dtype and shape) to `model.load_weights`. While
   vLLM uses one buffer, the next range is read into another.

A tensor bigger than a staging buffer gets its own GPU allocation.

After each load the loader logs one line (logger `vllm.racer_vllm.loader`,
so it uses vLLM's log format) such as:

```
racer: loaded 4 files, 14.96 GiB in 9.12s (1.64 GiB/s) from racer://models/llama/Llama-3.1-8B-Instruct; reads by delivery mode: gpu=60 (14.96 GiB) host=0 (0.00 GiB) stream=0 (0.00 GiB)
```

The counts cover tensor data reads. Safetensors header reads use the stream
and are not counted.

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
come from the dataplane's memory or disk cache.

To have each rank read only its own data, save a `sharded_state` checkpoint
with vLLM (`examples/features/sharded_state/save_sharded_state_offline.py`,
which uses `ShardedStateLoader.save_model`), upload it, and load with
`{"sharded_state": true}`. The loader probes this rank's parts
(`part-0`, `part-1`, ... until one is missing) and copies each tensor straight
into the model's state dict, as vLLM's `sharded_state` loader does. The
checkpoint must be saved with the same tensor and pipeline parallel sizes.
Each rank pins its own files at the ETags seen when its load starts.

With `clone` set to false, tensors are views into staging memory. They stay
valid only until vLLM asks for the next tensors after the buffer's last one.
Only use this when the model copies each weight at once.

## Tests

```
make racer-python-test
```
