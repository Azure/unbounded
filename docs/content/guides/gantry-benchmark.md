---
title: "Gantry Benchmark"
weight: 9
description: "How the Gantry image distribution benchmarks are run, what the 1,000-node cold-pull, fleet, 128 GiB single-layer, and Artifact Streaming measurements show, and how to read them."
---

## Overview

This page documents how the Gantry benchmarks are run and what their results actually
mean. Gantry is the peer-to-peer OCI distribution layer described in
[Distribute Container Images with Gantry]({{< relref "guides/gantry" >}}). The benchmarks
exist to answer a narrow question: when a large fleet pulls the same large image at the
same moment, how much traffic reaches the registry, how long do pods wait before their
containers start, and which parts of that time are under Gantry's control.

The measurements here come from a dedicated AKS test cluster and a repeatable harness
under `hack/gantry-benchmark/`. The consolidated dataset used for this page is
`hack/gantry-benchmark/GANTRY-ONLY-RESULTS.md`, and the design rationale behind the
cold-start behavior is in `designs/gantry-cold-pull-explained.md`,
`designs/gantry-cold-pull-rotating-chair.md`, `designs/gantry-128gib-single-layer-pulls.md`,
and `designs/gantry-acr-artifact-streaming.md`. Where those documents and the code
disagree, the code is authoritative.

Read the numbers as properties of the environment that produced them, not as service
levels. A different registry, network path, node disk, or image layout will move them.
The sections below state the cluster and image shape for each result so you can judge how
close your own environment is.

## What the benchmark measures

Every run in the main campaign follows the same basic shape. A 1,000 node AKS cluster
runs one pull pod per node, and all of the pods start at once against the same image that
is not present on any node. That is a deliberately hostile cold-start condition: there is
no warm cache to hide registry behavior, and no gradual arrival to spread the load.

The benchmark records four things about that event. It records how much data crosses the
ACR Private Endpoint, how much data Gantry moves between nodes, how long each pod takes
to reach a running container, and whether every pod completes. The first two are byte
counters, the third is a latency distribution, and the fourth is a correctness check that
keeps a fast but broken run from being reported as a win.

### Cluster and image shape

The main cold-pull results use 1,000 nodes running Kubernetes on AKS. The workload is a
Kubernetes Job with 1,000 completions and 1,000 parallelism, plus required hostname
anti-affinity keyed by run and phase, so exactly one pull pod lands on each target node.
The image is 40 GiB spread across 40 layers. Every node requests it cold.

Those three numbers interact. The node count sets the fan-out, the image size sets the
bytes each node needs, and the layer count sets how much of the transfer can overlap.
Forty layers give Gantry many independent digests to rank and distribute, while a single
giant layer removes that parallelism and exposes different limits, as the 128 GiB section
below explains.

### How latency is defined

Pod start latency is measured from container start to a successful running state, not
from the moment the Job object is created. In the harness, each pull container stays
running for 15 seconds after image startup so the AKS audit log reliably captures the
status transition. That hold is not included in the startup metric.

The 1,000-node tables report P50, P95, and P100 over all 1,000 pods. P100 is the slowest
pod in the run, so it is the number that exposes tail behavior: a single node with a slow
disk or a stalled peer connection shows up there and nowhere else. The fleet pulls in the
next section use a different definition, measured from DaemonSet creation to the latest
container start, so they include image pull, verification, unpack, and container creation
rather than only the network transfer.

## The two comparison arms

A benchmark needs a baseline that differs in exactly one thing. The harness runs two
phases against the same payload, the same image size, the same layer count, and the same
node set, and changes only how the bytes reach each node.

### Direct origin

In the direct arm, containerd talks to the registry itself. In `direct` mode each phase
has its own dedicated ACR and Private Endpoint, and the baseline phase pulls from the
baseline registry with no Gantry in the path. In the older `proxy` mode, both phases route
through a counting proxy that is the only measured ACR client, which makes origin bytes a
direct measurement.

The harness deliberately transfers `BENCHMARK_NODE_COUNT * BENCHMARK_IMAGE_SIZE_MIB` from
the registry in the baseline. That is the point of the baseline: it reproduces the full
fan-out cost that a cluster without peer distribution pays.

### Gantry cold pull

In the Gantry arm, containerd points at the node-local Gantry mirror instead of the
registry. Gantry checks its local content store, asks the distributed index whether a
peer holds the digest, and only falls back to the registry when the content is genuinely
cold. Because the Gantry phase runs on the same nodes after the baseline phase, the
harness gives each phase its own layer paths so containerd cannot reuse baseline blobs
from its digest-addressed cache.

