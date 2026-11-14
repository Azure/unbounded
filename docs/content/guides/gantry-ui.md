---
title: "Gantry Web Dashboard"
weight: 13
description: "Watch image distribution and Pod lifecycle across a Flex worker pool."
---

The standalone Gantry dashboard discovers tagged images in an Azure Container
Registry and creates digest-pinned demo DaemonSets on Linux Flex workers. It
does not modify the network-topology frontend.

## Run Locally

### On-demand GHCR build and Helm installation

This workflow is independent of Unbounded CI. It uses Docker Buildx or Podman
to build the embedded Go dashboard, pushes to GHCR, resolves the pushed digest,
then installs/upgrades only the dashboard Helm release. It does not install or
upgrade Gantry, change Azure permissions, delete demo workloads, or start a
public endpoint.

Prerequisites:

- Docker with Buildx, or Podman, running locally; Helm 3 and kubectl.
- A GHCR login with package write access to your chosen repository. Use your
  engine's normal login with `--password-stdin`; never put tokens in values files.
- An existing Unbounded cluster with Gantry and Flex workers.
- Azure Workload Identity enabled and federated identities for the chart's
  service accounts. The dashboard identity needs ACR catalog/read permissions;
  enabling generation requires a builder identity with repository pull/push.
- A public GHCR image, or existing image-pull Secrets in the demo namespace.
  GHCR packages may initially be private. ACR authentication and GHCR pull
  authentication are separate.

Configure a project-local values file using the chart's values as a reference:

```yaml
registry: example.azurecr.io
nodePool: flexpool
azure:
  clientID: YOUR_DASHBOARD_IDENTITY_CLIENT_ID
builder:
  enabled: true
  azure:
    clientID: YOUR_BUILDER_IDENTITY_CLIENT_ID
  base: example.azurecr.io/demo/base@sha256:YOUR_BASE_DIGEST
reclaimer:
  enabled: true
```

The base must be an existing small Linux image of the worker architecture in
the configured ACR. Helpers come from the GHCR dashboard image; generated demo
images still go to ACR. For private GHCR packages, configure both dashboard
`imagePullSecrets` and `helperImagePullSecret` with existing Secret names.

Run from the repository root:

```sh
bash hack/scripts/gantry-ui-deploy.sh \
  --kubeconfig ./cluster.kubeconfig \
  --context my-unbounded-context \
  --values ./tmp/gantry-ui-values.yaml \
  --image ghcr.io/vpatelsj/gantry-ui
```

The command requires an explicit context to avoid deploying to an unintended
cluster. It selects Docker first, then Podman, or accepts `--engine podman`.
The default image platform is `linux/amd64`; use `--platform linux/arm64` for
ARM system nodes and workers. The command sets the chart's architecture to
match the single-platform helper image, including its worker selection.
Cross-platform builds require engine emulation support or a native
builder. Helm NOTES prints the port-forward command.

The chart uses host networking and binds to node loopback only. Keep one
dashboard per system node/port, and use a dedicated demo namespace per release:
the dashboard owns all labeled demos in its namespace and uses a fixed reclaim
coordinator name. Federated identity subjects must match the service-account
names rendered by Helm. The command does not create Azure federated credentials.

The current hand-applied test deployment is not automatically adopted by Helm.
Use a new namespace/release and a different system node, or deliberately migrate
the old deployment before installing. The command never deletes or takes
ownership of an existing manual installation. Chart history remains in memory;
upgrading the dashboard restarts it and clears observed chart history.

Sign in to Azure with `az login`. The selected Azure identity needs permission
to enumerate repositories, tags, manifests, and image configuration blobs in
the registry. Authentication uses `DefaultAzureCredential`; environment and
workload identities are supported too. Credentials remain on the server and
are never sent to the browser.

Build and run:

```sh
make gantry-ui-build
kubectl --kubeconfig ./cluster.kubeconfig create namespace gantry-demo
bin/gantry-ui \
  --kubeconfig ./cluster.kubeconfig \
  --registry example.azurecr.io \
  --namespace gantry-demo \
  --node-pool flexpool
```

Open `http://127.0.0.1:8088`. Starting the server is read-only. **Selecting a
new image creates a DaemonSet and runs the image's default command on every
matching Flex worker.** Pods have a 512 MiB memory limit, request 10m CPU and
32 MiB memory, have no API token, and cannot gain privileges. This is a demo
workload runner, not a production application deployment system.

