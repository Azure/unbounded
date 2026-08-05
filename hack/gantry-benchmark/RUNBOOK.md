# Gantry ACR benchmark runbook

This workflow is for a dedicated test cluster. The benchmark itself runs only
on a private VM in the AKS VNet. Run the provisioning and Azure Run Command
commands below from the Unbounded repository root on an admin workstation.

## 1. Provision The Operator VM

```bash
export AZURE_SUBSCRIPTION_ID="<subscription-id>"
export AZURE_RESOURCE_GROUP="<resource-group>"
export AZURE_AKS_CLUSTER_NAME="<aks-cluster>"
export BASELINE_ACR_NAME="<baseline-acr-name>"
export GANTRY_ACR_NAME="<gantry-acr-name>"
export AZURE_LOG_ANALYTICS_WORKSPACE_NAME="<workspace-name>"
export AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID="<baseline-private-endpoint-id>"
export AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID="<gantry-private-endpoint-id>"
export OPERATOR_VNET_RESOURCE_GROUP="<vnet-resource-group>"
export OPERATOR_VNET_NAME="<vnet-name>"

# Optional scale overrides. Defaults are 5 nodes and a 128 MiB / 4-layer image.
export BENCHMARK_NODE_COUNT="5"
export BENCHMARK_IMAGE_SIZE_MIB="128"
export BENCHMARK_IMAGE_LAYERS="4"

# Optional operator storage overrides. These are the high-throughput defaults.
export OPERATOR_VM_SIZE="Standard_D32ds_v5"
export OPERATOR_VM_ZONE="1"
export OPERATOR_BUILD_DISK_GB="512"
export OPERATOR_BUILD_DISK_SKU="PremiumV2_LRS"
export OPERATOR_BUILD_DISK_IOPS="20000"
export OPERATOR_BUILD_DISK_MBPS="750"

# Optional private source delivery. Build this image from the exact local
# commit and set both values together; bootstrap rejects a revision mismatch.
export BENCHMARK_SOURCE_IMAGE="<gantry-acr>.azurecr.io/gantry-benchmark-source:<commit>"
export BENCHMARK_SOURCE_REVISION="<full-commit-sha>"

make -C hack/gantry-benchmark operator-vm-provision
```

To create the private source image without publishing the branch to GitHub:

```bash
SOURCE_REVISION=$(git rev-parse HEAD)
az acr build \
   --registry "$GANTRY_ACR_NAME" \
   --image "gantry-benchmark-source:${SOURCE_REVISION}" \
   --file images/gantry-benchmark-source/Containerfile \
   --build-arg "SOURCE_REVISION=${SOURCE_REVISION}" \
   .
export BENCHMARK_SOURCE_IMAGE="${GANTRY_ACR_NAME}.azurecr.io/gantry-benchmark-source:${SOURCE_REVISION}"
export BENCHMARK_SOURCE_REVISION="$SOURCE_REVISION"
```

Provisioning creates:

- A private `gantry-benchmark-operator` VM with no public IP.
- A dedicated operator subnet with no inbound NSG rules.
- A NAT gateway for package, GitHub, Go toolchain, and Azure API egress.
- A 128 GiB Premium OS disk for the operating system and durable artifacts.
- A dedicated 512 GiB Premium SSD v2 build disk, configured for 20,000 IOPS
   and 750 MB/s by default, mounted at `/opt/gantry-benchmark`.
- The repository, benchmark payloads, image layers, and Podman graphroot on the
   build disk. Durable result artifacts remain under `/var/lib/gantry-benchmark`
   on the OS disk.
- A system-assigned managed identity with `AcrPush` on both ACRs, AKS cluster
   admin credential access, resource-group `Reader`, and Log Analytics Reader.
- The repository, tools, VM-only configuration, kubeconfig, and systemd unit.

No ACR password, kubeconfig, or benchmark payload is copied from the admin
workstation. The VM obtains tokens and cluster credentials using managed
identity.

After bootstrap, verify storage placement before starting a large image build:

```bash
findmnt /opt/gantry-benchmark
podman info --format '{{.Store.GraphRoot}}'
```

The graphroot must be `/opt/gantry-benchmark/containers`.

## 2. Start The Full Lifecycle

Before provisioning the operator VM or starting the lifecycle on AKS, apply
the benchmark containerd configuration and require it to be Ready on every
target node. This enables debug unpack logs, sets the no-progress timeout to
15 minutes, and raises transfer-service layer downloads to six. The DaemonSet
performs one detached containerd restart per configuration hash.

```bash
kubectl create namespace gantry-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f hack/gantry-benchmark/manifests/containerd.yaml
kubectl -n gantry-system rollout status \
   daemonset/gantry-benchmark-containerd-config --timeout=45m
```

```bash
export OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"

az vm run-command invoke \
   -g "$AZURE_RESOURCE_GROUP" \
   -n "$OPERATOR_VM_NAME" \
   --command-id RunShellScript \
   --scripts 'systemctl start --no-block gantry-benchmark-operator.service'
```