The comparison is therefore not "Gantry versus a tuned registry". It is "the same cold
pull, with and without a peer distribution layer in front of the same kind of registry".
That is why the harness records the payload SHA-256 and the exact manifest digests: the
two arms must be provably serving identical bytes.

One distinction matters when reading the tables below. `GANTRY-ONLY-RESULTS.md` records
the Gantry arm of these campaigns: each row is a Gantry run with its own ACR wire counter
recorded alongside. The paired direct-origin phase is what the harness produces in
baseline mode, and the historical 300-node table later on this page is one such paired
result. The 1,000-node tables do not contain a direct-origin latency column, so do not
infer one from them.

## Why registry traffic is bounded

The design goal is easy to state and hard to achieve. Registry traffic should scale with
the image, not with the cluster. Without help, 1,000 nodes pulling a 40 GiB image request
42 TB from the registry, because nothing in Kubernetes deduplicates identical pulls.

### The cold-start problem

Gantry's normal path is peer-first. Every agent publishes the layers it holds into a
distributed index keyed by content digest, so a node that needs a layer can ask which
peers already have it. Availability grows as the pull spreads, because every node that
receives a layer publishes it in turn.

That path does nothing on the first pull of a brand-new image. The index is empty,
nobody holds the layer, and there is no peer to fetch from. The two obvious answers are
both wrong. Letting every node go to the registry recreates the 42 TB problem. Letting a
single node fetch 40 GiB makes that node a bottleneck and a single point of failure.

Gantry instead designates a small fixed set of nodes, called chairs, to fetch each layer
from the registry. A chair is not a coordinator and does not decide anything for other
nodes. It is a node that has agreed to be one of the designated origin fetchers for a
digest.

### Deterministic seed selection

For any given layer, all nodes must agree on which chairs should fetch it without talking
to each other. Gantry does this with a highest-random-weight hash of the layer digest
against the configured chair identities. The hash produces a ranking, and the top of the
ranking becomes the seed cohort for that digest.

Because the inputs are just the digest and the fixed chair names, every node computes the
same ranking independently. There is no election, no coordination round, and no chatter
on the cold path. A different layer hashes to a different ranking, so the 40 layers of an
image spread their seeding work across the chair pool instead of piling onto the same
nodes.

With the default configuration of 64 chairs and 8 seeds, eight copies of a 40 GiB image
is 343.6 GB. One seed would be 40 GiB but would let a single slow or dead node stall the
cluster. One thousand seeds would be the full 42 TB with no sharing at all. The design
picks eight as the point that tolerates several slow or failed seeds while keeping origin
traffic bounded.

## The rotating chair design

The chair design replaced an earlier membership-based approach that watched Pods and
Nodes cluster-wide. The change matters for the benchmark because it removes control plane
cost that would otherwise grow with node count, and it changes how a failed seed is
handled. Both effects show up in the measurements.

### Lease-based chairs

Each configured chair is a Kubernetes Lease object with a stable numbered name. Agents
claim empty Leases to become chair holders, renew them as a heartbeat, and let them expire
or hand them over when they rotate. The Lease API is the same primitive kubelet uses for
node heartbeats, so no new coordination service is involved.

A holder records its persistent libp2p peer ID, network address, generation, assignment
epoch, renewal time, lease duration, and the planned next holder. At most one chair is
held per node, and a holder is a final origin seed puller rather than a coordinator that
performs another selection round. In a cluster smaller than the configured chair count,
every node can hold a chair.

### Per-digest ranking and seed scaling

The active cohort size scales with how many chairs are actually selectable. Holder
capacity is the minimum of the configured chair count and the Gantry DaemonSet's desired
capacity. The number of seeds for a digest is the configured seed count multiplied by the
selectable holder count and divided by the chair count, rounded up, with a minimum of one
and a maximum of the seed count.

At the default 64 chairs and 8 seeds, three selectable holders select one seed, twenty
select three, thirty-two select four, and a full pool selects eight. This is why the
benchmark's full 1,000-node pool lands on the eight-copy floor while a small cluster does
not try to use eight seeds it cannot staff.

### Failure handling

The interesting decision is what happens when a chair does not answer. The intuitive
response is to recruit a replacement, and that response is wrong. A chair that has not
answered within the timeout has usually already started fetching. It is busy, not absent.
Recruiting a replacement does not move the work, it adds another fetcher.

