---
title: "Load vLLM Models from Racer"
weight: 11
description: "Serve a vLLM model from a Racer ClusterCache so model weights stream from node-local cache and peer nodes instead of being pulled from object storage by every replica."
---

A vLLM server spends a large part of its startup reading model weights. The GPU
is idle while the process fetches tens of gigabytes of safetensors, validates
the checkpoint, and copies tensors into device memory. When several replicas
start at once, each one repeats that work and the storage origin sees the same
bytes requested again for every replica. Racer removes that duplication by
turning the model files into cache objects that are read through a Unix socket
on each node.

This guide shows you how to put a Racer `ClusterCache` in front of a model
container, deploy vLLM with the cache socket mounted, fetch the small
configuration files that vLLM needs before it loads weights, and stream the
weights with the `racer` load format. It also covers the GPU path, tensor
parallelism, multi-node placement, tuning, and the failures you are most likely
to hit.

The mental model is simple. The origin remains the source of truth. Racer keeps
immutable 16 MiB pages of the model on node-local disks and in memory, serves a
local read without touching the network, and sends a miss to the nodes that own
the page. vLLM still builds the same model and allocates the same GPU memory. It
just stops asking object storage for the same bytes on every replica.

## Why Model Loading Dominates vLLM Startup

### Startup Is a Read Problem

A model server has two distinct startup phases. The first is initialization:
vLLM reads `config.json`, the tokenizer files, and the generation settings,
builds the model graph, and allocates GPU memory. The second is weight loading:
it reads every safetensors file, validates each tensor header, and copies each
tensor into the parameter that the model expects. The second phase moves the
most bytes by a wide margin.

An 8B parameter model stored in BF16 is roughly 16 GB of tensor data. A larger
model or a higher precision checkpoint can be several times that. Reading those
bytes from a remote object store is limited by the storage account, the network
path, and the rate at which one client can issue range requests. Reading them
from local NVMe is limited by the device. Reading them from memory is limited by
the copy into the GPU. The difference between those paths is the difference
between a rollout that takes minutes and one that takes tens of seconds.

The problem gets worse as the replica count grows. A deployment with one replica
pulls one copy. A deployment with twenty replicas pulls twenty copies of the
same checkpoint. Even if the origin can serve the bandwidth, the fan-out is
wasteful, and a rolling update repeats it. The model has not changed, but every
new pod treats it as new data.

### Where the Time Goes

Weight loading is not one read. A typical checkpoint is split into several
safetensors shards named by `model.safetensors.index.json`. The loader must read
a small header from each shard, parse the tensor table, then read the tensor data
in whatever order the model's loader requests it. A naive implementation issues
many small reads, and each read has its own latency. A good implementation reads
ahead in large ranges and keeps the GPU busy while the next range arrives.

There is also a correctness cost. The configuration files and the weights must
come from the same model version. If a deployment fetches `config.json` from one
upload and weights from another, the model may fail to build or, worse, load a
checkpoint whose tensor names and shapes no longer match. A loader that does not
pin versions can produce a confusing error far from the real cause.

### What Racer Changes

Racer changes the source of the bytes, not the shape of the model. The first
reader of a page on a node asks a page owner for it. The owner fetches the page
from the origin, encrypts it with the cache key, stores it, and returns it. Later
reads of the same page on that node come from memory or local disk. Reads on
other nodes ask the owners instead of the origin. The origin sees one fetch per
page under normal conditions, not one fetch per replica.

The cache is especially effective for model loading because the access pattern
is read-only and highly repetitive. Every replica reads the same files in
roughly the same order, and every restart reads them again. There is no write
path to invalidate, no shared file lock to coordinate, and no need for a
PersistentVolume. The model files are immutable objects, which is exactly the
contract Racer expects.

## How Racer Sits in Front of Model Storage

### The Data Path

Racer runs as a DaemonSet named `racer` in the system namespace, which defaults
to `unbounded-system`. Every dataplane pod can store pages, serve pages to local
clients, and act as an owner for a subset of pages. A `ClusterCache` names the
cache and, optionally, its origin. The cache name becomes part of the socket
path that applications connect to: `/run/racer/<cache>/client/socket`.

An application never mounts the cache as a filesystem. It opens the Unix socket
and speaks the Racer client protocol. The Python `racer-client` package wraps
that protocol, and `racer-vllm` uses it to read model files. The admission
webhook can inject the socket mount into a pod, which is how the vLLM
deployment in this guide gets access without a manual `hostPath` volume.

When a read misses locally, the dataplane ranks the page owners and asks them in
order. An owner that has the page returns it. An owner that does not have it
fetches the page-aligned range from the origin, validates the response against
the object metadata and ETag, encrypts the page once, and publishes it. The
reader verifies and decrypts the page before handing bytes to the application.
A read is pinned to one object version, so pages from two uploads cannot be
mixed in one response.

### Cache Identity and Object Layout

A `ClusterCache` is cluster-scoped and has no status field. Its Kubernetes UID
is its wire identity. Deleting and recreating a cache with the same name creates
a new cache with an empty key space, even if it points at the same container.
That matters when you are debugging a cold cache: a recreated cache is not the
cache that was warm a minute ago.

A model location in the vLLM plugin has three parts:

```text
racer://<cache>/<bucket>/<prefix>
```

The cache segment is the `ClusterCache` name. The bucket and prefix select the
object. With the built-in Azure Blob origin, the bucket segment must equal the
container name in `spec.origin.azure.containerURL`, and the prefix is the blob
path inside that container. For example, a cache named `models` that points at
the `llama` container uses `racer://models/llama/Llama-3.1-8B-Instruct`, and the
file `config.json` for that model is the blob
`Llama-3.1-8B-Instruct/config.json`.

The plugin builds the object key from the bucket and the prefix-relative file
name. It does not interpret the blob path itself. That means the layout in the
container must match the layout the checkpoint expects: the index file must name
its shards, and those shard names must resolve under the prefix.

### What the Plugin Adds

`racer-vllm` is a vLLM plugin. It registers a model loader under the load format
name `racer` through the `vllm.general_plugins` entry point. It also installs a
small command, `racer-vllm-fetch`, which copies the configuration and tokenizer
files that vLLM reads before weight loading and writes a marker file named
`racer-model.json`.

The plugin does not replace vLLM's model construction, tokenizer, scheduler, or
OpenAI server. It only supplies the weights. That is why the deployment looks
like a normal vLLM deployment apart from the load format, the init container,
and the cache socket mount.

## Before You Start

### Cluster and Client Requirements

You need a working Unbounded installation. The operator is installed with the
`kubectl unbounded` plugin, and Racer itself is installed on demand when the
first `ClusterCache` exists:

```bash
kubectl unbounded install --timeout=5m
```

The command waits for the operator rollout and for the required CRDs to become
established. Racer is not part of that install step. Creating the cache is what
causes the operator to create the `racer` DaemonSet.

You also need permission to create cluster-scoped `clustercaches` in the
`racer.unbounded-cloud.io` API group. Because the resource is cluster-scoped,
the grant must come from a `ClusterRole` bound with a `ClusterRoleBinding`, not
from a namespaced `Role`. A kubeconfig that can create namespaced objects is not
necessarily able to create a cache.

