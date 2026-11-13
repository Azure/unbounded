# racer-vllm

A vLLM plugin that loads model weights from a Racer cache. It adds the load
format `racer` and uses the `racer-client` package. Both are pure Python.

The full guide, including a serve manifest, troubleshooting, and how to verify
a load, is in
[docs/content/guides/racer-vllm.md](../../../docs/content/guides/racer-vllm.md).
Example manifests are in
[deploy/racer/examples/models-weights](../../../deploy/racer/examples/models-weights).

## Commands

| Command | What it does |
|---------|--------------|
| `racer-vllm materialize <racer://cache/bucket/prefix> <dest-dir>` | Copies config, tokenizer, chat template, index, other small files, and `racer-manifest.json` into a directory vLLM can serve |
| `racer-vllm import --source <snapshot-dir> --dest <blob-prefix> [--azcopy]` | Builds `racer-manifest.json` (sizes, SHA-256, safetensors headers), uploads the blobs, and writes the manifest last |
| `racer-vllm prepare <racer://...> <out-dir> -- <vllm serve args>` | Captures per-rank weights after loading and before `process_weights_after_loading` into `sharded/<fingerprint>/` |
| `racer-vllm warm <racer://...>` | Prefetches every weight file with local retention |
| `racer-vllm inspect <racer://...>` | Prints the manifest and sharded artifacts |
| `racer-vllm-fetch` | Deprecated alias of `materialize` |

Serve with:

```
vllm serve /models/<name> --load-format racer \
  --model-loader-extra-config '{"uri": "racer://<cache>/<bucket>/<prefix>"}'
```

## Settings

| Key | Default | Meaning |
|-----|---------|---------|
| `uri` | From `racer-manifest.json` | The `racer://` model location |
| `sharded` | `auto` | `auto`, `require`, or `off` for `sharded/<fingerprint>/` artifacts |
| `delivery` | `auto` | `auto`, `gpu`, or `host` |
| `staging_bytes` | 256 MiB | Size of each staging buffer |
| `max_staging_bytes` | 4 GiB | Cap on all staging memory |
| `concurrency` | `4` | Reads in flight |
| `clone` | `auto` | `auto`, `always`, or `never` |
| `verify` | `size` | `none`, `size`, or `full` |
| `capture_dir` | Unset | Used by `prepare` |
| `socket` | `RACER_SOCKET` | Client socket path |

## Build

`make racer-python-dist` builds wheels and sdists for both packages into
`tmp/racer-python-dist`. Both packages share one version; `racer-vllm`
requires the matching `racer-client` release.