Gantry therefore contacts the active cohort once and accepts partial answers. It moves
further down the ranking only when the entire cohort answers nothing at all, which means
the chairs are genuinely unreachable rather than merely slow. Measured on 1,000 nodes,
that distinction was the difference between 13.4 and 8.0 registry copies per layer in the
earlier eight-seed configuration.

If a node truly cannot get a layer from any peer or chair, it falls back to pulling from
the registry directly. The path exists for correctness but should be rare. In the cold
pull measurements discussed below, bytes containerd received from the registry directly
were zero.

### Rotation and control plane load

Chairs are not permanent, because being a seed is extra network and disk work. The role
moves on a six-hour assignment epoch derived from the clock, so every node knows the
current epoch without asking anyone. Five minutes before the boundary, the current holder
offers the seat to a reachable non-chair node. The successor must accept before it is
recorded, and if nobody accepts, the existing holder keeps the chair. Work already in
flight on the old holder finishes normally.

The design opens no watches on Pods or Nodes. Nodes refresh a cached chair snapshot when
the epoch changes or when a chair they tried to reach did not answer, and only holders
write, renewing their own Lease once a minute. At 1,000 nodes that replaces 2,000 watch
streams with zero. At 100,000 nodes it replaces 200,000, and the write rate stays bounded
by the chair count rather than growing with the fleet.

One practical check: the shipped RBAC still grants the agent `list` and `watch` on Pods
and Nodes. Those permissions are left over from the informer design and the current
binary does not use them. They are retained so agents from the previous release keep
working during a rolling upgrade, and they can be dropped once every agent is on the
chair design.

## Results at 1,000 nodes

The main campaign reports eleven completed runs of the 40 GiB, 40 layer image on 1,000
nodes. All eleven completed 1,000 of 1,000 pods. The first three are the chair-design
group; the remaining eight are labeled `current-gantry` in the dataset. The chair-design
rows are the ones to read for the eight-seed configuration, because that is the design the
cold-pull documents describe.

The table below reproduces the chair-design traffic measurements. ACR traffic is wire
bytes at the Private Endpoint. Gantry origin traffic is the payload Gantry accounts for
at its upstream response boundary. The difference between the two is the overhead that
the registry sees but Gantry does not attribute to the payload, and the dataset notes
that ACR traffic runs about 9% above the payload Gantry accounts for.

| Run | Nodes | Completed | Duration | ACR wire traffic | Gantry origin traffic | Difference | Peer traffic |
|---|---|---|---|---|---|---|---|
| `chair-design-112507` | 1,000 | 1,000/1,000 | 871.4 s | 382.2 GB | 352.2 GB | 30.0 GB | 42.7 TB |
| `chair-design-040127` | 1,000 | 1,000/1,000 | 823.5 s | 376.3 GB | 343.6 GB | 32.6 GB | 42.6 TB |
| `chair-design-030517` | 1,000 | 1,000/1,000 | 785.6 s | 375.0 GB | 343.6 GB | 31.3 GB | 42.6 TB |

The pattern is the one the design predicts. Two runs sit exactly on the 343.6 GB floor
that eight copies of a 40 GiB image implies, and the third is a few extra seed fetches
above it. Peer traffic is essentially constant at 42.6 to 42.7 TB because the cluster
must still move roughly the full image to every node. The registry no longer moves those
bytes; the cluster network does.

Put differently, the registry served 343.6 GB while peers moved 42.6 TB. The registry
therefore accounted for well under one percent of the bytes on the wire, and the saving
came from replacing repeated origin downloads with one copy per seed plus peer fan-out.

### Pod start latency

The same three runs produce the following startup distribution. These figures are the
full pod start metric described above: image pull, verification, unpack, and container
creation, measured to a running state.

| Run | P50 | P95 | P100 |
|---|---|---|---|
| `chair-design-112507` | 633.0 s | 714.8 s | 851.9 s |
| `chair-design-040127` | 635.0 s | 711.2 s | 796.2 s |
| `chair-design-030517` | 640.3 s | 712.0 s | 763.7 s |

The medians cluster within about seven seconds of each other, and P95 within about four
seconds. That is tight for a 1,000 node cold pull. P100 moves more, from 763.7 s to
851.9 s, which is expected: the slowest node in a fleet is sensitive to its own disk and
scheduling luck, not just to the distribution layer.