The `racer-vllm` image is built for `linux/amd64`. If your GPU nodes are ARM64,
use the Python packages on an upstream vLLM image for that architecture instead
of the published `racer-vllm` image.

### GPU Node Requirements

The vLLM pod needs a GPU that Kubernetes can allocate, so the node must
advertise `nvidia.com/gpu` through a device plugin. Unbounded prepares the node
stack so that the host driver, device nodes, and driver libraries reach a
container, but it does not install the device plugin for you. The
[NVIDIA GPU reference]({{< relref "reference/gpu/nvidia" >}}) covers the host
driver, CDI, isolation mode, and preflight checks.

The Racer dataplane has a separate GPU path. It does not request
`nvidia.com/gpu` from Kubernetes and does not consume a device plugin
allocation. It uses the host's NVIDIA devices through the container runtime,
controlled by `RACER_NVIDIA`, so the dataplane can serve `gpu-read` for any pod
on the node. That is why the dataplane pod can use the GPU even though the vLLM
pod is the one holding the Kubernetes GPU allocation.

A node that should store or serve cache data must also run Linux, allow a
privileged pod that mounts host devices, and have local disk for page storage.
The dataplane uses `/var/lib/racer/slabs` by default. Nodes labeled
`racer.unbounded-cloud.io/exclude` are removed from dataplane placement and from
cache membership, so do not put that label on a GPU node you intend to use.

### Model and Origin Requirements

You need an Azure Blob Storage container that holds the model under a prefix.
The container name must be 3 to 63 characters of lowercase letters, digits, and
single hyphens. The account must be reachable from every Racer node, including
any firewall or private endpoint rules.

For a private container, the Racer dataplane needs an identity that can read
blobs. `Storage Blob Data Reader` scoped to the container is enough because the
cache never writes to the origin. The application pod does not need the
credential and does not see it. The dataplane obtains it from the cache
configuration on each node.

The [Azure Blob Storage cache guide]({{< relref "guides/azure-blob-cache" >}})
walks through the cache, origin, and credential setup in more detail if you do
not already have a cache for model files.

### Namespace Requirements

The cache socket is mounted from a `hostPath` volume. Pod Security Admission
`baseline` and `restricted` both forbid `hostPath`, so put the vLLM workload in
a namespace that allows it. A dedicated namespace labeled
`pod-security.kubernetes.io/enforce: privileged` is the simplest choice.

Do not put the workload in `unbounded-system`. The admission configuration
excludes that namespace so the dataplane can always start, and mixing a model
server into the system namespace makes the failure modes harder to reason about.

## Create the ClusterCache

### A Cache for a Model Container

The cache is the object that ties a container to a pool of nodes. The following
manifest creates a cache named `models` for the `llama` container, gives it a
high priority so model pages survive pressure from other caches, and restricts
it to nodes in the `gpu-a` site:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
spec:
  priority: 900
  origin:
    azure:
      containerURL: https://<account>.blob.core.windows.net/llama
      credential:
        workloadIdentity:
          clientID: <client-id>
  nodeSelector:
    matchLabels:
      unbounded-cloud.io/site: gpu-a
```

Replace `<account>` with the storage account name and `<client-id>` with the
client ID of the federated identity. Apply the manifest and inspect the result:

```bash
kubectl apply -f models-cache.yaml
kubectl get ccache models -o yaml
```

`ccache` is the short name of `clustercaches`. The object has no status field,
so a successful `get` only proves that the API server stored it. The operator
and the dataplane do the rest.

The cache name is part of the socket paths `/run/racer/<name>/client/socket` and
`/run/racer/<name>/origin/socket`, so it is limited to 82 characters overall and
63 characters per DNS label. Choose a short lowercase name. The same name is the
first segment of every `racer://` URI for the cache.

The `nodeSelector` is optional, but it is useful for a GPU deployment. It limits
which nodes store and serve the cache, and the admission webhook restricts client
pods that use the cache to matching nodes. That keeps a model load from landing
on a CPU-only node that happens to have a Racer dataplane.

### Credentials

The `credential` block is optional for a public container. For a private one,
choose exactly one of the two supported methods. `default` uses the node's
managed identity through the Azure Instance Metadata Service. It supports an
optional `clientID` to select a user-assigned identity. `workloadIdentity` uses
Microsoft Entra Workload ID and requires a `clientID`; `tenantID` is optional and
is discovered from the storage account's bearer challenge when omitted.

The workload identity method needs a federated credential whose subject is the
Racer dataplane ServiceAccount, `system:serviceaccount:unbounded-system:racer`,
and whose audience is `api://AzureADTokenExchange`. The dataplane projects that
token into every pod, so enabling workload identity for a cache does not roll the
DaemonSet. When a credential is configured, the container URL must use `https`.

The credential is not part of the cache identity. You can change it in place.
The container URL is immutable, and so is the presence or absence of
`spec.origin`. If you need a different container, create a new `ClusterCache`.
If you only need a different model version, upload it under a new prefix in the
same container and leave the cache alone.

### Wait for the Dataplane

Creating the cache causes the operator to install the Racer dataplane. Wait for
the rollout before you deploy vLLM:

```bash
kubectl -n unbounded-system rollout status daemonset/racer --timeout=10m
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=racer -o wide
```

If the cluster has at least one `Site`, the operator also publishes a
`RacerReady` condition on each site. Without a `Site`, the DaemonSet and pod
state are the authoritative checks because there is nowhere to publish the
condition.

The control seats are elected through Kubernetes Leases. Check that a primary
exists before you depend on the admission webhook:

```bash
kubectl -n unbounded-system get lease racer-primary \
  -o jsonpath='{.spec.holderIdentity}{"\n"}'
```

The holder identity is a pod name. If the lease has no holder, no seat can serve
the webhook, and an opted-in vLLM pod will be denied rather than admitted
without its socket mount.

### Why the Cache Is Not a Volume

A `ClusterCache` is not a `PersistentVolume` or a `PersistentVolumeClaim`. It
does not appear in the pod spec and cannot be mounted as a filesystem. The only
thing a pod mounts is the client socket directory, and that mount is a control
channel, not a data volume. Model bytes travel over the socket.

This distinction matters when you plan storage. You do not size a PVC for the
model. You size the node cache through Racer's page storage and the cache's
priority relative to other caches. The model itself stays in the origin.

## Check GPU Mode on the Dataplane

### What auto Does

The published `racer` image is built with CUDA support by default. With
`RACER_ENABLE_GPU=auto`, which is the default, the dataplane starts without
touching a GPU. It logs a standby line and sets up the GPU helper on the first
`gpu-read` request:

```text
racer: stage=gpu state=standby start=first-gpu-read
```

When the first GPU read arrives, the helper enumerates devices and either turns
the path on or disables it. A successful start logs a line with the usable device
count, the number of devices that can decrypt, the staging and scratch slot
counts, and whether PCI peer-to-peer DMA is enabled:

```text
racer: stage=gpu state=enabled usable=<n> decrypt=<n> staging_slots=<n> scratch_slots=<n> streams=<n> queue_depth=<n> pci_p2pdma=<bool>
```

A failure logs a reason and leaves GPU reads off:

```text
racer: stage=gpu state=disabled reason=no-usable-gpu
```

The exact reason is the useful part. It can report that the binary was built
without CUDA, that no device was found, or that the driver rejected the device.
The dataplane continues to serve `host-read` and `stream` reads after this line,
so a model load can still succeed while the GPU fast path is unavailable.

### Reading the GPU Log Lines

Find a dataplane pod on the node where vLLM runs and read its log:

```bash
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=racer -o wide
kubectl -n unbounded-system logs <racer-pod> -c dataplane --tail=300 | grep 'stage=gpu'
```

Replace `<racer-pod>` with a pod name from the previous command. Do this after
the first model load, not only after the pod starts. With `auto`,
the interesting line does not exist until a client asks for `gpu-read`. A log
that shows only `state=standby` means the dataplane has not been asked for a GPU
read yet, not that GPU mode is broken.

### Forcing GPU Mode

Set `RACER_ENABLE_GPU` to `true` when you want fail-fast behavior. The dataplane
sets up the GPU helper at startup and refuses to start if no usable GPU is found.
That is appropriate for a dedicated GPU pool where a silent fallback would hide a
driver problem. It is a poor choice for a mixed pool with CPU-only nodes, because
every dataplane pod in the pool reads the same setting.

Change the setting in the `racer-settings` ConfigMap in the system namespace and
wait for the operator to roll the DaemonSet:

```bash
kubectl -n unbounded-system edit configmap racer-settings
kubectl -n unbounded-system rollout status daemonset/racer --timeout=10m
```

The operator preserves administrator data in that ConfigMap and hashes edits for
a managed rollout. A key the dataplane does not recognize fails the plan and
startup, so copy the spelling exactly. `false` turns GPU reads off entirely;
`gpu-read` then answers `501` and clients fall back.

### Driver Libraries and cuFile

`RACER_NVIDIA` controls how the dataplane sees the host's NVIDIA driver. The
default is `env`, which sets `NVIDIA_VISIBLE_DEVICES=all` and
`NVIDIA_DRIVER_CAPABILITIES=compute,utility` for the dataplane container. `cdi`
adds the pod annotation `cdi.k8s.io/racer: nvidia.com/gpu=all`, and containerd
then fails the pod on a node without an NVIDIA CDI spec. `off` adds no NVIDIA
environment, CDI annotation, or cuFile volume, and is the correct setting for a
pool that should never serve GPU reads. The operator reads `RACER_NVIDIA` from
the `racer-settings` ConfigMap when it renders the DaemonSet, so change it there
and wait for the rollout.

cuFile, which is the userspace API behind GPUDirect Storage, can read local NVMe
into GPU memory without a host bounce buffer. It needs either the `nvidia-fs`
kernel module or PCI peer-to-peer DMA. On a stock kernel without `nvidia-fs`,
set `RACER_GPU_PCI_P2PDMA=true` in the same ConfigMap to let the GPU shim use the
PCI path. If the driver rejects the setting or the device pair does not support
it, cuFile stays off until the dataplane restarts, and the dataplane uses host
staging. GPU reads still work; they are just slower.

## Prepare the Model Layout

### Files the Loader Expects

A standard transformer checkpoint needs a small set of files next to the weights.
`config.json` is mandatory. The fetch command fails if it is missing. The other
files are optional in the sense that a missing one is skipped, but vLLM may need
them depending on the model:

| File | Role |
| --- | --- |
| `config.json` | Model architecture and dtype. Required. |
| `generation_config.json` | Default generation settings. |
| `tokenizer.json` | Fast tokenizer data. |
| `tokenizer_config.json` | Tokenizer class and special-token settings. |
| `special_tokens_map.json` | Special token aliases. |
| `tokenizer.model` | SentencePiece model for some tokenizers. |
| `vocab.json`, `vocab.txt`, `merges.txt` | BPE vocabulary files. |
| `added_tokens.json` | Extra token definitions. |
| `chat_template.jinja`, `chat_template.json` | Chat template. |
| `preprocessor_config.json`, `processor_config.json` | Multimodal preprocessing. |
| `params.json`, `tekken.json` | Model-specific metadata. |
| `model.safetensors.index.json` | Shard map for a split checkpoint. |

`racer-vllm-fetch` also copies small files named in the index metadata when the
name ends in `.json`, `.txt`, `.model`, `.jinja`, `.py`, or `.tiktoken`. That is
how a chat template recorded in the index travels with the model. Files outside
that set are not copied, so a checkpoint that depends on an unusual sidecar file
needs that file supplied by another mechanism, such as a custom image layer.

### Weight File Layouts

The loader supports three layouts:

1. A single `model.safetensors` file.
2. A sharded checkpoint with `model.safetensors.index.json` whose `weight_map`
   names each shard.
3. A vLLM `sharded_state` checkpoint, where each tensor-parallel rank has its
   own part files.

For the first two layouts, the index is the authority. The loader reads the
index, sorts and de-duplicates the shard names, and rejects any name that starts
with `/` or contains a `..` path segment. If the index has an empty `weight_map`,
the load fails. If there is no index, the loader requires `model.safetensors`.

For a `sharded_state` checkpoint, the file names follow vLLM's patterns. The
tensor-parallel-only pattern is
`model-rank-{rank}-part-{part}.safetensors`. The combined pipeline and tensor
parallel pattern is
`model-pp-{pp_rank}-tp-{tp_rank}-part-{part}.safetensors`. The loader probes
`part-0`, `part-1`, and so on until a part is missing, so the parts for one rank
must be contiguous.

### Prefix and Bucket Rules

Upload the model under one prefix and keep the whole checkpoint under it. The
prefix may contain slashes. Use a clean path that does not start with a slash and
does not contain a `..` segment. The plugin strips leading and trailing slashes
from the prefix before it builds object names, but relying on that normalization
is unnecessary.

With the Azure origin, remember that the bucket segment is not a free-form
label. It must equal the container name. A model at
`llama/Llama-3.1-8B-Instruct/config.json` in the storage account is addressed as
`racer://models/llama/Llama-3.1-8B-Instruct` when the cache is named `models`
and points at the `llama` container. If you use a different cache name, only the
first segment changes.

### Versioning a Model

Racer expects object versions to be immutable. A new upload of the same blob
path gets a new ETag, which is fine, but a deployment that fetched the old
configuration and then reads the new weights will fail with
`ModelChangedError`. The failure is deliberate: it prevents a mixed-version
model from loading silently.

The clean pattern is to give each model revision its own prefix, for example
`Llama-3.1-8B-Instruct/2026-11-01`, and change the `racer://` URI in the
deployment when you promote a new revision. The cache can stay the same because
the container has not changed. The new prefix simply starts cold and fills as
pods read it.

## Deploy vLLM with the Cache Mounted

### The Pod Contract

A vLLM pod that uses Racer needs three things:

- The label `racer.unbounded-cloud.io/client-socket: enabled` on the pod
  template. The label is what makes the webhook consider the pod.
- The annotation `racer.unbounded-cloud.io/client-cache: models` on the pod
  template. The value must name an existing `ClusterCache`.
- A shared `emptyDir` volume that the fetch init container writes and the vLLM
  container reads, mounted at the model directory.

