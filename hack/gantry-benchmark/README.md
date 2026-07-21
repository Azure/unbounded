# Gantry ACR benchmark

This workflow compares direct ACR image distribution with Gantry on an existing
test cluster. Nothing is inserted into the registry data path.

The workflow expects:

- The configured number of Ready, schedulable nodes matching
  `BENCHMARK_IMAGE_PLATFORM`.
- A Ready Gantry DaemonSet on every target node.
- The target ACR in Gantry's `upstream_registries` configuration.
- kube-prometheus-stack, kube-state-metrics, the Prometheus Operator CRDs, and
  a Grafana dashboard sidecar.
- Containerd configured to read `/etc/containerd/certs.d`.
- Podman or Docker Buildx locally, ACR push permission, Azure CLI access to ACR
  metrics, and permission to create privileged hostPath DaemonSets.

## Pull paths

The command builds two equivalent, independently random, digest-pinned images.

| Phase | Pull path |
| --- | --- |
| Baseline | containerd -> ACR |
| Gantry cold | containerd -> local Gantry -> peer or ACR |

Required hostname anti-affinity places exactly one pull pod on each target
node. Gantry routing is strict, so containerd cannot bypass Gantry during the
cold phase.

There is no warm-cache phase and no content purge. Fresh random payloads make
both images cold without deleting unrelated node content.

## Measurements

The result combines telemetry already available from the platform:

- Pod start and finish P50, P95, and P100 from Kubernetes status timestamps.
- ACR `TotalPullCount` and `SuccessfulPullCount` from one-minute Azure Monitor
  windows covering each phase.
- Kubelet pull operations, errors, samples, total duration, and average duration
  from Prometheus counter deltas.
- Gantry origin attempts, successful origin layer pulls, and peer-fetch hits
  from Prometheus counter deltas.
- Secret-safe Kubernetes warning counts classified into bounded markers such as
  HTTP 429/5xx, ACR egress limit, auth, timeout, and connection failures.
- Gantry peer outcomes (`hit`, `busy`, `stall`, `notfound`, `unavailable`, and
  `error`) plus bounded origin failure classes.
- Estimated baseline origin bytes: completed nodes multiplied by configured
  image payload size.
- Estimated Gantry origin bytes: successful origin layer pulls multiplied by
  average configured layer size.

ACR metrics have no repository or digest dimension. Use a dedicated benchmark
registry, or ensure unrelated pulls are negligible during the run. Byte values
are estimates and are labeled as such in every artifact.

## Lifecycle

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
make -C hack/gantry-benchmark test
make -C hack/gantry-benchmark enable
make -C hack/gantry-benchmark preflight
make -C hack/gantry-benchmark run
make -C hack/gantry-benchmark status
make -C hack/gantry-benchmark disable
```

`enable` creates the benchmark namespace, Gantry PodMonitor, dashboard, state
ConfigMap, and cluster-wide lock. It does not change Gantry or node routing.

`run` backs up each node's ACR-specific `hosts.toml`, runs the direct baseline,
switches to strict local Gantry routing, runs the cold phase, writes artifacts,
and restores every node's original routing before returning. Restoration also
runs when a phase or regression gate fails.

`disable` is idempotent. It restores any interrupted routing change, validates
Gantry, removes the dashboard and benchmark namespace, and releases the lock.

## Results

```text
tmp/gantry-benchmark/<run-id>/
  baseline.json
  gantry-cold.json
  comparison.json
  comparison.md
  state.json
  gantry-config.original.yaml
```

A completed benchmark can return nonzero when a regression gate fails. The
artifacts remain available and node routing is still restored.

Latency remains in the comparison as an informational signal. It does not
affect PASS/FAIL. Existing local runs can be recalculated with:

```bash
make -C hack/gantry-benchmark report RUN_ID=<run-id>
```

See [RUNBOOK.md](RUNBOOK.md) for operational checks and recovery.