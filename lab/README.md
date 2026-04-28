# unbounded-lab

AI showcase running on top of unbounded-kube. See [`plan.md`](../plan.md) for the
sponsor-facing plan and wave structure.

## Hard rule

- [`deploy/`](../deploy/) is for unbounded-kube *component* manifests (machina, net,
  inventory) rendered via `make *-manifests`.
- [`lab/`](.) is for *AI workloads* running on top of unbounded-kube.

The two directories never mix.

## Layout

| Path | Wave items | What it is |
|---|---|---|
| [`inference/ollama-qwen-moe/`](inference/ollama-qwen-moe/) | W1.1 | Ollama serving Qwen MoE on spark-3d37 |
| [`inference/vllm-qwen-moe/`](inference/vllm-qwen-moe/) | W1.2 | vLLM serving Qwen MoE on spark-2c24 |
| [`inference/sglang-dense/`](inference/sglang-dense/) | W2.4 | SGLang third-engine pick |
| [`inference/vllm-vision/`](inference/vllm-vision/) | W2.6 | Multimodal vLLM |
| [`inference/vllm-tp2/`](inference/vllm-tp2/) | W3.1 | Two-Spark TP=2 vLLM via KubeRay or LWS |
| [`models/ollama-dense/`](models/ollama-dense/) | W2.5 | Dense chat model |
| [`models/whisper/`](models/whisper/) or [`models/reranker/`](models/reranker/) | W2.8 | Small specialized model |
| [`rag/`](rag/) | W2.7 | Embedding + ChromaDB + LLM |
| [`training/lora-job/`](training/lora-job/) | W2.2 | LoRA fine-tune as a Job |
| [`training/megatron-single/`](training/megatron-single/) | W2.1 / R1 | Megatron single-node |
| [`training/megatron-multinode/`](training/megatron-multinode/) | W3.2 | Megatron multi-node |
| [`training/eval-harness/`](training/eval-harness/) | W2.3 | lm-evaluation-harness |
| [`training/continuous-pretrain/`](training/continuous-pretrain/) | W4.1 | Continuous pre-training |
| [`training/scalarlm/`](training/scalarlm/) | W3.4 / W3.5 | ScalarLM, gated by R3 |
| [`observability/`](observability/) | W1.5 | Prometheus + Grafana + DCGM exporter |
| [`ingress/`](ingress/) | W1.4 | Shared ingress + auth + TLS pattern |
| [`geo/`](geo/) | Wave 5 | Front Door, regional routing, FL |
| [`bench/`](bench/) | W1.6 | Repeatable benchmark harness |
| [`storage-pain-journal.md`](storage-pain-journal.md) | all waves | Append-only measurements |
| [`sponsor-updates/`](sponsor-updates/) | all waves | YYYY-MM.md monthly updates |

## Per-item conventions

Each `lab/<area>/<item>/` directory contains:

- `README.md` - what it deploys, what it proves, deploy + teardown commands.
  Model license + provenance info goes in the README's intro for any item
  shipping or running model weights.
- Plain YAML manifests or a `kustomization.yaml`. Templating is allowed when reused.

## Cluster state at Wave 1 start

The `plan.md` Hardware Inventory section refers to `qwen35-35b` and `qwen36-35b`
namespaces as "currently running." On the live cluster those namespaces do not
exist; W1.1 and W1.2 are deployed greenfield directly into the new
`lab-ollama-qwen-moe` and `lab-vllm-qwen-moe` namespaces. The "grandfathering"
rule in the plan's Glossary therefore is a no-op for this cluster.

## Region A node labels

Wave 1 sets these labels on the Region A Sparks (idempotent):

```
lab.unbounded.cloud/region=a
lab.unbounded.cloud/hardware-class=dgx-spark-gb10
```

Apply via [`foundation/label-region-a.sh`](foundation/label-region-a.sh).