Use an isolated demo namespace. Worker selection requires the configured
agentpool, `nodepool-type=FlexNodes`, Linux, and the configured architecture.
The native AKS system node is not targeted. Set `--architecture arm64` for
ARM64 workers. Single-platform images of another architecture are marked
incompatible; multi-platform indexes resolve to the configured Linux platform.
The streaming badge is based on the resolved manifest's OverlayBD annotations,
not the repository name or the presence of a conversion operation.

The Kubernetes identity running the server needs:

| Scope | Resources | Verbs |
| --- | --- | --- |
| Cluster | `nodes` | `list` |
| Demo namespace | `daemonsets.apps` | `get`, `list`, `create` |
| Demo namespace | `pods`, `events` | `list` |
| Gantry namespace | `pods` | `list` |
| Gantry namespace, proxy mode only | `pods/proxy` | `get` |

Use a kubeconfig or an appropriately scoped in-cluster service account. The
server does not create namespaces, change registry roles, upgrade agents,
clear caches, delete existing workloads, or replace conflicting DaemonSets.

## Cluster Overview and Worker Views

The full-width workspace uses a searchable **Choose image / Change image**
picker instead of a permanent registry sidebar. **Gantry cluster activity**
stays above four simultaneous panels: image distribution, Pod lifecycle,
fleet readiness, and disk usage. The panels form a two-by-two grid on desktop
with compact 40-column heatmaps and stack on mobile. Selecting a worker
highlights it across all three heatmaps and opens its details below without hiding the panels or cluster
activity. **Clear selection**, a second click on that worker, or Escape
clears the selection.

Cluster activity shows live origin/peer receive rates, active layer transfers,
reported failures, and cumulative origin/peer received bytes observed during
the selected run. The scope is **cluster-wide traffic observed during the
selected run**, not exact traffic for the selected image.

Gantry aggregates cover all images on all discovered Gantry agents, including
native agents outside the selected Flex pool. The worker grid remains scoped
to the selected image deployment. Each agent contributes once, even with
multiple viewers or image selections. Peer uploads are not added to peer
downloads: these are received layer payload bytes, not wire-level TCP/TLS or
coordination traffic. Retries count as network traffic. Peer traffic share is
not a cache-hit rate or a claim of saved bandwidth.

Rates require two fresh observations from the same agent Pod. Samples older
than ten seconds, unreachable agents, and counter resets do not turn into
zero-rate samples or bandwidth spikes. Reporting coverage is shown explicitly;
partial aggregates sum known agents only. Missing source gauges stay
unavailable until the agents are upgraded.

The bandwidth and Ready-worker charts span the selected deployment's creation
through its latest observation, including runs longer than a minute. Their
shared **From / To** sliders zoom the time range; **Reset to full run** restores
it. Dashed baseline sections mark missing observations and collection gaps,
not zero traffic or zero Ready workers. Collection begins at the first
observation: earlier telemetry and traffic totals are not reconstructed.
History is held by the server, so browser refresh keeps collected samples
without storing large histories in the browser. History does not survive a
dashboard server restart.

**Image distribution** displays one cell per worker, with one overall
byte-availability bar. Clicking a cell opens the layer breakdown. Committed
containerd content, active ingest prefixes, and cached streaming ranges are
observed directly. Overlapping ranges count once, including overlap with an
in-flight complete-layer ingest. These are stored layer bytes, not unpacked
filesystem sizes. Already cached layers count immediately; this is not a
cache-clearing or cold-pull benchmark.

**Pod lifecycle** uses the same worker ordering and shows scheduling,
preparation, running, readiness, successful completion, and failures. The
selected worker shows observed timestamps and Kubernetes events. Kubernetes
does not expose a distinct unpack percentage, so preparation is labeled as a
combined image/filesystem/container phase rather than inventing unpack
progress. Events may expire; missing history is explicitly left unavailable.

**Fleet readiness** shows current Ready Pods and 50%, 95%, and 100% milestones,
measured from deployment creation using current Pods' observed Ready
timestamps. A missing milestone remains unavailable. The chart uses collected
Ready-worker counts rather than inventing telemetry before collection began.

**Disk usage** shows the filesystem backing Gantry on each worker. Its usage
includes all images and workloads, not just the selected image; percentages
exclude filesystem-reserved space. Missing filesystem observations remain
unknown. Worker details include used, available, total, and reserved bytes.

