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
  PodMonitors for both the proxy and Gantry.
- Containerd configured to read `/etc/containerd/certs.d`.
- Podman or Docker Buildx on the operator machine.
- Cluster permission to create privileged hostPath DaemonSets and patch the
  Gantry ConfigMap.

This is test-cluster tooling. The baseline deliberately transfers roughly
300 GiB from ACR when 300 nodes pull a fresh 1024 MiB random payload.

## What it runs

The command builds two equivalent but independently random images. Both are
single-platform and digest-pinned.

| Phase | Pull path |
| --- | --- |
| Baseline | containerd -> counting proxy -> ACR |
| Gantry cold | containerd -> local Gantry -> peer or counting proxy -> ACR |

The proxy is the only measured ACR origin in both phases. Baseline proxy traffic
is reported as `client_class="containerd"`. During the Gantry phase, host routing
is strict to the local Gantry mirror with no direct proxy fall-through; proxy
traffic should therefore be reported mostly as `client_class="gantry"`.
Substantial `client_class="containerd"` bytes during the Gantry phase indicate
the route did not flow through Gantry as intended.

The workload is a Kubernetes Job with 300 completions and 300-way
parallelism. Required hostname anti-affinity uses the run ID and phase, so the
workflow proves that exactly one pull pod ran on each of 300 distinct nodes.

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

Build and push the counting proxy, then run the lifecycle:

```bash
make -C hack/gantry-benchmark proxy-image
make -C hack/gantry-benchmark proxy-push
make -C hack/gantry-benchmark enable
make -C hack/gantry-benchmark preflight
make -C hack/gantry-benchmark run
make -C hack/gantry-benchmark status
make -C hack/gantry-benchmark disable
```

`enable` patches the matching Gantry upstream registry entry to point at the
counting proxy and rolls the Gantry DaemonSet, so the DHT can reconverge before
measurement. `run` restores per-node ACR routing before it exits, including when
a phase or regression gate fails. It leaves the proxy, Prometheus series,
Grafana dashboard, Jobs, structured artifacts, and Gantry proxy patch available
for inspection. `disable` restores the original Gantry ConfigMap and removes the
benchmark namespace and dashboard.

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
  proxy-summary.json
  state.json
  gantry-config.original.yaml
```

The comparison includes:

- ACR upstream bytes and bytes delivered to clients.
- Total, blob, and manifest request counts.
- Gantry origin pulls and peer-fetch hits.
- Pod start and finish P50, P95, and P100 latency.
- Every node identity used by each phase.
- Configurable byte-reduction and P95-latency gates.
- Per-phase HTTP response status counts and classified upstream transport
  errors. `proxy-summary.json` is captured during cleanup even when a phase
  times out or another run error prevents a comparison from being written.

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