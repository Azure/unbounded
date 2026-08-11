# Gantry ACR benchmark

## Repeatable full-stack deployment

Use `deploy.sh` as the only entrypoint for creating or reconciling the Azure and
Kubernetes infrastructure needed by this benchmark. Do not recreate the setup
from commands copied out of the playbook or shell history.

```bash
cp hack/gantry-benchmark/deploy.env.example hack/gantry-benchmark/deploy.env
# Edit the subscription, deployment name, and globally unique ACR names.

make -C hack/gantry-benchmark deploy-plan
make -C hack/gantry-benchmark deploy
make -C hack/gantry-benchmark deploy-status
```

Run these targets from the current clean checkout. `deploy` creates a small
source-carrier context from the exact committed revision with `git archive`, so
do not create or reuse a detached deployment worktree. This avoids uploading
local `tmp/` caches and ensures infrastructure uses the same deployment code as
the current commit.

The script is idempotent and rejects existing resources whose topology differs
from the config. It owns the VNet/subnets, 1000-node AKS shape, two Premium ACRs,
dedicated data endpoints, Private Endpoints/DNS, diagnostics, immutable branch
images, containerd settings, deterministic node-side ACR routing, bounded
Prometheus discovery, Gantry, and the operator VM. It leaves the stack
preflight-ready by default; set `START_BENCHMARK=true` in `deploy.env` only when
the same invocation should start the benchmark after every deployment gate
passes.

The deployment config contains names and topology only. Credentials remain in
Azure managed identities and short-lived ACR tokens.

The workstation needs one valid Azure management-plane login before invoking
`deploy.sh`; the script never invokes `az login`, `az acr login`, or workstation
Podman. It publishes only the revision-labelled source carrier through an ACR
Task, creates Private Endpoints and disables public registry access, then
bootstraps the operator VM. Gantry and pull-probe images are built and
pushed from that VM with its managed identity over Private Link.

The sections below document benchmark behavior and direct lifecycle control.
They do not replace `deploy.sh`; commands that assume an existing cluster are
for diagnosis or manual operation after full-stack deployment succeeds.

When invoking the benchmark tool directly, it expects:

- Exactly `BENCHMARK_NODE_COUNT` Ready, schedulable `linux/amd64` nodes.
- A Ready `gantry-system/gantry` DaemonSet on every benchmark node.
- A dedicated Gantry ACR listed exactly once in Gantry's
  `upstream_registries` configuration, plus a different baseline ACR.
- kube-prometheus-stack, the Prometheus Operator CRDs, kube-state-metrics, and
  a Grafana dashboard sidecar. The workflow installs benchmark-owned
  PodMonitors for Gantry and, in proxy mode, the proxy.
- Containerd configured to read `/etc/containerd/certs.d`.
- Containerd metrics listening on `0.0.0.0:10257` and debug logging enabled.
  The managed node template in this repository configures both. Preflight
  refuses to run without a containerd scrape from every target node, and the
  node-observer DaemonSet fails if effective containerd log level is not debug.
- An operator VM in the AKS VNet. The VM runs every benchmark command,
  builds and pushes both images, queries Azure telemetry, and stores artifacts.
- Cluster permission to create privileged hostPath DaemonSets. Proxy mode also
  patches the Gantry ConfigMap.

For source-authoritative Azure measurements, set
`BENCHMARK_AZURE_TELEMETRY=true`. This additionally requires:

- Both ACRs reachable only through their own approved Private Endpoints before
  operator and benchmark validation, with public access disabled throughout
  image preparation and measurement. The operator VM reaches both ACRs over
  Private Link while preparing and measuring the images.
- A Log Analytics workspace receiving `ContainerRegistryRepositoryEvents` and
  `AKSAuditAdmin` in resource-specific tables.
- Azure resource IDs for both ACRs, both Private Endpoints, and AKS.
- Azure CLI authentication with read access to those resources and the
  workspace.

This is test-cluster tooling. The baseline deliberately transfers
`BENCHMARK_NODE_COUNT * BENCHMARK_IMAGE_SIZE_MIB` from ACR.

## What it runs