The selected worker's **Startup waterfall** shows scheduling, waiting-to-pull,
image preparation, container start, and Pod readiness waterfall. Pod milestones
use observed status timestamps. Pull stages use events fenced to that exact
Pod UID, not events from a replacement Pod on the same node. Event-based
durations are approximate. A kubelet-reported successful pull duration is
shown separately and can still be displayed when the earlier Pulling event
has expired; its plotted start position is explicitly described as anchored
to the Pulled event.

Image preparation includes runtime pull/unpack work, not just network transfer.
Aggregated Pulling events can report attempt counts without preserving every
attempt's timing. Exact per-attempt durations and retry backoffs are not
invented. Cache hits are labeled only when the runtime explicitly reports
that the image was already present. Container restarts are shown separately,
and restarted containers are not presented as reconstructed first startups.
Missing or unfinished stages stay unavailable rather than becoming zero.

Worker details retain the observed layer breakdown, lifecycle events, startup
waterfall, disk usage, and node-wide receive rates. Build/push time is not part
of Pod startup latency.

For ACR Artifact Streaming, a Pod can become Ready before all bytes are
available. Full-image completion is neither a readiness prerequisite nor
guaranteed. The range cache is not durable across Gantry restarts, so reported
coverage can decrease. Peer/origin traffic rates are node-wide, may include
other images and retries, and are not used to calculate byte percentages.
Per-layer peer/origin attribution is not currently recorded.

## Generate Images

Worker heatmaps default to 25 columns and sort independently by status, with
errors and attention-needed workers first. Disk groups sort fullest first.
Charts and observed transfer totals freeze when all workers are Ready or
Completed; live Pod and storage observations continue refreshing.

**Generate & pull** creates random payload layers and publishes them before
starting a digest-pinned demo DaemonSet. **Enable ACR streaming** is unchecked
by default, preserving ordinary OCI generation.

Check it to request per-image ACR Artifact Streaming conversion. The build
shows conversion progress and waits for success, then verifies the artifact's
source digest, OverlayBD annotations, and Linux architecture before tagging
and deploying the streaming digest. Conversion failures fail the build without
starting a workload or falling back to OCI.

Streaming generation requires a Premium ACR, a builder identity with repository
pull/push permissions (such as `AcrPush` on registries using legacy RBAC), and
workers already configured for Gantry Artifact Streaming. It does not enable
streaming on workers or change registry-wide/repository auto-conversion policy.
The original OCI image remains under the build's `-oci` tag; the build tag
identifies the verified streaming artifact. Conversion adds registry storage
and service processing. The existing build deadline bounds the entire process.

## Reclaim Disk Space

Disk reclamation is disabled unless `--reclaimer-image` and
`--reclaimer-service-account` are configured. Use the same digest-pinned
dashboard image and a dedicated `gantry-reclaimer` identity. The optional
[reclaimer RBAC](../../../deploy/gantry-ui/reclaimer-rbac.yaml) targets the
`gantry-demo` namespace and the `gantry-ui` dashboard service account; adjust
both deliberately for other installations.

**Reclaim disk space** requires typing `DELETE ALL DEMOS`. It deletes all
validated dashboard-managed demo DaemonSets in the dashboard namespace,
waits for their owned Pods to terminate without force deletion, and prunes
unused images on every Linux Flex worker in the configured pool and
architecture. Other namespaces and non-dashboard DaemonSets are not deleted.
Registry images are never removed. Demo generation and deployment are blocked
while reclamation runs, and an active image build prevents reclamation.

The durable coordinator Job survives browser reloads and dashboard restarts.
The UI shows worker progress, failures, and observed filesystem bytes
recovered. A failed operation is not reported as full reclamation. Retry
removes the previous finished coordinator with background garbage collection.
Its finished worker Jobs remain fenced to the old coordinator UID while
Kubernetes collects them. If coordinator removal is still pending, the dialog
automatically retries for up to two minutes; closing it cancels further retries.

Use **Dismiss** to hide the status panel without stopping cleanup. It stays
hidden across reloads for that operation; **Show cleanup status** restores
it. A new cleanup operation shows its status automatically.

Node cleanup Jobs run as root with privileged access to the containerd
socket and a read-only mount of Gantry's data directory. They have no
Kubernetes service-account token. The coordinator itself is unprivileged,
but its ability to create these Jobs is powerful: use a dedicated trusted
demo namespace, restrict who can access the loopback dashboard/tunnel, and
do not grant this identity to unrelated workloads.