The webhook mounts `/run/racer/<cache>/client` read-only into each selected
container and sets `RACER_SOCKET` to `/run/racer/<cache>/client/socket`. A
read-only mount is enough because the client only connects to the socket; it
does not write to the directory.

The label and annotations belong under `spec.template.metadata`, not on the
Deployment metadata. Admission sees pods, not controllers. A label on the
Deployment has no effect, and a pod template change is what causes a rollout
that recreates the pods with the mount.

### A Complete Deployment

The following manifest runs one vLLM replica with a single GPU. Replace `<tag>`
with the release or nightly image tag and replace the model URI with your
values:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llama-31-8b
  namespace: vllm
spec:
  replicas: 1
  selector:
    matchLabels:
      app: llama-31-8b
  template:
    metadata:
      labels:
        app: llama-31-8b
        racer.unbounded-cloud.io/client-socket: enabled
      annotations:
        racer.unbounded-cloud.io/client-cache: models
    spec:
      initContainers:
        - name: racer-vllm-fetch
          image: ghcr.io/azure/racer-vllm:<tag>
          command:
            - racer-vllm-fetch
            - racer://models/llama/Llama-3.1-8B-Instruct
            - /models
          volumeMounts:
            - name: model
              mountPath: /models
      containers:
        - name: vllm
          image: ghcr.io/azure/racer-vllm:<tag>
          command:
            - vllm
            - serve
            - /models
            - --load-format
            - racer
            - --model-loader-extra-config
            - '{"staging_bytes": 268435456, "concurrency": 4}'
            - --port
            - "8000"
          resources:
            limits:
              nvidia.com/gpu: 1
          volumeMounts:
            - name: model
              mountPath: /models
          ports:
            - name: http
              containerPort: 8000
      volumes:
        - name: model
          emptyDir: {}
```

Save the manifest as `vllm.yaml`, create a namespace that permits the injected
`hostPath` mount, and apply the Deployment:

```bash
kubectl create namespace vllm
kubectl label namespace vllm pod-security.kubernetes.io/enforce=privileged
kubectl apply -f vllm.yaml
```

Skip the `create` if the namespace already exists. The label is only needed when
Pod Security Admission is enforced; without it, a `baseline` or `restricted`
namespace rejects the pod before the webhook can add the socket mount.

The `emptyDir` holds only the small files that `racer-vllm-fetch` writes. The
weights are not copied into it. vLLM reads `config.json` and the tokenizer files
from the volume, the plugin reads `racer-model.json` to find and pin the weights,
and the tensor data streams from Racer over the socket.

The resource limit is what makes the pod schedulable on a GPU node. Without
`nvidia.com/gpu`, the pod can land on a node with no GPU and vLLM fails when it
tries to initialize CUDA. The Racer dataplane does not consume this resource;
only the model server does.

### What the Webhook Adds

When the pod is admitted, the Racer webhook looks up the named cache and applies
a JSON patch. It adds a volume named `racer-client-socket` backed by the
`hostPath` `/run/racer/models/client`, mounts that volume read-only at the same
path in the selected containers, and sets `RACER_SOCKET` to the socket path. It
also records the injection in the
`racer.unbounded-cloud.io/client-socket-injected` annotation.

The webhook pins the pod to nodes that can serve the cache. It adds
`kubernetes.io/os=linux` to the node selector, adds the cache's
`spec.nodeSelector` to the pod's required node affinity, and requires that the
node does not carry `racer.unbounded-cloud.io/exclude`. If the cache has no
node selector, the pod can run on any node in the cache pool that otherwise
matches.

Admission fails closed. If no control seat can serve the webhook, a pod that
opts in is denied rather than admitted without its cache mount. During a Racer
outage, new opted-in pods do not start, while already running pods keep using
their existing socket.

### Container Scoping

By default the webhook mounts the socket into every init container and every
application container except the injected `racer-object` sidecar. That is what
lets `racer-vllm-fetch` read the model during init, before the vLLM container
starts. It is also why the manifest above does not need a separate flag for the
init container.

If you set `racer.unbounded-cloud.io/client-containers`, the value must be a
comma-separated list of distinct init or application container names. When the
annotation is present, only the listed containers get the mount and the
`RACER_SOCKET` variable. If you list only the vLLM container, the fetch init
container has no socket and fails. List both `racer-vllm-fetch` and `vllm`, or
omit the annotation and accept the default.

The annotation is also validated. An unknown name, a duplicate name, or an empty
value is rejected at admission, so a typo fails the pod instead of producing a
partially mounted pod.

### Why the Manifest Has No RACER_SOCKET

You might expect to set `RACER_SOCKET` yourself. Do not do that in an opted-in
pod. The webhook refuses a pod whose selected container already defines
`RACER_SOCKET`, because the value it would add must match the cache named in the
annotation. A manual value could point at a different cache, or at a path that
the webhook is about to mount.

The variable is still useful outside the webhook path. If you run vLLM without
the client-socket label, for example with a manually mounted socket, set
`RACER_SOCKET` to the socket path. The Python client resolves its endpoint in
this order: an explicit `socket` option in the loader config, then
`RACER_SOCKET`, then `/run/racer/<cache>/client/socket` from the model URI. The
last two are equivalent for a webhook-injected pod, which is why omitting the
variable is safe there.

## Install racer-vllm

### The Published Image

The simplest deployment uses the published `racer-vllm` image. It is built from
the upstream base `vllm/vllm-openai:v0.25.1` and installs both Python packages,
so `vllm serve --load-format racer` and `racer-vllm-fetch` work without any
extra setup. The image is published under the same registry and tag scheme as
the other Unbounded release images:

```text
ghcr.io/azure/racer-vllm:<tag>
```

Use the release tag that matches the rest of your Unbounded deployment. Nightly
builds use the `nightly-<shortsha>` tags produced by the nightly workflow. The
image is built for `linux/amd64` only.

The base image tag matters. `racer-vllm` targets vLLM v0.25.x, and the plugin
relies on vLLM entry points and loader APIs from that series. Do not assume that
a much older or much newer vLLM exposes the same plugin interface.

### Other vLLM Images

If you need a different base image, install `racer-client` and `racer-vllm` into
it yourself. Both packages are pure Python, so the wheels do not depend on the
base image's Python ABI beyond a compatible Python version. They are not
published to PyPI. Each GitHub release attaches the wheels and sdists as
assets.

Install from a release with `--find-links` pointed at the release asset page:

```bash
pip install --no-index \
  --find-links https://github.com/Azure/unbounded/releases/expanded_assets/<tag> \
  racer-client racer-vllm