In direct mode, `prepare` generates one random payload set, computes one payload
SHA-256, and pushes the same repository and tag to the baseline and Gantry ACRs.
Both images have the same payload bytes, size, and layer count. Each payload
layer uses a phase-specific destination path, so the OCI digests intentionally
differ and containerd cannot reuse baseline layer blobs during the Gantry phase
on the same nodes. Both digest references and the shared payload SHA are pinned
in benchmark state and results.

| Mode | Baseline | Gantry cold |
| --- | --- | --- |
| `proxy` | containerd -> counting proxy -> ACR | containerd -> local Gantry -> peer or counting proxy -> ACR |
| `direct` | containerd -> baseline ACR | containerd -> local Gantry -> peer or Gantry ACR |

The proxy is the only measured ACR client in both phases. During the Gantry
phase, direct containerd fallback also points at the proxy and is reported as
`client_class="containerd"`. Gantry-origin traffic is reported as
`client_class="gantry"`.

Direct mode deploys no proxy and never patches Gantry. Gantry's existing config
must point the dedicated Gantry ACR name at its HTTPS endpoint. Baseline origin bytes
are analytic (completed pods times image size); Gantry-cold origin bytes come
from `gantry_origin_bytes_total`, measured at the upstream response-body
boundary and including partial transfers and retries. Preflight requires that
metric on every target Gantry pod.

With Azure telemetry enabled, the primary measurements are instead:

- Image pulls: successful `ContainerRegistryRepositoryEvents` rows from the
  phase's ACR for the exact manifest digest.
- ACR bytes: the phase's Private Endpoint `PEBytesIn` over an isolated
  whole-minute window.
- Startup latency: `AKSAuditAdmin` pod create, binding, and first started-status
  receipt timestamps.
- Peer bytes: per-pod `gantry_peer_serve_bytes_total{kind}` deltas.

Preflight validates the audit path end to end by creating a unique ConfigMap
and requiring its create event to appear in `AKSAuditAdmin`. Merely being able
to query an empty table is not sufficient.

The workload is a Kubernetes Job with 300 completions and 300-way
parallelism. Required hostname anti-affinity uses the run ID and phase, so the
workflow proves that exactly one pull pod ran on each of 300 distinct nodes.
Each pull container remains Running for 15 seconds after image startup so
`AKSAuditAdmin` reliably captures a running status transition. The startup
metric ends at that transition; the hold is not included in startup latency.

There is no warm-cache phase and no containerd content purge. Separate ACR
hostnames alone do not isolate containerd's digest-addressed cache; the
phase-specific layer paths provide that isolation while preserving identical
payload bytes.

## Performance attribution artifacts

`enable` installs a benchmark-owned node-observer DaemonSet on every target
node. It exposes node-exporter metrics, provides a Prometheus target for the
host containerd metrics endpoint, and streams a filtered subset of the host
containerd journal. Preflight requires both `node_uname_info` and
`containerd_build_info_total` from every observer pod.

Each phase writes `<phase>-performance.json` with the unmodified Prometheus
range-query envelopes at 10-second resolution for:

- host disk bytes, I/O busy time, CPU, memory, and network bytes/errors;
- containerd process and built-in metrics;
- per-Gantry-pod peer outcomes and durations;
- exact latest peer busy/stall event timestamps plus interval counts;
- per-Gantry-pod DHT outcomes and durations;
- mirror bytes and final response-completion timestamps; and
- stream-completion-to-containerd-inventory observation distributions and
  latest measured durations.

The same artifact includes the observer-pod-to-node map, raw filtered
containerd journal, and phase-bounded structured events for `PullImage`,
successful pull completion, no-progress cancellation, `layer unpacked`, and
`image unpacked`. Capture fails unless every observer pod has an event in the
phase window. Containerd's
`layer unpacked` duration spans fetch, apply, and snapshot commit; it is not a
pure filesystem-write duration. Host disk metrics provide the independent
filesystem pressure signal during that span.

Phase JSON also includes:

- exact per-workload-pod container start/finish timestamps and node names; and
- per-Gantry-pod counter deltas and phase-local timestamp gauges.

These two node maps are the supported join key for correlating peer busy/stall
events, DHT latency, final layer-response time, containerd commit observation,
containerd unpack logs, host resource use, and workload startup latency.

## Lifecycle