The later `current-gantry` runs show the same shape with more spread. Their P50 ranges
from 619.2 s to 671.0 s, their completed durations from 761.2 s to 1,166.0 s, and their
ACR traffic from 561.3 GB to 1.7 TB. Their Gantry origin traffic ranges from 486.5 GB to
1.3 TB and their peer traffic stays between 41.3 and 42.4 TB.

Do not read the `current-gantry` group as a controlled comparison against the
chair-design group. The dataset does not record a configuration change that would explain
the difference, and the labels group runs by harness generation rather than by a single
independent variable. They are useful as evidence that the pattern repeats across runs,
not as an A/B result.

## Where the time goes

The most important interpretation point is that a 40 GiB image on 1,000 nodes is not
primarily a network benchmark. Each node must write and unpack 40 GiB to its own disk.
That work happens regardless of whether the bytes arrived from ACR or from a peer, so it
puts a floor under startup latency that no amount of registry-traffic reduction can
remove.

The dataset makes this explicit: latency is dominated by writing and unpacking 40 GiB per
node, so it is largely insensitive to registry-traffic changes. The chair-design runs cut
origin traffic to the eight-copy floor and still show P50 values around 633 to 640
seconds. A run with higher ACR traffic in the `current-gantry` group does not show a
proportional latency increase, because the extra bytes are not the bottleneck.

This is why the benchmark reports both traffic and latency and warns against optimizing
one in isolation. If your nodes have slow disks, faster distribution will not make the
pull fast. If your registry link is the constraint, faster disks will not reduce origin
spend. Gantry targets the second problem, and the unpack floor belongs to the node and to
containerd.

## 4,997-node fleet pulls

The main campaign uses 1,000 nodes for a controlled comparison. A separate set of ad hoc
runs exercises a much larger fleet to check that the path still completes when the node
count is close to 5,000. These runs use a DaemonSet with one pod on every node labeled
`gantry-benchmark=worker`, and they measure from DaemonSet creation to the latest
container `startedAt` timestamp. That definition includes image pull, verification,
unpack, and container creation, not only network transfer.

| Run | Image | Size | Payload layers | Completed | Duration |
|---|---|---|---|---|---|
| `fleet-10g-20260909-134716` | `sha256:aa95e2c0...a572fd56` | 10 GiB | 10 | 4,997/4,997 | 759 s (12m39s) |
| `fleet-20g-20260909-141523` | `sha256:c6c274fe...6a916b8e` | 20 GiB | 20 | 4,997/4,997 | 1,314 s (21m54s) |

The 10 GiB and 20 GiB images have ten and twenty unique 1 GiB payload layers
respectively. They share one Alpine base layer and no payload layers. Sharing the base
layer while differing in payload is deliberate: it exercises the case where a small
common layer is already available and the large layers are not.

The timestamps show how quickly the fleet came up once the downloads were underway. For
the 10 GiB run, the earliest container started at `2026-09-09T13:52:14Z` and all 4,997
were running by the last-container timestamp. For the 20 GiB run, the earliest container
started at `2026-09-09T14:23:38Z` and all 4,997 were running by the last-container
timestamp. Doubling the payload roughly doubled the duration, which is consistent with
the per-node write and unpack cost dominating.

These runs are a scale check, not a third comparison arm. They show that the DaemonSet
path completes at 4,997 nodes with the same image shapes, but they do not include a
simultaneous direct-origin baseline, so they cannot be used to compute a reduction
percentage on their own.

## 128 GiB single-layer pulls

Multi-layer images hide a different class of problem. Gantry can distribute layers in
parallel, and a stalled layer may be masked by progress on its siblings. A single 128 GiB
layer removes that parallelism and exposes absolute duration limits that a normal image
never reaches. The tracking document for this case is
`designs/gantry-128gib-single-layer-pulls.md`, and it is careful about what is measured
and what is arithmetic.

The document starts from Gantry `v0.8.0` and records that measurements from a production
pull are not yet available. All throughput thresholds in it are derived from configured
sizes and deadlines. That distinction matters: the numbers below tell you where the old
limits bind, not what a real 128 GiB pull achieved.

### The five-minute and 30-minute limits

Version `v0.8.0` had two independent absolute duration limits on origin pulls. Every
registry HTTP request had a five-minute `http.Client.Timeout`, which included reading the
entire response body. Separately, a chair's background origin pull started with a
five-minute budget, extended that budget using the advertised content size, and clamped
the result to 30 minutes.