The service performs `enable`, `prepare`, `preflight`, `run`, and `disable` from
the VM. Cleanup runs on success, failure, or interruption.

## 3. Inspect Status And Results

Use the repository progress dashboard while the service is running:

```bash
export AZURE_RESOURCE_GROUP="<resource-group>"
export OPERATOR_VM_NAME="gantry-benchmark-operator"
export OPERATOR_SSH_HOST="<operator-public-ip>"
export OPERATOR_SSH_KEY="tmp/gantry-benchmark-ssh-key"

# One snapshot:
make -C hack/gantry-benchmark operator-vm-status

# Continuously refresh until completion or failure:
WATCH_INTERVAL_SECONDS=30 make -C hack/gantry-benchmark operator-vm-watch
```

The dashboard combines lifecycle heartbeat, build progress, Podman storage,
VM disk, active image command, Kubernetes Jobs, Gantry DaemonSet status, and
recent logs. During a running lifecycle it never displays a previous run's
comparison as if it were current.

Direct SSH is the preferred monitoring transport. Restrict TCP/22 to the admin
workstation's `/32`; when SSH variables are absent, the command falls back to
Azure Run Command.

```bash
az vm run-command invoke \
   -g "$AZURE_RESOURCE_GROUP" \
   -n "$OPERATOR_VM_NAME" \
   --command-id RunShellScript \
   --scripts \
      'systemctl status gantry-benchmark-operator.service --no-pager || true' \
      'tail -100 /var/log/gantry-benchmark/service.log' \
      'cat /var/lib/gantry-benchmark/artifacts/last-run.json 2>/dev/null || true' \
      'cat /var/lib/gantry-benchmark/artifacts/latest/comparison.md 2>/dev/null || true'
```

Complete artifacts stay on the VM under
`/var/lib/gantry-benchmark/artifacts/<run-id>/`. The remaining sections describe
the lifecycle performed by the service and are primarily for troubleshooting.

## VM Lifecycle Internals

### Build The Proxy (legacy proxy mode only)

```bash
make -C hack/gantry-benchmark test
make -C hack/gantry-benchmark proxy-image
make -C hack/gantry-benchmark proxy-push
```

The proxy exposes:

- Port `5002`: OCI registry proxy.
- Port `9090`: Prometheus metrics, `/debug/summary`, and authenticated phase
  control.

Skip this section in direct mode.

### Enable Instrumentation

```bash
make -C hack/gantry-benchmark enable
```

`enable` fails unless:

- The confirmed and current kubectl contexts match.
- Exactly 300 schedulable Ready nodes match `BENCHMARK_IMAGE_PLATFORM`.
- Gantry reports 300 desired, updated, Ready, and available pods.
- No benchmark state ConfigMap already exists.

It then creates in both modes:

- Namespace `gantry-benchmark`.
- Cluster-wide lock ConfigMap `gantry-system/gantry-benchmark-lock`.
- A PodMonitor scoped to Gantry. Its samples carry
   `gantry_benchmark="true"` so existing scrapes cannot double the results.
- The `Gantry ACR Benchmark` Grafana dashboard.
- A state ConfigMap containing the exact original Gantry configuration but no
  credentials.

Proxy mode additionally creates the ACR credential/phase-control Secret, one
counting-proxy Deployment and Service, and the proxy PodMonitor.

No containerd or Gantry routing is changed by `enable`.

### Prepare Workload Images

The VM exchanges its managed-identity AAD token for one short-lived refresh
token per private ACR, exports those tokens only for `prepare`, and removes the
container-engine logins immediately afterward:

```bash
make -C hack/gantry-benchmark prepare
```

`prepare` generates one random payload set and pushes the same repository and
tag to both ACRs. The payload SHA, bytes, size, and layer count are identical.
Phase-specific paths inside every payload layer intentionally produce different
OCI digests so the Gantry phase cannot reuse baseline content on the same node.
It does not run pull pods or warm target-node caches.

Both ACRs remain private-only before, during, and after preparation. Their login
and data endpoints resolve through the AKS VNet Private Endpoints.

### Run Preflight

```bash
make -C hack/gantry-benchmark preflight
```

Preflight performs these mandatory checks before routing changes:

1. Requires Prometheus to report the current revision for every Gantry pod.
2. Requires the minimum Gantry DHT health score to be greater than zero.
3. In direct mode, requires `gantry_origin_bytes_total` on every Gantry pod.
4. Requires `gantry_peer_serve_bytes_total` on every Gantry pod.
5. With Azure telemetry, verifies Azure authentication, both ACR/Private
   Endpoint bindings, disabled public access on both ACRs, and `PEBytesIn`.
   It also requires a resource-specific `kube-audit-admin` diagnostic setting
   targeting the configured workspace, creates a unique ConfigMap, and waits
   for that exact write to appear in `AKSAuditAdmin`.
6. In proxy mode, smoke-tests the proxy, checks reachability from every target
   node, and requires the setup request in Prometheus.

Do not run the benchmark if preflight fails.