All lifecycle commands run on the operator VM under its system-assigned managed
identity. The admin workstation provisions the VM and uses repository targets
to start or inspect its systemd service. The VM has one static public IP for
key-only SSH on TCP 50001. Its NSG limits that port to
`OPERATOR_SSH_SOURCE_CIDR`, which defaults to the deploying workstation's
current public IPv4 `/32`. A separate subnet NAT gateway supplies outbound
package, GitHub, and Azure API access.

Bootstrap writes the VM-only configuration to `/etc/gantry-benchmark/env` and
fetches an admin kubeconfig using managed identity. The service executes:

```text
enable -> prepare -> preflight -> run -> disable
```

Provision/bootstrap from the admin workstation:

```bash
make -C hack/gantry-benchmark operator-vm-provision
```

Provisioning regenerates `tmp/<resource-group>/ssh-config`, pins the current VM
host key, and verifies a real SSH connection. Open a shell without editing
`~/.ssh/config`:

```bash
make -C hack/gantry-benchmark operator-vm-ssh
```

If the workstation's public IPv4 changes while the Azure stack remains in
place, refresh the NSG source, authorized key, host key, and local config with:

```bash
make -C hack/gantry-benchmark operator-vm-ssh-configure
```

Start the full VM lifecycle over SSH:

```bash
make -C hack/gantry-benchmark operator-vm-start
```

Follow progress from the workstation in a separate terminal:

```bash
make -C hack/gantry-benchmark operator-vm-watch
```

All post-bootstrap lifecycle and status targets use SSH on TCP 50001. Azure Run
Command is reserved for initial bootstrap and explicit SSH recovery. Bootstrap
disables the port-22 socket and fails unless sshd's effective configuration
contains only port 50001.

To build and push a fresh 40 GiB/40-layer image pair without running either
pull phase, start the image-only lifecycle from the workstation:

```bash
make -C hack/gantry-benchmark operator-vm-prepare
make -C hack/gantry-benchmark operator-vm-watch
```

The dedicated operator service runs `enable`, `prepare`, and `disable`. Payload
generation, both image builds, and both ACR pushes happen on the private
operator VM. It exits before `preflight`, never creates a pull Job, and retains
the digest-pinned image references and shared payload SHA in the run artifacts.

The live view reports the lifecycle stage and start time, immutable run shape,
payload files/bytes/percentage, each baseline and Gantry-cold image reference,
image size, layer count, build/push state, completed digest, active image
operation and elapsed time, per-phase Kubernetes Job completion, Gantry
readiness, recent logs, and the final report. Podman 4.9 does not expose a
machine-readable live push byte percentage, so the view reports that limitation
instead of estimating it. Use `operator-vm-status` for a single snapshot.
Override the refresh cadence with `WATCH_INTERVAL_SECONDS` (default 5).

For the Gantry pull phase, use the dedicated live monitor from the workstation:

```bash
make -C hack/gantry-benchmark monitor
```

It redraws every second and shows per-phase-minute peer outcomes (`busy`,
`hit`, `stall`, `notfound`, `unavailable`) alongside layer bytes, aggregate
and per-node throughput, cumulative payload percentage, and live Pod counts for
completed, running, creating, image-pull failures, and failed Pods. Pod counts
come from one Kubernetes list followed by watch events rather than polling all
1000 Pod objects.

Two node-paged grids show the current image in detail. The layer-by-node grid
uses `.` for pending and `0-9/A-Z` for the phase minute when Gantry finished
writing that layer to the node's containerd client (`Z` means minute 35 or
later). The image-by-node grid uses `.` for not started, `0` for started,
`1-9` for unpacked-layer deciles, and `#` after containerd reports the image
unpacked.

The same node page shows one-minute CPU utilization averaged across cores and
current memory-used percentage (`1 - MemAvailable / MemTotal`) for every node
as `0-9` deciles, plus fleet p50, p95, and maximum values. These resource
values advance at the 10-second Prometheus scrape cadence. Node columns are
sorted and identified by their three-character
suffix. The default page width follows the terminal, bounded to 16-96 nodes.
Select another page or a fixed width with, for example:

```bash
make -C hack/gantry-benchmark monitor \
  MONITOR_ARGS="--node-page 2 --nodes-per-page 64"
```

The header uses ANSI emphasis on a TTY and remains plain when redirected or
piped. The monitor uses one server-side aggregated Prometheus range query per
display refresh and one exact-image progress query every 10 seconds. Prometheus
scrapes at 10-second cadence, so the screen updates each second while grid and
counter values advance at scrape cadence.
Use `MONITOR_ARGS="--once --no-clear"` for a single non-interactive snapshot.