```

Replace `<tag>` with the release tag you are deploying.

`--no-index` keeps pip from resolving the packages or their dependencies from
PyPI. That matters because the packages are not on PyPI, so an index cannot
supply them. Install both packages in the same command so `racer-vllm` finds the
matching `racer-client` from the same release.

For a fully offline image build, download the wheel files into a directory in
the build context and point `--find-links` at that directory:

```bash
pip install --no-index --find-links /wheels racer-client racer-vllm
```

You can also install a wheel by direct URL. The release assets use versioned file
names, so pin the URL to the release you tested rather than to a moving link.
After installation, check that the plugin and command are present:

```bash
python -c "import racer, racer_vllm"
command -v racer-vllm-fetch
```

### From a Checkout

From a repository checkout, install the two local packages in one command:

```bash
pip install ./clients/python/racer ./clients/python/racer-vllm
```

To build wheels and sdists instead, run:

```bash
make racer-python-dist
```

The output goes to `tmp/racer-python-dist`. The equivalent direct commands are
`python -m build clients/python/racer` and
`python -m build clients/python/racer-vllm`. The two projects share a version,
and `racer-vllm` requires the matching `racer-client` release, so build and
install them together.

vLLM discovers the plugin through the `vllm.general_plugins` entry point named
`racer`. The entry point calls `racer_vllm:register`, which registers the
`racer` model loader with vLLM. Registration is idempotent, so importing the
module twice does not add the loader twice.

## Run: Fetch and Serve

### Fetch the Config and Tokenizer

vLLM reads the model configuration and tokenizer before it requests any weight.
Those files must be on local disk, because vLLM opens them as ordinary files.
The fetch command copies them from Racer and writes the marker file:

```bash
racer-vllm-fetch racer://models/llama/Llama-3.1-8B-Instruct /models
```

The command prints the name of each file it writes. In the deployment it runs as
an init container and writes into the shared `emptyDir`. On a workstation it can
write into any writable directory.

`racer-vllm-fetch` reads a fixed list of standard configuration files, plus any
small files named in the index metadata. It writes each file through a
temporary `<name>.tmp` and renames it into place, so a failed fetch does not
leave a half-written `config.json`. It then states every weight file and records
the ETag of each one in `racer-model.json`, together with the model URI.

The command does not download the weights. That is the point. The init container
finishes quickly, and the long read happens inside vLLM with the staging path
active.

### Serve the Weights

Start vLLM on the directory that the fetch populated:

```bash
vllm serve /models --load-format racer \
  --model-loader-extra-config '{"staging_bytes": 268435456, "concurrency": 4}'
```

The model path is a local directory, not a `racer://` URI. The loader obtains
the URI from `racer-model.json` in that directory. You can also pass the URI
explicitly with `{"uri": "racer://models/llama/Llama-3.1-8B-Instruct"}` in the
extra config. The marker is the normal path because it carries the ETag pins.

The extra config is optional. The defaults are 256 MiB per staging buffer, four
buffers in flight, cloning enabled, exportable GPU memory enabled, and a 300
second retry for a refused delivery mode. The next sections explain when to
change them.

### What the Loader Reads

The loader starts by finding the weight files. It reads
`model.safetensors.index.json` and uses the `weight_map`, or falls back to
`model.safetensors` when there is no index. It then states every file before it
reads any tensor data. When `racer-model.json` has an ETag for a file, the stat
uses that ETag. Every later read sends `If-Match` with the same value, so the
whole model is read at one version.

After the stats, the loader reads the 8-byte header length and the JSON header
of each file. All headers are read before any tensor data. The header describes
each tensor's dtype, shape, and byte offsets. The loader groups tensors into
ranges that fit a staging buffer, and each range ends on a Racer page boundary
of 16 MiB. A tensor that crosses the boundary is split across two ranges and
joined again before vLLM sees it. That alignment prevents the same page from
being read twice for two adjacent tensors.

Each range is read into GPU memory with `gpu-read` when the dataplane supports
it. If the dataplane refuses that mode, the client falls back to `host-read`
into pinned memory and copies to the GPU, and then to the normal stream path if
host reads are also refused. Up to `concurrency` ranges are in flight, each in
its own staging buffer. While vLLM consumes one buffer, the next range is read
into another. A CUDA event fences a buffer before it is reused, and every buffer
is fenced before it is dropped, including on an early close or an error.

A tensor that is larger than a staging buffer gets its own GPU allocation. It is
not split into staging-sized pieces, because the destination must be a single
contiguous tensor.

### What racer-model.json Pins

`racer-model.json` contains the model URI and a map from file name to ETag. The
map covers every small file that the fetch wrote and every weight file that the
fetch saw. It is the contract between the configuration on disk and the weights
in the cache.

If a weight file changes after the fetch, the loader's stat at the recorded ETag
fails with `ModelChangedError`. If the checkpoint layout changes, for example an
index is uploaded after a single-file fetch, the loader finds a file that the
marker does not pin and raises `ModelChangedError` with a message that tells you
to rerun `racer-vllm-fetch`. Without a marker, nothing is pinned, and each rank
pins its files at the ETags it sees when its own load starts.

The marker is also why the init container matters on every pod start. It
re-fetches the small files and records the current ETags, so a restarted pod
does not inherit a marker from an older model revision.

## Model Load Reporting

### The racer: loaded Line

After each load, the plugin writes one line to the `vllm.racer_vllm.loader`
logger, so it uses vLLM's log format. The line reports the file count, the bytes
read, the elapsed time, the effective throughput, the model URI, and a breakdown
by delivery mode:

```text
racer: loaded 4 files, 14.96 GiB in 9.12s (1.64 GiB/s) from racer://models/llama/Llama-3.1-8B-Instruct; reads by delivery mode: gpu=60 (14.96 GiB) host=0 (0.00 GiB) stream=0 (0.00 GiB)
```

The byte counts cover tensor data reads. Safetensors header reads use the stream
path and are not counted, so the reported bytes are smaller than the sum of the
file sizes by the header overhead. The file count is the number of weight files,
not the number of configuration files.

The throughput is the number that matters for capacity planning, but compare it
only against loads of the same model on the same hardware. A first load includes
the origin fetch and the cache fill. A later load on the same node reads from
memory or local disk. A later load on a different node reads from peers. Those
are three different paths, and the log does not label which one served each
page.

### Delivery Modes and Fallbacks

`gpu` means the range was written directly into GPU memory. `host` means the
range went into pinned host memory and was copied to the GPU. `stream` means the
range used the ordinary streamed read path. A load that reports mostly `gpu`
bytes is using the fast path. A load that reports `stream` bytes is still
correct, but it is not using the GPU transport.

The client tries the fastest mode first and falls back when the dataplane refuses
it. It records the refusal and logs one warning through the `racer.client`
logger, including the status and the reason. A typical warning says that
`gpu-read` is not available from the socket and that the client is falling back
to host mode. The warning is repeated only after the mode is retried and refused
again, so a long load does not fill the log with the same message.

### Retrying a Refused Mode

`mode_retry_seconds` controls how long a refusal is remembered. The default is
300 seconds. When the timer expires, the client tries the refused mode again on
the next load. Set it to a smaller value when you are actively fixing a driver
problem and want a new pod to pick up the fix quickly. Set it to `null` to retry
only after the dataplane restarts, which is useful when the mode depends on
hardware that will not change while the node is up.

The retry state is per socket and per process. A vLLM restart creates a new
client, so a new pod always starts with a clean view of the dataplane's modes.
A dataplane restart on the same node is also detected, and the recorded refusal
is discarded when the socket is replaced.

## Tensor Parallelism

### Regular Checkpoints

With a regular checkpoint, every tensor-parallel rank reads every weight file,
and each rank's `weight_loader` keeps the slice it needs. The plugin cannot read
only the needed bytes in general, because the slicing decision lives inside each
model's loader, after the tensor has been materialized. The plugin therefore
reads the full tensor and hands it to vLLM, which discards the parts the rank
does not own.

