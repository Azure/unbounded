# racer-vllm

A vLLM plugin that loads model weights from a Racer cache. It adds the load
format `racer`. It uses the `racer-client` package.

## Install

```
pip install ./clients/python/racer ./clients/python/racer-vllm
```

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
container:

```
racer-vllm-fetch racer://models/my-bucket/llama-3-8b /models/llama-3-8b
```

This copies `config.json`, tokenizer files, `model.safetensors.index.json`
and similar files, plus small files named in the index metadata. It also
writes `racer-model.json`, which tells the loader where the weights are.

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

On fast local disks, more reads in flight help only when the dataplane also
fetches further ahead. Set `concurrency` to 8 together with a larger
`RACER_RANGE_WINDOW_PAGES` and `RACER_GPU_STAGING_BYTES` on the dataplane. See
the GPU reads section of the Racer reference.

## How it loads

1. Find the safetensors files from `model.safetensors.index.json`, or use
   `model.safetensors`.
2. For each file, read the 8-byte header length and the JSON header.
3. Group tensors into ranges that fit a staging buffer. Each range ends on a
   Racer page boundary (16 MiB), so no page is read twice. A tensor that
   crosses the boundary is split across two ranges and joined again before vLLM
   sees it. Read each range into
   GPU memory with `gpu-read`. If the dataplane cannot do that, use
   `host-read` into pinned memory and copy, else the normal stream.
4. Give each tensor (right dtype and shape) to `model.load_weights`. While
   vLLM uses one buffer, the next range is read into another.

A tensor bigger than a staging buffer gets its own GPU allocation.

With `clone` set to false, tensors are views into staging memory. They stay
valid only until vLLM asks for the next tensors after the buffer's last one.
Only use this when the model copies each weight at once.

## Tests

```
make racer-python-test
```