### Gantry CPU profiles

Benchmark deployments enable Go pprof on each Gantry pod at the loopback-only
address `127.0.0.1:6060`. It is not declared as a pod port and is reachable
from the workstation only through `kubectl port-forward`. During an active
Gantry-cold phase, capture concurrent CPU profiles from the three nodes with
the highest one-minute CPU utilization:

```bash
make -C hack/gantry-benchmark profile-gantry
```

Override the sample duration and node count with `GANTRY_PPROF_SECONDS` and
`GANTRY_PPROF_COUNT`. The command stores individual and merged protobuf
profiles plus text reports under `tmp/gantry-pprof/<run>-<timestamp>/` and
prints the merged top functions. Open the merged profile interactively with
the `go tool pprof -http=...` command printed at completion.

CPU profiling adds runtime overhead to the selected pods. Treat a profiled
run as diagnostic and do not use it for benchmark comparisons. The sampler
annotates the active Job with the capture timestamp, duration, requested pod
count, and successfully captured pod count so the diagnostic status remains
visible after the run finishes. If one target fails, two or more valid profiles
are still merged and the failed target's port-forward log is retained.

## Reusable Gantry image pool

For a one-off Gantry-only run that creates a brand-new random 40 GiB image
inside the lifecycle, use the retained baseline without involving the image
pool:

```bash
AZURE_RESOURCE_GROUP=vapa-gantry-benchmark1 \
OPERATOR_VM_NAME=gantry-benchmark-operator \
GANTRY_ONLY_BASELINE_RUN_ID=run-20260806-205719-51c38730 \
make -C hack/gantry-benchmark operator-vm-run-fresh
```

Gantry-only benchmarks can consume prebuilt images instead of generating,
building, and pushing 40 GiB during every benchmark lifecycle. Start a batch
on the operator VM from the workstation:

```bash
AZURE_RESOURCE_GROUP=vapa-gantry-benchmark1 \
OPERATOR_VM_NAME=gantry-benchmark-operator \
GANTRY_IMAGE_POOL_COUNT=10 \
make -C hack/gantry-benchmark operator-vm-prebuild
```

The command starts `gantry-benchmark-image-builder.service` asynchronously.
The builder authenticates its managed identity once, generates each random
payload sequentially, pushes each image to the Gantry ACR, records its
immutable digest and payload SHA-256, removes the local image, and deletes the
40 GiB build context before starting the next image. Durable pool metadata
lives under `/var/lib/gantry-benchmark/image-pool`; transient build data lives
on the `/opt/gantry-benchmark` build disk.

Use the bounded status view while it runs:

```bash
AZURE_RESOURCE_GROUP=vapa-gantry-benchmark1 \
OPERATOR_VM_NAME=gantry-benchmark-operator \
make -C hack/gantry-benchmark operator-vm-image-pool-status
```

Start a Gantry-only benchmark with the oldest compatible ready image:

```bash
AZURE_RESOURCE_GROUP=vapa-gantry-benchmark1 \
OPERATOR_VM_NAME=gantry-benchmark-operator \
GANTRY_ONLY_BASELINE_RUN_ID=run-20260806-205719-51c38730 \
make -C hack/gantry-benchmark operator-vm-run-pool
```

The benchmark restores the retained baseline metadata automatically, atomically
moves one image from `ready/` to `claimed/`, and adopts its immutable digest.
Claimed images are never offered to a later run, preserving the cache-cold
contract. Pool adoption performs no image build, push, or registry credential
exchange inside the benchmark lifecycle.

Pool building and benchmark execution are mutually exclusive on one operator
VM. Both services hold the same lifecycle lock, and the builder also refuses
to run while a benchmark state exists. This is required for measurement
correctness: pushing another image to the Gantry ACR during a measured phase
would produce unrelated repository events and invalidate the Azure telemetry
completeness gate. Build the 5-10 image batch before starting benchmark runs,
not concurrently with them.

For direct CLI use with an already-authenticated container engine, the
equivalent targets are `prebuild-gantry`, `image-pool-status`, and
`prepare-gantry-pool`. Configure metadata and scratch locations with
`BENCHMARK_IMAGE_POOL_ROOT` and `BENCHMARK_IMAGE_POOL_BUILD_ROOT`.