The five-minute HTTP client timeout was the first effective limit. A 128 GiB layer,
treated as 131,072 MiB and ignoring protocol, authentication, storage, and commit
overhead, must average at least about 436.9 MiB/s to finish inside five minutes. The
30-minute ceiling would require 72.8 MiB/s. Raising only the background budget cannot
extend the inner HTTP client deadline, which is why the fix had to remove that deadline
rather than make it configurable.

The same arithmetic produces the following transfer-time estimates at constant payload
throughput. These are derived, not observed.

| Throughput | Derived transfer time |
|---|---|
| 10 MiB/s | 3 h 38 m 27 s |
| 25 MiB/s | 1 h 27 m 23 s |
| 50 MiB/s | 43 m 41 s |
| 100 MiB/s | 21 m 51 s |
| 250 MiB/s | 8 m 44 s |
| 500 MiB/s | 4 m 22 s |

The background budget calculation used a 10 MiB/s throughput floor plus five minutes, but
then clamped the result to 30 minutes. For 128 GiB the unclamped budget would exceed three
hours and was reduced to 30 minutes, while the registry HTTP client still ended the active
response at five minutes. That combination is why a large single layer could fail even on
a link that was making steady progress.

### What changed

The remediation stack separates "can a progressing pull finish" from "can an interrupted
pull resume efficiently". The first is about absolute deadlines; the second is about
replaying a prefix from origin.

The changes removed the origin HTTP total deadline and replaced the detached-pull ceiling
with a configurable no-progress timeout, `origin_pull_progress_timeout`. A progressing
body may now run longer than five and 30 minutes, while a body that makes no progress is
canceled after the configured idle interval. Connection establishment, TLS handshake, and
response-header waits remain bounded, so a dead endpoint still fails rather than hanging.

Range support is the second half. The origin client can forward a byte range, validate
the origin's `206` and `Content-Range` response, and serve matching range semantics back
to containerd. A detached chair ingest can expose a verified writer offset, reopen the
origin at that offset, and continue the existing ingest instead of starting over. When
the origin cannot honor the range, Gantry aborts and restarts from byte zero and logs that
decision. Delegated registry authorization stays request-scoped and is never sent to a
peer.

### What is still unmeasured

The design document's final validation item is a measured 128 GiB single-layer pull that
records elapsed time, throughput, retries, origin bytes, and final digest verification.
That item is not checked. The repository has end-to-end compatibility coverage for
pre-header failure and mid-body interruption, and it can stream a large logical body
without allocating a 128 GiB fixture, but the full-size measured run has not been
recorded.

The document also lists accepted constraints rather than open Gantry bugs. Once Gantry
has returned `200` and started a response body, it cannot replace that response with a
`5xx` to force containerd onto the next registry host, so recovery has to complete,
resume, or fail the current fetch. Containerd's `image_pull_progress_timeout` is a
no-progress timeout rather than a total-duration limit, so it does not replace Gantry's
own stall detection. Efficient resume depends on the origin registry honoring byte ranges,
and Gantry must report restart-from-zero behavior when it does not.

## ACR Artifact Streaming

ACR Artifact Streaming changes the startup problem rather than just the distribution
problem. Instead of downloading every layer before a container starts, OverlayBD mounts
the image as a remote block device and reads only the byte ranges the container needs.
That makes the first pod start before the full image is local, which is a different
tradeoff from Gantry's complete-layer distribution.

Gantry and Artifact Streaming solve different parts of the same problem, and the
integration runs both paths at once. Gantry recognizes a streaming manifest, warms the
mount-critical byte ranges, asks a bounded set of chairs to download complete layers, and
keeps reconciling until participating nodes have complete local content. OverlayBD ranges
are served from local complete content, a local partial-range cache, a peer that can serve
the exact range, or the configured ACR endpoint.

### How the streaming path behaves

Artifact Streaming is disabled by default. Enabling the feature makes the range endpoint,
range cache, index prefetcher, and convergence reconciler available, but an image enters
the streaming path only when the registry host ends in `.azurecr.io`, the manifest is
immutable and available locally, and every layer has matching OverlayBD `blob-digest` and
`blob-size` annotations with no external URLs. An ordinary ACR image fails closed to the
existing complete-object path.

The first container start does not wait for the chair cohort. Gantry can serve early
ranges from ACR while complete seeding runs in the background. Each chair downloads its
assigned layer, commits it to the local containerd store, verifies the OCI digest, and
advertises itself as a provider. A per-manifest reconciler repeats dispatch every 30
seconds until the local store contains every layer, and a per-node seeder copies complete
layers from peers.

