# Gantry ACR benchmark

This workflow compares direct ACR image distribution with Gantry on an
existing 300-node test cluster. It is adapted from the standalone Gantry demo
that produced the project's published 300-node results.

The workflow does not provision AKS, create ACR, or install Gantry. It expects:

- Exactly 300 Ready, schedulable `linux/amd64` nodes.
- A Ready `gantry-system/gantry` DaemonSet on all 300 nodes.
- A dedicated Gantry ACR listed exactly once in Gantry's
  `upstream_registries` configuration, plus a different baseline ACR.
- kube-prometheus-stack, the Prometheus Operator CRDs, kube-state-metrics, and
  a Grafana dashboard sidecar. The workflow installs benchmark-owned
  PodMonitors for Gantry and, in proxy mode, the proxy.
- Containerd configured to read `/etc/containerd/certs.d`.
- A private operator VM in the AKS VNet. The VM runs every benchmark command,
  builds and pushes both images, queries Azure telemetry, and stores artifacts.
- Cluster permission to create privileged hostPath DaemonSets. Proxy mode also
  patches the Gantry ConfigMap.

For source-authoritative Azure measurements, set
`BENCHMARK_AZURE_TELEMETRY=true`. This additionally requires:

- Both ACRs reachable only through their own approved Private Endpoints, with
  public access disabled throughout. The operator VM reaches both ACRs over
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

## Lifecycle

All lifecycle commands run on the operator VM under its system-assigned managed
identity. The admin workstation only provisions the VM and uses Azure Run
Command to start or inspect its systemd service. The VM has no public IP or
inbound NSG rules; a subnet NAT gateway supplies outbound package, GitHub, and
Azure API access.

Bootstrap writes the VM-only configuration to `/etc/gantry-benchmark/env` and
fetches an admin kubeconfig using managed identity. The service executes:

```text
enable -> prepare -> preflight -> run -> disable
```

Provision/bootstrap from the admin workstation:

```bash
make -C hack/gantry-benchmark operator-vm-provision
```

Start the full VM lifecycle with Azure Run Command:

```bash
az vm run-command invoke -g "$AZURE_RESOURCE_GROUP" \
  -n "${OPERATOR_VM_NAME:-gantry-benchmark-operator}" \
  --command-id RunShellScript \
  --scripts 'systemctl start --no-block gantry-benchmark-operator.service'
```

Follow progress from the workstation in a separate terminal:

```bash
export OPERATOR_SSH_HOST="<operator-public-ip>"
export OPERATOR_SSH_KEY="tmp/gantry-benchmark-ssh-key"
make -C hack/gantry-benchmark operator-vm-watch
```

SSH mode is preferred because each refresh is immediate. The operator VM SSH
NSG rule must allow TCP/22 only from the current workstation `/32`. If
`OPERATOR_SSH_HOST` is unset, the watcher falls back to Azure Run Command using
`AZURE_RESOURCE_GROUP` and `OPERATOR_VM_NAME`.

The live view reports the lifecycle stage and start time, immutable run shape,
payload files/bytes/percentage, active Podman build or push, VM disk usage,
Kubernetes Job completion, Gantry readiness, recent logs, and the final report.
Use `operator-vm-status` for a single snapshot. Override the refresh cadence
with `WATCH_INTERVAL_SECONDS` (default 30).

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