Artifacts persist on the VM under
`/var/lib/gantry-benchmark/artifacts/<run-id>/`; `latest` points at the newest
run. By default the operator is a `Standard_D32ds_v5` with a dedicated 512 GiB
Premium SSD v2 build disk configured for 20,000 IOPS and 750 MB/s. The disk is
mounted at `/opt/gantry-benchmark`; the repository, random payloads, image build
layers, and Podman graphroot all live there. Durable artifacts remain on the OS
disk under `/var/lib/gantry-benchmark`.

`prepare` is the only common-lifecycle step that logs in to either ACR or pushes
workload content. `run` restores both per-node ACR routing files before
it exits, including when a phase or regression gate fails. It leaves the
instrumentation, Prometheus series, Grafana dashboard, Jobs, and structured artifacts
available for inspection. `disable` verifies restoration again and removes
the benchmark namespace and dashboard.

After the Gantry-cold Job completes, `run` waits for zero
`p2p_in_flight_pulls` and 20 seconds of unchanged counters (two PodMonitor
scrape intervals) before writing results. This prevents late background
prefetches from being omitted from byte totals.

Azure metric windows begin and end on UTC minute boundaries because
`PEBytesIn` is the service-to-client direction at an Azure Private Endpoint and
has a one-minute time grain. Each phase holds a three-minute trailing guard for
delayed metric buckets before the next phase can begin. The runner then polls
delayed Log Analytics and platform metrics until all four sources are complete
and unchanged for at least 30 seconds. Missing telemetry invalidates the run; it
is never treated as zero.

Private Endpoint bytes are also checked for physical plausibility. Baseline
`PEBytesIn` must be at least completed pods times configured payload bytes;
Gantry `PEBytesIn` must be at least `gantry_origin_bytes_total`. A newly created
or unhealthy endpoint can expose non-null control-traffic counters while
omitting blob bytes. Those points remain incomplete and are never promoted.

`enable` also creates `gantry-system/gantry-benchmark-lock`. The fixed
Gantry-namespace lock prevents concurrent runs even when operators choose
different benchmark namespaces. `disable` releases it only after routing is
restored and benchmark resources are gone.

If the command is interrupted, rerun `disable`. Per-node backups live under
`/var/lib/gantry-benchmark/<run-id>/<registry-host>` until restoration succeeds. The command
refuses to overwrite a Gantry ConfigMap or ACR-specific `hosts.toml` changed
by another operator after the benchmark took ownership.

## Results

Local results are retained at:

```text
tmp/gantry-benchmark/<run-id>/
  baseline.json
  gantry-cold.json
  comparison.json
  comparison.md
  state.json
  gantry-config.original.yaml
```

See [RESULTS.md](RESULTS.md) for the consolidated development, 1000-node, and
2000-node benchmark campaign results.

The comparison includes, in both modes:

- The shared workload payload SHA-256.
- ACR origin bytes with an explicit source.
- Gantry origin pulls and peer-fetch hits.
- Pod start and finish P50, P95, and P100 latency.
- Every node identity used by each phase.
- Configurable byte-reduction and P95-latency gates.

Proxy mode additionally reports bytes delivered to clients and total/blob/
manifest proxy request counts. ACR pull reduction is available in either mode
when Azure telemetry is enabled; otherwise proxy mode falls back to proxy
request counts and direct mode marks it unavailable.

A completed benchmark can return nonzero when a regression gate fails. The
artifacts are still written and cluster routing is still restored.

## Historical reference

The standalone implementation recorded these values on a prior 300-node
cluster. They are a sanity reference, not hard-coded expected output:

| Metric | Baseline | Gantry cold |
| --- | ---: | ---: |
| Proxy requests | 1,480 | 306 |
| ACR bytes | 323.24 GB | 4.30 GB |
| Blob requests | 880 | 6 |
| Manifest-by-digest requests | 600 | 300 |
| Pod start P50 | 6m29s | 49s |
| Pod start P95 | 7m20s | 1m6s |
| Gantry origin pulls | n/a | 9 |
| Gantry peer hits | n/a | 620 |

See [RUNBOOK.md](RUNBOOK.md) for operational checks and failure recovery.