The defaults shape the observed behavior. There are 64 active chairs, 8 seed chairs, and
3 streaming coordinators per manifest. The partial-range cache holds up to 4 GiB per node
and caches individual requests up to 4 MiB. Mount-critical prefetch warms the first
256 KiB and last 2 MiB of each sized layer, peer lookup is bounded to 250 milliseconds,
and a failed provider is bypassed for up to 3 peers before the range falls back to origin.

### Validation results

The corrected AKS benchmark used run `run-20261016-052815-2f1c1a39` with 1,000 worker
nodes and a fresh 40 GiB image. The source image had 40 generated payload layers, and the
converted manifest exposed 41 total layers per host. The measured results were:

- 1,000 of 1,000 explicitly node-bound pull pods succeeded.
- The first observation of all pod completions was 2 minutes 26 seconds after launch.
- The physical verifier inspected 1,000 unique host content stores.
- All 1,000 hosts contained all 41 layers.
- Fleet totals were 41,000 layer files and 43,325,418,496,000 bytes.
- The first complete physical scan was 18 minutes 1 second after launch.

The scan history is worth reading carefully. The preceding scan at 12 minutes 31 seconds
was incomplete, so actual full-fleet convergence happened somewhere between those two
scans. The 2 minute 26 second figure is the first observation of all pod completions, not
a P95 and not a promise that every layer was local at that moment.

The run's post-pull metric-settlement gate timed out while 231 transfers were still
active. It therefore did not persist final P95 startup or settled origin-traffic
measurements. The pod counts and physical host-store measurements are direct observations
that do not depend on the missing settlement artifact. When you compare this run to the
classic cold-pull campaign, remember that the two measure different things: one measures
when pods started, the other measures when complete content had converged.

## How to reproduce the benchmark

The harness is designed to be run as a unit. Use `deploy.sh` as the only entrypoint for
creating or reconciling the Azure and Kubernetes infrastructure, and do not recreate the
setup from commands copied out of shell history. The full-stack deployment owns the VNet
and subnets, the 1,000-node AKS shape, two Premium ACRs, Private Endpoints and DNS,
diagnostics, immutable branch images, node-side ACR routing, benchmark pull tuning,
bounded Prometheus discovery, Gantry, and the private operator VM.

### Deploy the stack

Start from the example environment and fill in the subscription, deployment name, and
globally unique ACR names. The script rejects existing resources whose topology differs
from the configuration, and it is idempotent when the topology matches.

```bash
cp hack/gantry-benchmark/deploy.env.example hack/gantry-benchmark/deploy.env
# Edit the subscription, deployment name, and globally unique ACR names.

make -C hack/gantry-benchmark deploy-plan
make -C hack/gantry-benchmark deploy
make -C hack/gantry-benchmark deploy-status
```

To resize an existing deployment, update both `AKS_NODE_COUNT` and
`BENCHMARK_NODE_COUNT`, then reconcile the node pool before the full stack.

```bash
make -C hack/gantry-benchmark deploy-scale
make -C hack/gantry-benchmark deploy
```

The stack is preflight-ready by default. Set `START_BENCHMARK=true` in `deploy.env` only
when the same invocation should start the benchmark after every deployment gate passes.

### Choose a measurement mode

`BENCHMARK_MODE=direct` is the dual-ACR benchmark. The baseline phase pulls from its own
dedicated ACR with no proxy, and the Gantry phase routes containerd through the local
Gantry agent to a different dedicated ACR. Direct mode never patches or rolls Gantry, and
it requires Gantry's existing configuration to point the dedicated ACR name at its HTTPS
endpoint.

`BENCHMARK_MODE=proxy` routes both phases through a counting proxy that is the only
measured ACR client. Origin bytes are then a direct measurement. During the Gantry phase,
direct containerd fallback also points at the proxy, so a bypass is still counted.

For source-authoritative Azure measurements, set `BENCHMARK_AZURE_TELEMETRY=true`. That
switches the primary measurements to ACR pull events and Private Endpoint bytes from Azure
telemetry, plus AKS audit-log timestamps for startup latency. It also requires both ACRs
to be reachable only through their approved Private Endpoints, with public access disabled
throughout preparation and measurement.

### Run the lifecycle

All lifecycle commands run on the operator VM under its system-assigned managed identity.
The admin workstation provisions the VM and uses Azure Run Command to start or inspect
its systemd service. The VM has no public IP or inbound security group rules.

