# Storage Pain Journal

Append-only record of measured AI-data friction encountered while building the
unbounded-lab. See `plan.md` -> Storage Pain Journal section for the why.

**Rules:**
- One row per measurement event. Date stamped. Cluster + node identified.
- Numbers, not narrative. If a metric is "felt painful" but not measured, it does
  not belong here yet.
- Never edit an existing row; append a new row if the situation changes.
- This file is **internal**; sponsor updates summarize it.

## Entries

| Date | Wave item | Metric | Value | Notes / how measured |
|---|---|---|---|---|
| 2026-04-28 | W1.1 | Cold pull origin egress (qwen3:30b-a3b) | 18.557 GB | Final `total` byte count from `/api/pull` NDJSON on `spark-3d37` (Region A, GB10). Single 18556685856-byte blob `sha256:58574f...c8eabf` + 4 small manifest blobs. |
| 2026-04-28 | W1.1 | Cold pull wall time (registry.ollama.ai -> spark-3d37 PVC, through ingress) | 408 s | `curl -X POST /api/pull` from operator workstation. 18.557 GB / 408 s = 45.5 MB/s sustained. 19 x 1 GB parallel parts (Ollama default). Pull completed at 2026-04-28T17:13:21Z. |
| 2026-04-28 | W1.1 | Cold model load into GPU (first generate after pull) | 34.4 s | `load_duration` from first `/api/generate`. Model resident in 119.7 GiB GB10 unified memory. |
| 2026-04-28 | W1.1 | Warm tokens/sec (qwen3:30b-a3b Q4_K_M, single stream, think=false, 200 tokens) | 77.4 t/s | `eval_count / eval_duration` from `/api/generate`. Single GB10 GPU, prompt: "Write one paragraph about kubernetes." |
| 2026-04-28 | W1.1 | Warm-PVC pod restart -> Ready (weights survive) | 11.7 s | `kubectl delete pod ollama-0` -> `kubectl wait --for=condition=Ready`. PVC `models-ollama-0` not deleted; container restarts in place. |
| 2026-04-28 | W1.1 | Warm-PVC pod restart -> first inference reply | 45.9 s | 11.7 s pod-Ready + 34.6 s model reload into GPU + ~0.4 s eval. The model load is not amortized across pod restarts; it is paid every time the runner process starts. |
| 2026-04-28 | W1.1 | Disk footprint after pull (Ollama models dir) | 18.557 GB blob + ~5 KB manifests | Reported by `/api/tags` (`size: 18556699314`). |

### Observations (2026-04-28, W1.1)

- **Cold pull from `registry.ollama.ai`** at ~45 MB/s is bandwidth-bound on the
  Spark uplink, not CPU-bound. This is the "first time on this node" cost.
  Re-pulls of the same digest are no-ops because Ollama deduplicates by
  content-addressed blob.
- **Pod restart is fast (~12 s) but first-inference is dominated by the
  34 s model reload** into GPU memory. Until Ollama has a "keep model
  resident across runner restarts" option, every pod recreate pays this cost.
  Candidate "warm cache" requirement for the future Unbounded Storage product.
- **Same logical model, different engines, different quantizations.** Pending
  W1.2: once vLLM pulls Qwen3-30B-A3B BF16 (~60 GB HF safetensors) we can
  compare against the 18.6 GB GGUF Q4_K_M Ollama uses for "the same model."
- **Cross-namespace dedup is not free.** A second namespace pulling the same
  Ollama tag would pull the 18.6 GB blob again into its own PVC. To be
  measured at W2.x or whenever a second Ollama deployment lands.