All container references, pinned images, sandbox images, and uncertain
runtime leases remain protected. Removing an unused image record does not
guarantee immediate removal of shared layers or leased data. The operation
is not a disk wipe, does not remove live containers, and does not restart
containerd or snapshotters.

Pod deletion can precede containerd's removal of stopped container records.
Workers retry pruning the deleted demos' images until those runtime references
clear, within the worker's two-minute cleanup deadline. A remaining protected
demo image is reported as a failure, not successful reclamation. Unrelated
container references remain protected without waiting for their removal.

## Agent Compatibility and Collection

The **Node storage** tab shows a filesystem-usage heatmap for the workers.
Hover a cell for used bytes, space left, and capacity; select it for the full
breakdown, reserved space, and the observation timestamp. Zooming the grid
shows each worker's remaining space. Colors distinguish below 85% usage,
85-95%, and 95-100%.

These are real filesystem readings from the existing host-mounted directory
backing Gantry's identity and range cache. They include every file on that
filesystem, not just Gantry data or the selected image, and do not inventory
other mounted disks. Space left excludes filesystem-reserved blocks; the
usage percentage is used bytes divided by used plus available bytes.
Storage requires upgraded agents. Missing first observations stay unknown;
agent timeouts retain the last reading without reachability warnings.

If unused images accumulate and node events report `ImageGCFailed` for
`io.containerd.snapshotter.v1.gantry-overlaybd`, the proxy snapshotter must
export its actual host data root. Use a node configurator with the separate
snapshotter filesystem-reporting fix, which writes
`proxy_plugins.gantry-overlaybd.exports.root` from
`overlaybdConfig.snapshotterRoot`, rather than relying on containerd's
in-process snapshotter directory default. The dashboard does not install that
fix. Applying a changed registration
requires restarting containerd. Update the OnDelete configurator only on
drained nodes; do not replace snapshotter Pods supporting live workloads.

Live byte availability requires Gantry agents built with the read-only
`GET /v1/progress?digest=sha256:...,...` operations endpoint on port 9095.
Released agents without this endpoint report **unknown** byte progress, with
an upgrade-required explanation in server logs and the API; Pod lifecycle remains observable. Racer
does not currently implement this endpoint. This feature does not fix the
separately tracked private-origin streaming authentication or chair-wait
follow-ups.

The source-specific activity and continuous bandwidth counters require agents
with `gantry_active_transfers{source,kind}` and
`gantry_transfer_bytes_total{source,kind}`. Bytes update while response bodies
are read, and active counts span request setup through a terminal body read
or Close. They include complete pulls and on-demand range fetches. The legacy
terminal-byte and pull-success arithmetic remains unchanged.
`GET /v1/progress` without a digest query returns activity without accessing
containerd content; the dashboard uses it for agents outside the worker grid.

Browser updates use server-sent events. A first snapshot establishes the grid;
subsequent worker deltas are flushed in batches at most every 100 milliseconds
as agent observations arrive, without waiting for the slowest worker. Multiple
viewers share one collector per deployment. After the first viewer starts
collection, it continues with no viewers until the deployment is deleted or
evicted from the server cache. Node details use the same incrementally updated cache,
rather than sending every layer for every worker to every browser. Collection concurrency is bounded to 256
requests, individual agent requests to three seconds, and a collection pass
to 25 seconds. The one-second default interval is a minimum between collection
starts, not a promise of per-second observations on every worker. There is no
extra interval wait after a collection that already exceeded that minimum.
When an agent is unreachable or returns invalid progress, the grid and layer
details retain its last successfully reported byte state until another valid
observation arrives. Its original observation timestamp is preserved, without
showing agent-reachability warnings in the dashboard. Diagnostics remain in
server logs and the API. Workers that have never reported byte
telemetry remain unknown. Fresh observations can decrease coverage, for example
after an agent restart. Pod lifecycle observations continue updating independently.
Very short pulls may finish between observations.

The browser merges worker deltas and batches rendering on animation frames.
Unchanged grid cells are not rewritten. Selected-worker details refresh only
when that worker has a new observation. Retained event history is fetched on
selection or switching to lifecycle/startup views and refreshed every five
seconds while those views are open, not for every byte update.

Local mode uses the Kubernetes Pod proxy, with bounded client QPS. For large
clusters, prefer running the server where Gantry Pod IPs are reachable and
passing `--direct-agents`. Direct collection avoids one API-server proxy
request per worker. Ensure network policy permits access to the agents'
operations port. No browser contacts agents or Azure directly.