The audit probe is an end-to-end ingestion check, not just a table-schema
query. It can take several minutes after creating or replacing an AKS
diagnostic setting. A timeout means the benchmark must not start because audit
startup latency cannot be recovered from Kubernetes objects after cleanup.
If the setting is correctly configured but both `AKSAuditAdmin` and
`AKSControlPlane` remain empty, delete and recreate the AKS diagnostic setting.
This forces Azure Monitor to register the exporter again, which can be required
after deleting and recreating an AKS cluster with the same resource ID.

### Run The Comparison

```bash
make -C hack/gantry-benchmark run
```

The command executes one transaction using the images recorded by `prepare`:

1. Backs up each node's ACR-specific containerd configuration.
2. Installs direct routing for the baseline ACR and runs the 300-pod baseline Job.
3. Installs strict local-mirror routing for the Gantry ACR and runs the Gantry
   cold Job. Direct mode leaves Gantry pointed at its dedicated ACR.
4. Writes phase results and the comparison.
5. Restores both prior registry-specific files on every node or removes each
   file when it was originally absent.
6. Proxy mode restores the exact Gantry ConfigMap; direct mode verifies it was
   never changed.

Each pull container sleeps for 15 seconds after starting so AKS audit records a
running status patch. Audit startup latency is creation-to-first-running-status
receipt, not Job completion time.

In proxy mode, phase changes wait up to `BENCHMARK_ROLLOUT_TIMEOUT` for all
proxy requests attributed to the current phase to drain. Both proxy-mode
routing phases are fail-closed through the proxy, so fallback traffic remains
measured rather than escaping directly to ACR.

In direct mode, Gantry-cold origin bytes come from
`gantry_origin_bytes_total`. After the Job completes, the runner waits for zero
in-flight pulls and 20 seconds of stable counters before recording the phase.

With Azure telemetry enabled, each phase is isolated on whole UTC-minute
boundaries and includes a three-minute trailing guard for delayed Private
Endpoint metric buckets. The runner then polls until ACR pulls, `PEBytesIn`, and
all expected audit pod timelines are complete and stable. `comparison.json`
uses those Azure measurements as primary values and retains Kubernetes/Gantry
values as cross-checks.

The collector rejects implausibly small `PEBytesIn` totals even when Azure marks
the metric point non-null. Inspect `minimum_expected_bytes` in the phase
artifact when endpoint metering remains incomplete.

### Inspect Results

Print state:

```bash
make -C hack/gantry-benchmark status
```

Find the run ID in that output, then inspect:

```bash
jq . tmp/gantry-benchmark/<run-id>/comparison.json
cat tmp/gantry-benchmark/<run-id>/comparison.md
```

Open Grafana:

```bash
kubectl -n monitoring port-forward service/kps-grafana 3000:80
```

Select dashboard `Gantry ACR Benchmark` and choose the run ID. Check:

- `comparison.json` for ACR pull counts, `PEBytesIn`, and audit latency.
- Origin-byte and ACR image-pull reduction.
- Per-pod Gantry peer bytes.
- Gantry phase request source by `client_class`.
- Peer hits versus origin pulls.
- Completed pull pods by phase.
- Proxy CPU, network, and inflight requests.

A saturated single proxy can distort latency even though byte and request
totals remain valid. Treat sustained proxy CPU limits or a growing inflight
queue as an invalid latency run.

### Disable

After recording the results:

```bash
make -C hack/gantry-benchmark disable
```

`disable` is idempotent. It:

1. Switches the proxy to the idle phase when available.
2. Restores node routing from per-node backups.
3. Restores and rolls Gantry if necessary.
4. Requires Gantry to be fully Ready.
5. Deletes the Grafana dashboard and benchmark namespace.
6. Retains local result artifacts.

If restoration fails, the namespace and state ConfigMap remain in place. Fix
the reported conflict and rerun `disable`.

## Failure recovery

| Symptom | Action |
| --- | --- |
| Command interrupted during a phase | Rerun `make -C hack/gantry-benchmark disable`. |
| Enable was killed before state was written | Run `disable` with the same environment; it removes the partial namespace and Gantry-namespace lock. |
| Current context confirmation fails | Set `BENCHMARK_CONFIRM_CONTEXT` to the intended exact context after independently verifying it. |
| Fewer than 300 eligible nodes | Repair NotReady, cordoned, OS, or architecture mismatches before retrying. |
| Proxy OCI smoke fails | Inspect `kubectl -n gantry-benchmark logs deployment/acr-origin-proxy` and verify ACR admin credentials. |
| Node reachability is below 300 | Do not continue. Verify AKS node-to-Service routing and network policy. |
| Gantry ConfigMap hash conflict | Another operator changed the ConfigMap. Reconcile that change manually; the workflow will not overwrite it. |
| Node `hosts.toml` ownership conflict | Inspect the named node and ACR-specific directory. Preserve concurrent operator changes; do not remove markers blindly. |
| Regression gates fail | Inspect the generated comparison and Grafana. Routing is restored even though `run` returns nonzero. |