```bash
make -C hack/gantry-benchmark operator-vm-provision
```

Then start the full lifecycle and follow it from a separate terminal:

```bash
export AZURE_RESOURCE_GROUP="<resource-group>"
export OPERATOR_VM_NAME="gantry-benchmark-operator"

az vm run-command invoke -g "$AZURE_RESOURCE_GROUP" \
  -n "${OPERATOR_VM_NAME:-gantry-benchmark-operator}" \
  --command-id RunShellScript \
  --scripts 'systemctl start --no-block gantry-benchmark-operator.service'

make -C hack/gantry-benchmark operator-vm-watch
```

The service executes `enable -> prepare -> preflight -> run -> disable`. `prepare`
generates one random payload set, computes one payload SHA-256, and pushes the same
repository, tag, payload, size, and layer count to both registries. Each payload layer
uses a phase-specific destination path, so the OCI digests intentionally differ and
containerd cannot reuse baseline blobs during the Gantry phase on the same nodes.

`preflight` refuses to start if the cluster shape, Gantry readiness, registry
configuration, containerd settings, or telemetry path is wrong. Do not skip it. A
benchmark that starts from a failing preflight produces numbers that describe the broken
setup, not Gantry.

### Watch and collect

For an active Gantry phase, the dedicated monitor redraws every second and shows peer
outcomes (`busy`, `hit`, `stall`, `notfound`, `unavailable`) alongside layer bytes,
throughput, payload percentage, and pod counts. It also shows per-node layer and image
progress grids.

```bash
make -C hack/gantry-benchmark monitor
```

A single non-interactive snapshot is useful in scripts:

```bash
make -C hack/gantry-benchmark monitor MONITOR_ARGS="--once --no-clear"
```

The harness writes per-run artifacts under `tmp/gantry-benchmark/<run-id>/`, including
`baseline.json`, `gantry-cold.json`, `comparison.json`, `comparison.md`, `state.json`,
and the original Gantry configuration. The comparison records the shared payload SHA-256,
ACR origin bytes with an explicit source, Gantry origin pulls and peer-fetch hits, pod
start and finish P50/P95/P100, every node identity used by each phase, and whether the
configured gates passed.

A completed benchmark can return nonzero when a regression gate fails. The artifacts are
still written and cluster routing is still restored. Treat a gate failure as a result to
investigate, not as a reason to delete the run.

### Reproduce the Artifact Streaming variant

Artifact Streaming is opt-in and preserves the classic deployment when unset:

```bash
BENCHMARK_ARTIFACT_STREAMING="true"
```

With that setting, deployment creates a one-node System pool and a separate
`AKS_NODE_COUNT`-node User pool named `stream` by default. The benchmark, Gantry,
containerd tuning, private DNS guard, and pull workload select only the User pool. The
pool is created with AKS Artifact Streaming enabled, and that setting cannot be added to
an existing pool.

The Helm release routes containerd through Gantry on port 5000 and chains Gantry's OCI
upstream through the node-local AKS helper on port 8578. OverlayBD range requests use the
same Gantry listener under `/blobs/`, and fail-open routing falls back to port 8578 so
native Artifact Streaming still works if Gantry is unavailable.

Artifact Streaming currently supports the standalone lifecycle only. The classic
two-phase lifecycle rejects the streaming flag rather than measuring an unconverted image.

```bash
make -C hack/gantry-benchmark operator-vm-run-standalone
make -C hack/gantry-benchmark operator-vm-watch
```

## How to interpret the results

The benchmark is a stress test, not a capacity plan. It deliberately creates a cold
cluster, a single large image, and a simultaneous start, because that combination makes
the registry bottleneck visible. Production clusters rarely look exactly like that:
images arrive over time, some nodes are warm, and pods are scheduled by a controller
rather than all at once. Use the numbers to understand the mechanism and the order of
magnitude, then measure your own environment.

### What the numbers do and do not prove

The 343.6 GB origin floor in the chair-design runs is a direct consequence of eight
seeds and a 40 GiB image. If you change the seed count, the image size, or the number of
selectable chairs, the floor moves with them. If you change the layer count, the seeding
work spreads differently across the chair pool. None of those changes invalidates the
design, but each one changes the arithmetic you should expect.

The latency figures include disk write and unpack time. They are not a measure of Gantry's
network throughput, and they are not comparable to a benchmark that stops the clock when
the last byte arrives. The harness measures to a running container precisely because that
is what a workload experiences, but it also means a faster distribution layer cannot
improve the result past the node's own write and unpack cost.