When running with host networking, the dashboard can use `dnsPolicy: Default`
to resolve ACR through the native node's resolver. Kubernetes API access uses
the in-cluster address, and direct agent access uses Pod IPs; neither requires
cluster DNS names. Registry requests and agent polling use separate connection
pools so a thousand agent endpoints do not evict ACR's reusable connections.
Registry error messages retain the failure but strip URL query strings,
including signed blob-access parameters.

The server binds only to loopback and rejects cross-origin and non-loopback
Host headers. For a remote server, use an authenticated SSH tunnel; for a
server inside a Pod, use `kubectl port-forward`. Do not expose this
workload-creation interface through an unauthenticated public proxy.

## Resume and Cleanup

DaemonSet identity is derived from the registry, repository, and image digest,
so selecting the same image again reuses its deployment. Browser reloads and
server restarts discover the existing DaemonSets and read current cluster
state. The browser remembers its selected deployment, and its URL can be
bookmarked. Neither reload nor reconnect creates a new deployment.

Demo workloads remain running when the UI closes. To remove one deliberately:

```sh
kubectl --kubeconfig ./cluster.kubeconfig -n gantry-demo delete daemonset GANTRY_DEMO_NAME
```

## Generate Images On Demand

Image generation is disabled unless builder settings are supplied. When enabled,
**Generate & pull** asks for the number of payload layers and MB per layer,
creates an in-cluster build Job, pushes the completed ordinary OCI image to ACR,
and then creates its digest-pinned Flex DaemonSet. Checking **Enable ACR
streaming** adds verified per-image conversion before the DaemonSet is created.

Limits are 100 payload layers and 100 GB (100,000 MB) total random payload. MB means
1,000,000 bytes. Each payload layer contains independent cryptographically
random bytes, stored without compression; tar/gzip headers and the small runtime
base add a little overhead. The base is an additional layer. Builds stream one
temporary layer at a time rather than holding the image in memory. Generated
Pods run a shell sleep loop. For example, 100 layers of 1,000 MB each produce
100 GB of payload. Workers need sufficient disk for both the stored layers and
unpacked files, roughly twice the payload for these incompressible ordinary OCI
images. A 100 GB payload will not fit on a 128 GB worker disk.

Configure the server with:

```sh
--builder-image example.azurecr.io/gantry-ui@sha256:BUILDER_DIGEST \
--builder-base example.azurecr.io/demo/base@sha256:BASE_DIGEST \
--builder-service-account gantry-image-builder \
--builder-repository gantry-demo/generated \
--builder-concurrency 1
```

Both images must be digest-pinned. The runtime base must stay in the configured
ACR; the builder and reclaimer can come from GHCR. Set
`--helper-image-pull-secret` for private helper images; that existing Secret is
attached to builder, coordinator, and worker Jobs. The builder image must
contain `/unbounded/bin/gantry-ui`; the single-platform Linux base must match the
configured architecture and contain `/bin/sh` and `sleep`. Use a small BusyBox
base mirrored into ACR. Build Jobs target native AKS system nodes of that
architecture, not the Flex workers.

Use one dashboard replica. Concurrent requests and dashboard restarts preserve
the configured active-Job limit; separate dashboard replicas do not share an
admission lock. Jobs have no retries, a six-hour deadline, and a one-day
completed-Job TTL. Each uses a non-root identity, read-only root filesystem,
bounded resources, and a writable temporary volume sized for one payload layer
plus packaging and log headroom. The volume limit and ephemeral-storage request
and limit scale together, including for a single 100 GB layer; the native builder
node must have that space available. Failed builds show the sanitized error
cause and Job name; use `kubectl logs job/NAME` in the demo namespace for full
diagnostics.
Already-uploaded blobs may remain after a failed build.

The dashboard service account additionally needs `get`, `list`, and `create`
on `jobs.batch`, and `get` on `pods/log`, in the demo namespace. Use a separate
builder service account with `create` on `daemonsets.apps` in that namespace.
Enable AKS Workload Identity, annotate the builder service account with a
dedicated managed-identity client ID, and federate its exact service-account
subject. Give that identity registry-scoped `AcrPush`; leave the dashboard's
Azure identity read-only. Workload-identity tokens stay inside the build Pod.

The UI shows observed Job and layer generation/upload progress and resumes an
active build after reload. The builder creates the workload after publication;
reading build status never creates workloads. Completed images and demo
DaemonSets remain until deliberately removed, regardless of Job TTL or closing
the browser.
