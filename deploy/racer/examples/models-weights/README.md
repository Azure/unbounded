# Model weights through Racer: example manifests

Plain YAML examples for serving vLLM models whose weights come from a Racer
cache. They are templates: replace every `<placeholder>` before applying. The
full walkthrough is the [vLLM guide](../../../../docs/content/guides/racer-vllm.md).

These examples depend on the `racer-vllm` commands described in the guide.
Check that your release includes them.

| File | What it holds |
|------|---------------|
| `clustercaches.yaml` | Two ClusterCaches, `models-hillsboro` and `models-dapple`, one per site |
| `import-job.yaml` | Namespace `racer-models`, ServiceAccount `model-importer`, and a Job that imports a node's Hugging Face snapshot |
| `serve-qwen.yaml` | A single-GPU Deployment; the simplest template |
| `serve-glm5.yaml` | An 8-GPU tensor and expert parallel Deployment that uses sharded artifacts when present |

## Placeholders

| Placeholder | Meaning |
|-------------|---------|
| `<hillsboro-site>`, `<dapple-site>` | The value of the `unbounded-cloud.io/site` label on each site's nodes |
| `<storage-account>` | The Azure storage account that holds the `models` container |
| `<managed-identity-client-id>` | Client ID of the reader identity the dataplane uses (see below) |
| `<writer-identity-client-id>` | Client ID of the identity the import job uses |
| `<tenant-id>` | Microsoft Entra tenant ID of the writer identity |
| `<registry>`, `<tag>` | Where the Racer images are published, and the release tag |
| `<vllm-version>` | A supported `vllm/vllm-openai` tag (0.30 or 0.33 series) |
| `<model-name>`, `<model-prefix>`, `<glm5-prefix>` | The model's name and its path inside the `models` container |
| `<node-with-snapshot>` | The node whose disk holds the snapshot to import |
| `<hf-cache>`, `<org>`, `<model>`, `<revision>` | The snapshot directory on that node, usually `/root/.cache/huggingface/hub/models--<org>--<model>/snapshots/<revision>` |

To add a site, copy one ClusterCache, give it a new name and site, and
change `racer.unbounded-cloud.io/client-cache` and the `racer://` URIs in the
serve manifests to the new cache name.

## Prerequisites

- **Reader identity.** A user-assigned managed identity with the
  `Storage Blob Data Reader` role on the `models` container, and a federated
  credential for subject `system:serviceaccount:unbounded-system:racer` (the
  Racer dataplane ServiceAccount) with audience `api://AzureADTokenExchange`.
- **Writer identity.** A separate identity with `Storage Blob Data
  Contributor` on the container, and a federated credential for
  `system:serviceaccount:racer-models:model-importer`. Keep it separate so the
  dataplane cannot write the origin.
- **Raw devices (optional).** To use NVMe devices directly, set the Node
  annotation `racer.unbounded-cloud.io/block-devices` to a regex over
  `/dev/disk/by-id` basenames. The first and last 1 MiB of each matched
  device must already be zero. **Racer never zeros them**, and it overwrites
  everything between them. The operator must check each matched device and
  clear the guards in a separate, approved step before enabling it. In device
  mode `RACER_SLAB_BYTES_PER_WORKER` does not limit capacity.

## Rollout order

1. Upgrade the Racer dataplanes on every node to a release with the
   `racer-vllm` manifest and sharded-artifact support.
2. Apply `clustercaches.yaml`.
3. Run the import job and wait for it to finish:
   `kubectl apply -f import-job.yaml`.
4. Apply a serve manifest: `kubectl apply -f serve-qwen.yaml`.

## Checks

Reach a dataplane's diagnostics port through the API server:

```bash
POD=<dataplane-pod>
kubectl get --raw "/api/v1/namespaces/unbounded-system/pods/${POD}:9090/proxy/debug/hardware"
kubectl get --raw "/api/v1/namespaces/unbounded-system/pods/${POD}:9090/proxy/metrics" \
  | grep -E 'racer_effective_payload_bytes|racer_delivery_bytes_total|racer_gpu_read_requests_total|lookup_(hits|misses)_total'
```

- `/debug/hardware` lists block devices and the ones Racer claimed. Use it
  to confirm device mode picked the intended devices.
- `racer_effective_payload_bytes` is the cache payload capacity after
  overhead. It should exceed the models you expect a node to hold.
- `racer_delivery_bytes_total` grows by about the model size for each load.
- `racer_gpu_read_requests_total{outcome="gpu"}` counts GPU deliveries;
  other outcomes explain fallbacks.
- `racer_*_lookup_hits_total` and `racer_*_lookup_misses_total` show where
  pages came from.
