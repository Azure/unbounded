# Gantry deployment artifacts

This directory carries the Helm chart and operator-facing rendered manifests
needed to roll out the Gantry agent as a Kubernetes DaemonSet.

## Files

The Helm chart under `chart/` is the source of truth for resources shared by
standalone and operator-managed installations. `make gantry-manifests` renders
the internal operator profile into `deploy/gantry/rendered/`; the Unbounded
operator embeds those files and applies its own image, ConfigMap, and Lease
ownership semantics. The target also renders examples used by development and
benchmark tooling; the node configurator is rendered only by the standalone chart.

| Source | Rendered to | Purpose |
| --- | --- | --- |
| `chart/templates/daemonset.yaml` | `rendered/daemonset.yaml` | One-pod-per-node DaemonSet. |
| `chart/templates/serviceaccount.yaml` | `rendered/serviceaccount.yaml` | Namespace + ServiceAccount + Role + PriorityClass. |
| `chart/templates/configmap.yaml` | `rendered/configmap.yaml` | Default `config.yaml` (mirrors `config.NewDefault()`). |
| `chart/templates/rendezvous-leases.yaml` | `rendered/rendezvous-leases.yaml` | Fixed chair Lease set. |
| `chart/templates/node-config.yaml` | Standalone chart only | Continuously reconciles containerd's default Gantry mirror route. |
| `examples/registry-secret.example.yaml.tmpl` | `rendered/examples/registry-secret.example.yaml` | Template Secret for upstream-registry credentials. |
| `examples/networkpolicy.yaml.tmpl` | `rendered/examples/networkpolicy.yaml` | **Hardening overlay (NOT applied by default).** See [Hardening overlays](#hardening-overlays) below. |
| `hosts.toml.template` | (not rendered) | containerd registry mirror config; one file per upstream registry under `/etc/containerd/certs.d/<host>/hosts.toml`. |

## Installation paths

- Operator-managed clusters use the manifests embedded in the
   `unbounded-operator` binary. The operator never runs Helm.
- Clusters without the operator install the released OCI chart. The chart
   continuously reconciles `/etc/containerd/certs.d/_default/hosts.toml` on
   every selected node. Containerd must already be configured to read
   `/etc/containerd/certs.d`; the chart does not edit or restart containerd.

The paths are mutually exclusive. `PriorityClass/gantry-low` records the active
manager, and both installers reject ownership by the other path.

```sh
helm upgrade --install gantry oci://ghcr.io/azure/charts/gantry \
   --version <release-without-v> \
   --namespace gantry-system \
   --create-namespace \
   --set image.digest=sha256:<gantry-image-digest> \
   --set gantry.upstreamRegistries[0].name=registry.example.com \
   --set gantry.upstreamRegistries[0].endpoint=https://registry.example.com
```

The container image is built from `images/gantry/Containerfile` via
`make image-gantry-local` (or `make image-gantry-push` to push).

## Operator Profile

```sh
# Render the profile embedded by unbounded-operator.
make gantry-manifests

# Validate and package the standalone chart.
make gantry-chart-lint
make gantry-chart-package GANTRY_CHART_VERSION=0.1.0 GANTRY_CHART_APP_VERSION=v0.1.0
```

## Building the image locally

```sh
# Single-arch into local container engine:
make image-gantry-local

# Build and push to $(CONTAINER_REGISTRY):
make image-gantry-push CONTAINER_REGISTRY=ghcr.io/your-org

# Explicit tag:
make image-gantry-local VERSION=v0.6.0
```

## Per-node containerd setup

Nodes provisioned by `unbounded-agent` already have containerd configured to
read `/etc/containerd/certs.d` and carry the managed default Gantry mirror
entry in `/etc/containerd/certs.d/_default/hosts.toml`. On those nodes, install
the Gantry DaemonSet normally; the mirror activates when the pod starts
listening on `127.0.0.1:5000`.

Standalone Helm installations run `DaemonSet/gantry-containerd-config`. Its
resident reconciler checks the default `hosts.toml` every five seconds and
atomically restores the chart-owned payload when the file is missing or
different, including after a node upgrade resets host configuration. Graceful
shutdown removes the file only when it still matches the chart payload. Set
`nodeConfig.enabled=false` when another node-management system owns this file.

Externally managed installations can instead drop a registry-specific
`hosts.toml` at:

```
/etc/containerd/certs.d/<registry-host>/hosts.toml
```

derived from `hosts.toml.template` (substitute `${REGISTRY_SERVER}`
with the registry's `https://...` URL). containerd reloads `certs.d`
on its own; no restart needed.

## Opt-in Racer backend

Set the chart value `racer.enabled=true`, or layer
`chart/values-racer.yaml` over the chart defaults. The schema requires a boolean
and rejects unknown Racer profile keys. This selects the deployment identity,
socket mount, and `GANTRY_RACER_ENABLED=true` process environment together.
The default remains the legacy backend with UID 65532. The Racer cache name is
fixed to `gantry`. Runtime tuning belongs in the Gantry config YAML, supplied
through the existing `gantry.config` string (`--set-file gantry.config=...`),
using the YAML fields accepted by the deployed Gantry version; the chart does
not interpret or duplicate those SDK settings. Supplying config alone does not
select the deployment profile.

Racer mode starts the SDK client, Gantry's registry origin callback, the OCI
mirror, the operations endpoint, and optional existing loopback pprof endpoint.
It does not start libp2p/DHT, Lease chairs, peer transfer, containerd storage or
subscriptions, inventory advertisement, or legacy coordination and metrics.
Mirror/operations listeners, upstream registries, logging, and pprof settings
are still validated. Legacy-specific settings are not required in Racer mode;
environment and YAML parsing still reject malformed values.

### Provisioning

Provision Racer separately with a cache named `gantry`, and expose these
canonical paths to the Gantry process:

| Path | Owner and purpose |
| --- | --- |
| `/run/racer/gantry/client/socket` | Racer serves this Unix socket; Gantry's SDK client connects to it. |
| `/run/racer/gantry/origin/socket` | Gantry's SDK origin server creates this Unix socket; Racer connects to it for registry reads. |

The Racer chart profile mounts the node directory `/run/racer/gantry` at the
same path, writable, with `hostPath.type=DirectoryOrCreate`. It mounts a directory
rather than individual sockets or a `subPath`, so replacement sockets remain
visible. Only this cache is exposed to Gantry. Kubelet creates missing hostPath
directories; Gantry creates `origin/` with mode `0755` and preserves existing
directory modes. Gantry can start before Racer creates the client endpoint.

**Identity model:** Gantry runs as UID/GID 0 to match the Rust dataplane. Both
drop all Linux capabilities. Gantry retains a read-only root filesystem,
`allowPrivilegeEscalation: false`, and `RuntimeDefault` seccomp. It is not a
privileged container. The Rust workload runs as root
(`internal/racer/workload.go:61,89`); the SDK origin socket defaults to `0600`
(`pkg/racersdk/origin.go:96-97`). Matching UIDs permits the capability-stripped
dataplane to connect to that private socket. The current Rust client listener
explicitly permits mounted clients with mode `0666`
(`cmd/racer-dataplane/src/client/listener.rs`, `allow_socket_access`), but that
does not relax Gantry's origin ownership requirement.

There is no socket-directory chown/chmod init container, shared writable group,
or `fsGroup`. The profile omits the legacy libp2p hostPath, its chown init
container, containerd runtime mount, and legacy peer ports. The origin directory
must remain owned by Gantry's UID and must not be group/world writable
(`pkg/racersdk/origin_owned.go:151-155`). Gantry explicitly enables the SDK's
owned stale-socket recovery (`cmd/gantry/agent_racer_socket.go:26`): recovery uses
exclusive ownership locks and socket witnesses, refuses foreign paths and
symlinks, and preserves replacements at shutdown. Do not remove the ownership
files or change a preexisting foreign-owned directory to bypass these checks.

The chart does not install Racer or create `ClusterCache/gantry`. Provision those
separately on the same selected nodes. Keep containerd's mirror route pointing to
Gantry. Upstream configuration and registry credential negotiation remain with
Gantry: the mirror uses its origin client for authentication probes, while
content reads go through Racer and its Gantry origin callback. Racer errors
never switch the Gantry agent back to its legacy backend or trigger direct
content-origin fallback in the mirror.

Digest-addressed GETs stream through Racer with bounded memory. Gantry validates
metadata and transport framing; the OCI consumer verifies the complete object's
SHA-256 digest, including resumed assembly. HEAD uses SDK metadata-only `Stat`;
open-ended blob resumes use one Stat snapshot and a pinned offset read without
rereading the skipped prefix in Go (`internal/gantry/mirror/racer.go:45-86,125-129`).
The dataplane still fetches whole pages covering the requested range, including
the starting page for an unaligned offset. Stream failures abort
the response. Unsupported range forms are ignored as in the legacy mirror.
Tags still return 503 for containerd to resolve through its existing registry
host chain. Containerd's own origin fallback is unchanged.

The origin callback identifies immutable versions by their quoted OCI digest
and gives metadata a 24-hour admission TTL. It validates the configured registry,
repository, pin, total size, and requested page. Production GET callbacks use
bounded registry Range requests, learning size and content type from GET without
per-page HEAD. A registry must return an exact 206 with known total size, or a
complete 200 at offset zero whose known Content-Length fits the requested page.
Unknown-size, oversized, and nonzero-offset 200 responses are rejected without
draining the object. Explicit HEAD still requires a known size. These rules avoid
open-ended tail-download amplification while supporting small manifests from
registries that ignore Range (`internal/gantry/racer/racer.go:212-294`,
`internal/gantry/origin/range.go:16-73`). Credentials travel separately from adapter
metadata and cache keys. On a Racer 401, Gantry can repeat the bounded registry
challenge probe; the SDK does not transport a remote origin's repository-specific
`WWW-Authenticate` challenge, so only locally known or registry-level challenges
can be recovered.

Object MIME metadata survives through Racer without Gantry parsing manifest
bodies. Legacy absence is unknown; two present values must agree. Typed peer
metadata uses a signed v2 extension that old strict peers reject, so mixed-version
typed peer paths can fail until participating nodes are upgraded. Typed disk
records and new checkpoints use v2; new readers also accept v1, but old binaries
cannot read v2 data. See [SDK compatibility](../../designs/racer-sdk.md#content-type-and-compatibility)
for the precise wire and rollback limits.

SDK fake tests cover protocol behavior; `make e2e-racer` is the deployed Rust and
containerd acceptance suite. The throughput rewrite is **VERIFIED, not yet
merged**, as of 2026-09-28, including a full deployed E2E pass in 410.354s.
See the [dated acceptance summary](../../designs/racer-sdk-verification.md) for
the final gates, `e2e/racer/README.md` for builds and prerequisites, and
[throughput performance](../../designs/racer-sdk-throughput-performance.md) for
current measurement and verification status. Historical SDK reports are not
evidence that this deployment has passed.

### Workload tuning and capacity

Supply these runtime fields in the complete Gantry YAML passed through
`--set-file gantry.config=...` for Helm, or update `gantry-config` for the
operator-managed workload. Each also has a `GANTRY_` environment override formed
by uppercasing its YAML name, and a CLI flag formed by replacing underscores with
hyphens. Zero selects the default; negative values are invalid. Backend selection
remains `GANTRY_RACER_ENABLED=true`, set by the deployment profile, not a YAML
`racer_enabled` field (`internal/gantry/config/config.go:59-78,655-668,753-765`,
`cmd/gantry/agent_racer.go:235-250`).

| YAML field | Default | Scope |
| --- | --- | --- |
| `racer_max_connections` | 64 | Bulk connections and live Values |
| `racer_max_queued_requests` | 128 | Waiting bulk calls only |
| `racer_metadata_connections` | 4 | Reserved HEAD connections/operations |
| `racer_metadata_queued_requests` | 16 | Independent HEAD queue |
| `racer_small_object_connections` | 4 | Reserved manifest/small-object connections and live Values |
| `racer_small_object_queued_requests` | 128 | Independent small-object queue |
| `racer_queue_timeout` | `5s` | Maximum wait for an admission slot |
| `racer_response_header_timeout` | `60s` | SDK request-write bound, then response-head wait after request write |
| `racer_origin_max_connections` | 128 | Accepted origin connections, including idle |
| `racer_origin_concurrent_requests` | 64 | Active origin GET callbacks and bodies |
| `racer_origin_concurrent_head_requests` | 4 | Separately reserved origin HEAD callbacks |
| `racer_origin_request_timeout` | `60s` | Complete origin operation, including callback, body, EOF probe, final write |
| `racer_write_timeout` | `30s` | Each downstream bounded write/flush, not total object duration |

Defaults are wired in `internal/gantry/config/config.go:494-507`. For example,
`GANTRY_RACER_SMALL_OBJECT_CONNECTIONS=8` or
`--racer-small-object-connections=8` changes that pool; it does not resize the
HEAD or bulk pool. Unsupported SDK-only settings, such as DialTimeout and
IdleConnTimeout, retain SDK defaults (5s and 90s). The origin SDK's write/header/
idle defaults remain 30s/5s/30s; `racer_write_timeout` is the mirror's downstream
setting, not the origin write timeout (`cmd/gantry/agent_racer.go:129,235-250`,
`pkg/racersdk/client.go:101-115`, `pkg/racersdk/origin.go:80-94`).

Capacity interpretation:

- The default client has 64 bulk, 4 HEAD, and 4 small-object slots, with separate
  queues of 128, 16, and 128 (272 total waiting calls). Full queues fail immediately; queue timeouts fail
  after the configured wait. Live Values hold capacity until EOF, error, or
  Close, including while a consumer is slow. Reservations isolate SDK admission,
  not all downstream resources. Origin HEAD slots are separate from GET slots,
  but accepted connections remain a shared cap. The 128-entry small-object queue
  accommodates a 64-request cold-manifest burst at SDK admission while keeping
  active small-object work bounded at four. Queue deadlines and downstream
  capacity still apply; this is not an E2E success guarantee.
- SmallObject checks the total object size, at most 16 MiB. A tiny range of a
  larger object does not qualify. Manifest GETs select this pool without a HEAD
  preflight. Ordinary GETs use one bootstrap and at most one pinned remainder;
  Go does not issue one request per page.
- SDK copy scratch is capped independently at 32 KiB times bulk plus small-object
  copy capacity: 2.125 MiB at defaults. Origin copy scratch is up to 2 MiB at the
  default 64 GET callbacks. This is not a process memory estimate: add headers,
  runtime, caller buffers, kernel sockets, and Rust's separately bounded pages.
  Increasing queue depth permits more waiters, not more active throughput.
- Tune against actual registry, Rust page/pipe, and node CPU/memory capacity.
  Rust defaults include `RACER_CLIENT_CONNECTIONS=128`,
  `RACER_ORIGIN_CONNECTIONS_PER_CACHE=8`, `RACER_PIPES=16`, and
  `RACER_RANGE_WINDOW_PAGES=2`. These are dataplane environment settings, not
  Gantry YAML or ClusterCache fields (`cmd/racer-dataplane/src/config.rs:174-194`,
  `api/racer/v1alpha1/clustercache_types.go:18-21`).

For long objects, distinguish timeouts rather than increasing every limit.
SDK body lifetime follows the caller's context. Gantry refreshes the downstream
write deadline for each bounded write and final flush. Rust bounds initial
metadata/first-page work with `RACER_REQUEST_TIMEOUT_MS` (default 30000), then
new distinct pages in normal pinned ranges receive fixed child acquisition
deadlines; already admitted pages and retries do not renew their budgets.
Client writes use `RACER_READER_STALL_TIMEOUT_MS` (default 10000), renewed only
on positive socket progress. Explicit aggregate-budget and peer operations keep
absolute deadlines. Origin RequestTimeout still caps each whole-page callback
operation. A healthy long remainder may therefore exceed the initial request
timeout, while a stalled page or consumer still fails
(`internal/gantry/mirror/racer_io.go:25-54,99-126`,
`cmd/racer-dataplane/src/read/range_stream.rs:30-65,223-315`,
`cmd/racer-dataplane/src/memory/delivery.rs:153-205,261-265`).

### Racer metrics

The operations `/metrics` endpoint includes runtime/process collectors plus
Racer-specific metrics (`cmd/gantry/agent_racer_metrics.go:25-37,83-104`):

| Metric family | Interpretation |
| --- | --- |
| `gantry_racer_sdk_queue_depth`, `bulk_queue_depth`, `metadata_queue_depth`, `small_object_queue_depth` | Aggregate and per-pool queue gauges; all abbreviated names have the `gantry_racer_sdk_` prefix |
| `gantry_racer_sdk_active_bulk`, `active_metadata`, `active_small_objects` | Occupied admission slots, including dials; same prefix convention |
| `gantry_racer_sdk_connections`, `idle_connections` | Open and reusable connections across all three pools |
| `gantry_racer_sdk_queue_waits_total`, `queue_wait_seconds_total`, `queue_rejections_total`, `queue_timeouts_total` | Cumulative admission waits, completed wait duration, full-queue failures, timeouts |
| `gantry_racer_sdk_dials_total`, `connection_reuses_total`, `retries_total`, `bytes_read_total` | Dial attempts, idle leases, eligible stale-connection retries, body bytes consumed |
| `gantry_racer_mirror_requests_total` | Method, status, and `complete`/`aborted` outcome |
| `gantry_racer_mirror_bytes_total`, `gantry_racer_mirror_duration_seconds` | Actual bytes accepted downstream and full handler duration by method |
| `gantry_racer_origin_requests_total` | Upstream round trips by method/status, including authentication; status 0 means transport failure |
| `gantry_racer_origin_bytes_total` | Upstream GET body bytes consumed by kind, including partial transfers |

SDK Stats is sampled once per scrape; individual fields are concurrent samples,
not a transactional accounting record. Labels exclude keys, repositories, URLs,
and credentials. Consumed SDK/origin bytes can include failed transfers; mirror
completion is not OCI digest verification. Compare queue saturation, aborted
responses, origin GET/HEAD counts, and actual body bytes for each workload. Do
not infer cache-hit ratio or a universal amplification bound from aggregate
byte counters alone; use Racer's own cache/peer/storage telemetry and a controlled
cold/warm workload (`pkg/racersdk/stats.go:8-43`,
`cmd/gantry/agent_racer_metrics.go:42-69`).

### Production profile and E2E integration

From the repository root, with the E2E image loaded into the kind nodes and
Racer plus `ClusterCache/gantry` provisioned, first create
`tmp/racer-gantry-config.yaml` containing the complete Gantry configuration for
your registry. Then deploy the production chart profile:

```sh
make install-helm
bin/helm upgrade --install gantry deploy/gantry/chart \
  --namespace unbounded-system --create-namespace \
  --values deploy/gantry/chart/values-racer.yaml \
  --set-string image.repository=docker.io/library/gantry \
  --set-string image.tag=e2e \
  --set image.pullPolicy=IfNotPresent \
  --set-file gantry.config=tmp/racer-gantry-config.yaml \
  --wait --timeout 90s
```

The configuration you supply must include the actual registry endpoint, mirror listener on
`0.0.0.0:5000`, `mirror_bind_allow_non_loopback: true` for the chart's hostPort
routing, and operations listener on `0.0.0.0:9095`. The automated E2E fixture
instead supplies registry values directly to Helm and mounts its test CA; it
does not generate this example configuration file. For another image,
replace `image.repository` with `<registry>/<repository>` and `image.tag` with
the built tag, or use `image.digest`. The chart uses a full repository path,
not a separate registry value. For manifest-based harnesses, replace
`upgrade --install` with `template` and omit `--create-namespace`, `--wait`, and
`--timeout 90s`, then apply the rendered output. Do not replace the rendered pod
with a handwritten E2E DaemonSet.

Keep operator-managed Gantry disabled on every Site before installing this Helm
release; an already operator-owned installation must first be uninstalled through
its existing ownership workflow. The operator may still manage Racer. Set
`nodeConfig.enabled=false` if the fixture or unbounded-agent owns containerd's
mirror configuration; otherwise the standalone chart manages it continuously.
Validate an actual digest-pinned containerd pull, including byte integrity and
restart recovery, rather than treating socket readiness as end-to-end success.

### Explicit operator activation

The default embedded operator profile remains legacy. There is no
`Site.spec.components.gantry.racer` field. To activate an existing
operator-managed Gantry workload, merge the `gantry-racer.yaml` data key from
[`examples/racer-operator-overrides.yaml`](examples/racer-operator-overrides.yaml)
into `ConfigMap/unbounded-component-overrides` in the operator namespace. On a
cluster without that ConfigMap, the complete example can be applied directly:

```sh
kubectl apply -f deploy/gantry/examples/racer-operator-overrides.yaml
kubectl -n unbounded-system rollout status daemonset/gantry --timeout=90s
```

This uses the existing workload override mechanism. Tests apply the example to
the real operator plan and compare its socket access and security settings with
the chart Racer profile. Overrides cannot delete operator-managed content, so
this path retains the legacy volumes, port declarations, and libp2p-only chown
init container. That init container does not mount or modify Racer directories.
The operator keeps image version lockstep; update `gantry-config` for runtime
tuning. Provision Racer and its cache separately. Removing this override data
key restores the default legacy workload on reconciliation.

For build-time render inspection, `values-racer.yaml` also layers over the
internal operator values (whose ownership fields use the existing schema bypass):

```sh
bin/helm template gantry deploy/gantry/chart \
  --namespace unbounded-system \
  --values deploy/gantry/chart/values-operator.yaml \
  --values deploy/gantry/chart/values-racer.yaml \
  --skip-schema-validation \
  --set-string image.repository=docker.io/library/gantry \
  --set-string image.tag=e2e
```

### Readiness and shutdown

`/livez` reports process liveness. `/readyz` and `/healthz` report whether both
canonical Unix sockets recently accepted connections, polled every 250 ms with
a shared 200 ms probe deadline. **Socket availability is not Racer readiness:**
these probes do not verify cache provisioning, cluster health, origin
registration, registry access, or a successful object fetch. The SDK has no
origin-ready API. Validate an actual image pull as part of deployment checks.

The mirror's startup gate returns 503 until the first successful socket check.
Later socket failures make operations readiness fail; the mirror remains open
and reports request failures through its normal Racer error handling. An origin
startup or serve failure terminates the agent with an error. Essential mirror
and operations HTTP failures also terminate it. SIGINT/SIGTERM clears readiness
and drains HTTP requests, then cancels the origin server and closes the SDK
client. Cleanup uses a 10-second HTTP/origin shutdown budget and force-closes
HTTP connections when draining times out. Pprof remains diagnostic and optional.

Racer mode exposes the runtime/process and [Racer metrics](#racer-metrics) above,
not legacy Gantry P2P or containerd metrics. Use Racer's own observability for its
cache and cluster. The rollout checks below describe the default legacy backend.

## What to verify after rollout (legacy backend)

| Check | How |
| --- | --- |
| Agents are running | `kubectl -n unbounded-system get ds gantry` |
| Liveness / readiness | `/livez`, `/readyz` on 9095 per pod |
| Metrics | `curl http://<pod-ip>:9095/metrics` or scrape from Prometheus |
| Routing-table grew | `p2p_dht_health_score` ≥ 0.7 |
| Mirror is being used | `p2p_cache_hit_total` increments while a workload rolls out (= containerd content-store hits on the mirror endpoint after Phase 8) |
| Storage backend is containerd | `gantry_storage_mode_info{mode="containerd"} == 1` |
| Advertiser reconciling | `gantry_advertise_reconcile_total` increases at the configured cadence |
| Leases are being created on `please_pull` | `gantry_containerd_lease_created_total` increments during cold-start rollouts |
| Origin fallback is rare | `p2p_origin_fallback_total` stays at ~0 |

See `docs/detailed-design.md` §7.6 for the full metric catalog.

## Hardening overlays

`deploy/examples/` carries optional hardening manifests that are
intentionally **not** part of the default `kubectl apply` workflow.
Every overlay there contains at least one site-specific value (CIDR,
endpoint, label) that no shipped manifest can guess correctly across
arbitrary clusters, so applying them unedited will fail the cluster
into a state that is hard to debug.

> **Production guidance:** the default install leaves the mirror
> listener (5000) and transfer listener (5001) reachable from other
> pods on the cluster network at `<podIP>:port`. The `hostIP:
> 127.0.0.1` binding on the DaemonSet's hostPort only restricts
> *host-side* reach; the listener inside the pod is still
> `0.0.0.0`. Production installs **should** adopt
> [`examples/networkpolicy.yaml.tmpl`](examples/networkpolicy.yaml.tmpl) (or
> an equivalent NetworkPolicy in their own overlay) to close that
> pod-network gap. The overlay is shipped as an example rather than
> a default because its allow-list depends on the cluster's
> node-CIDR range, which is site-specific (see the workflow below).

### `examples/networkpolicy.yaml`

Locks transfer (5001), libp2p (4001), mirror (5000), and metrics
(9095) to the minimum traffic each port needs. Holds the manifest
shape required by §7.5 but **defers four CIDR choices to the
operator** - apiserver endpoint, kubelet probe source, mirror DNAT
source, registry egress. See the long "OPERATOR ACTION REQUIRED"
block at the top of the file and the [Production caveats](#production-caveats)
table below.

Workflow:

1. Roll out the DaemonSet without the overlay and verify
   `kubectl -n unbounded-system rollout status ds/gantry`,
   `p2p_cache_hit_total`, and a successful workload pull.
2. Copy the overlay into your own repository (or a Kustomize /
   Helm chart), edit every ipBlock marked "OPERATOR ACTION
   REQUIRED", and review against your CNI's hostPort SNAT
   behavior (`kubectl get nodes -o yaml | grep -A2 podCIDR`,
   etc.).
3. Apply with `kubectl apply -f your-overlay/networkpolicy.yaml`.
   Watch `/readyz` and any in-flight mirror pulls for at least one
   full image pull cycle - a wrong CIDR will surface as
   `dht routing table empty` (no peer libp2p traffic) or as
   containerd `connection refused` on 5000 (wrong mirror source
   CIDR), not as a NetworkPolicy validation error.
4. Roll back with `kubectl delete networkpolicy -n unbounded-system
   gantry-agent` if anything regresses.

Future hardening overlays (Pod Security Standards, dedicated
PriorityClass, alternative `hostNetwork: true` topology) will live
in the same directory and follow the same "deferred to operator,
not in default install" rule.

## Production caveats

A few configuration knobs that need operator attention before going
to production:

| Item | Where | What to change |
| --- | --- | --- |
| API server egress CIDR | `examples/networkpolicy.yaml` | The egress to TCP/443 and TCP/6443 defaults to `0.0.0.0/0` because managed control planes (EKS / GKE / AKS) and self-hosted clusters reach the apiserver at IPs that don't match a `namespaceSelector`. Replace with the apiserver's actual CIDR - `kubectl get endpoints kubernetes -n default -o jsonpath='{.subsets[*].addresses[*].ip}'` for self-hosted clusters; the managed-service docs for hosted control planes. |
| Origin registry egress | `examples/networkpolicy.yaml` | The egress to TCP/443 for origin pulls also defaults to `0.0.0.0/0`. If the cluster only pulls from a known set of registry endpoints (your private registry, ghcr.io, etc.), restrict this rule to those IPs or labels. |
| Kubelet probe source | `examples/networkpolicy.yaml` | Metrics ingress on TCP/9095 currently allows `0.0.0.0/0` so kubelet liveness/readiness probes (sourced from the node IP) reach the pod on strict CNIs. Replace with the node CIDR - `kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}'`. |
| Mirror port 5000 source | `examples/networkpolicy.yaml` | Ingress on TCP/5000 defaults to a deliberately-narrow `127.0.0.1/32` placeholder. Most CNIs (Calico, Cilium, and managed offerings) SNAT hostPort traffic so the in-pod source-IP after DNAT is the node IP, NOT 127.0.0.1 - the placeholder will then drop containerd's mirror pulls. Replace with the node CIDR (same command as the kubelet probe row). MUST NOT widen to the pod-network CIDR: that bypasses the `hostIP: 127.0.0.1` binding's loopback-only intent. |
| containerd socket access | `daemonset.yaml` | The pod mounts `/run/containerd`, rather than the socket file, so reconnects observe the replacement socket after containerd restarts. It runs with non-root UID 65532 and primary GID 0 because many nodes expose `containerd.sock` as `root:root` mode 0660. Validate this on your target node pool before production. If your runtime uses a dedicated socket group, patch `runAsGroup`/`fsGroup` to that group; if your policy forbids GID 0, adjust node socket ownership or run a site-specific privileged wrapper. **Clearing `containerd_socket` is no longer a valid escape hatch** - after plan-final-copilot-v2 §Phase 8 containerd is Gantry's sole storage backend; without socket access the agent has no content store to read from or write to. The `storage_mode` config value must remain `containerd`. |
| Kubernetes RBAC scope | `serviceaccount.yaml` | The agent's only Kubernetes access is `leases` (`get`, `list`, `create`, `update`) on the 64 chair Leases in its own namespace. It runs no informer and opens no watch, and it needs no access to Pods or Nodes. Review the `Role` to confirm scope hasn't drifted; a `ClusterRole` should not exist. |

### HEAD semantics on cache miss

`GET /v2/<repo>/blobs/<digest>` on a cache miss warms the cache as a
side effect; `HEAD` on the same URL does NOT. This is intentional
(see the comment block in `internal/mirror/mirror.go` at the HEAD
return after `writeBlobHeaders`) - caching a multi-GB blob just
because a client asked for its size would defeat the bandwidth
amplification fix Gantry exists to provide. A subsequent GET for
the same digest follows the cache-miss path normally and warms
the cache then.

If your client emits HEAD-then-GET patterns where you'd prefer to
amortize the origin metadata round-trip, raise the issue upstream
(containerd's puller, BuildKit's resolver, etc.) - those clients
generally have a one-shot resolve-and-pull mode that skips the
HEAD entirely.