That sounds wasteful, and at the network level it would be. Racer changes the
economics because ranks on one node share the node's dataplane. The first rank
to read a page asks a peer or the origin for it. The other ranks on the same node
read the same page from the dataplane's memory or disk cache. Only the first read
of a page leaves the node. On a node with eight ranks, the origin or peer sees
one read per page, not eight.

The sharing is per node, not per pod. Two vLLM pods on the same node with the
same cache also share the dataplane cache. That is why co-locating replicas or
ranks is often more valuable than spreading them, even though spreading improves
failure isolation.

### Sharded State Checkpoints

A vLLM `sharded_state` checkpoint stores each rank's tensors in separate files.
The loader can then read only this rank's parts, copy each tensor straight into
the model's state dictionary, and skip the rest. This is the layout to use when
tensor parallelism crosses node boundaries, because each node reads only the
bytes its rank needs instead of every rank reading the whole model.

Save the checkpoint with vLLM's sharded-state saver, using the same tensor and
pipeline parallel sizes that you will use at load time. Upload the resulting
files under the model prefix. Then load with:

```bash
vllm serve /models --load-format racer \
  --model-loader-extra-config '{"sharded_state": true}'
```

The loader probes the rank's parts starting at `part-0` and stops at the first
missing part. Each rank loads only the parts pinned in `racer-model.json`. A new,
missing, or replaced part fails with `ModelChangedError`, so a partial upload is
caught instead of being loaded as a truncated model.

If your checkpoint uses custom file names, pass the pattern to both the fetch and
the loader. The fetch needs it to find and pin every rank's files, and the loader
needs it to find this rank's files:

```bash
racer-vllm-fetch racer://models/llama/Llama-3.1-8B-Instruct /models \
  --pattern 'model-pp-{pp_rank}-tp-{tp_rank}-part-{part}.safetensors'
```

Use the same pattern string in `--model-loader-extra-config` under the
`pattern` key. The pattern is not inferred from the checkpoint metadata, so the
two sides must agree.

### Choosing a Layout

Use a regular checkpoint when tensor parallelism stays inside one node and you
want the simplest upload and the fewest files. The node-local cache deduplicates
the repeated reads, and the checkpoint is the format most model publishers
provide.

Use `sharded_state` when ranks are spread across nodes, when the model is large
enough that reading the full checkpoint per rank saturates the network, or when
you want each rank's load to be independent. The tradeoff is an extra conversion
step, a stricter requirement that the parallel sizes match, and a file layout
that is tied to a particular parallelism.

## Warm Up and Verify

### Verify Admission

Apply the Deployment and check that the pod was mutated. The injected annotation
is the quickest signal:

```bash
kubectl -n vllm get pods -l app=llama-31-8b \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.racer\.unbounded-cloud\.io/client-socket-injected}{"\n"}{end}'
```

A value equal to the cache name means the webhook mounted the socket and added
the node affinity. An empty value means the pod was not selected, which usually
means the label is on the Deployment instead of the pod template. A cache name
that does not exist fails admission instead.

If admission is denied, read the webhook's message. The common causes are a
missing `client-cache` annotation, a cache name that does not exist, a container
list that names a container that is not in the pod, or a pod that already sets
`RACER_SOCKET`. The message identifies the field, and the fix is in the pod
template.

### Verify the Fetch

Watch the init container:

```bash
kubectl -n vllm logs deploy/llama-31-8b -c racer-vllm-fetch
```

The log lists the files that were written. A fetch that ends without
`racer-model.json` did not complete. The usual causes are a missing
`config.json` at the prefix, a wrong cache or bucket segment, a socket that the
container cannot reach, or a model layout that does not match the pattern you
passed.

You can also inspect the volume from the running container:

```bash
kubectl -n vllm exec deploy/llama-31-8b -c vllm -- ls -l /models
```

You should see the configuration and tokenizer files plus `racer-model.json`.
You should not see multi-gigabyte safetensors files. If you do, something copied
the weights into the `emptyDir`, and the loader is not being used as intended.

### Verify the Load

After the init container finishes, vLLM starts and the loader runs. Follow the
vLLM log:

```bash
kubectl -n vllm logs deploy/llama-31-8b -c vllm -f
```

Look for the `racer: loaded` line. It should report the expected number of weight
files and a byte count close to the checkpoint size. A byte count that is much
smaller than expected usually means the model directory contains a partial
checkpoint, or the index points at only a subset of the shards.

Also check the dataplane GPU line on the same node after the load, using the pod
name from the earlier check:

```bash
kubectl -n unbounded-system logs <racer-pod> -c dataplane --tail=300 | grep 'stage=gpu'
```

If the load reports `stream` bytes and the dataplane says `state=disabled`, the
model still loaded but the GPU path was not used. Fix the driver or the
`RACER_ENABLE_GPU` setting before you tune loader concurrency.

### Verify the Server

Once vLLM is ready, check the OpenAI-compatible endpoint. Forward the port and
query the model list:

```bash
kubectl -n vllm port-forward deploy/llama-31-8b 8000:8000
curl -sS http://127.0.0.1:8000/v1/models
```

The response should name the model that vLLM loaded. This proves that the
process finished initialization and that the tokenizer and configuration are
usable. It does not prove that every weight tensor came from the intended
version, because the loader already enforced that with ETags.

A short generation request is a better end-to-end check:

```bash
curl -sS http://127.0.0.1:8000/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{"model": "/models", "prompt": "Racer caches", "max_tokens": 16}'
```

If the server answers, the model is loaded and the GPU is executing. If it
returns a model-not-found error, use the identifier from `/v1/models` in the
request.

### Measure the Warm Path

The first load on a node is the cold load. It fetches missing pages from the
origin and fills the node cache. To measure the warm path, restart the pod on the
same node and compare the `racer: loaded` throughput. The second load should be
faster if the pages are still resident or if a peer already owns them.

Kubernetes does not guarantee that a recreated pod lands on the same node. Use a
node selector, a node affinity rule, or a single-node pool when you need a
controlled comparison. Do not compare a cold load on one node with a warm load
on a different node and attribute the difference to the cache alone; peer
placement and network distance also change the result.

## Multi-Node Considerations

### Placement and Cache Pools

A cache with a `nodeSelector` is served only by matching nodes, and client pods
are constrained to those nodes. That is usually what you want for a GPU model
server: the cache pool and the GPU pool are the same set of nodes, so a model
read never lands on a node that cannot run the model.

If you leave the selector unset, every Racer node can store and serve the cache.
That gives the cache more capacity and more peers, but it also means a vLLM pod
can be scheduled on a node outside your GPU pool if other scheduling constraints
allow it. Add a GPU node selector to the pod or set the cache selector to keep
the two aligned.

The webhook adds the cache's selector to the pod's required node affinity. Do
not add a conflicting selector to the same pod. A conflict produces a pod that
cannot be scheduled, and the event message names the unsatisfiable affinity
rather than the cache.

### Tensor Parallel Across Nodes

