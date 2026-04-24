# Plan: unbounded-lab

*AI showcase and proving ground for unbounded-kube, running on DGX Spark today and
designed to transplant onto GB200/GB300 tomorrow.*

## Goals

1. **Demonstrate** AKS + unbounded-kube credibly orchestrates the AI workloads.
2. **Build Experience** in deploying production AI workloads on 
    Kubernetes, including inference, fine-tuning, RAG pipelines, multi-node distributed work, and multi-region orchestration.
3. **Hardware-portable patterns:** every artifact built on DGX Spark (GB10, ARM64) is
   designed to transplant onto GB200/GB300 hardware when it lands - Spark is the on-ramp,
   not the destination.
4. **Internal product input:** the Storage Pain Journal feeds the unbounded-kube and
   future Unbounded Storage product teams with measured friction from real workloads.

**The infrastructure of the showcase is the edge-join story:** unbounded-agent +
unbounded-net (WireGuard) joining ARM64 DGX Sparks (eventually across 3 regions) to an
AKS control plane - one cluster, one kubectl context. Every other demo (inference engines,
RAG, fine-tuning, multi-node, multi-region) sits on top of that spine. It is the one
thing only unbounded-kube + AKS does in the Microsoft-aligned stack today.

**Baseline assumption:** AKS-managed control plane is the control-plane substrate. Eventually we might want to show unbounded-kube running on other control planes (kubeadm, OpenShift, NeoCloud's k8s service), but for this showcase we focus on AKS. The story is "unbounded-kube extends AKS to the edge with heterogeneous GPU nodes," not "unbounded-kube runs on every control plane."

## Hardware Inventory (Demo Lab)

The spine of the story is "ARM64 DGX Sparks under one AKS control plane via unbounded-agent."
One hardware class today (DGX Spark, GB10, ARM64); multi-region comes online when Regions B
and C land.

**Already deployed:**
- **Region A**: spark-3d37 + spark-2c24 (both DGX Spark, GB10 ARM64 sm_120, 120 GiB
  unified memory, 273 GB/s bandwidth). ConnectX-7 at 200 Gbps intra-region link.
  Currently running: Ollama on spark-3d37, vLLM on spark-2c24.
- **AKS control plane** in Canada Central, gateway nodes with public IPs, cert-manager,
  nginx-ingress. Region A Sparks attached via unbounded-agent + unbounded-net (WireGuard).

**Pending (not a blocker before Wave 5):**
- **Region B and Region C**: two more DGX Sparks each, same hardware class, in the process
  of being deployed. Identical to Region A's hardware; will join the same AKS control
  plane via the same unbounded-agent + WireGuard pattern. Waves 1-4 assume Region A only.
  Wave 5 is gated on these landing.

**Future (post-Spark):**
- GB200 / GB300 hardware when available. Patterns built in this lab on Spark MUST be
  designed to transplant onto GB200/GB300 with minimal re-work. See "GB200/GB300 Transfer
  Plan" section.

**Not in scope:**
- No Azure GPU VM node pool. The spine differentiator is edge-Sparks-to-AKS via
  unbounded-agent, not hardware heterogeneity within one cluster.
- No Jetson or other ARM edge hardware in this round.

**Shared infrastructure:**
- AKS, Azure CNI, nginx-ingress, cert-manager - standard Microsoft stack throughout.

## What Sponsors Should See

Each demo should illustrate one or more of these unbounded-kube + AKS capabilities:

1. **Edge node joining**: ARM64 Blackwell GB10 Sparks behind NAT, joined to an AKS
   control plane via unbounded-agent + WireGuard. One cluster, one kubectl.
2. **GPU discovery and scheduling**: unbounded-agent generates CDI specs, RuntimeClass,
   device plugin on Sparks; pods request `nvidia.com/gpu` and just work.
3. **Workload portability**: manifests built on Spark (ARM64) are designed to transplant
   onto GB200/GB300 (also ARM64) with minimal re-work.
4. **Model/weight management**: PVCs on local-path storage keep models on the node;
   weights stay where the GPU is.
5. **Secure exposure**: auth, TLS, ingress - standard k8s patterns, work for AI workloads.
6. **Multi-node coordination (intra-region)**: distributed inference/training across two
   Sparks over ConnectX-7 using k8s-native primitives.
7. **Multi-region under one control plane (Wave 5)**: when Regions B and C land, 3
   regions of Sparks all visible via one kubectl; workloads placed by region-aware scheduling.
8. **AKS as first-party citizen**: AKS control plane, Azure CNI, Azure Container Registry,
   Azure Blob, Azure Monitor - the Microsoft stack is visible and load-bearing throughout.

## Showcase Structure

Six phases, each proving a specific unbounded-kube capability. The demos progress from
"one node joined to a cluster" through "multiple edge Spark nodes coordinated in one
region over ConnectX-7" to "a globally distributed GPU platform under one kubectl
context".

**Every phase tracks two things:**
1. **unbounded-kube capability proved** - what the demo shows is possible today
2. **Storage pain observed** - real friction (egress, duplication, disk pressure, auth glue)
   that we encounter while building the demo, logged in the Storage Pain Journal at the
   bottom of this plan

This dual view makes each demo a working capability proof AND a requirements data point
for the future Unbounded Storage product. We are NOT deploying Unbounded Storage in this
lab - it doesn't exist yet - but we ARE capturing the problems it would solve.

---

### Phase 0: Foundation - "unbounded-kube runs AI on any node"

**unbounded-kube capability proved:** Edge GPU nodes behind NAT, far from the cloud control
plane, become first-class Kubernetes citizens with full GPU scheduling, storage, and networking.

**0.1 - Edge node join**
- DGX Spark joined to AKS via unbounded-agent + WireGuard tunnel (already running)
- Demo point: "I can put a GPU server in my factory/lab/office and manage it from
  my existing AKS control plane in Azure."

**0.2 - Automatic GPU discovery and scheduling**
- unbounded-agent generates CDI specs, installs RuntimeClass, configures NVIDIA device plugin
  (already running)
- Demo point: "I don't have to hand-configure GPU drivers, containerd, CDI. The node
  joins and GPU-scheduled pods just work."

**0.3 - Local model/weight storage**
- Model weights stored on local-path PVC on the Spark - weights stay on the edge node
  (already running)
- Demo point: "My proprietary models never leave my hardware. The control plane
  only sees orchestration metadata, never the weights."

**0.4 - Standard ingress/auth/TLS patterns**
- nginx-ingress, cert-manager, auth proxy - exactly the same patterns as any other AKS
  workload (already running)
- Demo point: "AI workloads are just workloads. I don't need an AI-specific platform."

**0.5 - Edge Sparks as first-class AKS nodes (THE SPINE)**
- 2 DGX Sparks (ARM64, GB10) in a Boulder lab, joined to an AKS control plane in Canada
  Central via unbounded-agent + unbounded-net (WireGuard)
- `kubectl get nodes -L hardware-class,region` shows the Sparks alongside the AKS gateway
  nodes, in a distant region, under one cluster
- This is the artifact ONLY unbounded-kube + AKS produces in the Microsoft-aligned stack.
  Every other phase sits on top of it. Highlighted in every sponsor update as the
  differentiator.

**Storage pain observed in Phase 0:**
- Model weights living on local-path PVC = pinned to one node. Two distinct failure modes
  to measure separately:
  - **Node reboot (disk survives):** PVC contents persist under
    `/opt/local-path-provisioner/...`; pod restarts fast, no re-pull. This is the happy case.
  - **Node loss or PVC deletion (disk gone):** weights must be re-pulled from HF Hub on
    recovery. This is the painful case. Cannot reschedule to a different node automatically.
- No way to share a cached weight between namespaces on the same node without re-downloading
  (separate PVCs).
- Evidence for Unbounded Storage: even in the simplest single-node case, local-path PVC
  creates a "weights trapped on one node" problem AND a "no reschedule on node loss" problem.

---

### Phase 1: Inference Diversity - "any inference engine, same cluster"

**unbounded-kube capability proved:** The platform is engine-agnostic. Multiple inference
runtimes coexist on the same nodes, each picked for its strengths, exposed via standard
Kubernetes primitives.

**1.1 - Ollama (developer-friendly GGUF serving)**
- Qwen 3.5 35B MoE, Q4_K_M, 54.6 t/s (already running)
- Strengths: Easy model management, GGUF support, quick swapping
- Demo point: "For quick experimentation and dev use, Ollama is a great fit."

**1.2 - vLLM single-node (production-grade, high throughput)**
- Qwen 3.6 35B-A3B MoE, BF16 (~70 GB weights on disk, already running)
- Strengths: Continuous batching, paged attention, OpenAI-compatible API
- Demo point: "For production serving at scale, vLLM on the same nodes."
- Note: Each DGX Spark has only 1 GB10 GPU, so single-node vLLM is 1-GPU TP=1. Multi-GPU
  tensor parallelism (TP>1) requires multi-node on this hardware - covered in Phase 4.
- **Reality check:** 70 GB weights + KV cache + activations on 120 GiB unified memory
  leaves ~40-50 GiB for KV. At 32K context with batching, this is tight. Wave 1.2 must
  measure actual achievable batch x context and document, not claim unlimited headroom.

**1.3 - A third engine for breadth**
- Options:
  - SGLang (structured output, good for agents/tool-use)
  - TGI (HuggingFace Text Generation Inference)
  - llama.cpp server (lightweight, GGUF)
- Demo point: "Pick the best tool for each workload - no platform lock-in."

**1.4 - Uniform ingress/auth across engines**
- Same nginx-ingress + auth + TLS pattern exposes all three engines
- Demo point: "One set of ops patterns works for every AI engine."

**Storage pain observed in Phase 1:**
- Each engine uses a different artifact for the same logical model: Ollama ships GGUF
  (~24 GB for 35B-A3B Q4_K_M), vLLM ships HF safetensors (~70 GB BF16). Not the same
  bytes - different quantizations of the same model. Total on disk: ~94 GB for "one
  model, two engines." Evidence for a **content-addressable cache that can transcode
  formats**, not just dedupe bytes.
- Pod restart on Ollama triggers re-load from local PVC (fast) but a fresh deployment on a
  different node forces a re-pull from HF Hub (slow, costly).
- No cross-engine model cache sharing. Evidence for a unified content-addressable cache.

---

### Phase 2: Model Diversity - "one cluster, many model classes"

**unbounded-kube capability proved:** The platform orchestrates every interesting AI workload
class, not just chat LLMs.

**2.1 - Dense chat/coding model**
- Primary pick: **Qwen 3.6 27B Dense** if the model is publicly released and GGUF quants
  have stabilized by the time Wave 2 Track A starts. As of this plan the model is a
  discussion topic, not a shipped artifact.
- Committed fallback chain (decide at Wave 2 kickoff): Qwen 3.5 32B Dense → Llama 3.3
  70B Q4 → Mistral Large 2 Q4. Plan does not block on Qwen 3.6 landing.
- Shows: dense transformer inference, high-memory footprint use case

**2.2 - Mixture-of-Experts (MoE)**
- Qwen 3.5/3.6 35B-A3B (already running)
- Shows: bandwidth-efficient inference, different scaling characteristics

**2.3 - Multimodal / vision**
- Qwen-VL or Llama 3.2 Vision 11B
- Shows: images in/text out, code-review-of-screenshots use case

**2.4 - Embeddings + vector store + RAG**
- Small embedding model (nomic-embed-text) + ChromaDB/Qdrant pod + LLM
- Shows: multi-component AI pipeline, not just single-model inference
- Shows: PVC-backed vector store on the edge node

**2.5 - Small specialized models**
- E.g., reranker, classifier, tiny translator, or speech-to-text
- Shows: workloads far beyond "chatbot", including non-LLM AI

**Storage pain observed in Phase 2:**
- RAG dataset (2.4) wants to live somewhere persistent and shareable across nodes. Local-path
  PVC pins it to one node; a shared store means external dependencies.
- Having 5+ different model classes active = 5+ independent sets of weights on disk, each
  cached locally per engine. No deduplication, no P2P sharing.
- Vector store size grows with documents; no tiered storage story (hot vectors on NVMe, cold
  on blob). Evidence for DataPolicy CRD retention concept.

---

### Phase 3: Training on unbounded-kube - "not just inference"

**unbounded-kube capability proved:** The platform handles long-running training jobs with
checkpointing, not just stateless inference pods. Shows breadth from lightweight LoRA jobs
to full distributed pretraining stacks.

**3.1 - LoRA fine-tuning as a Kubernetes Job**
- Qwen 3.6 14B base + HuggingFace PEFT + PyTorch, as a k8s Job (or PyTorchJob CRD)
- Domain: e.g., Kubernetes troubleshooting Q&A, or customer-specific codebase
- Shows: Job lifecycle, PVC-backed checkpoints, GPU scheduling for training
- Lightweight entry point: fits on a single Spark

**3.2 - Megatron-LM single-node training**
- NVIDIA Megatron-LM / Megatron Core on one Spark
- Framework known for production transformer training at scale (2B-462B on H100 clusters),
  with Blackwell optimizations on the roadmap
- Use case: supervised fine-tuning (SFT) or RLHF on a smaller model (7B-14B) on one node
- Shows: customers can run the *real* NVIDIA training stack on unbounded-kube, not just toy
  examples
- Key risk: Megatron's container is built on NGC PyTorch which we'd need to verify on ARM64+CUDA 12.9
- Files: Kubernetes Job manifest launching the Megatron pretrain_gpt.py script with checkpoint PVC

**3.3 - ScalarLM closed-loop platform**
- ScalarLM integrates vLLM (inference) + Megatron-LM (training) + HF Hub in a single
  Kubernetes deployment with a shared-checkpoint model
- Demonstrates: "online learning" - query running model, generate training data from results,
  submit post-training job against same deployment, updated checkpoint picked up
  automatically at next inference request
- Validated models per ScalarLM docs: Gemma 3, Qwen 3.5 35B-A3B, Qwen 3.5 122B-A10B,
  gpt-oss, Nemotron 3 Super (MoE models run natively on NVIDIA and AMD)
- Key risk: Per `docs/dgx-spark-deployment-challenges.md`, ScalarLM had no ARM64+CUDA image
  at initial deployment attempt. Need to re-verify current state; if still blocked, work
  with TensorWave (ScalarLM maintainer) to get an ARM64+CUDA build or build one ourselves.
- If successfully deployed, this becomes the flagship Phase 3 demo: the only platform that
  closes the inference-to-training loop.

**3.4 - Continuous pre-training / domain adaptation**
- Longer-running Megatron or PyTorch job with periodic checkpointing to PVC
- Shows: restartable training, checkpoint resume, multi-hour GPU workloads

**3.5 - Evaluation pipeline**
- Eval harness (lm-evaluation-harness or similar) as another k8s Job
- Compares base vs fine-tuned model on benchmarks
- Shows: the "train + evaluate + deploy" loop, all on the same cluster

**Storage pain observed in Phase 3:**
- Training datasets pulled from HF Hub per job. If two fine-tuning experiments use the same
  dataset, it's downloaded twice. Multi-TB datasets would be brutal.
- Checkpoint accumulation: continuous pre-training writes N checkpoints over hours. Local
  PVC fills up; manual cleanup scripts needed. This is the exact "checkpoint chaos" the
  Unbounded Storage problem statement calls out.
- ScalarLM shared-checkpoint model depends on a place both training and inference pods can
  read/write. In-cluster this is a PVC with ReadWriteMany; not all storage classes support
  that well. Evidence for Unbounded Storage's async write-back pattern.
- No deduplication of base-model weights across experiments. Fine-tuning 14B x 5 experiments
  = 5 copies of the base weights sitting on disk.

---

### Phase 4: Multi-Node & Distributed (Intra-Region) - "scale out, not up"

**unbounded-kube capability proved:** Edge GPU nodes in the same region act as
one coordinated pool. Workloads span nodes using the high-speed interconnect (ConnectX-7 at
200 Gbps between the two Sparks in a region pair).

**Important context:** Each DGX Spark has ONE GB10 GPU. "Multi-GPU" on this hardware means
multi-NODE within a region pair (ConnectX-7 reach). Everything in Phase 4 exercises the
intra-region, low-latency inter-node path. Inter-region coordination is Phase 5.

**4.1 - Tensor-parallel inference across both Sparks (vLLM TP=2)**
- vLLM with tensor parallelism TP=2 across spark-2c24 and spark-3d37
- Target: a model that needs >120 GiB effective memory (Llama 3.1 70B BF16, Qwen 3.5
  122B-A10B MoE, or quantized 200B+ models)
- vLLM coordinates via Ray across nodes; traffic rides ConnectX-7 at 200 Gbps
- Shows: 240 GiB effective memory via distributed serving
- Demo point: "Models that don't fit on one node are served transparently via
  standard k8s primitives."

**4.2 - Megatron-LM distributed training (TP + PP + DP across 2 nodes)**
- Megatron-LM launched as multi-pod training job (MPIJob / PyTorchJob / custom)
- Fine-tune or continue-pretrain a 30-70B model using Megatron's native TP/PP/DP strategies
- Shows: the full NVIDIA production training stack running on unbounded-kube's edge
  node pool, coordinating across edge nodes
- Demo point: "Scale the training frameworks NVIDIA ships at hyperscalers down to
  your own edge hardware, without rewriting anything."

**4.3 - ScalarLM multi-node closed-loop**
- If Phase 3.3 ScalarLM works single-node, scale it to 2 nodes
- Training: Megatron-LM across both Sparks via ScalarLM's Slurm-in-k8s scheduler
- Inference: vLLM with TP=2 across both Sparks
- Shows: the full train-infer loop distributed across the edge node pool
- Demo point: "Online learning on edge hardware at scale."

**4.4 - Failure/resilience demo**
- Cordon or drain one Spark during a distributed workload
- Show rescheduling / graceful degradation / recovery
- Demo point: "Standard k8s resilience patterns apply to distributed AI workloads too."

**Storage pain observed in Phase 4:**
- **The thundering herd in miniature**: vLLM TP=2 cold start - both Sparks independently
  pull the same ~70 GB FP8 (or ~35 GB/node TP-shard, per W3.1's sizing) from HF Hub. 2x
  the egress, same content, no P2P sharing even though ConnectX-7 is sitting there at 200
  Gbps idle.
- **Distributed training data pipeline**: Megatron TP/PP/DP with all ranks pulling the same
  dataset shards = classic thundering-herd egress explosion. At scale (10s of nodes) this
  would saturate origin bandwidth.
- **Checkpoint coordination**: multi-pod training needs a shared writable path for
  checkpoints. RWX PVC choice affects performance dramatically. Async write-back pattern
  (NVMe buffer + async upload) would materially help.
- **Evidence for Unbounded Storage**: this is literally the hero use case - one origin fetch
  serves the cluster via P2P over ConnectX-7. Our lab would DIRECTLY validate the P2P claim.

---

### Phase 5: Geo-Distributed - "one cluster, many regions"

**unbounded-kube capability proved:** ONE AKS control plane orchestrates GPU nodes across
multiple regions. Data stays local, workloads follow policy, operations stay unified. The
differentiator is the edge-join pattern itself: unbounded-agent + WireGuard joins
region-local Sparks behind NAT to a central AKS control plane without requiring Azure Arc
licensing, customer datacenter ingress, or separate regional control planes. Other managed
fleet products (Arc, Anthos, Rancher) solve adjacent problems, but none do AKS-native
NAT-traversing edge join with one kubectl context out of the box.

**Hardware layout (target):**
- Region A (current): 2x DGX Spark + ConnectX-7 intra-region link
- Region B (new pair): 2x DGX Spark + ConnectX-7 intra-region link
- Region C (new pair): 2x DGX Spark + ConnectX-7 intra-region link
- Total: 6 GB10 nodes, ~720 GiB unified memory, all joined to one AKS control plane

**Critical design constraint:** Cross-region = WAN latency (tens to hundreds of ms) +
shared bandwidth. Fine-grained distributed work (tensor parallelism, pipeline parallelism,
all-reduce gradients) DOES NOT work cross-region; those stay intra-region (Phase 4).
Cross-region coordination is coarse-grained: independent replicas, periodic aggregation,
policy-driven routing.

**5.1 - Geo-routed inference (data sovereignty)**
- Same model deployed independently in all 3 regions (each using Phase 4.1 intra-region TP=2)
- Topology-aware k8s routing or external GSLB sends user requests to the nearest/policy-matching region
- EU user's prompt stays in EU. US user's prompt stays in US. APAC stays in APAC.
- Demo point: "Data sovereignty without running three separate platforms. One set
  of manifests, one control plane, three regions."

**5.2 - Regional model specialization**
- Different models per region: language-tuned model in APAC, EU-language model in EU,
  English coding model in US
- Same service abstraction; region-aware routing picks the right model
- Demo point: "Customize the workload per region without fragmenting operations."

**5.3 - Follow-the-sun batch training**
- Long-running training job scheduled to whichever region has off-peak hours or cheap/green power
- Checkpoint to shared storage (Azure Blob) or region-local PVCs with periodic sync
- K8s taints/tolerations + node selectors drive the placement
- Demo point: "Shift compute to where it's cheapest or greenest, globally, automatically."

**5.4 - Federated fine-tuning across regions**
- Each region trains on its own local data (compliant with data residency rules)
- Periodic aggregation (FedAvg, DiLoCo, or similar) across regions via the control plane
- Gradients and model deltas travel; raw data does NOT
- Framework options: Flower, NVIDIA FLARE, or a custom aggregator (need ARM64+CUDA validation)
- Demo point: "Learn from every site's local data while respecting every site's data rules."
- Key risk: federated learning frameworks on ARM64+CUDA 12.9 - needs verification

**5.5 - Disaster recovery demo**
- "Kill" a region (cordon all nodes, or simulate WAN outage via WireGuard tunnel teardown)
- Traffic automatically reroutes to surviving regions
- Region comes back, rejoins the pool, picks up work
- Demo point: "Regional outages don't mean service outages."

**5.6 - Unified observability across regions**
- Prometheus + Grafana (standard k8s stack) showing per-region: GPU utilization, tokens/sec,
  WAN egress, inference latency percentiles, training job progress
- Demo point: "Full operational visibility across all GPU hardware globally -
  no AI-specific monitoring stack, no per-region fragmented tooling."

**Storage pain observed in Phase 5:**
- **3x origin fetch**: each region independently pulls model weights from HF Hub (or wherever
  the source lives). 3 regions x 65 GB = 195 GB of WAN egress for the same content. A regional
  cache tier would eliminate 2/3 of that.
- **Multi-provider auth explosion**: if Region B is on a NeoCloud and Region C is on-prem,
  each region has different credentials to reach the source data (Azure Blob IAM, S3 keys,
  NFS mounts). Today = per-region glue code. Evidence for rclone-backed unified auth.
- **Follow-the-sun checkpoint movement (5.3)**: when a job moves from Region A to Region B,
  the checkpoint must move too. Today this is a manual rsync or Azure Blob copy. Evidence
  for async write-back + cross-region replication.
- **Federated learning aggregation (5.4)**: gradient deltas need a shared meeting point.
  Today = rig up a blob store and hope everyone can auth to it. Evidence for a unified
  global namespace.
- **DR demo (5.5)**: when Region A dies, does Region B have the model weights and latest
  checkpoint? Today: only if someone set up cross-region replication manually. Evidence for
  DataPolicy CRD declarative DR.
- **This is the strongest evidence base**: Phase 5 is where Unbounded Storage's value
  proposition is most visible. Every cross-region problem we encounter validates the
  product requirements.


---

## Storage Pain Journal

**Purpose:** The future Unbounded Storage product solves a set of AI-data problems that this
showcase will naturally encounter. Rather than pretend the product exists, we log the pain
points as we build each phase. This serves two audiences:

1. **Anyone reviewing the demo** (sponsors, future team members): we can honestly say
   "here's what would be hard without a caching/replication layer, here's how much
   egress/disk/time it costs." No hand-waving.
2. **The Unbounded Storage product team**: real requirements data from a real cluster running
   real AI workloads, not hypotheticals.

**What to measure per phase (concrete numbers to capture as we build):**

| Metric | Phase(s) | How to capture |
|---|---|---|
| Time to first inference after cold pod start | 0, 1 | Pod event timestamps vs readiness |
| Origin egress per pod start (GB) | 0, 1, 2 | Network monitoring on node or blob store metrics |
| Duplicate weights on disk (same model, different engines) | 1 | `du` across model cache dirs |
| Dataset re-download count across training jobs | 3 | HF Hub download count or network counters |
| Checkpoint disk growth rate during long training | 3.4 | PVC usage over time |
| RWX PVC write throughput for multi-pod training | 4.2, 4.3 | fio from inside pods |
| Cold start origin fetches for TP=2 multi-node | 4.1 | Per-node egress during vLLM startup |
| Cross-region origin egress (same model, 3 regions) | 5.1 | Per-region network counters |
| Multi-provider credential glue (LOC and auth systems) | 5.x | Count distinct auth mechanisms in use |
| Cross-region checkpoint transfer time | 5.3 | Job migration wall time |
| Time to recover in DR (model weights availability) | 5.5 | From region kill to first response in survivor |

**Deliverable:** A running table kept alongside this plan (or in a sibling doc) that fills
in actual measured values as each phase ships. By the end of the showcase, we will have a
data-backed story of "this is what AI storage friction looks like today" - which is exactly
what Unbounded Storage's pitch needs to land.

**Note:** We are NOT building Unbounded Storage in this lab. We are NOT deploying Alluxio
or Fluid to solve these problems prematurely either - that would hide the pain we want to
document. We deploy naive-but-real storage patterns (local-path PVC, PVC with RWX, HF Hub
pulls, Azure Blob as origin) and measure what hurts.

**Audience:** This journal is **internal** - it feeds the unbounded-kube and Unbounded
Storage product teams. It is not a sponsor-facing artifact on its own; sponsor updates
summarize it, raw entries stay internal.

---

## GB200 / GB300 Transfer Plan

**Why this section exists:** sponsors will ask "does this work translate when we get
GB200/GB300?" Have the answer ready, not improvised. Every wave's deliverables get a
transfer-review at the end; this section is the rubric.

### What transplants unchanged

- All Kubernetes manifests (Deployments, StatefulSets, Jobs, Services, Ingresses,
  ConfigMaps, Secrets, NetworkPolicies). Sparks and GB200s both run k8s; manifests are
  hardware-agnostic.
- All container images that target ARM64 + CUDA. GB200/GB300 are also ARM-based (Grace
  CPU + Blackwell GPU). The ARM64 bring-up pain we eat today on Spark IS the work that
  pays off on GB200.
- unbounded-agent + unbounded-net join pattern. Same WireGuard-over-NAT approach;
  GB200/GB300 in a customer rack joins an AKS control plane the same way.
- Inference engine choices (Ollama, vLLM, SGLang). All have ARM64 builds.
- Training stack (PyTorch + HF Transformers + PEFT + Megatron-LM if R1 closes). NGC
  PyTorch ARM64 images are the same lineage on both.
- Observability (Prometheus + Grafana + DCGM exporter). Hardware-class is just a label.

### What changes (mostly relaxes)

- **Memory budget** balloons. GB200 has 192 GB HBM3e per GPU; GB300 has more. Models
  that need TP=2 multi-node on Spark fit on a single GB200 GPU. The "multi-node TP"
  demos become "single-node TP=N" demos - but the manifests STILL need to express this
  via env vars (`--tensor-parallel-size`), so the YAML changes by 1 line.
- **NVLink replaces ConnectX-7** for multi-GPU within a node. Multi-node still needs
  RoCE/IB. The intra-region multi-node story we build on Spark remains relevant but
  scales out (4-8 GB200s per node) instead of out (2 nodes per region).
- **MIG becomes available** on GB200/GB300. Multi-tenant GPU sharing - which is impossible
  on a 1-GPU Spark - becomes a real story. Out of scope for this lab; flag as future work.
- **Power, cooling, networking** at GB200 scale require datacenter-grade infra. Spark
  patterns assume edge-class power and cooling; the sizing math has to be re-done.

### What breaks (be honest)

- Anything we tune specifically for sm_120 (GB10) won't carry to sm_100/sm_103
  (GB200/GB300). The MoE kernel tuning JSON in `hack/E=256,N=1024,device_name=NVIDIA_GB10.json`
  is hardware-specific and will need a re-tune. This is normal and expected.
- Cost-per-token math is completely different. Spark numbers are not predictive of GB200
  numbers. Do NOT publish or quote Spark perf numbers as if they predict GB200 perf.
- Anything we hard-code to "1 GPU per node" assumptions breaks. Manifests should use
  `nvidia.com/gpu` resource requests, not implicit "the only GPU." Audit before transfer.

### Per-wave transfer review checklist

At end of each wave, fill in:

| Item | Transplants? | Changes needed | Re-test required |
|---|---|---|---|
| W1.1 Ollama manifests | y/n | | |
| W1.2 vLLM manifests | y/n | | |
| ... | | | |

Catches drift early. Anything that is Spark-specific gets called out and either generalized
or marked "Spark-only learning artifact, do not transplant."

---

## Team Learning Objectives

By end of plan, the engineer should have personally shipped at least one of each:

- **L1 - Inference deployment:** stand up a production-ish inference server (vLLM
  preferred, Ollama acceptable) end-to-end on k8s, including ingress, auth, observability,
  and a benchmark run
- **L2 - Fine-tuning:** run a LoRA fine-tune as a k8s Job, produce a usable adapter,
  evaluate it against the base model
- **L3 - RAG pipeline:** wire up an embedding model + vector store + LLM into a working
  retrieval pipeline served via a single endpoint
- **L4 - Multi-node distributed work:** debug at least one NCCL/Ray/multi-pod issue and
  ship a working multi-node deployment (vLLM TP=2 OR multi-node training)
- **L5 - Multi-region operations:** operate a workload across regions via the existing
  unbounded-agent joins and observe all regions from the central AKS control plane
- **L6 - GPU/driver troubleshooting:** diagnose and resolve at least one CUDA/driver/CDI
  /NCCL issue in writing, document for future team members / successors

### Tracking

End-of-wave retro: which objectives the engineer hit, which remain open. Anything not hit
by end of plan is a known gap, called out explicitly in the final sponsor update.

### Things we will NOT pretend to learn

- Bleeding-edge research (RL training, novel architectures, paper reproductions). Out of
  scope; consume models, don't train them from scratch.
- ML engineering as a discipline (loss curves, hyperparameter tuning, dataset cleaning).
  Out of scope; we deploy what works, we don't optimize what doesn't.
- Becoming an AI researcher. The engineer becomes an AI infra operator - a more durable
  skill and the one the sponsor is funding.

---

## First-Party Microsoft Story

**Why this section exists:** AKS + Microsoft alignment is the funded mandate. Make the
Microsoft stack visible and load-bearing throughout, not incidental. Sponsors should be
able to point at any layer and see Azure.

### Microsoft / Azure components in use

| Layer | Service | How it's used |
|---|---|---|
| Control plane | AKS | k8s control plane + gateway nodes |
| Networking | Azure CNI / Azure Load Balancer | Pod and service networking; public ingress |
| DNS / TLS | Azure DNS + cert-manager (ACME) | `vapa-ollama.canadacentral.cloudapp.azure.com` |
| Container registry | Azure Container Registry (ACR) | All custom-built images pushed here |
| Object storage | Azure Blob | Model weights origin, training datasets, checkpoints |
| Identity (workload) | Azure Workload Identity | Pod -> Blob auth without static keys |
| Monitoring | Azure Monitor + Container Insights | Cluster + node metrics, log shipping |
| Edge join | unbounded-agent + unbounded-net (WireGuard) | Sparks into AKS |
| Geo-routing (Wave 5) | Azure Front Door or Traffic Manager | Multi-region request routing |
| Disaster recovery (Wave 5) | Azure Blob geo-replication | Cross-region weight + checkpoint stage |

### Things to avoid

- Don't pull from Hugging Face Hub directly in a pod for production patterns - mirror to
  ACR or Azure Blob and pull from there. The Microsoft story requires Azure-hosted
  origins, not third-party origins.
- Don't use S3 / GCS / R2 for anything in this lab. Even if convenient for a one-off,
  the sponsor narrative breaks if a slide shows AWS in the architecture diagram.
- Don't roll our own auth where Azure AD / Workload Identity fits. The current Bearer
  token proxy is a tactical exception; document it as such, plan to replace with AAD
  integration before any cross-region endpoint (Wave 5) or external sharing.

### Architecture diagram requirement

Every wave's deliverable bundle includes an architecture diagram with Azure service
icons used everywhere they appear. Sponsors look at diagrams; make Azure visible.

---

## Execution Order

The phases above are organized by theme for the sponsor narrative. Actual execution
follows the wave structure below, sequentially, by one engineer. Risk spikes and
dependencies drive ordering within each wave.

### Wave 1: Solidify what we have

**Goal:** everything currently running is reproducible from source-controlled manifests,
documented, and measured. No demo is credible on an ad-hoc setup, and we need a
stable baseline to measure the Storage Pain Journal against.

**Preconditions:** none (the deployments already exist ad hoc).

**Scope:**

- **W1.1 - Ollama manifests under `deploy/ollama/`**
  - StatefulSet pinned to spark-3d37 via node selector
  - PVC on local-path storage for model weights (Qwen 3.5 35B-A3B MoE, ~24 GB)
  - Service + Ingress fronted by the existing auth proxy
  - Engine parameters via env vars on the container (`OLLAMA_NUM_PARALLEL=2`,
    `OLLAMA_CONTEXT_LENGTH=65536`, `OLLAMA_KEEP_ALIVE`, etc.) - NOT a ConfigMap of
    "engine parameters"; Ollama reads env, not a config file
  - `make` target or doc that applies the bundle from scratch on a clean namespace
- **W1.2 - vLLM manifests under `deploy/vllm/`**
  - Same pattern pinned to spark-2c24 (Qwen 3.6 35B-A3B MoE BF16, ~70 GB)
  - Includes the `hack/scratch/vllm-proxy.py` sidecar or a cleaned-up version
  - Includes the MoE tuning JSON as a ConfigMap (that one IS a config file)
  - **Sanity check as part of W1.2:** measure actual sustainable batch x context x prompt
    throughput, document in `docs/`. The "already running" claim is on the edge of OOM
    for KV cache at 32K and needs real numbers, not hand-waving.
- **W1.3 - Baseline Storage Pain Journal entries**
  - Measure, with stopwatch or `kubectl get events`:
    - Time from `kubectl apply` of a fresh namespace to first successful inference
    - Bytes egressed from Hugging Face / registry per pod start (tcpdump or az monitor)
    - Duplicate bytes on disk (same weights pulled by Ollama + vLLM separately)
    - Cold start time after node reboot (weights must repopulate PVC? Or persisted?)
  - These numbers are the "before" for any future Unbounded Storage product
- **W1.4 - Uniform ingress/auth doc**
  - Short writeup (1 page) showing how BOTH engines sit behind the same ingress, cert,
    and auth proxy - this IS the demo story for "standard k8s patterns apply to AI"
- **W1.5 - Observability foundation**
  - Prometheus + Grafana deployed on AKS, scraping the gateway nodes and both Sparks
  - NVIDIA DCGM exporter on each Spark for GPU utilization/memory/power
  - Node-exporter on each node for host-level metrics
  - Initial Grafana dashboard: per-node GPU util, memory, pod status, inference request
    rate for the deployed engines
  - Every later wave's measurements (Storage Pain Journal, benchmarks) go through this
    stack - it is a Wave 1 prerequisite, not a Wave 5 afterthought
  - Deliverable: `deploy/observability/` + one dashboard screenshot in sponsor update

**Definition of done:**
- Namespace `qwen35-35b` and `qwen36-35b` can be destroyed and recreated from repo in one
  command each
- Storage Pain Journal has 4 measured rows (not TODOs)
- `docs/` has a single "what's currently deployed" page
- `kubectl get nodes` shows the AKS gateway nodes AND both Region A Sparks under one
  control plane - the spine artifact is real, source-controlled, and reproducible
- Prometheus/Grafana dashboard live with GPU metrics from both Sparks
- Wave 1 transfer-review checklist filled in (GB200/GB300 transferability per item)

**Dependencies:** blocks all other waves. Solo engineer; no parallel work.

---

### Wave 2: Breadth

**Goal:** deploy the AI workloads customers run in production (one at a time, sequentially)
on the spine. The intra-region distributed climax (vLLM TP=2, Megatron multi-node) is
Wave 3. Single engineer works through items in order.

**Preconditions:** Wave 1 complete. Risk spike R1 (Megatron on ARM64) begins as the first
item of Wave 2 so a blocker surfaces early for downstream (Wave 3/4) dependencies.

**Scope (execute sequentially, top to bottom):**

- **W2.1 - Megatron-LM single-node on Spark (phase item 3.2) - RISK SPIKE R1**
  - First in the wave so a blocker surfaces early. See Risk Register R1 for test/fallback.
  - Deliverable: working `deploy/megatron-single/` manifest OR blocker report
- **W2.2 - LoRA fine-tuning as a K8s Job (phase item 3.1)**
  - PEFT + Transformers + small base model (Qwen 3.6 7B or Llama 3.2 3B)
  - Plain `batch/v1` Job, not a CRD - keep it boring
  - Validates: PyTorch ARM64+CUDA, bf16 training on GB10, Jobs can own a GPU
  - Deliverable: `deploy/training/lora-job.yaml` + a notebook showing the adapter works
- **W2.3 - Evaluation pipeline (phase item 3.5)**
  - `lm-evaluation-harness` in a K8s Job, targets Ollama/vLLM endpoints
  - Emits results as ConfigMap or Grafana-friendly metric
  - Deliverable: `deploy/eval/` + sample run comparing base vs LoRA
- **W2.4 - Third inference engine (phase item 1.3)**
  - Recommendation: **SGLang** (structured output, agents). Fallbacks: TGI or llama.cpp
  - Deploy one mid-size dense model
  - Deliverable: `deploy/sglang/` + a one-pager comparing with Ollama/vLLM
- **W2.5 - Dense chat/coding model (phase item 2.1)**
  - Qwen 3.6 27B if shipped, else fallback chain (Qwen 3.5 32B → Llama 3.3 70B Q4)
  - Deliverable: `deploy/ollama-dense/` + benchmark comparison vs MoE
- **W2.6 - Multimodal / vision (phase item 2.3)**
  - Qwen2.5-VL 32B or Llama 3.2 Vision 11B, on vLLM or Ollama
  - Deliverable: `deploy/vllm-vision/` + curl demo
- **W2.7 - RAG pipeline (phase item 2.4)**
  - Embedding model (BAAI/bge-m3) + ChromaDB (StatefulSet + local-path PVC) + LLM
  - Ingestion Job loads `docs/` into vector DB
  - Deliverable: `deploy/rag/` + a query script
- **W2.8 - Small specialized model (phase item 2.5)**
  - Whisper large-v3 (speech-to-text) OR reranker (bge-reranker) integrated with RAG
  - Deliverable: `deploy/whisper/` or `deploy/reranker/` + curl demo

The "single endpoint, multiple nodes" artifact is Wave 3's W3.1 (vLLM TP=2 across both
Sparks), not a Wave 2 item.

**Definition of done (Wave 2):**
- Items W2.1-W2.8 shipped, each with its own `deploy/` subfolder and deliverable
- Storage Pain Journal grows with: dataset re-download pain, checkpoint disk growth rate,
  duplicate weight accumulation across multiple deployments
- Wave 2 transfer-review checklist filled in (each item annotated for GB200/GB300 fit)
- Learning objectives L1, L2, L3 hit (inference, fine-tuning, RAG)

**Dependencies:** Wave 1 done. Sequential execution; the single engineer owns the order.
W2.2 LoRA uses HF Transformers + PEFT and is independent of R1 (Megatron); it can ship
even if R1 is still open. R1 must close (or have an accepted fallback) before Wave 3 W3.2
(Megatron multi-node) and before W4.1 (continuous pre-training).

---

### Wave 3: Multi-node intra-region

**Goal:** prove unbounded-kube coordinates distributed GPU workloads across two edge nodes
using the ConnectX-7 link, for both inference and training.

**Preconditions:** Wave 2 complete; R1 (Megatron ARM64) closed or on accepted fallback.
R4 (ConnectX-7 NCCL bandwidth) is closed *inside this wave* at W3.0 before W3.1 starts;
it is not a pre-wave gate. R3 depends on R3.prime's pre-wave answer (see Risk Register).

Note: W3.1 (vLLM TP=2) does not depend on R1; it can start as soon as W3.0 passes, in
parallel with R1 closure if needed.

**Scope:**

- **W3.0 - Risk spike R4: ConnectX-7 NCCL bandwidth validation**
  - RDMA/RoCE is available on the Sparks; this spike measures achieved bandwidth, it is
    not an availability investigation.
  - Run `nccl-tests` all_reduce_perf between spark-3d37 and spark-2c24; also run
    `ib_send_bw` / `ib_write_bw` as an RDMA-path sanity check.
  - Expected: bandwidth within 70% of link spec (200 Gbps nominal -> expect 15-20 GB/s).
    Significant shortfall points at NIC driver, MTU, or RoCE config to debug before any
    model work.
  - **Pod plumbing to confirm once:** container needs `/dev/infiniband` device mounts,
    `IPC_LOCK` capability, NCCL built with IB support, and for GPUDirect-RDMA the
    `nv_peer_mem` kernel module. Verify the current unbounded-agent CDI/device-plugin
    setup surfaces all of these on the Spark pods; fix the pod template where needed.
  - Document: link configuration (IP, MTU, NIC driver), measured bus bandwidth, and the
    pod-spec pattern that produced it (for re-use by W3.1/W3.2/W3.5).
- **W3.1 - vLLM TP=2 across both Sparks (phase item 4.1)**
  - Deploy via **KubeRay** (`kuberay-operator` + `RayCluster` CR) OR **LeaderWorkerSet**
    (v0.4+, which vLLM now has first-class support for). Pick KubeRay if we want the
    mature, widely-used path; LWS if we want the leaner, single-dependency path. This
    IS the integration work, not a hand-rolled Deployment+Service.
  - Target model, honestly sized: **Llama 3.1 70B FP8** (~70 GB) or **Qwen 3.5 122B-A10B
    at Q4** (~70 GB) - splits to ~35 GB/node, leaves ~85 GB/node for KV cache,
    activations, and vLLM overhead. **Llama 3.1 70B BF16 (~140 GB) is marginal** at
    ~70 GB/node with TP=2: technically fits in unified memory but leaves little KV room.
    Default to FP8 unless we prove BF16 works.
  - Demo: a single endpoint that's actually 2 nodes under the hood, `nvidia-smi` on both
    shows GPU activity, `kubectl cordon` or `pkill` on one worker shows graceful request
    failure and the Ray / LWS reconcile behavior
  - Deliverable: `deploy/vllm-tp2/`
- **W3.2 - Megatron-LM TP+PP across both Sparks (phase item 4.2)**
  - TP=2 across the two GPUs (intra-region all-reduce on ConnectX-7)
  - Small model for the demo (GPT 2-3B) but real distributed training
  - Tensorboard or wandb-ish output showing loss curve, both nodes active
  - Deliverable: `deploy/megatron-multinode/` + a training run screenshot
- **W3.3 - Failure/resilience demo (phase item 4.4)**
  - While vLLM TP=2 is serving, cordon one node; show the behavior (inference stops,
    k8s doesn't reschedule the worker until node returns - that's honest, don't pretend)
  - While Megatron is training, kill a worker pod; show checkpoint recovery
  - Deliverable: a short recorded demo + a doc explaining what k8s gives you for free and
    what still requires app-level work (honest about limits, good customer conversation)
- **W3.4 - ScalarLM single-node (phase item 3.3) - RISK SPIKE R3**
  - **Pre-wave check (do THIS WEEK, not at Wave 3):** check TensorWave GHCR / repo for an
    arm64 tag; file an issue if absent. See R3.prime in the Risk Register for owner/date.
  - If image available: deploy on spark-3d37, run closed-loop train+serve example
  - If not: decide - (a) build our own image (2-3 eng-weeks), (b) skip and let Megatron
    be the flagship, (c) upstream a PR to TensorWave
  - Deliverable: working demo OR a clean go/no-go call
- **W3.5 - ScalarLM multi-node (phase item 4.3)**
  - Only if W3.4 succeeded. Scale the same setup to both Sparks.
  - Deliverable: `deploy/scalarlm/`

**Definition of done:**
- Two-node distributed inference serves a model that wouldn't fit on one node
- Two-node distributed training produces a loss curve
- Failure demo is recorded and understood
- ScalarLM path has a clear status (working, blocked, or explicitly skipped)

**Dependencies:** Wave 2 done. R1 closed or on fallback; R3 answer known via R3.prime;
R4 closes inside the wave at W3.0.

---

### Wave 4: Stragglers

**Goal:** round out the demo catalog with items that aren't on the critical path.

**Preconditions:** any wave with capacity. The engineer picks these up when other items
are blocked or in cooldown.

**Scope:**

- **W4.1 - Continuous pre-training with checkpointing (phase item 3.4)**
  - Extend Wave 3's Megatron job: longer run, periodic checkpoint to PVC, resume-on-restart
    demonstrated by deleting the pod
  - Adds data to Storage Pain Journal: checkpoint disk growth over N hours, RWX throughput
    if checkpoints shared across workers
- **W4.2 - MoE vs Dense side-by-side (phase item 2.2)**
  - Grafana dashboard or a small web UI that queries Ollama-MoE, vLLM-MoE, Ollama-dense,
    SGLang-dense in parallel
  - Shows tokens/sec, memory used, quality on a fixed eval prompt
  - **Depends on** the dense-model decision from Wave 2 W2.5 - whichever model we actually
    shipped is what goes here; do not write against Qwen 3.6 27B as a given.

**Definition of done:** both items land in `deploy/` and have a doc page.

**Dependencies:** W4.1 needs a working training stack from Wave 2/3 - either Megatron
(W2.1/W3.2) if R1 closed, or the R1 fallback stack (HF Transformers + Accelerate +
DeepSpeed). W4.2 needs W2.5 complete.

---

### Wave 5: Geo-distributed (hardware-gated)

**Goal:** show unbounded-kube managing GPU workloads across 3 regions from one AKS
control plane.

**Preconditions:** Waves 1-3 complete in Region A. Regions B and C Sparks physically
installed and joined to AKS via unbounded-agent / WireGuard. R5 spike scheduled near
the end.

**Wave 0 for this wave (onboarding Regions B and C):**
- Repeat the W1.1 / W1.2 smoke tests on the B and C Sparks: same Ollama / vLLM manifests,
  different namespace/region. Proves the join worked.
- Storage Pain: per-region model download - we now pull the same weights to every region.

**Scope (in execution order):**

- **W5.1 - Multi-region smoke test**
  - Once B and C onboarding is complete, verify the same workload runs in all 3 regions
  - `kubectl get pods -A -o wide` shows the same workload live in all 3 regions
- **W5.2 - Cross-region observability rollup**
  - Extend Wave 1 Prometheus to scrape nodes in Regions B and C (federation or direct)
  - Single Grafana dashboard showing per-region GPU, throughput, egress, pod state
  - Note: the foundational observability stack already exists from W1.5; this is the
    "aggregate across regions" extension
- **W5.3 - Geo-routed inference (phase item 5.1)**
  - Same model deployed in all 3 regions behind Azure Front Door (or Traffic Manager)
  - Demo: curl from 3 client locations, show which region served each request
  - Measure: cross-region egress if a request accidentally routes wrong
- **W5.4 - Disaster recovery demo (phase item 5.5)**
  - Kill Region A (cordon all nodes); traffic fails over to B/C
  - Measure: time to failover, model ready state in surviving regions (big Storage Pain
    Journal entry if weights weren't pre-staged)
- **W5.5 - Regional model specialization (phase item 5.2)**
  - Each region runs a different fine-tuned variant (regional language tuning, for example)
  - Client chooses region by endpoint; shows multi-tenant specialization story
- **W5.6 - Follow-the-sun batch training (phase item 5.3)**
  - Training Job migrates between regions as timezone shifts utilization
  - Coarse-grained coordination only (WAN latency forbids TP across regions)
  - Storage Pain: cross-region checkpoint transfer cost is the headline number
- **W5.7 - Federated fine-tuning (phase item 5.4) - RISK SPIKE R5**
  - Flower or NVIDIA FLARE running on all 3 regions, aggregating weight updates
  - ARM64 support unverified - ambitious, but kept in scope because eventual GB200/GB300
    hardware makes this work non-throwaway
  - If frameworks don't work, narrative pivot: "federated-pattern-via-checkpoint-sync"
    using W5.6's infrastructure

**Definition of done:** all 7 sub-items have demos. Cross-region observability works. DR
is demonstrable. Storage Pain Journal has all cross-region rows populated with real numbers.

**Dependencies:** Waves 1-3 done. R5 resolved (or fallback accepted) before W5.7.


### Critical risks and de-risk first

Detailed treatment in the Risk Register section below. TL;DR:
1. **R1** Megatron-LM on ARM64+CUDA (IS the W2.1 spike; blocks W3.2, W3.4, W4.1)
2. **R2** vLLM multi-node via Ray on ARM64 (blocks W3.1, W3.5)
3. **R3** ScalarLM ARM64+CUDA image (blocks W3.4, W3.5)
4. **R4** ConnectX-7 NCCL/RDMA bandwidth (blocks all of Wave 3, distributed parts of W5)
5. **R5** Federated learning framework on ARM64+CUDA (blocks W5.7 only)
6. **R3.prime** TensorWave ScalarLM ARM64 image status check (pre-wave, due within 1 week of
   this plan's adoption - a failure here reshapes Wave 3)

---

## Risk Register

Each risk has: **trigger** (what kicks off the spike), **test** (the minimum validation),
**pass criteria** (what "green" looks like), **if blocked** (fallback), **owner**, **blocks**.

### R1 - Megatron-LM on ARM64+CUDA (GB10)

- **Trigger:** first day of Wave 2. Run in parallel with LoRA work.
- **Test:** pull `nvcr.io/nvidia/pytorch:<latest>-py3`, confirm ARM64 + sm_120 (GB10)
  support via `torch.cuda.get_device_capability()`. Clone Megatron-LM, run
  `pretrain_gpt.py` on a tiny (125M-350M) config with synthetic data for 10 steps.
- **Pass criteria:** loss decreases, no NaN, no "unsupported arch" warnings, throughput
  within an order of magnitude of published small-config numbers.
- **If blocked:** (a) build Megatron from source against a Spark-compatible PyTorch build;
  (b) substitute HuggingFace Transformers + Accelerate + DeepSpeed for the "real training"
  story (less impressive but real); (c) escalate via NVIDIA enterprise channel.
- **Owner:** engineer
- **Blocks:** W3.2 (Megatron multinode), W3.4 (ScalarLM), W4.1 (continuous pretraining).
  Note: R1 IS the W2.1 spike, so it does not "block" W2.1; it gates downstream waves.

### R2 - vLLM multi-node via Ray on ARM64

- **Trigger:** start of Wave 3 (right after Wave 2 wraps).
- **Test:** deploy vLLM with `--tensor-parallel-size 2` across spark-2c24 and spark-3d37,
  serving a small model (Qwen 3.6 7B) to rule out OOM as a confounder. Verify both
  `nvidia-smi`s show load; run a 1000-prompt throughput test.
- **Pass criteria:** serves correctly, throughput > single-node throughput for a model
  that fits on one node (sanity check), no Ray worker crashes over 10 min.
- **If blocked:** (a) try vLLM with native `--pipeline-parallel-size` path (no Ray);
  (b) fall back to SGLang multi-node (also uses Ray); (c) demote W3.1 to a "future work"
  and lean on Megatron for the multi-node story.
- **Owner:** engineer
- **Blocks:** W3.1, W3.5 (ScalarLM multi-node), W5.x distributed inference pattern

### R3 - ScalarLM ARM64+CUDA image

- **Trigger:** start of Wave 3, *only if R3.prime returned "image available"*. If
  R3.prime answered "unavailable" or "no response," R3 does not fire; W3.4/W3.5 fall back
  to Megatron-standalone per R3.prime's "if blocked" path.
- **Test:** check TensorWave GHCR / docs for arm64 tag. If present, pull and run their
  `vllm + megatron + HF hub` smoke test on spark-3d37.
- **Pass criteria:** image pulls, starts, their example train+serve loop completes.
- **If blocked:** (a) build our own image from ScalarLM sources against an arm64 PyTorch
  (2-3 eng-weeks); (b) file an upstream issue/PR; (c) skip ScalarLM entirely - Megatron
  becomes the flagship training demo. Option (c) is the recommended fallback if we are
  schedule-pressured.
- **Owner:** engineer
- **Blocks:** W3.4, W3.5

### R3.prime - Pre-wave check of ScalarLM ARM64 status (schedule risk)

- **Trigger:** within 1 week of this plan being adopted. NOT at start of Wave 3 - that's
  too late to reshape plans.
- **Test:** (a) check TensorWave's GHCR and docs for an arm64 image tag; (b) file a GitHub
  issue on the ScalarLM repo asking for status if not obvious; (c) email TensorWave if we
  have a contact.
- **Pass criteria:** a written answer in the plan's notes - "image exists at tag X" OR
  "confirmed not yet, ETA Y" OR "no response in 5 days, assume unavailable".
- **If blocked (no answer in time):** assume unavailable, downshift W3.4/W3.5 to
  Megatron-standalone, note in the plan.
- **Owner:** engineer
- **Blocks:** accurate Wave 3 scoping; not a technical dependency

### R4 - ConnectX-7 NCCL/RDMA bandwidth between Sparks

- **Trigger:** start of Wave 3 (W3.0).
- **Test:** `nccl-tests all_reduce_perf` across the two Sparks over the ConnectX-7 link.
  Also run `ib_send_bw` / `ib_write_bw` if RDMA is on. Document MTU, NIC driver, IP config.
- **Pass criteria:** bus bandwidth >= 15 GB/s (75% of 200 Gbps nominal) for large messages.
  RDMA path confirmed if available.
- **If blocked:** (a) debug NIC driver / RoCE config (likely what's needed); (b) accept
  Ethernet TCP speeds and re-scope expectations (distributed training will be slow but
  functional). Must NOT proceed to W3.1 or W3.2 before this is resolved because failures
  there will be hard to attribute.
- **Owner:** engineer
- **Blocks:** W3.1, W3.2, W3.3, all Wave 5 distributed items

### R5 - Federated learning framework on ARM64+CUDA

- **Trigger:** start of Wave 5 item W5.7.
- **Test:** deploy Flower server + 3 clients (one per region) running a simple CIFAR-style
  FL round. Or NVIDIA FLARE equivalent.
- **Pass criteria:** one full federated round completes with weight aggregation.
- **If blocked:** narrative pivot - demo "federated-pattern-via-checkpoint-sync" using
  W5.6's cross-region checkpoint transfer infrastructure. Explicitly scope W5.7 as
  "investigate, report findings, ship if feasible" rather than a must-ship item.
- **Owner:** engineer
- **Blocks:** W5.7 only

### Risk review cadence

- End of each wave: review the register, mark closed risks, add any new risks surfaced.
- If a risk blocks for more than 1 week past its trigger, escalate to the "if blocked"
  path - do not let open risks stall the wave.

---

## Sponsor Update Cadence

Updates go to sponsors, not external customers. Single channel: monthly written. The goal
is "yes, fund the next quarter" - not selling to a prospect. No live walkthroughs are
planned; if a sponsor asks for one, we can assemble it ad hoc from the existing artifacts.

### Monthly written update (~1 page)

Format, every month, in `docs/sponsor-updates/YYYY-MM.md`:

1. **Headline (1 sentence):** the most important thing that shipped this month.
2. **Spine artifact status:** state of the edge-Spark pool. Nodes online per region,
   what's serving from each, recent failures.
3. **What shipped:** wave items completed with links to manifests + any recorded demos.
4. **What's blocked:** open risks from the Risk Register; what we're doing about them.
5. **Storage Pain Journal deltas:** new measured rows this month. Numbers, not narrative.
6. **Learning progress:** which L1-L6 objectives the engineer hit this month.
7. **GB200/GB300 transfer notes:** anything we learned that affects portability.
8. **Asks of the sponsor:** explicit (more hardware, ARM64-image escalation contact, etc.).

Brutal brevity. One page. If it's longer, it's not a status update, it's a wishlist.

### Artifact maintenance (supports the monthly update)

- Keep one short-recorded demo current for the spine artifact (re-record after significant
  changes). This is the go-to asset if a sponsor asks "can we see it?"
- **Rotate the demo endpoint's API key** before any external sharing - the current key
  `498199654e...` has been circulated in planning docs and must not be used.
- Keep the architecture diagram current as Microsoft-first services are added.

---

## Sponsor Checkpoints

Replaces the previous "Minimum Viable Showcase" framing. Same idea: explicit stop points
so we have something coherent to show at end of each wave, regardless of what slips later.

**SC-1 (after Wave 1):** Foundation + Spine.
- What shipped: Phase 0, manifests for Ollama and vLLM, edge Sparks joined to AKS via
  unbounded-agent, observability stack (Prometheus/Grafana/DCGM).
- Sponsor narrative: "The spine works. One engineer can stand up AI inference on edge
  ARM64 GPU nodes joined to an AKS control plane, with live observability."
- Decisions needed from sponsor: confirm Wave 2 scope.

**SC-2 (after Wave 2):** Breadth of AI workloads on the spine.
- What shipped: third inference engine, multi-modal, RAG, LoRA fine-tuning, eval
  pipeline, dense chat model - all running on Region A Sparks.
- Sponsor narrative: "The engineer can deploy and operate the AI workloads customers use
  in production. The spine carries real workloads, not just hello-world."
- Decisions needed: confirm we proceed to multi-node (Wave 3) vs add more breadth.

**SC-3 (after Wave 3):** Scale-out.
- What shipped: vLLM multi-node TP=2, Megatron multi-node training, honest resilience
  demo, ScalarLM if R3 closed.
- Sponsor narrative: "Models that don't fit on one node are served. Training spans
  nodes. Failure modes are documented honestly."
- Decisions needed: confirm Wave 5 scope.

**SC-4 (after Wave 5, hardware-gated on Regions B and C):** Geo + the climax architecture diagram.
- What shipped: Regions B and C onboarded, geo-routed inference, DR demo, cross-region
  observability, follow-the-sun training, federated fine-tuning if R5 closed.
- Sponsor narrative: "One AKS control plane, three regions of ARM64 edge GPU nodes,
  AI workloads serving and training across all of it - delivered by one engineer."
- Decisions needed: GB200/GB300 hardware procurement timeline; transition plan from
  Spark lab to GB200 production reference.

**Explicit rule:** each SC corresponds to a monthly written update with extra detail on
the wave just closed. Live walkthroughs are not promised; if a sponsor asks, assemble
ad hoc from the existing artifacts.

---


## Security and Multi-Tenancy (Internal Posture)

Internal lab. No external customers. Posture goals: keep the demo endpoint from being
abused; keep the lab from being a security embarrassment for sponsors.

**Current state (risk):**
- Single demo endpoint, Bearer token auth, key in planning docs.
- No rate limiting, no WAF, no abuse protection beyond the auth proxy.
- All namespaces on the cluster can talk to each other by default.
- Single tenant (us).

**Minimum posture before any sponsor walkthrough:**
- Rotate API key; store only in `Secret` objects, never in docs or chat.
- Move from Bearer token to Azure AD / Workload Identity for new endpoints (existing
  endpoint can keep Bearer until it's replaced).
- Rate limit at the ingress (nginx annotation or equivalent); at minimum 10 req/s per IP.
- NetworkPolicy denying cross-namespace traffic by default; allow-list only what wave
  items need (RAG retriever can talk to vector DB, etc.).
- Document that the demo endpoint is internal-lab-only, not production.

**GPU sharing reality on this hardware:**
- Each DGX Spark has 1 GB10 GPU. No MIG.
- At any moment, each GPU is running ONE workload at a time.
- With ~8 model deployments from Wave 2 and only 2 GPUs in Region A, most deployments are
  "deployable, not always running." The engineer schedules which are warm at any time.
- Sponsor walkthrough requirement: when we click into a model, allow warm-up time off-camera
  (model swap on Ollama is seconds; vLLM pod swap is much longer). Pre-warm, don't hot-swap.
- Multi-tenant GPU sharing on GB10 is NOT a story. On GB200/GB300 with MIG it becomes one;
  flagged as future work, not a Spark-lab capability.

---

## Licensing and Provenance (Internal Audit)

Even for an internal lab, we respect model licenses. Sponsors and audit will care.

| Model | License | Internal lab use OK? | Notes |
|---|---|---|---|
| Qwen 3.5/3.6 family | Apache 2.0 (most) | Yes | Check specific SKU; some Qwen releases had custom terms |
| Llama 3.x | Llama Community License | Yes | <700M MAU; lab use is fine |
| Gemma | Gemma Terms of Use | Yes with responsible-use clauses | Avoid prohibited-use scenarios in demos |
| Mistral Large | Mistral MNPL / Commercial | Avoid - commercial use requires paid license | Use Apache-2.0 alternatives |
| Whisper | MIT | Yes | |
| bge-m3 / nomic-embed | Apache 2.0 / MIT | Yes | |

**Rule:** every `deploy/<model>/` folder includes a `LICENSE.md` citing the model card
and the license text/link. If a model's license is unclear, default to not deploying it.
Prefer Apache-2.0 / MIT models when there's a real choice.

---

## Benchmark Methodology

One-off curl numbers (e.g., "54.6 t/s on one prompt") are not credible performance data.
We need a repeatable harness.

**Benchmark script requirements:**
- Lives in `hack/bench/` or similar; source-controlled
- Parameters: engine (ollama/vllm/sglang), endpoint URL, auth, model, prompt length,
  generation length, concurrency, number of runs
- Reports: p50/p95/p99 latency, tokens/sec, prompt eval rate, memory footprint at peak
- Runs warm-up requests before measurement
- Dumps JSON results for ingestion into a Grafana dashboard or doc

**Methodology:**
- Minimum 20 runs per config; drop first 3 as warm-up
- Report ALL of: prompt length, generation length, concurrency, quant, engine version,
  driver version, date - numbers without these are useless
- Re-run baselines monthly; staleness kills credibility

**Where the existing "54.6 t/s" number goes:**
- Recorded with context in `docs/dgx-spark-inference-perf.md` with a date stamp
- Not quoted as "current performance" - quote the latest harness run instead
- The April 2026 measurement becomes a historical data point for regression checks

---

## Rollback and Environment Strategy

Today's situation: one cluster, one set of edge nodes, live endpoints that users may be
pointing at. Wave 2 deploys new workloads that could destabilize existing ones.

**Rules going forward:**
- Existing `qwen35-35b` and `qwen36-35b` namespaces are **protected**. Changes only via
  manifests under `deploy/`, and only after a dry-run on a scratch namespace.
- New engines/models from Wave 2+ deploy into fresh namespaces; existing endpoints stay
  untouched.
- If we need a breaking migration (e.g., switch Ollama to a new engine parameter), do it
  in a new namespace, cut traffic over, then tear down the old.
- No `kubectl edit` on production namespaces; always through manifest PRs.

**A "staging" cluster would help but we don't have one.** Work within one cluster using
namespaces + RBAC + NetworkPolicy as soft isolation. When Regions B and C come online for
Wave 5, consider dedicating one region (or one Spark within a region) to staging.

---


## Verification

Per-phase verification is captured in the "Storage pain observed" block (pain metrics) plus
a handful of functional checks:

- **Phase 0/1**: Each deployed engine responds to curl requests with correct timing stats.
  Engine manifests live under `deploy/`.
- **Phase 2**: Each model class returns plausible output for its workload type (dense chat
  completes; vision identifies an image; RAG retrieves and cites).
- **Phase 3**: Fine-tuned model scores higher than base on the same eval harness run.
  Checkpoints resumable across pod restart.
- **Phase 4**: Both Sparks show GPU utilization during the workload. Drain one node and show
  workload behavior matches documented expectation.
- **Phase 5**: Per-region dashboards visible in Grafana. Region kill triggers traffic failover
  within expected SLO. Cross-region egress matches projections.
- **Storage Pain Journal**: table has measured values for every metric, not hypotheticals.
