# Inference Toolkit

## Prerequisites

- Two `Standard_ND96asr_v4` nodes labeled `accelerator=nvidia` and `sku=gpu`, with all eight GPUs allocatable.
- NVIDIA drivers, Container Toolkit, GPU device plugin, GPU feature discovery, and an RDMA-capable network operator configuration.
- [LeaderWorkerSet](https://github.com/kubernetes-sigs/lws) 0.9.0 or a compatible release.
- A current `kubectl` with Kustomize support.
- An existing ext4 filesystem of at least 2 TB at `/dev/sdb1` on each GPU node. The storage manifest mounts it at `/mnt/inference-cache`; it never formats a device.

Install the pinned LeaderWorkerSet controller and prepare node-local model caches:

```sh
kubectl apply --server-side -k platform/lws
kubectl wait --for=condition=Available -n lws-system deployment/lws-controller-manager --timeout=5m
kubectl apply -k platform/storage
kubectl rollout status -n inference-platform daemonset/local-cache-preparer --timeout=5m
```

Verify the node type, GPUs, InfiniBand devices, and rendered manifest before deploying:

```sh
kubectl get nodes -L accelerator,sku,nvidia.com/gpu.present,nvidia.com/gpu.product
kubectl kustomize deploy
```

## Deploy

```sh
kubectl apply -k deploy
kubectl wait -n inference-system --for=condition=Available leaderworkerset/vllm --timeout=65m
```

## Call And Inspect

The API is an unauthenticated, cluster-internal ClusterIP:

```sh
kubectl port-forward -n inference-system svc/vllm-api 8000:8000
curl --fail http://127.0.0.1:8000/health
curl --fail http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "qwen",
    "messages": [{"role": "user", "content": "Explain pipeline parallelism in one sentence."}],
    "chat_template_kwargs": {"enable_thinking": false},
    "max_tokens": 128
  }'
```

```sh
kubectl get leaderworkerset,pod,svc -n inference-system
kubectl logs -n inference-system -l app.kubernetes.io/component=leader -c vllm --tail=200
kubectl get events -n inference-system --sort-by=.lastTimestamp
```

## Tune And Benchmark

Run GuideLLM from a separate CPU node. Required Pod anti-affinity prevents the benchmark client from consuming CPU or memory on either inference VM. The client requests two CPUs and 4 GiB of memory so it fits the validated four-vCPU system nodes; it has no CPU limit and can use otherwise-idle capacity.

```sh
kubectl apply -k benchmarks
kubectl wait -n inference-system --for=condition=Complete job/guidellm --timeout=2h
kubectl logs -n inference-system job/guidellm
```

The benchmark sweeps 128, 192, and 256 concurrent streams with fixed synthetic input and output lengths. The validated peak at 256 streams is approximately 1,455 output tokens/s and 7,285 total tokens/s with zero request errors and zero preemptions.
Results are retained in the `guidellm-results` PVC. The long-running, read-only results Pod permits artifact retrieval after the Job container exits:

```sh
kubectl cp -n inference-system guidellm-results-reader:/results ./results
```