A tensor-parallel group that spans nodes has one rank per node, or several ranks
per node, depending on the parallel size and the GPU count. Each rank still
needs its weights. With a regular checkpoint, every node reads every shard, and
the cache deduplicates only within a node. With `sharded_state`, each node reads
only the parts for the ranks it hosts, which is the difference between reading
the whole model per node and reading one rank's share.

The dataplane on each node serves its local ranks over the local socket, so a
rank's read does not cross the network to reach its own node's cache. A miss is
sent to the page owners, which may be other nodes. The first reader of a page in
the group pays the peer or origin latency; the other ranks on that node reuse
the page.

### Replicas and Fan-Out

Replicas that start together create the worst fan-out. Each replica runs its own
init container and its own load. The init containers are cheap because they read
only small files. The loads are the expensive part, and they all start at
roughly the same time.

Racer handles the fan-out by making the page owners the first stop. If ten
replicas on ten nodes ask for the same page at once, the owners coalesce the
requests and fetch the page once under normal conditions. The replicas then read
the page from the owners. This is best-effort deduplication, not exactly-once
delivery: races between owners and retries can still produce more than one origin
request. Size the origin's rate limits for that behavior rather than for a
guarantee.

Staggering a rollout still helps. A RollingUpdate with a small `maxSurge` warms
the cache before the next batch starts, and the later replicas read from a cache
that already holds the pages.

### Control Plane and Origin Health

Every dataplane pod runs the control role it is elected to hold. The control
seats serve the admission webhook and maintain the cache publication state. If
the primary lease has no holder, new opted-in pods are denied. Existing pods
keep running, but a rollout stalls until a seat is elected.

The origin is contacted only on a cache miss. A transient origin failure during
a cold load causes the read to fail, and the client may retry according to its
own bounds. Once the pages are cached, a later load does not need the origin. If
an origin update changes the ETag of a pinned file, the next load fails with
`ModelChangedError` rather than serving a mix of old and new pages.

## Performance Tuning

### Loader Options

The loader exposes a small set of options through
`--model-loader-extra-config`. They control how much GPU memory is used for
staging and how many reads are in flight:

| Key | Default | Effect |
| --- | --- | --- |
| `uri` | from `racer-model.json` | The `racer://` model location. |
| `staging_bytes` | 256 MiB | Size of each GPU staging buffer. |
| `concurrency` | 4 | Staging buffers, and ranges read in flight. |
| `clone` | true | Copy each tensor out of staging before handing it to vLLM. |
| `socket` | none | Client socket path. |
| `use_vmm` | true | Allow exportable `cuMemCreate` staging, needed for `gpu-read`. |
| `mode_retry_seconds` | 300 | Retry a refused delivery mode after this many seconds. |
| `sharded_state` | false | Read a vLLM `sharded_state` checkpoint. |
| `pattern` | as vLLM | File name pattern for `sharded_state`. |

Staging memory is not free. Each buffer is allocated in GPU memory, so the
default of four 256 MiB buffers reserves about 1 GiB that vLLM cannot use for
weights or KV cache. Raising `concurrency` to 8 doubles that reservation. Raise
it only when the dataplane can fetch far enough ahead to keep the buffers busy,
and when the model still leaves enough memory for the workload.

`staging_bytes` controls the size of one read. A larger buffer means fewer
requests and better sequential throughput, but it also means a larger allocation
and more memory held while a range is consumed. A tensor larger than the buffer
gets its own allocation, so a very small buffer can cause many dedicated
allocations and lose the benefit of batching.

`clone` is a correctness setting as much as a performance one. With `clone` set
to `true`, each tensor is copied out of staging memory before vLLM receives it,
so the tensor remains valid after the staging buffer is reused. With `clone` set
to `false`, tensors are views into staging memory and are valid only until vLLM
asks for the next tensors after the buffer's last one. Only disable cloning when
you have verified that the model copies each weight immediately and never keeps
a view.

`use_vmm` allows the loader to allocate exportable memory with `cuMemCreate`,
which is what makes direct `gpu-read` into the buffer possible. If the allocation
fails, the loader logs that exportable GPU memory is unavailable and falls back
to ordinary torch memory, which uses `host-read` and a copy. Leaving it enabled
is the right default; disable it only when the driver cannot allocate exportable
memory and you want to skip the failed attempt.

### Dataplane Options

The loader's concurrency and the dataplane's read-ahead must be sized together.
On fast local disks, more reads in flight help only when the dataplane also
fetches further ahead. The relevant dataplane settings are in the
`racer-settings` ConfigMap:

- `RACER_RANGE_WINDOW_PAGES` is the page window for range reads. The default is
  4 and the maximum is 64. A larger window helps on high-latency links and costs
  memory held for the read.
- `RACER_GPU_STAGING_BYTES` is the per-GPU cap on ciphertext staging memory. The
  default is 256 MiB, and it must be a nonzero multiple of 2 MiB.
- `RACER_GPU_PCI_P2PDMA` enables the PCI peer-to-peer DMA path for cuFile when
  `nvidia-fs` is not available. The default is `false`.
- `RACER_GPU_LANDING_BYTES` is the per-GPU buffer that peers' NICs write
  GPU-read pages into over RDMA. The default is 256 MiB, and `0` turns it off.

The common tuning pair is `concurrency` set to 8 together with a larger
`RACER_RANGE_WINDOW_PAGES` and a larger `RACER_GPU_STAGING_BYTES`. Change the
dataplane first, wait for the rollout, then change the loader. Changing only the
loader gives you more buffers waiting on the same dataplane window, which does
not improve throughput and does use more GPU memory.

### Cache Priority and Capacity

Model pages compete with every other cache on the node. `spec.priority` is the
relative weight used to share disk and memory. The default is 500. A model cache
that should survive pressure from a dataset cache or a container-image cache can
use a higher value, up to 1000. The `models` example uses 900.

Priority is not a reservation. It changes eviction preference, not the amount of
disk or memory that exists. If the model is much larger than the node's cache
capacity, pages are evicted and re-fetched, and no priority value prevents that.
Size the pool so that the model fits across the nodes that read it, or accept
that some fraction of each load comes from peers or the origin.

The cache is also shared across all clients that use it. A vLLM deployment and a
batch job reading the same model can help each other by sharing pages. A second
cache for the same container does not share pages with the first, because cache
entries are keyed by the `ClusterCache` UID. Use one cache per container unless
you deliberately want separate capacity and eviction domains.

### RDMA and GPUDirect

Peer transfers use TCP by default and can use RDMA when both ends are eligible
and the edge is selected for RDMA. `RACER_ENABLE_RDMA=auto` enables native RDMA
when the binary and the ports support it, `true` reserves the resources even
without ports for hotplug, and `false` disables it. RDMA reduces peer-transfer
cost, which matters when a node reads a large model from other nodes rather than
from the origin.

GPUDirect RDMA is the landing path for GPU reads. When GPU decryption and RDMA
are both enabled, each decrypting GPU gets a landing buffer sized by
`RACER_GPU_LANDING_BYTES`. A peer's NIC writes pages directly into that buffer,
and the GPU decrypts them without a host copy. If allocation, export, or
registration fails, that rail and GPU stay host-only instead of failing the
workload. That fallback is deliberate: a partially working GPU path is better
than a model that does not start.

