# Gantry ACR benchmark

This workflow compares direct ACR image distribution with Gantry on an
existing 300-node test cluster. It is adapted from the standalone Gantry demo
that produced the project's published 300-node results.

The workflow does not provision AKS, create ACR, or install Gantry. It expects:

- Exactly 300 Ready, schedulable `linux/amd64` nodes.
- A Ready `gantry-system/gantry` DaemonSet on all 300 nodes.
- ACR already listed once in Gantry's `upstream_registries` configuration.
- kube-prometheus-stack, the Prometheus Operator CRDs, kube-state-metrics, and
  a Grafana dashboard sidecar. The workflow installs benchmark-owned
  PodMonitors for Gantry and, in proxy mode, the proxy.
- Containerd configured to read `/etc/containerd/certs.d`.
- Podman or Docker Buildx on the operator machine.
- Cluster permission to create privileged hostPath DaemonSets. Proxy mode also
  patches the Gantry ConfigMap.

For source-authoritative Azure measurements, set
`BENCHMARK_AZURE_TELEMETRY=true`. This additionally requires:

- ACR reachable only through an approved Private Endpoint, with public access
  disabled during preflight and both measured phases. Build and push both
  workload images first while the operator machine can still reach ACR.
- A Log Analytics workspace receiving `ContainerRegistryRepositoryEvents` and
  `AKSAuditAdmin` in resource-specific tables.
- Azure resource IDs for ACR, AKS, and the ACR Private Endpoint.
- Azure CLI authentication with read access to those resources and the
  workspace.

This is test-cluster tooling. The baseline deliberately transfers
`BENCHMARK_NODE_COUNT * BENCHMARK_IMAGE_SIZE_MIB` from ACR.

## What it runs

`prepare` builds two equivalent but independently random images. Both are
single-platform and digest-pinned in benchmark state.

| Mode | Baseline | Gantry cold |
| --- | --- | --- |
| `proxy` | containerd -> counting proxy -> ACR | containerd -> local Gantry -> peer or counting proxy -> ACR |
| `direct` | containerd -> ACR | containerd -> local Gantry -> peer or ACR |

The proxy is the only measured ACR client in both phases. During the Gantry
phase, direct containerd fallback also points at the proxy and is reported as
`client_class="containerd"`. Gantry-origin traffic is reported as
`client_class="gantry"`.

Direct mode deploys no proxy and never patches Gantry. Baseline origin bytes
are analytic (completed pods times image size); Gantry-cold origin bytes come
from `gantry_origin_bytes_total`, measured at the upstream response-body
boundary and including partial transfers and retries. Preflight requires that
metric on every target Gantry pod.

With Azure telemetry enabled, the primary measurements are instead:

- Image pulls: successful `ContainerRegistryRepositoryEvents` rows for the
  exact phase manifest digest.
- ACR bytes: Private Endpoint `PEBytesIn` over an isolated whole-minute phase
  window.
- Startup latency: `AKSAuditAdmin` pod create, binding, and first started-status
  receipt timestamps.
- Peer bytes: per-pod `gantry_peer_serve_bytes_total{kind}` deltas.

The workload is a Kubernetes Job with 300 completions and 300-way
parallelism. Required hostname anti-affinity uses the run ID and phase, so the
workflow proves that exactly one pull pod ran on each of 300 distinct nodes.
Each pull container remains Running for 15 seconds after image startup so
`AKSAuditAdmin` reliably captures a running status transition. The startup
metric ends at that transition; the hold is not included in startup latency.

There is no warm-cache phase and no containerd content purge. The benchmark
answers the requested direct-versus-Gantry cold-start question without
deleting workload content from every node.

## Lifecycle

Create the local configuration:

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
```

Set `BENCHMARK_CONFIRM_CONTEXT` to the exact output of:

```bash
kubectl config current-context
```

Set `BENCHMARK_MODE=direct` to avoid the proxy. Build/push the proxy only when
`BENCHMARK_MODE=proxy`, then run the common lifecycle:

```bash
# Proxy mode only:
# make -C hack/gantry-benchmark proxy-image
# make -C hack/gantry-benchmark proxy-push
make -C hack/gantry-benchmark enable
make -C hack/gantry-benchmark prepare
az acr update -g "$RESOURCE_GROUP" -n "$ACR_NAME" --public-network-enabled false
make -C hack/gantry-benchmark preflight
make -C hack/gantry-benchmark run
make -C hack/gantry-benchmark status
make -C hack/gantry-benchmark disable
```

`prepare` is the only common-lifecycle step that logs in to ACR or pushes
workload content. `run` restores per-node ACR routing before
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

`enable` also creates `gantry-system/gantry-benchmark-lock`. The fixed
Gantry-namespace lock prevents concurrent runs even when operators choose
different benchmark namespaces. `disable` releases it only after routing is
restored and benchmark resources are gone.

If the command is interrupted, rerun `disable`. Per-node backups live under
`/var/lib/gantry-benchmark/<run-id>` until restoration succeeds. The command
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

The comparison includes, in both modes:

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