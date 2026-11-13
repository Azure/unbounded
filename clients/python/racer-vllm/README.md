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
| `racer-vllm materialize <racer://cache/bucket/prefix> <dir>` | Copies config, tokenizer, chat template, index, other small files, and `racer-manifest.json` into a directory vLLM can serve, and records each file's ETag in `racer-source.json` |
| `racer-vllm import --source <snapshot-dir> --dest <container-url>[/<prefix>]` | Hashes every file of a Hugging Face snapshot, uploads the blobs that changed, and writes `racer-manifest.json` last. Needs the `azure` extra |
| `racer-vllm prepare <racer://...> --dest <container-url>[/<prefix>] [--workdir <dir>] -- <vLLM engine args>` | Loads the model once with vLLM, captures each rank's weights after loading and before `process_weights_after_loading`, uploads them under `sharded/<fingerprint>/`, and writes `racer-sharded-index.json` last |
| `racer-vllm warm <racer://...>` | Prefetches every weight file into the cache and waits for the prefetch to finish |
| `racer-vllm inspect <racer://...>` | Prints the manifest summary, the sharded artifacts, and whether each file is served at its manifest size |
| `racer-vllm-fetch <racer://...> <dir>` | Deprecated alias of `materialize` |

`racer-vllm import` and `prepare` upload with the Azure SDK and
`DefaultAzureCredential`, which picks up workload identity from
`AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, and `AZURE_FEDERATED_TOKEN_FILE`.
Install the extra for them:

```
pip install './clients/python/racer-vllm[azure]'
```

A `file:///path` destination writes to a local directory instead, which
needs no extra. Only `prepare` imports vLLM; the other commands run without
vLLM or torch.

Serve with:

```
vllm serve /models/<name> --load-format racer \
  --model-loader-extra-config '{"uri": "racer://<cache>/<bucket>/<prefix>"}'
```

## Settings

| Key | Default | Meaning |
|-----|---------|---------|
| `uri` | From `racer-source.json` | The `racer://` model location |
| `sharded` | `auto` | `auto`, `require`, or `off` for `sharded/<fingerprint>/` artifacts |
| `delivery` | `auto` | `auto`, `gpu`, or `host` |
| `staging_bytes` | 256 MiB | Size of each staging buffer |
| `max_staging_bytes` | 4 GiB | Cap on all staging memory |
| `concurrency` | `4` | Reads in flight |
| `clone` | `auto` | `auto`, `always`, or `never` |
| `verify` | `size` | `none`, `size`, or `full` |
| `capture_dir` | Unset | Used by `prepare` |
| `role` | `target` | `target`, or `draft` for a speculative-decoding draft model; `draft` selects the draft's own sharded artifacts and checkpoint filter |
| `socket` | `RACER_SOCKET` | Client socket path |

## Tests

```
cd clients/python/racer-vllm
PYTHONPATH=../racer/src:src pytest -q
pytest -q -m vllm   # with vLLM installed: checks the loader against its real DefaultModelLoader
```

## Build

`make racer-python-dist` builds wheels and sdists for both packages into
`tmp/racer-python-dist`. Both packages share one version; `racer-vllm`
requires the matching `racer-client` release.