Peer traffic is not free. The cluster network moves roughly the full image to every node
in the Gantry arm, and the benchmark reports that cost rather than hiding it. The saving
is in registry egress and Private Endpoint capacity, not in total bytes moved. If your
cluster network is the scarce resource, that tradeoff deserves explicit analysis.

### Telemetry versus direct traces

The harness can derive measurements from Azure telemetry or from Gantry's own counters,
and the two have different properties. Telemetry-derived data is aggregated at the
registry and audit-log layer, so it reflects what the registry and control plane actually
observed, but it is subject to sampling, window boundaries, and table ingestion. Direct
counters such as `gantry_origin_bytes_total` are measured at the upstream response-body
boundary and include partial transfers and retries, which makes them closer to the
application path but dependent on the agent being healthy and scraped.

When the two disagree, treat the gap as information. The dataset already notes that ACR
wire bytes run about 9% above the payload Gantry accounts for. Some of that is protocol
and request overhead; some is traffic that never reached the response-body boundary Gantry
counts. Neither number is wrong, but they answer different questions.

### Run-to-run consistency

The chair-design runs are consistent enough to support a conclusion: three independent
1,000-node cold pulls all completed, all landed at or near the eight-copy origin floor,
and all produced P50 values within about seven seconds of each other. That is the evidence
that the result is architectural rather than a lucky run.

The `current-gantry` group is less consistent, with origin traffic ranging from 486.5 GB
to 1.3 TB and durations from 761.2 s to 1,166.0 s. The dataset does not record a single
changed variable that explains the spread. Report the range rather than picking the best
run, and do not average across groups as if they were repetitions of one configuration.

### The historical 300-node reference

The harness README keeps an older 300-node result as a sanity reference, explicitly not
as hard-coded expected output. In that run, the baseline made 1,480 proxy requests and
moved 323.24 GB from ACR, while the Gantry cold phase made 306 proxy requests and moved
4.30 GB. Blob requests fell from 880 to 6, manifest-by-digest requests from 600 to 300,
pod start P50 from 6m29s to 49s, and P95 from 7m20s to 1m6s. The Gantry phase recorded 9
origin pulls and 620 peer hits.

Those numbers come from a different cluster size and an older harness generation, so they
are useful for checking that a new deployment is in the right neighborhood, not for
setting a target. A fresh 1,000-node deployment should be compared against the 1,000-node
tables in this page and against its own baseline phase, not against the 300-node summary.

### Failure modes to watch for

A successful run is one where every pod completes and the origin counters agree with the
expected floor. A run that completes 999 of 1,000 pods is not a 99.9% success story; it is
a failed run with one node to investigate. Check the per-node maps in the artifacts, the
peer outcome counters, and the containerd journal before drawing conclusions.

Watch the difference between ACR traffic and Gantry origin traffic. A large gap can mean
containerd bypassed Gantry, a pod fell back to origin, or the measurement window did not
line up. In `proxy` mode a bypass still counts at the proxy, which is why that mode
remains useful for attribution even though `direct` mode is the cleaner comparison.

Finally, watch the registry itself. The benchmark can show that Gantry reduced origin
traffic, but it cannot show that the registry would have stayed healthy under the full
fan-out. A registry that rate-limits or saturates its Private Endpoint may produce a
baseline that is slower than the hardware deserves, which flatters the Gantry phase. The
harness runs the baseline first and validates the telemetry path for this reason, but the
caveat remains.

## Related

- [Distribute Container Images with Gantry]({{< relref "guides/gantry" >}}) explains the
  agent, the mirror path, the distributed index, and the chair model in operational
  detail.
- [Trace Gantry Pulls]({{< relref "guides/gantry-tracing" >}}) shows how to follow a
  single pull across the mirror, peer, and origin hops when a benchmark run raises
  questions about where time went.
- `hack/gantry-benchmark/README.md` documents the harness, deployment environment, and
  artifact layout in full.
- `hack/gantry-benchmark/RESULTS.md` contains the consolidated development, 1,000-node,
  and 2,000-node campaign results.
- `designs/gantry-cold-pull-explained.md` and
  `designs/gantry-cold-pull-rotating-chair.md` describe the cold-start and chair design.
- `designs/gantry-128gib-single-layer-pulls.md` tracks the large-single-layer limits and
  their remediation.
- `designs/gantry-acr-artifact-streaming.md` describes the Artifact Streaming
  integration, its defaults, and its validation run.