These paths are optional. A cluster without RDMA still benefits from the local
cache and the GPU read path. Tune them only after the basic load works and you
have measured where the time goes.

### What Not to Tune

Do not set `concurrency` to a large value to compensate for a slow origin. The
origin is contacted on a miss, and the dataplane's owner selection and
single-flight behavior already bound how many origin requests one page can
produce. A large client concurrency can increase memory pressure without
increasing origin throughput.

Do not disable `clone` to save a copy unless you have read the model's loader and
confirmed that it consumes each tensor immediately. The failure mode is subtle:
the model may load successfully and produce incorrect results when a staging
buffer is reused while a tensor still points into it.

Do not change the `ClusterCache` container URL to point at a new model location.
The field is immutable, and the cache identity is tied to the UID. Create a new
cache for a new container, and use a new prefix for a new model revision in the
same container.

## Troubleshooting

### The Fetch Does Not Write racer-model.json

`racer-model.json` is written only after the fetch has read the configuration
files and stated every weight file. If it is missing, the fetch did not finish.
Check the init container log first. A missing `config.json` produces a
`FileNotFoundError` naming the object, which almost always means the prefix or
the bucket segment is wrong.

With the Azure origin, the bucket segment must be the container name. A URI that
uses a friendly name such as `models` as the bucket fails the origin's identity
check even though the cache name is also `models`. The bucket is the container,
and the cache is the first segment.

If the fetch reads the configuration but cannot find weights, check whether the
checkpoint is a single file, an indexed shard set, or a `sharded_state` layout.
A `sharded_state` checkpoint has no `model.safetensors` for the default fetch to
find, so pass `--pattern` or rely on the default vLLM patterns. A custom pattern
that does not match the uploaded names also produces this result.

### Concurrent Fetches Collide

`racer-vllm-fetch` writes each file through `<name>.tmp` and renames it into
place. Two fetches running against the same destination directory use the same
temporary names, so they can overwrite each other's files or rename a file that
the other process is still writing. There is no cross-process lock in the
command.

Run one fetch per destination. In Kubernetes, use one init container per pod and
a per-pod `emptyDir`. Do not mount a shared volume and start several fetch
containers against it. If you need to prepare a directory outside Kubernetes,
serialize the commands instead of running them in parallel.

### The Origin Changed

A model update at the origin produces a new ETag. A load that started with the
old ETag fails with `ModelChangedError`, and the message says that the file
changed during the load. This is not a cache corruption. It is the version pin
doing its job.

Rerun `racer-vllm-fetch` to record the new ETags and the new configuration, then
restart the vLLM pod. If the model was replaced in place and the old weights are
gone, the fetch must run before any load can succeed. A Deployment restart does
this automatically because the init container runs on every new pod.

A layout change produces the same error with a different message. If the marker
pins no index and an index appears, or the index names a weight file that the
marker does not pin, the loader refuses the unpinned file and tells you to rerun
the fetch. The fix is the same: fetch again, then load again.

### Pods Are Not Admitted

Admission failures happen before the pod exists. Read the event or the API
server's denial message. The common causes are:

- The pod has the `client-socket` label but no `client-cache` annotation. The
  annotation is required and must name an existing cache.
- The annotation names a cache that does not exist, or uses a name that is not a
  valid DNS subdomain within the length limit.
- `client-containers` lists a name that is not an init or application container,
  lists a duplicate, or is empty.
- A selected container already sets `RACER_SOCKET`.
- The pod already defines a volume named `racer-client-socket`.
- The pod is not Linux, or its node affinity conflicts with the cache's node
  selector.

A pod that opts in is denied when the webhook is unavailable. Check the
`racer-primary` lease and the dataplane pods before you change the manifest. A
webhook outage is a cluster problem, not a pod spec problem.

### The Pod Starts but Never Becomes Ready

If the pod is admitted but vLLM does not become ready, separate the layers.
First check the init container. A failed fetch leaves the pod in
`Init:Error` or `Init:CrashLoopBackOff`, and the vLLM container never starts.
The fetch log names the object it could not read.

If the init container succeeds, check the vLLM container. The image must be
present on the node, the GPU resource must be allocatable, and the model
directory must contain the files the loader expects. An `ImagePullBackOff` on a
private registry is an image credential problem, not a Racer problem. A
`CreateContainerConfigError` often means a volume or environment reference is
missing.

If vLLM starts but the load stalls, check the socket. A container that cannot
connect gets a connection error or a timeout, and the loader does not fall back
to the origin directly; the dataplane is the only path. Verify that the
`racer-client-socket` volume is mounted at `/run/racer/models/client` and that
`RACER_SOCKET` points at the socket inside it.

### The Load Uses stream Reads

A load that reports `stream` bytes instead of `gpu` bytes is using the portable
path. Check the dataplane log for `stage=gpu`. `state=standby` means the helper
has not been started, which is normal until the first GPU read. `state=disabled`
means it tried and failed; the reason field identifies the cause. `state=enabled`
means the helper is up, so a stream-only load points at the client side or at a
mode that the dataplane refused for this request.

If the helper is enabled but the client still uses stream, check whether the
vLLM container's GPU is visible and whether the loader has `use_vmm` enabled. A
failed exportable allocation falls back to torch memory and host reads, and the
loader logs that fallback. A driver or container runtime that does not expose
the GPU to the client produces the same symptom.

### The Load Fails with Missing or Unpinned Weights

An error that names a file as not pinned means the checkpoint layout changed
after the fetch. Rerun the fetch and restart the pod. An error that says weights
were not loaded means vLLM built the model but the loader did not supply every
parameter. That can happen with a checkpoint whose tensor names do not match the
model configuration, or with a quantization path that hides parameters from the
check.

Check the model's configuration and the checkpoint's index together. Racer
returns the bytes the origin has; it cannot make a mismatched checkpoint match a
model. If the model loads with a local copy of the same files but not through
Racer, compare the fetched file list and the index with the local directory.

### Slow First Load, Fast Second Load

A slow first load and a fast second load on the same node is the expected cache
behavior. The first load fills pages from the origin and pays the origin and
network cost. The second load reads the pages from memory or local disk. If the
second load is still slow, the pages may have been evicted by another cache, the
node may have restarted, or the pod may have landed on a different node.

Check the node name on both pods and the cache priority. A model cache with a low
priority loses pages to busier caches under pressure. A node with a small slab
budget also evicts more often. If the working set does not fit, the cache still
helps by serving pages from peers, but it cannot guarantee a local hit.

## Related

- [Cache Azure Blob Storage with Racer]({{< relref "guides/azure-blob-cache" >}})
  for the cache, origin, credential, and sidecar basics.
- [Racer Distributed Cache]({{< relref "concepts/racer" >}}) for the capacity,
  placement, and data path model.
- [Racer Reference]({{< relref "reference/racer" >}}) for the API fields and
  dataplane settings.
- [racer-object Reference]({{< relref "reference/racer-object" >}}) for the
  socket protocol and the object adapter.
- [NVIDIA GPU Support]({{< relref "reference/gpu/nvidia" >}}) for driver, CDI,
  and GPUDirect prerequisites.
- [GPU Reference]({{< relref "reference/gpu" >}}) for the higher-level GPU
  model.
