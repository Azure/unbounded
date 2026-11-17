---
title: "Configuration"
weight: 3
description: "Reference for every unbounded-net runtime setting, its CLI flag, and its default, including publication modes and rollout behavior."
---

The unbounded-net controller and node agent share a single runtime configuration
file. This page explains where that file comes from, how it interacts with
command-line flags, what each field does, and what you must do for a change to
take effect. It also covers the settings that are not in the file: the
controller and node agent flags that remain flag-only, the environment variables
the manifests inject, the ports you must allow through a firewall, and the
resource requests the shipped workloads use.

Read it as a lookup page. The tables give the exact YAML key, its flag, its type,
and the default that ships in the rendered manifests. The prose around each
table explains the behavior that the type alone cannot express.

## Runtime Configuration

### How the shared file is delivered

The operator or the rendered manifests create a ConfigMap named
`unbounded-net-config` in the `unbounded-system` namespace. It has two data keys:
`config.yaml`, which holds the YAML document described on this page, and
`LOG_LEVEL`, an integer that the manifests pass as the container's initial `-v`
argument. The controller Deployment mounts `config.yaml` at
`/etc/unbounded-net/config.yaml` with `subPath: config.yaml` and
`readOnly: true`, and the node DaemonSet mounts the same key at the same path
the same way. Both containers also receive `--config-file=/etc/unbounded-net/config.yaml`
explicitly, so the path in the manifest matches the built-in default rather than
relying on it.

The operator creates the ConfigMap only if it is absent, so edits you make to it
survive reconciliation.

The file has three top-level sections:

- `common` holds settings that both binaries understand.
- `controller` holds settings that only `unbounded-net-controller` applies.
- `node` holds settings that only `unbounded-net-node` applies.

Each binary parses the whole document but reads only its own section plus
`common`. A key placed in the wrong section is silently ignored, and so is a key
whose spelling does not match the schema. The loader does not enable strict
field checking, so a typo does not produce an error at startup. Copy key names
from the tables on this page rather than from memory.

The shipped ConfigMap sets a value for almost every controller and node key. In
a default installation, that value is therefore the effective default, even when
the compiled flag default differs. The tables below list the shipped value and
call out every place where the flag default is different, so you can predict
what happens if you point `--config-file` at a smaller file that omits a key.

### Path override, precedence, and validation

Both binaries accept `--config-file` to read a different path. The default is
`/etc/unbounded-net/config.yaml`. The controller registers the flag in its
general flag group, and the node agent registers the same name with the same
default.

Configuration is resolved in this order, from highest to lowest precedence:

1. A command-line flag that you explicitly set.
2. A nonempty value in the runtime configuration file.
3. The compiled default for that flag.

The binary enforces this by checking whether each flag changed on the command
line before it applies the corresponding file value. If you pass
`--status-detail-cache-ttl=60s`, a `controller.statusDetailCacheTTL` value in the
file is ignored. If you do not pass the flag, the file value is parsed and
applied, and an invalid value stops startup.

Startup is strict about the file itself. If the file is missing, unreadable, or
not valid YAML, `LoadRuntimeConfig` returns an error, the command's `PreRunE`
returns that error, and the process exits with status 1 before it builds a
Kubernetes client, writes a CNI file, or joins leader election. There is no
fallback to compiled defaults when the file is absent. In an operator-managed
installation this is safe because the ConfigMap is created if absent and the
volume mount is not optional. If you manage the manifests yourself and choose a
custom path, you must provide the file.

Individual fields have their own validation rules. The most important ones are:

| Setting | Rejected when |
|---------|---------------|
| `controller.statusDetailCacheTTL` | The duration is malformed, zero, or negative. |
| `controller.statusDetailRequestTimeout` | The duration is malformed, zero, or negative. |
| `controller.statusWsKeepaliveFailureCount` | The value is below 1. |
| `node.statusDetailMode` | The value is not `summary` or `full`. |
| `node.tunnelIPFamily` | The value is not `IPv4` or `IPv6`, other than an empty string, which becomes `IPv4`. |
| `node.statusWebsocketApiserverMode` | The value is not `never`, `fallback`, or `preferred`. |
| `node.mtu` | The value is negative. |
| `node.stunEnabled` | It is true while `stunHost` is empty or `stunPort` is outside 1 to 65535. |
| `node.geneveInterfaceName`, `node.vxlanInterfaceName`, `node.ipipInterfaceName` | A name is empty, longer than 15 bytes, duplicated, or equal to `unbounded0`. |
| `node.wireGuardInterfacePrefix` | The prefix is empty, longer than 10 bytes, contains `/`, or equals `unbounded0`. |

Duration fields use Go duration syntax, for example `300s`, `5m`, `1h`, or
`120ms`. An invalid duration fails startup with the field name in the error, so
you can find the key without guessing.

{{< callout type="warning" >}}
An empty string in the file usually means "not set", not "clear the default".
Keys whose loader checks for a nonempty string, such as `node.statusPushURL`,
`controller.oidcIssuerURL`, or `node.nodeName`, keep the flag default when the
file value is `""`. To clear such a value, omit the key or change the manifest
argument instead of writing an empty string.
{{< /callout >}}

### Reload behavior

Almost every setting is loaded once, when the process starts. Editing the
ConfigMap after that has no effect until the affected pods restart. The single
exception is `common.logLevel`: both binaries start a file watcher that watches
the parent directory of the configured path, because a Kubernetes ConfigMap
update swaps a `..data` symlink rather than writing the mounted file in place.
When the watcher sees that swap, it re-reads the file and calls the `v` flag's
setter with the new level.

The live reload has two practical consequences. First, you can raise or lower
verbosity without restarting anything. Second, the `LOG_LEVEL` ConfigMap key
only feeds the `-v=$(LOG_LEVEL)` argument at container start. Changing
`LOG_LEVEL` alone does nothing until a restart, and changing `common.logLevel`
alone changes the running verbosity without altering the starting value. Keep
both keys in sync in the ConfigMap if you want a restart and a live edit to
produce the same verbosity.

To restart the workloads after changing any other key, use the shipped object
names:

```bash
kubectl -n unbounded-system rollout restart deployment/unbounded-net-controller
kubectl -n unbounded-system rollout restart daemonset/unbounded-net-node
kubectl -n unbounded-system rollout status deployment/unbounded-net-controller
kubectl -n unbounded-system rollout status daemonset/unbounded-net-node
```

Restart the controller before the node agents when a change depends on both
sides, and read the publication-mode ordering below before changing
`node.statusDetailMode`. A mixed-version rollout is the usual cause of a status
page that shows errors for only some nodes.

### Summary publication and explicit details

Routine node publications used to carry a complete snapshot on every push:
peers, routes, BPF entries, and per-peer measurements. At scale that traffic
dominates the control plane, and most of the data is only needed when someone
asks about a specific node. The publication mode separates the two cases. In
`summary` mode, the node agent publishes overview data and aggregate counts on
its normal cadence, and collects the expensive details only for an explicit
single-node request. In `full` mode, it keeps the legacy behavior and publishes
the full snapshot.

Three settings control this behavior. All three are loaded at startup, so a
change requires a restart of the affected pods.

| Setting | CLI override | Type | Default | Allowed values |
|---------|--------------|------|---------|----------------|
| `node.statusDetailMode` | `--status-detail-mode` | string | `summary` | `summary`, `full` |
| `controller.statusDetailCacheTTL` | `--status-detail-cache-ttl` | duration | `300s` | Strictly positive |
| `controller.statusDetailRequestTimeout` | `--status-detail-request-timeout` | duration | `120s` | Strictly positive |

The controller also accepts `--status-detail-cache-ttl` and
`--status-detail-request-timeout` as flags. The node agent accepts only
`--status-detail-mode`; the two lifetimes belong to the controller, which owns
the cache and the request state.

#### What summary and full mode publish

In `summary` mode, the periodic WebSocket or HTTP publication contains the node
identity, readiness and health overview, aggregate counts, and node errors. It
does not contain peer lists, route entries, or BPF maps. That is enough for
dashboards, inventory, and most alerting, because those views aggregate rather
than inspect a single node.

In `full` mode, the node agent builds a full status snapshot and publishes it.
The WebSocket transport sends a full snapshot on its first write and after a
resynchronization, then typed deltas. The HTTP transport uses deltas only when
`node.statusPushDelta` is enabled, and sends a full snapshot otherwise. This is
the behavior that predates the summary split.

Bulk APIs and global broadcasts remain summary-only in either mode. Enabling
`full` restores full and delta publication from each node; it does not turn the
cluster-wide status JSON, the WebSocket stream that feeds the dashboard, or the
aggregated API responses back into detailed exports. Those responses are built
from overview data by design, so a single large response cannot carry every
node's routes and maps.

A node in `summary` mode advertises detail support on every publication. If the
controller's acknowledgment says the controller does not understand summaries,
the node records a `status-summary-unsupported` error and stops publishing
instead of silently falling back to full streaming. That behavior is deliberate:
an old controller would otherwise receive a message shape it cannot interpret,
and the operator would see missing data with no explanation.

#### Detail cache lifetime

The controller keeps received details in a leader-local, in-memory cache. The
cache lifetime starts when actual details arrive, not when a summary update or a
read happens. Only a successful store renews the deadline. Duplicate, late, or
mismatched replies do not replace data and do not renew the TTL, and neither do
reads. Reading an entry after its deadline returns a miss even if the proactive
expiry loop has not run yet, so an expired snapshot cannot be served by timing.

Continuous legacy full publications also refresh the cache, because they carry
real detail. That is one reason a cluster that has not yet moved to summary mode
sees a warmer cache than its request rate would suggest.

An expired request is reported as `Expired`, and a request that completed but
whose snapshot has since expired is also reported as `Expired` with a message
that the details expired or were replaced. Clients should treat both as "ask
again", not as a permanent failure.

#### Request timeout, coalescing, and delivery fallback

`controller.statusDetailRequestTimeout` is the end-to-end deadline for one
explicit detail request. It covers the WebSocket attempt, the HTTP pull attempt,
and any wait for a later status POST together. It is not a per-hop timeout.

Requests are coalesced per node. If two viewers ask for the same node while a
request is active, the second caller receives the result of the in-flight
request instead of starting another dispatch. Requests for different nodes start
independent dispatch workers, so one slow node does not serialize the rest.
Deadlines and TTL cleanup bound how long state remains, not how many requests
can exist during a burst. The controller's background aggregate-pull
concurrency limit, which defaults to 20 concurrent pulls when the cluster status
is rebuilt, does not apply to these explicit requests.

Delivery tries the following paths in order:

1. If an active, capable node WebSocket exists, the controller dispatches the
   request over it and waits for the reply.
2. If no WebSocket accepted the command, the controller pulls the node's status
   endpoint directly over HTTP with a five-second pull context.
3. If the direct pull fails, the request stays pending and is attached to the
   acknowledgment of the node's next authenticated status POST. This is the
   polling fallback: the node learns about the request from the response to a
   publication it was already making.

A successful WebSocket handshake is not enough to consider details delivered;
the write must be accepted. A node whose WebSocket exists but is unhealthy can
therefore fall through to the HTTP pull in the same request.

The fallback in step 3 requires outbound POST publication to resume. If both
publishers are disabled for a node, local HTTP diagnostics still work when you
can reach the node, but a failed pull has no polling path and the request ends at
its deadline. Keep `node.statusPushEnabled` or `node.statusWebsocketEnabled`
enabled on nodes you expect to inspect.

Explicit requests are available at two paths. The direct path is
`GET /status/node/<name>/details`, and the aggregated path is
`GET /apis/status.net.unbounded-cloud.io/v1alpha1/nodes/<name>/details`. Adding
`?live=true` forces a refresh instead of accepting a cached snapshot. A
subsequent `?requestId=<id>` read returns the result for that specific request.
Both paths require the same authentication and authorization as the rest of the
dashboard and status endpoints, and both are served only by the leader.

#### Upgrade ordering and rollback

Upgrade the controller and its consumers, meaning the dashboard, CLI, and any
tooling that reads the status API, before you restart node agents in `summary`
mode. A new node speaking to an old controller reports
`status-summary-unsupported` and stops publishing; it does not silently resume
full streaming. The explicit error is there so the mismatch is visible during
the rollout instead of appearing as missing nodes.

For a node-publication rollback, set `node.statusDetailMode: full` and restart
the node agents. That restores full and delta publication. It does not restore
detailed bulk exports, and it does not restore retired connectivity views that
were removed when the cluster-wide status surface became summary-only.

Requests and cached details are leader-local and in memory. A controller
restart, a leadership change, or a failover requires the client to retry. When
the leader releases expired references, the memory becomes eligible for garbage
collection, but nothing guarantees that RSS drops immediately. Size controller
memory for the detail cache you allow, not for the amount you expect to be live
at any one moment.

### Config structure

The entire file is small enough to read in one screen. The following is a
complete, minimal example that exercises all three sections. Every key shown
exists in the schema and is honored by the binary that reads it.

```yaml
common:
  azureTenantId: ""
  apiserverURL: ""
  logLevel: 2

controller:
  healthPort: 9999
  nodeAgentHealthPort: 9998
  informerResyncPeriod: 300s
  statusStaleThreshold: 90s
  statusDetailCacheTTL: 300s
  statusDetailRequestTimeout: 120s
  statusWebsocketKeepaliveInterval: 30s
  statusWsKeepaliveFailureCount: 3
  registerAggregatedAPIServer: true
  requireDashboardAuth: true
  oidcIssuerURL: ""
  oidcAudience: ""
  kubeProxyHealthInterval: 30s
  managedKubeProxy:
    enabled: true
    image: ""
  leaderElection:
    enabled: true
    leaseDuration: 30s
    renewDeadline: 15s
    retryPeriod: 10s
    resourceNamespace: unbounded-system
    resourceName: unbounded-net-controller

node:
  informerResyncPeriod: 3600s
  cniConfDir: /host/etc/cni/net.d
  cniConfFile: 10-unbounded.conflist
  ipamSocketPath: /run/unbounded-net/ipam.sock
  installIPAMPlugin: true
  ipamBinDir: /host/opt/cni/bin
  bridgeName: cbr0
  wireGuardDir: /host/etc/wireguard
  wireGuardPort: 51820
  mtu: 0
  healthPort: 9998
  statusDetailMode: summary
  preferredPrivateNetworkEncapsulation: GENEVE
  preferredPublicNetworkEncapsulation: WireGuard
  routeTableId: 252
  tunnelDataplaneMapSize: 16384
  tunnelIPFamily: IPv4
```

The sections below document every key, including the ones this minimal example
omits because their defaults are acceptable.

### Common settings

The `common` section holds the three settings shared by both binaries.

| YAML key | CLI flag | Type | Default | Behavior |
|----------|----------|------|---------|----------|
| `common.azureTenantId` | none | string | `""` | Tenant ID used to build Azure Portal links in the status UI. The controller seeds the value from the `AZURE_TENANT_ID` environment variable, and a nonempty file value replaces it. Set it only when the managed VMs are in a different tenant from the administrator's signed-in portal session. |
| `common.apiserverURL` | `--apiserver-url` | string | `""` | Overrides the Kubernetes API server URL for the process. Empty means use the kubeconfig when `--kubeconfig` is set, otherwise in-cluster discovery. |
| `common.logLevel` | `-v` | int | `2` | klog verbosity. Both binaries watch the file and apply changes without a restart. The starting verbosity still comes from `-v`, which the manifests set from the `LOG_LEVEL` ConfigMap key. |

`common.azureTenantId` is read only by the controller. The node agent parses the
key as part of the shared section but never uses it, so setting it has no effect
on a node.

The API server override is a literal host or URL, not a kubeconfig context. When
`--kubeconfig` is empty, the controller builds an in-cluster configuration and
then replaces its host with `common.apiserverURL` if the override is nonempty.
When `--kubeconfig` is set, the override is passed to
`clientcmd.BuildConfigFromFlags` as the master URL. Either way, the certificate
authority and credentials still come from the normal kubeconfig or service
account, so an override that points at a different cluster fails authentication
unless the credentials are valid there too.

### Controller settings

The `controller` section controls leader election, the status and health
server, the detail request machinery, managed kube-proxy, and dashboard
authentication.

| YAML key | CLI flag | Type | Default | Behavior |
|----------|----------|------|---------|----------|
| `controller.informerResyncPeriod` | `--informer-resync-period` | duration | `300s` | Full resync interval for the shared Kubernetes informers. Values of zero or less are ignored and the previous value stays. |
| `controller.healthPort` | `--health-port` | int | `9999` | Port for the HTTPS health, readiness, status, and status push server. `0` disables the server. |
| `controller.nodeAgentHealthPort` | `--node-agent-health-port` | int | `9998` | Port the controller uses when it links to or pulls from a node agent's status endpoint. |
| `controller.statusStaleThreshold` | `--status-stale-threshold` | duration | `90s` | Age at which a pushed node status is considered stale, which makes the cluster status fall back to pulling from the node. |
| `controller.statusDetailCacheTTL` | `--status-detail-cache-ttl` | duration | `300s` | Lifetime of cached node details, measured from receipt. Must be strictly positive. |
| `controller.statusDetailRequestTimeout` | `--status-detail-request-timeout` | duration | `120s` | End-to-end deadline for an explicit detail request. Must be strictly positive. |
| `controller.statusWebsocketKeepaliveInterval` | `--status-ws-keepalive-interval` | duration | `30s` | Interval between controller-side keepalive pings on node status WebSockets. `0` disables pings. The compiled flag default is `10s`; the shipped ConfigMap raises it to `30s`. |
| `controller.statusWsKeepaliveFailureCount` | `--status-ws-keepalive-failure-count` | int | `3` | Sequential keepalive failures before the controller closes a node status WebSocket. Must be at least 1. The compiled flag default is `2`. |
| `controller.registerAggregatedAPIServer` | `--register-aggregated-apiserver` | bool | `true` | Serves node status push endpoints under the aggregated API paths in addition to the direct controller paths. |
| `controller.requireDashboardAuth` | `--require-dashboard-auth` | bool | `true` | Requires authentication and SubjectAccessReview authorization for the dashboard and status endpoints. This key is not present in the shipped ConfigMap, so the flag default applies. |
| `controller.oidcIssuerURL` | `--oidc-issuer-url` | string | `""` | Explicit OIDC issuer for local validation of node service account tokens. Empty means discover from the controller's mounted token and fall back to TokenReview. |
| `controller.oidcAudience` | `--oidc-audience` | string | `""` | Audience required in locally validated node tokens. Empty infers it from the mounted token during discovery, or uses the explicit issuer URL. |
| `controller.kubeProxyHealthInterval` | `--kube-proxy-health-interval` | duration | `30s` | Interval between kube-proxy health checks on the controller node. `0` disables the check. |
| `controller.managedKubeProxy.enabled` | `--managed-kube-proxy` | bool | `true` | Creates kube-proxy DaemonSets for unbounded-managed Site nodes that the provider's kube-proxy does not cover. |
| `controller.managedKubeProxy.image` | `--managed-kube-proxy-image` | string | `""` | Image for managed kube-proxy DaemonSets. Empty means derive one from the provider image or the default repository. |
| `controller.leaderElection.enabled` | `--leader-elect` | bool | `true` | Enables leader election for the controller. |
| `controller.leaderElection.leaseDuration` | `--leader-elect-lease-duration` | duration | `30s` | How long a non-leader waits before forcing acquisition. |
| `controller.leaderElection.renewDeadline` | `--leader-elect-renew-deadline` | duration | `15s` | How long the acting leader retries renewal before giving up. |
| `controller.leaderElection.retryPeriod` | `--leader-elect-retry-period` | duration | `10s` | Delay between leader election attempts. |
| `controller.leaderElection.resourceNamespace` | `--leader-elect-resource-namespace` | string | `unbounded-system` | Namespace that holds the leader election lease. |
| `controller.leaderElection.resourceName` | `--leader-elect-resource-name` | string | `unbounded-net-controller` | Name of the leader election lease. |

The controller also reads `node.mtu` even though the key lives in the node
section. It uses the value to check every node's
`net.unbounded-cloud.io/tunnel-mtu` annotation and report a mismatch when a node
cannot support the configured MTU. A value of `0` is normalized to `1280` for
that check, while the node agent treats `0` as automatic. See
[MTU guidance](#mtu-guidance) for the full behavior.

A few controller flags have no YAML key. They are either process identity or
one-shot modes, so the ConfigMap cannot express them:

| Flag | Type | Default | Why it is flag-only |
|------|------|---------|---------------------|
| `--kubeconfig` | string | `""` | Local development and out-of-cluster runs only. |
| `--dry-run` | bool | `false` | Runs one evaluation and prints proposed changes. Pod CIDR assignment for Sites is not supported in dry-run. |
| `--force-not-leader` | bool | `false` | Keeps a replica in standby. Requires leader election to stay enabled. |
| `--node-token-lifetime` | duration | `4h` | Lifetime of HMAC tokens issued to node agents. |
| `--viewer-token-lifetime` | duration | `30m` | Lifetime of HMAC tokens issued to dashboard viewers. |
| `-v` | int | `0` | Starting verbosity, normally supplied by the manifest from `LOG_LEVEL`. |

## Controller Configuration

### Managed kube-proxy

The controller creates a kube-proxy DaemonSet for each Site whose nodes are not
covered by the cloud provider's kube-proxy. Provider-managed clusters typically
run one kube-proxy DaemonSet with a node selector that matches only the
provider's own nodes. Nodes that join through Unbounded do not match that
selector, so they would have no service routing without a second DaemonSet.
Managed kube-proxy fills that gap per Site, using the Site's pod CIDR and the
labels the controller assigns to Site nodes.

Set `controller.managedKubeProxy.enabled` to `false`, or pass
`--managed-kube-proxy=false`, when another component already programs service
routing on those nodes. The controller logs that the managed kube-proxy
controller is disabled and does not create or delete any DaemonSets.

When the operator manages unbounded-net, the image can also be set through the
operator's overrides ConfigMap. That path passes `--managed-kube-proxy-image` to
the controller container, so it takes precedence over
`controller.managedKubeProxy.image`. See
[Overriding the managed kube-proxy image]({{< relref "reference/workload-overrides" >}}).

#### Image selection

With an empty `controller.managedKubeProxy.image`, the controller reads the
`kube-proxy` container image from the `kube-system/kube-proxy` DaemonSet. A
provider image that carries a tag or digest is reused exactly, including its
digest. If the provider image has no tag, only its repository is kept. If there
is no provider image at all, the repository falls back to
`registry.k8s.io/kube-proxy`.

An explicit `controller.managedKubeProxy.image` changes the rules:

- A reference with a tag or digest is used as-is. The controller does not read
  the cluster and does not search a registry.
- A reference without a tag or digest, such as
  `myregistry.example.com/kube-proxy`, keeps its repository and takes the tag
  from `kube-system/kube-proxy` if the provider image has one.
- If there is no provider tag, the controller searches the chosen repository
  for the API server's `major.minor.patch` version and prefers the highest
  numeric maintenance suffix. For example, `v1.36.2-2` beats `v1.36.2-1` and
  `v1.36.2`, and any of them beats no tag at all.
- The controller never chooses `latest` automatically. An explicit `:latest` is
  preserved because it is an explicit reference.

An image that names a registry port is parsed correctly: the colon in
`localhost:5000/kube-proxy` belongs to the name, not to a tag. The same parser
rejects malformed names, tags, and digests before any registry call, so a typo
fails with a clear message rather than a confusing pull error later.

#### Registry access for API server version discovery

The registry search runs in the controller Pod, not on the nodes. The Pod needs
DNS resolution and outbound network access to the selected registry. HTTPS is
the normal case; plain HTTP is allowed for `localhost` and private IP addresses,
so a local mirror can be reached without TLS. The search is anonymous: node pull
credentials and image pull secrets are not used, and no credentials are
forwarded across registry origins. If your registry requires authentication for
tag listing, an unversioned custom image cannot be resolved through the search
path, so give the image an explicit tag or digest or use a registry that allows
anonymous listing.

The search is bounded. It lists only the configured repository, never a
different registry or path, with a page size of 1000 tags and at most 100 pages.
The whole lookup has a 30-second timeout. Redirects are followed up to 10 times,
and an HTTPS-to-HTTP redirect is refused. Registry metadata is capped at 32 MiB.
Matching is exact on the server version, so `1.36.2` does not select `1.36.20`
or a release candidate.

The version source and registry query differ by configuration:

| Configured image | Provider image | Version source | Registry query |
|------------------|----------------|----------------|----------------|
| Tag or digest | Not read | Explicit reference | None |
| Empty | Versioned reference | Provider reference | None, the provider image is reused as-is |
| Empty | Unversioned reference | API server version | Yes, in the provider's repository |
| Empty | Missing or unreadable after the retry budget | API server version | Yes, in `registry.k8s.io/kube-proxy` |
| Repository only | Versioned reference | Provider tag | None |
| Repository only | Unversioned or missing | API server version | Yes, in the configured repository |

#### Startup and failure handling

The image is resolved once, when the process starts leading. Resolution runs in
a background goroutine so a slow registry cannot block the other controllers or
the shared informers. The controller starts the managed kube-proxy controller
only after the image resolves, then starts the informer factories again so any
informers the new controller requested are launched.

Reading `kube-system/kube-proxy` is retried with exponential backoff from 10
seconds up to a 5-minute cap. A `NotFound` result falls back immediately,
because there is simply no provider image to read. Other read errors continue
for a 12-hour budget that covers both requests and waits. When the budget
expires, the controller logs the attempt count and falls back to the default
repository. Cancellation of the parent context, which happens when leadership
ends, stops discovery without falling back.

If discovery or registry search fails, the managed kube-proxy controller does
not start. The rest of the controller keeps running, and the failure is logged
with the repository and version it was trying to match. The usual causes are
missing outbound access to the registry, a registry that does not allow
anonymous tag listing, or an API server version for which the repository has no
matching tag.

### Leader Election

Leader election is enabled by default. One replica holds the lease and runs the
Site controller, gateway pool controller, node assignment controller, peering
aggregation controller, managed kube-proxy, and the detail request manager. The
other replicas serve health and status endpoints but do not mutate cluster
state.

The lease timings are the standard Kubernetes values: a 30-second lease, a
15-second renew deadline, and a 10-second retry period. Lowering the renew
deadline makes failover faster but also makes the leader give up during a brief
API server hiccup. Raising it reduces false failovers at the cost of a longer
window with no active leader.

`--force-not-leader` keeps a replica in standby even if it could acquire the
lease. It is intended for maintenance and for verifying that the standby path
serves health endpoints correctly. It requires leader election to remain
enabled, and the controller refuses to start if you combine it with
`--leader-elect=false`.

The mutating node webhook assigns Site labels only. It does not allocate pod
CIDRs, and it admits a node without a matching Site unchanged; reconciliation
handles nodes whose internal IPs appear after admission. Pod CIDR allocation
happens during node sync, and only while the controller has confirmed it holds
the lease. The fence is based on the renew deadline: a leader whose renewals stop
succeeding stops allocating before another replica can acquire the lease, so two
replicas cannot assign overlapping CIDRs during a failover.

Allocators are seeded before they serve node workers. Seeding preserves existing
and unconfirmed allocations, including blocks of a different size that overlap
candidate CIDRs. A pending allocation can move to a different allocator only
when its IP families, block sizes, and pool containment match. Otherwise,
reconciliation waits until a changed Node UID or resourceVersion proves the
previous patch cannot apply before it allocates replacement CIDRs. Successful
patches stay reserved until the Node informer observes their CIDRs, even if the
original assignment is removed in the meantime. Reservations copied into a new
allocator are released from every recipient when abandoned, unless another Node
or pending assignment still owns the CIDR.

### Health and Monitoring

The controller serves HTTPS health endpoints on `--health-port`, which defaults
to `9999`. `/healthz` checks API connectivity, and `/readyz` reports readiness.
When local OIDC validation is selected, readiness stays 503 until the Pod and
service account informer caches have synced. Readiness does not wait for
leadership or for the Site CRD caches, so a standby replica can be ready while
it holds no lease.

`--node-agent-health-port` defaults to `9998` and tells the controller where to
find a node agent's status endpoint for dashboard links and for pull fallback.
Change it only if you also change the node agent's health port; the two are
separate settings and are not derived from each other.

`--status-stale-threshold` defaults to `90s`. When the newest pushed status for a
node is older than the threshold, the cluster status marks it stale and tries to
pull current overview data from the node. If the pull also fails, the response
uses the stale cached data and includes a fetch error that names the age and the
pull failure, so the dashboard shows both the last known state and why it is not
fresh.

`--register-aggregated-apiserver` defaults to `true` and exposes the node status
push and WebSocket endpoints through the aggregated API server paths. Disable it
only when you have removed the APIService, for example because your cluster does
not run the aggregation layer.

`--informer-resync-period` defaults to `300s`. This is a full resync, not a watch
reconnect; raising it reduces API server load on large clusters at the cost of
slower recovery from missed events. Values of zero or less are ignored.

`--kube-proxy-health-interval` defaults to `30s` and controls a local check on
the controller's own node. `0` disables the check.

### Logging

`-v` sets the klog verbosity from 0 to 10. The compiled default is `0`, but both
shipped manifests pass `-v=$(LOG_LEVEL)`, and the ConfigMap sets `LOG_LEVEL` to
`2`. A default installation therefore logs at level 2. `--logtostderr` defaults
to `true`, which is what you want in a container; the file logging flags exist
because the binaries embed the standard klog flag set.

`common.logLevel` is the live-reloadable control. It is parsed as an optional
integer, and the watcher ignores values below 0. Both binaries apply it to the
running `v` flag, so a ConfigMap edit can raise verbosity for a specific problem
and then lower it again without a restart.

## Node Agent Configuration

The node agent runs as a DaemonSet on every node that participates in the
fabric, including gateway nodes. It reads the `node` section of the shared file,
writes the CNI configuration, programs tunnel interfaces and routes, manages
WireGuard keys, runs the IPAM server, and publishes status. Most node settings
control dataplane details that you should not need to change, but the table
below is complete so you can confirm a value rather than infer it.

| YAML key | CLI flag | Type | Default | Behavior |
|----------|----------|------|---------|----------|
| `node.informerResyncPeriod` | `--informer-resync-period` | duration | `3600s` | Full resync interval for the node agent's informers. The compiled flag default is `600s`. |
| `node.nodeName` | `--node-name` | string | `""` | Node name. Defaults to the `NODE_NAME` environment variable, which the manifest sets from `spec.nodeName`. Startup fails if the result is empty. |
| `node.stunEnabled` | `--stun-enabled` | bool | `false` | Discovers the node's public IP with STUN when no explicit override or provider ExternalIP exists. |
| `node.stunHost` | `--stun-host` | string | `""` | STUN server host. Required when STUN is enabled. |
| `node.stunPort` | `--stun-port` | int | `3478` | STUN server port. |
| `node.stunRecheckInterval` | `--stun-recheck-interval` | duration | `1h` | Interval between STUN public IP rechecks. Must be positive. |
| `node.cniConfDir` | `--cni-conf-dir` | string | `/host/etc/cni/net.d` | Directory where the agent writes the CNI configuration, as seen inside the container. The compiled flag default is `/etc/cni/net.d`. |
| `node.cniConfFile` | `--cni-conf-file` | string | `10-unbounded.conflist` | Name of the generated CNI configuration file. |
| `node.ipamSocketPath` | `--ipam-socket-path` | string | `/run/unbounded-net/ipam.sock` | Unix socket shared with the `unbounded-ipam` plugin. Must be a valid socket path. |
| `node.installIPAMPlugin` | `--install-ipam-plugin` | bool | `true` | Installs or upgrades the bundled IPAM binary on startup. |
| `node.ipamBinDir` | `--ipam-bin-dir` | string | `/host/opt/cni/bin` | Host-mounted CNI binary directory used for installation. Must be a clean absolute path when installation is enabled. |
| `node.manageSameSitePodRoutes` | `--manage-same-site-pod-routes` | bool | `false` | Manages same-site PodCIDR routes even when the Site does not manage the CNI plugin. |
| `node.bridgeName` | `--bridge-name` | string | `cbr0` | Name of the pod bridge interface. |
| `node.wireGuardDir` | `--wireguard-dir` | string | `/host/etc/wireguard` | Directory for WireGuard keys, as seen inside the container. The compiled flag default is `/etc/wireguard`. |
| `node.wireGuardPort` | `--wireguard-port` | int | `51820` | Listen port for the node-to-node WireGuard interface. |
| `node.enablePolicyRouting` | `--enable-policy-routing` | bool | `false` | Deprecated. Enables connmark/fwmark/ip-rule policy routing on gateway interfaces. Kept for backward compatibility only. |
| `node.mtu` | `--mtu` | int | `0` | Upper bound for tunnel and bridge MTUs. `0` derives each value from the underlay route and encapsulation overhead. Negative values are rejected. |
| `node.healthPort` | `--health-port` | int | `9998` | Port for the node agent's HTTPS health and status server. `0` disables it. |
| `node.healthCheckPort` | `--healthcheck-port` | int | `9997` | UDP health check probe port. Present in the shipped ConfigMap but not applied by the loader; use the flag or rely on the built-in default. |
| `node.baseMetric` | `--base-metric` | int | `1` | Base metric for programmed routes. Present in the shipped ConfigMap but not applied by the loader; use the flag or rely on the built-in default. |
| `node.statusPushEnabled` | `--status-push-enabled` | bool | `true` | Enables periodic HTTP status push to the controller. |
| `node.statusPushURL` | `--status-push-url` | string | `""` | Explicit controller push URL. Empty builds one from the controller Service environment variables. |
| `node.statusPushInterval` | `--status-push-interval` | duration | `60s` | Interval between HTTP status pushes. |
| `node.statusPushApiserverInterval` | `--status-push-apiserver-interval` | duration | `60s` | Minimum interval between aggregated API server push attempts. |
| `node.statusPushDelta` | `--status-push-delta` | bool | `true` | Uses deltas for periodic HTTP pushes. A rejected delta causes the next push to be full. |
| `node.statusDetailMode` | `--status-detail-mode` | string | `summary` | Routine publication mode: `summary` or `full`. |
| `node.statusWebsocketEnabled` | `--status-ws-enabled` | bool | `true` | Enables the WebSocket status transport. |
| `node.statusWebsocketURL` | `--status-ws-url` | string | `""` | Explicit controller WebSocket URL. Empty uses automatic endpoint selection. |
| `node.statusWebsocketApiserverMode` | `--status-ws-apiserver-mode` | string | `fallback` | `never` uses direct endpoints only; `fallback` prefers direct endpoints and uses aggregated endpoints as a fallback; `preferred` is an alias for `fallback`. |
| `node.statusWebsocketApiserverURL` | `--status-ws-apiserver-url` | string | `wss://$(KUBERNETES_SERVICE_HOST)/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews` | Aggregated WebSocket URL, also used to derive the aggregated push URL. |
| `node.statusWebsocketApiserverStartupDelay` | `--status-ws-apiserver-startup-delay` | duration | `60s` | Delay from the start of a direct transport outage before aggregated fallback is allowed. Must be zero or greater. |
| `node.statusWebsocketKeepaliveInterval` | `--status-ws-keepalive-interval` | duration | `10s` | Interval between node WebSocket keepalive pings. `0` disables pings. |
| `node.statusWsKeepaliveFailureCount` | `--status-ws-keepalive-failure-count` | int | `2` | Sequential keepalive failures before the node reconnects. Must be at least 1. |
| `node.removeConfigurationOnShutdown` | `--remove-configuration-on-shutdown` | bool | `false` | Removes WireGuard configuration, routes, masquerade rules, and tunnel interfaces on shutdown. |
| `node.shutdownRemoveWireGuardConfiguration` | `--shutdown-remove-wireguard-configuration` | bool | `false` | Deprecated. Use `removeConfigurationOnShutdown`. |
| `node.shutdownRemoveIPRoutes` | `--shutdown-cleanup-netlink` | bool | `false` | Deprecated. Use `removeConfigurationOnShutdown`. |
| `node.shutdownRemoveMasqueradeRules` | `--shutdown-remove-masquerade-rules` | bool | `false` | Deprecated. Use `removeConfigurationOnShutdown`. |
| `node.criticalDeltaEvery` | `--status-critical-interval` | duration | `15s` | Maximum publish frequency for critical changes. Changed fields are batched up to this interval. |
| `node.statsDeltaEvery` | `--status-stats-interval` | duration | `60s` | Maximum publish frequency for statistics changes. Statistics carry a freshness timestamp even when values are unchanged. |
| `node.fullSyncEvery` | `--status-full-sync-interval` | duration | `2m` | Forced full status sync interval so the controller periodically receives complete state. |
| `node.preferredPrivateNetworkEncapsulation` | `--preferred-private-encap` | string | `GENEVE` | Encapsulation chosen for private links when the CRD says `Auto`. Valid values are `GENEVE`, `IPIP`, `VXLAN`, and `WireGuard`. |
| `node.preferredPublicNetworkEncapsulation` | `--preferred-public-encap` | string | `WireGuard` | Encapsulation chosen for public links when the CRD says `Auto`. Valid values are `WireGuard`, `IPIP`, `GENEVE`, and `VXLAN`. |
| `node.healthFlapMaxBackoff` | `--health-flap-max-backoff` | duration | `120s` | Maximum backoff for health check flap dampening. Overrides the per-profile backoff. |
| `node.kubeProxyHealthInterval` | `--kube-proxy-health-interval` | duration | `30s` | Interval between local kube-proxy health checks. `0` disables the check. |
| `node.routeTableId` | `--route-table-id` | int | `252` | Routing table used for managed routes. `254` selects the main table. |
| `node.netlinkResyncPeriod` | `--netlink-resync-period` | duration | `300s` | Interval between full netlink cache resyncs. |
| `node.tunnelDataplaneMapSize` | `--tunnel-dataplane-map-size` | int | `16384` | Maximum LPM trie entries in the eBPF tunnel map. |
| `node.tunnelIPFamily` | `--tunnel-ip-family` | string | `IPv4` | Underlay address family for tunnel encapsulation: `IPv4` or `IPv6`. An empty value becomes `IPv4`. |
| `node.vxlanSrcPortLow` | `--vxlan-src-port-low` | int | `47891` | Low bound of the VXLAN UDP source port range. |
| `node.vxlanSrcPortHigh` | `--vxlan-src-port-high` | int | `47922` | High bound of the VXLAN UDP source port range. |
| `node.geneveInterfaceName` | `--geneve-interface` | string | `geneve0` | Shared flow-based GENEVE interface name. |
| `node.vxlanInterfaceName` | `--vxlan-interface` | string | `vxlan0` | Shared flow-based VXLAN interface name. |
| `node.ipipInterfaceName` | `--ipip-interface` | string | `ipip0` | Shared flow-based IPIP interface name. |
| `node.wireGuardInterfacePrefix` | `--wireguard-interface-prefix` | string | `wg` | Prefix for per-port WireGuard interfaces. The runtime name is the prefix followed by the port. |

Three keys deserve an explicit warning. `node.healthCheckPort` and
`node.baseMetric` appear in the shipped ConfigMap but have no field in the
runtime schema, so the loader ignores them and only `--healthcheck-port` and
`--base-metric` change the behavior. The ConfigMap also
writes `node.cleanupNetlinkOnShutdown`, while the loader reads
`node.shutdownRemoveIPRoutes` for the same deprecated flag, so that template key
has no effect. If you depend on netlink cleanup at shutdown, set
`node.removeConfigurationOnShutdown: true` and avoid the deprecated keys
entirely.

The GENEVE destination port, GENEVE VNI, and VXLAN destination port have no YAML
key at all. They are flag-only:

| Flag | Type | Default | Behavior |
|------|------|---------|----------|
| `--geneve-port` | int | `6081` | GENEVE UDP destination port. |
| `--geneve-vni` | int | `1` | GENEVE Virtual Network Identifier. |
| `--vxlan-port` | int | `4789` | VXLAN UDP destination port. |
| `--healthcheck-port` | int | `9997` | UDP port for health check probes. Use the same port on every node so replies are accepted. |
| `--base-metric` | int | `1` | Base metric for programmed routes. |
| `--kubeconfig` | string | `""` | Out-of-cluster kubeconfig, for development only. |

### Large-cluster neighbor tables

The kernel's IPv4 ARP and IPv6 neighbor tables have fixed-size garbage
collection thresholds. A mesh with thousands of peers creates one neighbor entry
per unique next hop, so on a large cluster the default thresholds fill and new
allocations fail. The node agent sizes both tables automatically instead of
requiring an operator to estimate the mesh size and set `gc_thresh*` by hand.

The sizing works as follows:

1. Build the set of mesh endpoints from enabled peers and gateways. WireGuard
   peers that are neither in a peered Site nor reachable through a peered
   GatewayPool are skipped, because they do not become neighbors.
2. Resolve a route to each endpoint and collect unique `(egress link, next-hop
   IP)` pairs. Multipath routes contribute every live next hop, and hops marked
   dead or link-down are skipped.
3. Count pairs separately for IPv4 and IPv6. Loopback links, links without ARP
   (`IFF_NOARP`), and interfaces managed by the agent itself do not become
   kernel neighbors and are excluded.
4. Set `gc_thresh1`, `gc_thresh2`, and `gc_thresh3` from the count with roughly
   10 percent of headroom between thresholds.

Existing values act as floors. The agent never lowers a threshold, never writes
a value below the kernel's current value, and enforces
`gc_thresh1 <= gc_thresh2 <= gc_thresh3`. It raises the hard limit first and
works down, so a crash between writes cannot leave the three values out of
order. Thresholds are table-wide, not per interface, which is why the count is a
deduplicated set of next hops rather than a sum over links. Other consumers of
the same table, such as a host network agent or a local workload, share the
space, so treat the computed values as a floor for the mesh rather than as a
guarantee that everything else fits.

A zero count skips that family, so a node with no IPv6 mesh peers does not touch
the IPv6 table. If the IPv6 table does not exist at all, the agent logs that it
is skipping IPv6 tuning and continues. Any other read or write error fails the
reconciliation and is surfaced as a mesh neighbor capacity error, because a
partially applied threshold set is not something to hide.

Sizing is reapplied on every peer reconciliation and at every agent start. There
is no shutdown restore: the raised thresholds stay in place after the agent
stops. That is intentional. Lowering `gc_thresh3` while entries still exist can
evict live neighbors and break the mesh, and the next agent start would raise
the values again anyway.

The count is limited to about 1.65 billion per family to keep the arithmetic
from overflowing a Go integer. That ceiling is far above any real mesh; hitting
it means the endpoint set is malformed, not that the node is too large.

#### Neighbor table exhaustion warnings

Automatic sizing prevents most exhaustion, but it cannot prevent every case. A
container or external process can create neighbors the agent did not count, and
the kernel can still hit the hard limit between reconciliations. The agent
therefore watches for pressure and reports it as a node error.

Occupancy is the number of GC-managed entries in the family's neighbor table.
Permanent entries, externally learned entries, entries flagged as validated, and
entries on loopback interfaces are excluded, because the kernel does not garbage
collect them and they cannot be the cause of an allocation failure. When
occupancy reaches `gc_thresh3`, the node reports `neighborTableFullIPv4` or
`neighborTableFullIPv6` with the current count and the limit.

Allocation failures are counted separately. The kernel exposes a per-CPU
`table_fulls` counter in `/proc/net/stat/arp_cache` and
`/proc/net/stat/ndisc_cache`. The agent sums the columns and compares the total
with its previous sample. When the total increases, the node reports
`neighborTableOverflowIPv4` or `neighborTableOverflowIPv6` with the number of
new failures and the before and after values. The check is a baseline delta, so
an old counter value does not produce a warning at startup. A missing stat file
is skipped at high verbosity; a malformed file reports
`neighborTableCheckFailed`.

These warnings are ordinary node errors, so they appear in the status payload
and on the node's detail view while the condition persists. They are re-logged
at a bounded interval rather than on every status collection, which keeps a full
table from flooding the log. Clearing the condition clears the warning on the
next refresh.

#### Other scale-sensitive sysctls

The node agent deliberately does not tune the rest of the kernel network stack.
Forwarding, reverse-path filtering, source validation, and two conntrack
behaviors are set by the init container in `images/net/node-agent-init.sh`, and
the neighbor tables are managed by the agent. Everything else is left to the
host image or to you.

The init container runs in the host namespaces and sets
`net.ipv4.ip_forward=1`, `net.ipv6.conf.all.forwarding=1` when IPv6 exists,
`net.ipv4.conf.all.src_valid_mark=1` and the default equivalent,
`net.ipv4.conf.all.rp_filter=0` and the default equivalent, then the same values
on every per-interface `src_valid_mark` and `rp_filter` file. It also sets
`net.netfilter.nf_conntrack_be_liberal=1` and
`net.netfilter.nf_conntrack_ignore_invalid_rst=1` on a best-effort basis,
because those files are absent on some kernels. Disabling reverse-path
filtering is required: decapsulated overlay packets have source addresses that
do not match the arrival interface, and both strict and loose filtering would
drop them.

The agent monitors conntrack usage and reports `conntrackNearMax` when
`nf_conntrack_count` reaches 80 percent of `nf_conntrack_max`. That is a warning,
not an adjustment. Sizing the table and its buckets is a host decision, and so
are the receive processing budget (`net.core.netdev_max_backlog` and
`dev_weight`), socket buffer maximums (`rmem_max` and `wmem_max`), listen and SYN
backlogs (`somaxconn` and `tcp_max_syn_backlog`), and the IPv4 and IPv6 fragment
memory limits and timeouts. All of these depend on memory, kernel version, and
workload. Measure under the real workload rather than scaling from node count
alone: a few pods with many short-lived connections can exhaust conntrack before
a much larger cluster of long-lived flows does.

Two settings from older tuning guides are obsolete. `net.ipv4.ipfrag_low_thresh`
is ignored on current kernels, which size the fragment queues from the high
threshold and the reassembly timeout instead. The route cache and its
`gc_interval` were removed with the old routing cache, so modern kernels use a
per-prefix FIB and have nothing for that tunable to control. Do not carry either
setting into a node image; spend the effort on the live limits instead.

### General

The node name is the one required setting. The manifest supplies it from
`spec.nodeName` through `NODE_NAME`, and `--node-name` overrides it. If the
resolved name is empty, the agent logs that the node name is required and exits.
There is no fallback to the hostname, because a wrong name would silently
attribute routes and status to another node.

`node.informerResyncPeriod` defaults to `3600s` on the node, which is longer than
the controller's `300s`. Nodes watch fewer resource kinds, and a resync
principally repairs missed events; the longer interval keeps the aggregate watch
load manageable on large clusters. Raise it further for very large clusters, as
described in [Tuning recommendations](#tuning-recommendations).

`node.routeTableId` defaults to `252` and holds the routes the agent programs.
Setting it to `254` puts managed routes in the main table. Change it only when
another component already uses table 252 and you cannot move that component
instead.

`node.preferredPrivateNetworkEncapsulation` and
`node.preferredPublicNetworkEncapsulation` are the tie-breakers for CRD
`tunnelProtocol: Auto`. They do not override an explicit protocol on a Site, a
peering, an assignment, or a pool. See
[Tunnel protocol selection](#tunnel-protocol-selection).

### CNI Configuration

The node agent writes a CNI conflist named `10-unbounded.conflist` into
`/etc/cni/net.d` on the host, which the container sees as
`/host/etc/cni/net.d`. The write is atomic: the agent writes a temporary file
and renames it into place, so the runtime never reads a partial document. It
also warns when a lexicographically earlier CNI file exists, because that file
wins and unbounded-net is then not the active plugin.

The generated document uses CNI version `0.4.0` and network name
`unbounded-net`. It contains three plugins:

1. A `bridge` plugin on `node.bridgeName`, marked as gateway and default
   gateway, with `forceAddress` and hairpin mode enabled, masquerade disabled,
   the fabric MTU, and IPAM of type `unbounded-ipam`.
2. A `portmap` plugin with the `portMappings` capability.
3. A `loopback` plugin.

Masquerade is disabled in the conflist because the agent manages its own
masquerade rules for cross-Site traffic. Enabling it here as well would
double-NAT traffic that already leaves through a tunnel.

`node.installIPAMPlugin` controls installation of the bundled `unbounded-ipam`
binary independently of `Site.spec.manageCniPlugin`. Setting it to `false` lets
you use a preinstalled binary. The agent still serves IPAM either way, and
generated conflists still name `unbounded-ipam`. The default DaemonSet mounts
the host's `/opt/cni/bin` at `/host/opt/cni/bin` for installation and shares
`/run/unbounded-net` with the host for the socket.

The IPAM exchange is a length-delimited protobuf request over
`node.ipamSocketPath`, not HTTP or gRPC, and it is not exposed on the agent's
TCP status endpoint. Protocol version 1 is used, with a 64 KiB message limit and
a five-second deadline. On ADD, the plugin asks the agent for the node's verified
pod CIDRs. A missing assignment, an unverified assignment, an unavailable
socket, or a malformed or timed-out reply fails the ADD without allocating an
address. The runtime can retry pod sandbox creation. There is no disk-cached CIDR
and no direct host-local fallback.

Allocation itself uses the upstream host-local allocator and store, so
reservations remain compatible under `/var/lib/cni/networks/<network-name>`.
Requested IPs must belong to the verified ranges, and caller-supplied IPAM
settings and `runtimeConfig.ipRanges` are rejected so they cannot bypass
verification. DEL releases local reservations without contacting the agent, and
CHECK and VERSION also work without it. DEL is safe to repeat.

Set `node.ipamSocketPath` and, for a third-party CNI, the matching optional
`ipam.socketPath` to use a nondefault socket. Managed conflists include the
socket path automatically when it differs from the default. Custom socket
directories and a custom `node.ipamBinDir` require matching writable host mounts
through workload overrides, the installation directory must already exist, and
the socket directory must be owned by the agent user and not writable by others.
The agent keeps a `<socketPath>.lock` file beside the socket; do not remove it
while an agent is running.

### MTU Guidance

The `node.mtu` setting is an optional upper bound for tunnel interfaces, per
destination routes, and the CNI bridge. Each encapsulation adds a fixed
overhead, so the largest payload you can carry is the underlay MTU minus that
overhead:

| Protocol | Overhead | Encrypted |
|----------|----------|-----------|
| WireGuard | 80 bytes | Yes |
| GENEVE | 58 bytes | No |
| VXLAN | 50 bytes | No |
| IPIP | 20 bytes | No |
| None | 0 bytes | No |

With the default value of `0`, the agent looks up the actual route to each peer,
subtracts the selected encapsulation overhead, and uses the lowest applicable
Site, peering, assignment, or pool `tunnelMTU`. A WireGuard peer reached through
a 1500-byte Ethernet route therefore gets an automatic MTU of 1420.

The effective MTU for a tunnel is `min(configured MTU, detected MTU)`. A node
with a smaller physical link clamps down even if the configured value is higher.
At startup, the agent detects its default-route interface MTU and annotates itself
with `net.unbounded-cloud.io/tunnel-mtu`. If the configured MTU exceeds the
detected maximum, the agent logs an error and surfaces an `mtuMismatch` node
error. The controller reads `node.mtu` and compares it with every node's
annotation, logging a warning for each mismatch when it observes the Node object.

Forwarding paths constrain the node as follows:

- Direct mesh peers use both endpoint Sites and their enabled `SitePeering`.
- Worker-to-gateway paths use assignments that target the worker's own Site and
  the selected GatewayPool.
- Gateway-to-gateway paths also use the governing `GatewayPoolPeering`.
- Direct-mesh components share the lowest configured Site or enabled mesh
  peering MTU in the component. Shared pod and tunnel interfaces make this
  necessary even without transitive routing: if A meshes with B at 1200 and C at
  1400, A's pod interfaces use 1200, so C must also use 1200 when sending to A.
  This propagates an MTU constraint, not routes between B and C.
- Disabled peerings, `meshNodes: false` peerings, missing Sites, and
  disconnected components do not contribute. Gateway assignments and pool
  peerings do not join direct-mesh components; their limits still follow actual
  gateway paths.

The CNI MTU is the lowest effective MTU among actual mesh and gateway peers plus
the configured direct-mesh component cap. At startup and whenever that value
changes, the agent updates the CNI bridge and both endpoints of existing pod veth
pairs. Forwarded TCP SYN packets on every interface are MSS-clamped to the
site-wide fabric MTU: `MTU - 40` for IPv4 and `MTU - 60` for IPv6. A fabric MTU
of 60 or less is rejected as too small to clamp.

### WireGuard

| YAML key | Flag | Type | Default | Behavior |
|----------|------|------|---------|----------|
| `node.wireGuardDir` | `--wireguard-dir` | string | `/host/etc/wireguard` | Key storage directory. Must be absolute, free of `..`, and under `/var/`, `/etc/`, `/run/`, or `/host/`. |
| `node.wireGuardPort` | `--wireguard-port` | int | `51820` | Listen port for the node mesh interface. |
| `node.wireGuardInterfacePrefix` | `--wireguard-interface-prefix` | string | `wg` | Prefix for per-port interfaces. The runtime name is `<prefix><port>`, for example `wg51820`. |

The agent generates a key pair on first start, stores it in the key directory,
and annotates the node with the public key
(`net.unbounded-cloud.io/wg-pubkey`). Gateway WireGuard interfaces use additional
ports allocated by the controller from 51821 upward, and their names use the same
prefix. That is why the naming prefix matters for firewall rules as well as for
interface management.

### GENEVE

| YAML key | Flag | Type | Default | Behavior |
|----------|------|------|---------|----------|
| `node.geneveInterfaceName` | `--geneve-interface` | string | `geneve0` | Shared flow-based GENEVE interface name. |
| none | `--geneve-port` | int | `6081` | GENEVE UDP destination port. Flag-only. |
| none | `--geneve-vni` | int | `1` | GENEVE Virtual Network Identifier. Flag-only. |

GENEVE is the default preference for private links when the CRD says `Auto`. It
avoids encryption overhead on high-bandwidth internal paths, which matters on
100 Gbps links where WireGuard becomes the bottleneck. The single shared
interface carries flows to every GENEVE peer, so the destination port must be
the same on every node that participates.

### VXLAN

| YAML key | Flag | Type | Default | Behavior |
|----------|------|------|---------|----------|
| `node.vxlanInterfaceName` | `--vxlan-interface` | string | `vxlan0` | Shared flow-based VXLAN interface name. |
| `node.vxlanSrcPortLow` | `--vxlan-src-port-low` | int | `47891` | Low bound of the UDP source port range. |
| `node.vxlanSrcPortHigh` | `--vxlan-src-port-high` | int | `47922` | High bound of the UDP source port range. |
| none | `--vxlan-port` | int | `4789` | VXLAN UDP destination port. Flag-only. |

VXLAN uses one external flow-based interface, similar to GENEVE, and can benefit
from hardware VXLAN offload on some adapters. The source port range is
deliberately narrow. It spans 32 ports by default, which limits the number of
distinct 5-tuple flows a VM originates and helps stay within per-VM flow table
limits on some cloud platforms. Widening the range gives more entropy and better
ECMP distribution, at the cost of more flows to track.

### IPIP

| YAML key | Flag | Type | Default | Behavior |
|----------|------|------|---------|----------|
| `node.ipipInterfaceName` | `--ipip-interface` | string | `ipip0` | Shared flow-based IPIP interface name. |

IPIP has the lowest overhead of any tunnel type at 20 bytes and requires no
protocol-specific port or identifier. It is appropriate only on networks where
the underlay already provides confidentiality, because it carries no encryption.
IPIP entries are created and managed automatically when `tunnelProtocol` is
`IPIP`.

### Tunnel interface names

The three shared tunnel names and the WireGuard prefix are configurable because
the defaults can collide with another network plugin or with site conventions.
All four settings are validated at startup, and the agent fails to start when a
constraint is violated.

- Shared names (`geneve0`, `vxlan0`, `ipip0`) must be nonempty, at most 15 bytes
  (the Linux `IFNAMSIZ` limit), pairwise distinct, and not equal to `unbounded0`.
- The WireGuard prefix must be nonempty, at most 10 bytes so that the prefix
  plus a five-digit UDP port fits in `IFNAMSIZ`, must not contain `/`, and must
  not equal `unbounded0`.

The dedicated `unbounded0` dummy interface is used by the eBPF dataplane and is
not configurable.

Renaming an existing deployment has different cleanup behavior per protocol.
GENEVE and VXLAN devices are swept automatically when the agent finds a
flow-based device whose UDP port matches the configured port and whose name does
not match the configured name. Matching on the configured port keeps the sweep
safe when another CNI runs flow-based encapsulation on a different port. IPIP is
not swept, because the kernel's external-mode device exposes nothing that lets
the agent distinguish its own device from another agent's; remove the old device
manually with `sudo ip link del <old-name>`. WireGuard interfaces are swept when
their name matches the agent's `<prefix><port>` convention and their listen port
is one the agent would assign, but the prefix differs from the configured one.
Tooling that uses a different naming convention, such as `tailscale0` or `wg0`,
is left alone.

### Tunnel Dataplane

The node agent uses an eBPF dataplane: a single `unbounded0` interface with TC
egress programs and an LPM trie that maps destination CIDRs to tunnel endpoints
for GENEVE, IPIP, VXLAN, or WireGuard.

| YAML key | Flag | Type | Default | Behavior |
|----------|------|------|---------|----------|
| `node.tunnelDataplaneMapSize` | `--tunnel-dataplane-map-size` | int | `16384` | Maximum entries in each LPM trie map. |
| `node.tunnelIPFamily` | `--tunnel-ip-family` | string | `IPv4` | Underlay address family for the outer header. |

Each LPM trie entry supports up to four nexthops per prefix with ECMP through
HRW (highest random weight, or rendezvous) hashing. The BPF program selects a
nexthop deterministically per 5-tuple flow, so a nexthop change rehashes only the
flows that used it. Each nexthop carries a `TUNNEL_F_HEALTHY` flag maintained by
the health check system; unhealthy nexthops are skipped for normal traffic,
while health probes are always forwarded so a recovered path can be detected.

`node.tunnelIPFamily` changes only the outer header. Setting it to `IPv6` makes
tunnel endpoints use IPv6 addresses while the inner overlay traffic remains
dual-stack. The value must be `IPv4` or `IPv6`; an empty value becomes `IPv4`.

### Tunnel Protocol Selection

`tunnelProtocol` is a CRD field available on every scope: Site, SitePeering,
GatewayPool, SiteGatewayPoolAssignment, and GatewayPoolPeering. The node
settings only decide what `Auto` resolves to.

| Value | Overhead | Encrypted | Use case |
|-------|----------|-----------|----------|
| `WireGuard` | 80 bytes | Yes | Cross-site links over public or untrusted networks. |
| `GENEVE` | 58 bytes | No | High-throughput internal links. |
| `VXLAN` | 50 bytes | No | Internal links where hardware VXLAN offload helps. |
| `IPIP` | 20 bytes | No | Minimal-overhead internal links. |
| `None` | 0 bytes | No | Direct L3 routing, when the underlay is already reachable. |
| `Auto` | Varies | Varies | The system selects from the link type. This is the default. |

When a scope is `Auto`, links that use external IPs resolve to
`node.preferredPublicNetworkEncapsulation`, which defaults to `WireGuard`.
Links that use only internal IPs resolve to
`node.preferredPrivateNetworkEncapsulation`, which defaults to `GENEVE`.

Security wins for `Auto` links. When the governing scope is `Auto` or unset and
the link uses public addresses, the public preference applies, and its default is
`WireGuard`, so the link is encrypted. Only an explicit public preference can opt
out of encryption. An explicit protocol on the governing scope is used as written,
so a `SitePeering` or assignment that names `GENEVE` overrides the `Auto` public
default. This is why the governing scope, not the whole hierarchy, decides the
protocol.

![Decision flow for choosing a tunnel protocol from CRD scope settings and link type](../../../img/networking-protocol-selection.svg)

### Status Push

The node agent publishes status over two transports: a WebSocket that carries
protobuf (and accepts JSON for compatibility) and a periodic HTTP POST. The
controller can also pull directly from the node's status endpoint. The push
settings decide which transports are enabled and how often they run.

| YAML key | Flag | Type | Default | Behavior |
|----------|------|------|---------|----------|
| `node.statusPushEnabled` | `--status-push-enabled` | bool | `true` | Enables periodic HTTP push. |
| `node.statusPushURL` | `--status-push-url` | string | `""` | Explicit push URL. Empty builds `https://<service-host>:<port>/status/push` from `UNBOUNDED_NET_CONTROLLER_SERVICE_HOST` and `UNBOUNDED_NET_CONTROLLER_SERVICE_PORT`. |
| `node.statusPushInterval` | `--status-push-interval` | duration | `60s` | Interval between HTTP pushes. |
| `node.statusPushApiserverInterval` | `--status-push-apiserver-interval` | duration | `60s` | Minimum interval between aggregated push attempts. |
| `node.statusPushDelta` | `--status-push-delta` | bool | `true` | Uses deltas for periodic HTTP pushes. |
| `node.statusWebsocketEnabled` | `--status-ws-enabled` | bool | `true` | Enables the WebSocket transport. |
| `node.statusWebsocketURL` | `--status-ws-url` | string | `""` | Explicit WebSocket URL. |
| `node.statusWebsocketApiserverMode` | `--status-ws-apiserver-mode` | string | `fallback` | Endpoint preference: `never`, `fallback`, or `preferred`. |
| `node.statusWebsocketApiserverURL` | `--status-ws-apiserver-url` | string | `wss://$(KUBERNETES_SERVICE_HOST)/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews` | Aggregated WebSocket URL, also used to derive the aggregated push URL. |
| `node.statusWebsocketApiserverStartupDelay` | `--status-ws-apiserver-startup-delay` | duration | `60s` | Delay before aggregated fallback is allowed during a direct outage. |
| `node.statusWebsocketKeepaliveInterval` | `--status-ws-keepalive-interval` | duration | `10s` | Keepalive ping interval. `0` disables pings. |
| `node.statusWsKeepaliveFailureCount` | `--status-ws-keepalive-failure-count` | int | `2` | Failures before reconnecting. |
| `node.criticalDeltaEvery` | `--status-critical-interval` | duration | `15s` | Maximum critical publish frequency. |
| `node.statsDeltaEvery` | `--status-stats-interval` | duration | `60s` | Maximum statistics publish frequency. |
| `node.fullSyncEvery` | `--status-full-sync-interval` | duration | `2m` | Forced full sync interval. |

URL values expand `$(KUBERNETES_SERVICE_HOST)` and
`${KUBERNETES_SERVICE_HOST}` at runtime, including
`node.statusWebsocketApiserverURL` and any `node.statusPushURL` that contains
the placeholder. The same ConfigMap therefore works in clusters with different
service CIDRs. When the service environment variables are absent, for example
because CoreDNS and service links are both unavailable, set an explicit URL.

`fallback` and `preferred` are the same behavior: direct controller endpoints
are tried first, and aggregated API server endpoints are used only as a fallback.
`never` disables the aggregated endpoints entirely. The startup delay exists so
a brief direct outage does not immediately shift every node onto the aggregated
path, which is more expensive because each aggregated HTTP upload and new
WebSocket handshake may require a TokenReview.

Critical updates are change-only. Root collection timestamps, aggregate health
timestamps, tunnel counters and handshakes, and peer RTT and uptime do not
trigger a critical publish. Health state, health enablement, topology, metadata,
routes, BPF entries, and errors, including clearing an error, do. When critical
changes are published, existing peers keep their last published measurements and
unrelated statistics wait for the statistics interval. Statistics updates carry
a timestamp even when measurements are unchanged, so the controller can tell
that the node is alive and current.

HTTP delta mode has a specific recovery contract. If the controller cannot apply
a delta because its base state is missing or mismatched, it returns HTTP 429 and
the node immediately sends a full state on the next push. A rejected delta never
leaves the controller with a partially updated cache.

WebSocket backpressure is handled by allowing only one outstanding status
message per connection. Updates batch while the acknowledgment is pending. A
missing acknowledgment reconnects after 30 seconds instead of advancing an
unacknowledged base.

Shutdown cleanup is controlled by `node.removeConfigurationOnShutdown`. The
three deprecated keys, `shutdownRemoveWireGuardConfiguration`,
`shutdownRemoveIPRoutes`, and `shutdownRemoveMasqueradeRules`, still work: if any
of them is true, the agent promotes the consolidated setting to true. Prefer the
consolidated key, because the deprecated keys cannot remove tunnel interfaces and
are retained only for older manifests.

### Health Check (UDP Probes)

The health check protocol monitors overlay peers with a custom UDP probe
protocol running over every tunnel type. The agent creates a session per peer
using the peer's overlay health IP when the resolved health-check profile is
enabled.

| YAML key | Flag | Type | Default | Behavior |
|----------|------|------|---------|----------|
| none (flag-only) | `--healthcheck-port` | int | `9997` | UDP port for the probe listener. Use the same port on every node. |
| none (flag-only) | `--base-metric` | int | `1` | Base metric for programmed supernet routes. |
| `node.healthFlapMaxBackoff` | `--health-flap-max-backoff` | duration | `120s` | Maximum backoff for flap dampening. Overrides the profile's backoff. |

Transmit and receive intervals default to 15 seconds, with a detect multiplier
of 3 and a maximum flap backoff of 120 seconds. The detection timeout is the
multiplier times the larger of the two intervals, which is 45 seconds with
defaults. Compared with a one-second probe cadence, that sends one fifteenth as
many probes per peer and trades a nominal three-second timeout for 45 seconds of
steady-state quiet. Timeouts are checked every half timeout, with a floor of
100 milliseconds, so an unresponsive established session can take up to another
check interval to be marked down. Set both intervals to `1s` if you need the
older, faster detection and can afford the probe traffic.

The selected profile comes from the most specific governing scope:

- `Site.spec.healthCheckSettings` governs node-to-node routes within one Site.
- `SitePeering.spec.healthCheckSettings` governs node-to-node routes between the
  Sites in that peering.
- `GatewayPool.spec.healthCheckSettings` governs same-pool gateway peers.
- `GatewayPoolPeering.spec.healthCheckSettings` governs routes between gateway
  pools in that peering.

For mesh peers, an explicit gateway pool or pool peering profile wins, followed
by a SiteGatewayPoolAssignment, a SitePeering, and then the peer's Site. For
node-to-gateway peers, the node's own Site or pool assignment governs; gateway
nodes use their explicit peer profile or the peer's pool profile, with a
remote-site or pool assignment as a fallback. Shared-tunnel gateway peers may
fall back to the local Site profile when no governing association exists;
WireGuard gateway peers do not use that fallback. An explicitly disabled
governing profile blocks every fallback.

The selected scope is merged with fresh defaults, not with the last applied
profile or with values from lower-priority scopes. Specifying only
`transmitInterval: 60s` therefore uses a 15-second receive interval and a detect
multiplier of 3. Both duration strings and integer milliseconds are accepted.
The node-wide maximum flap backoff overrides the selected profile's backoff.

Settings-only changes for an existing peer and overlay IP update the live session
in place, preserving health state, uptime, RTT, packet counters, and flap
history. Probe and detection timers wake promptly instead of waiting for the old
interval to elapse. New sessions begin down until enough replies arrive.
Changing a peer's overlay IP replaces and cancels the old session.

Health sessions replace the older gateway health mechanism. Health state does
not change the route metric; it drives the healthy flag on the peer's eBPF
nexthop. A down peer is removed from the eligible next-hop set, so ECMP stops
selecting it, and it is restored when probes succeed again. `--base-metric`
defaults to 1 and sets the priority of the supernet routes the agent programs,
which is a separate knob: lower metrics are preferred, and every supernet route
uses the same base unless a specific route overrides it. If two peerings define
conflicting settings for the same target, the controller processes peerings in
deterministic name order and keeps the first profile, logging both the kept and
ignored values.

## Site, Peering, and Gateway Selection

The custom resources are the part of configuration that changes most often. They
describe intent, and the controller and agents reconcile the dataplane from them.
This section covers the fields that affect networking configuration. For complete
manifests and validation rules, see
[Custom Resources]({{< relref "reference/networking/custom-resources" >}}).

### Site fields

A `Site` is a network location and its address plan. `spec.nodeCidrs` lists the
CIDRs that contain node internal addresses, and it is how the controller decides
which Site a node belongs to. `spec.podCidrAssignments` defines the pools and
per-node block sizes used for pod addressing and for inter-site routing.

| Field | Required | Default | Effect |
|-------|----------|---------|--------|
| `nodeCidrs` | Yes | none | CIDRs that identify nodes at this site. |
| `podCidrAssignments` | Yes | none | Pod CIDR pools and allocation rules. |
| `podCidrAssignments[].cidrBlocks` | Yes | none | CIDR pools to allocate from. |
| `podCidrAssignments[].nodeBlockSizes.ipv4` | No | `/24` | IPv4 subnet size per node, 1 to 32. |
| `podCidrAssignments[].nodeBlockSizes.ipv6` | No | pool prefix plus 16 | IPv6 subnet size per node, 1 to 128. |
| `podCidrAssignments[].nodeRegex` | No | empty | Node name patterns that may use this assignment. |
| `podCidrAssignments[].priority` | No | `100` | Lower values win when several assignments match. |
| `podCidrAssignments[].assignmentEnabled` | No | inherits `manageCniPlugin` | Turn allocation on or off for this assignment. |
| `manageCniPlugin` | No | `true` | Write CNI configuration and create same-site tunnel peers. |
| `nonMasqueradeCIDRs` | No | empty | Destinations that keep their pod source address when leaving the node. |
| `localCidrs` | No | empty | CIDRs never routed through gateway pools. |
| `healthCheckSettings` | No | built-in profile | Health check profile for same-site peers. |
| `tunnelProtocol` | No | `Auto` | Protocol for this site's links. |
| `tunnelMTU` | No | automatic | MTU cap for this site's links, 576 to 9000. |

`manageCniPlugin: false` is the integration point for an external CNI. The agent
stops writing the conflist and stops creating same-site tunnel peers, but the
Site's pod CIDR assignments are still required because they define the ranges used
for inter-site routing. An explicit `assignmentEnabled` on an assignment overrides
the inherited value.

### SitePeering fields

A `SitePeering` meshes sites directly. Without it, cross-site traffic follows the
gateway pool path. With it, nodes in the listed sites can exchange traffic without
a gateway hop, subject to the same protocol and MTU rules.

| Field | Required | Default | Effect |
|-------|----------|---------|--------|
| `enabled` | No | `true` | Whether the peering is active. |
| `sites` | Yes | none | At least two site names to peer. |
| `meshNodes` | No | `true` | Whether nodes in the sites mesh directly. |
| `healthCheckSettings` | No | built-in profile | Health profile for peered links. |
| `tunnelProtocol` | No | `Auto` | Protocol for peered links. |
| `tunnelMTU` | No | automatic | MTU cap for peered links, 576 to 9000. |

When `meshNodes` is false, the sites are still considered network-peered for
reachability and protocol selection, but nodes do not create direct mesh links to
each other. This is used when direct node-to-node connectivity across sites is not
available and traffic must still follow a gateway path.

### Gateway pool fields

A `GatewayPool` selects gateway nodes by label and defines how traffic to its
routed CIDRs is carried. `spec.nodeSelector` is required and must match at least
one label pair. Only nodes with a WireGuard public key are eligible, and an
`External` pool additionally requires a usable external IP.

| Field | Required | Default | Effect |
|-------|----------|---------|--------|
| `type` | No | `External` | `External` requires a reachable public IP; `Internal` uses internal addresses. |
| `nodeSelector` | Yes | none | Labels a node must have to belong to the pool. |
| `routedCidrs` | No | empty | CIDRs advertised through the pool. |
| `healthCheckSettings` | No | built-in profile | Health profile for links to this pool. |
| `tunnelProtocol` | No | `Auto` | Protocol for links to this pool. |
| `tunnelMTU` | No | automatic | MTU cap for links to this pool, 576 to 9000. |

`SiteGatewayPoolAssignment` attaches one or more sites to one or more pools. It is
the object a worker uses when it resolves a site-to-pool link, so its protocol and
MTU fields apply there. `GatewayPoolPeering` links two or more pools and governs
gateway-to-gateway links between them.

| Field | Applies to | Notes |
|-------|------------|-------|
| `sites` | `SiteGatewayPoolAssignment` | At least one site name. |
| `gatewayPools` | `SiteGatewayPoolAssignment`, `GatewayPoolPeering` | At least one pool for an assignment, at least two for a peering. |
| `enabled` | Both | Defaults to true. |
| `tunnelProtocol` | Both | `Auto` by default. |
| `tunnelMTU` | Both | 576 to 9000 when set. |
| `healthCheckSettings` | Both | Embedded profile. |

### Gateway pool selection

Selection is controller-driven and deterministic. A node can belong to only one
pool. For each node, the controller evaluates every pool and keeps the eligible
ones, then sorts their names and assigns the lexicographically first. If a node
matches several pools, the controller logs the choice at verbosity level 2. There
is no priority field on a pool, so name the pools with that tie-break in mind.

A pool is eligible for a node only when all of these hold:

- The pool's `nodeSelector` matches the node labels.
- For an `External` pool, the node has at least one usable external IP.
- The node has the WireGuard public key annotation
  `net.unbounded-cloud.io/wg-pubkey`.

External IP resolution checks, in order, the administrator-declared
`net.unbounded-cloud.io/declared-public-ip` annotation, the node's reported
external addresses, and the agent-discovered
`net.unbounded-cloud.io/discovered-public-ip` annotation. A discovered address is
only used until its `net.unbounded-cloud.io/discovered-public-ip-expires-at`
timestamp passes. If the external address configuration is invalid, the node is
not eligible for an `External` pool and the controller logs a warning.

The controller writes `net.unbounded-cloud.io/gateway-pool` on the Node when the
node is selected, and removes it when the node is no longer eligible. Gateway nodes
also receive a unique WireGuard port annotation, starting at 51821, so each gateway
listens on a distinct port for gateway-to-gateway links.

The node agent adopts its assignment at startup and does not change it in place. It
records the adopted Site and pool in the `net.unbounded-cloud.io/effective-site` and
`net.unbounded-cloud.io/effective-gateway-pool` annotations on its own Pod. If the
Node assignment changes, the controller deletes the stale node agent Pod so the
DaemonSet recreates it with the new assignment. A Pod whose acknowledgment is still
pending is left alone, which avoids a delete loop during startup.

This design means a pool change can restart the node agent on the affected node.
Plan for a brief dataplane reconcile gap when you move a node between pools. The
gateway port is released when the node leaves the pool and may be reassigned to
another gateway.

## Worked Examples

The following examples show complete changes rather than isolated keys. Each one
starts from a default installation and ends with the restart or verification
step that makes the change take effect.

### Move a cluster from full to summary publication

Start with the controller, because a new node talking to an old controller
reports `status-summary-unsupported` and stops publishing. Set the following
keys in the ConfigMap:

```yaml
node:
  statusDetailMode: summary
  statusPushEnabled: true
  statusWebsocketEnabled: true
controller:
  statusDetailCacheTTL: 300s
  statusDetailRequestTimeout: 120s
```

Apply it and roll the controller first, then the nodes:

```bash
kubectl -n unbounded-system edit configmap unbounded-net-config
kubectl -n unbounded-system rollout restart deployment/unbounded-net-controller
kubectl -n unbounded-system rollout status deployment/unbounded-net-controller
kubectl -n unbounded-system rollout restart daemonset/unbounded-net-node
kubectl -n unbounded-system rollout status daemonset/unbounded-net-node
```

Open the status page and select one node. The overview should be current, and a
request for details should complete within the request timeout. If the node
shows `status-summary-unsupported`, the controller rollout did not finish or the
controller is older than the node, so finish the controller upgrade before
retrying.

### Shorten the detail cache and request timeout

Put the values in the file:

```yaml
controller:
  statusDetailCacheTTL: 60s
  statusDetailRequestTimeout: 20s
```

Then restart the controller. If you need a temporary override without editing
the ConfigMap, add the flags to the controller container instead. They win over
the file and are useful for a single-replica debugging deployment. Use a patch
to append them to the existing argument list:

```bash
kubectl -n unbounded-system patch deployment unbounded-net-controller --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--status-detail-cache-ttl=60s"},
       {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--status-detail-request-timeout=20s"}]'
```

A shorter TTL frees memory sooner but makes repeated detail views miss more
often. A shorter request timeout fails faster on a slow node, at the risk of
expiring a request that would have completed after one polling interval.

### Pin managed kube-proxy to a private mirror

Add the image to the controller section:

```yaml
controller:
  managedKubeProxy:
    enabled: true
    image: myregistry.example.com/mirror/kube-proxy
```

Because the reference has no tag, the controller takes the tag of
`kube-system/kube-proxy` when the provider image has one. If the provider image
is unversioned or absent, the controller searches
`myregistry.example.com/mirror/kube-proxy` for the API server's version and
chooses the highest numeric maintenance suffix. That search is anonymous, so a
mirror that requires authentication for tag listing will not work; pin a digest
instead:

```yaml
controller:
  managedKubeProxy:
    image: myregistry.example.com/mirror/kube-proxy@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

A digest reference is used exactly as written. The controller does not read the
provider DaemonSet, does not contact the registry, and does not require the
controller Pod to have registry access beyond what the kubelet needs to pull the
image.

### Override the API server URL for both binaries

This is useful when the pods cannot resolve the in-cluster service name, or when
a proxy fronts the API server:

```yaml
common:
  apiserverURL: https://api.example.internal:6443
```

Both binaries apply it. The controller passes it to
`clientcmd.BuildConfigFromFlags` when a kubeconfig is set, and otherwise
replaces the host of the in-cluster configuration. The node agent does the same.
Credentials still come from the normal source, so the override must point at the
same cluster or at a proxy that accepts the same service account tokens.

### Tune a large cluster

For a cluster in the thousands of nodes, start with resync periods rather than
with concurrency:

```yaml
controller:
  informerResyncPeriod: 600s
  statusDetailCacheTTL: 120s
  statusDetailRequestTimeout: 60s
node:
  informerResyncPeriod: 7200s
  criticalDeltaEvery: 30s
  statsDeltaEvery: 60s
  fullSyncEvery: 5m
```

The controller resync doubles the default interval, and the node resync doubles
it again, which cuts the periodic list load substantially. Publication
intervals stay close to their defaults, because status freshness drives
alerting. Neighbor tables size themselves from the real peer set, so there is no
node-count setting to raise; if you see `neighborTableOverflowIPv4` or
`neighborTableOverflowIPv6`, check for neighbors created outside the mesh rather
than raising a threshold by hand. If you see `conntrackNearMax`, the host's
conntrack table is the constraint, not the mesh configuration.

## Firewall Requirements

Open the following ports between the components that need them. The directions
matter: a rule that allows node-to-node traffic does not allow the controller to
pull status, and a rule for the mesh WireGuard port does not cover gateway
interfaces.

| Port | Protocol | Direction | Purpose |
|------|----------|-----------|---------|
| `51820` | UDP | Node to node, both directions | WireGuard mesh interface, controlled by `node.wireGuardPort`. |
| `51821` and up | UDP | Gateway to gateway, both directions | Per-gateway WireGuard interfaces. The controller allocates ports from 51821 upward. |
| `6081` | UDP | Node to node, both directions | GENEVE tunnel traffic, when GENEVE is selected. |
| `4789` | UDP | Node to node, both directions | VXLAN tunnel traffic, when VXLAN is selected. |
| `9997` | UDP | Node to node, both directions | Health check probes. Probes are always forwarded, even to unhealthy nexthops. |
| `9998` | TCP | Controller to node, and operator to node | Node agent health, readiness, and status endpoints. |
| `9999` | TCP | Node to controller, and dashboard to controller | Controller HTTPS health, status, status push, and WebSocket endpoints. |

IPIP has no port of its own; it is IP protocol 4, so allow that protocol between
nodes if you use IPIP. If you change any port through the configuration, change
the firewall rule in the same commit. The controller's `nodeAgentHealthPort` must
match the node's `healthPort`, or pull fallback and dashboard links will fail
even though the node is healthy.

The controller Service is a ClusterIP with no selector; the leader pod publishes
the Endpoints and EndpointSlice. That means network policy must allow traffic to
the controller Pod on 9999, not merely to the Service IP.

## Resource Requirements

The rendered workloads request modest resources and set limits only on the
controller. The numbers below come from the manifests in `deploy/net` and are the
values you get from a default render.

### Controller

| Resource | Request | Limit |
|----------|---------|-------|
| CPU | `100m` | `8` |
| Memory | `128Mi` | `8192Mi` |

The controller container runs as a nonroot user with a read-only root
filesystem, drops all capabilities, and uses the `RuntimeDefault` seccomp
profile. It mounts an `emptyDir` at `/tmp` and the ConfigMap at
`/etc/unbounded-net/config.yaml`.

### Node Agent

| Resource | Request | Limit |
|----------|---------|-------|
| CPU | `100m` | none |
| Memory | `64Mi` | none |

The node agent runs as root and privileged, because it creates interfaces,
changes routes, writes iptables rules, and installs CNI binaries on the host. It
mounts the host CNI directories, the WireGuard directory, `/etc/iproute2`, and
`/run/unbounded-net`. It has no CPU or memory limit by default so that a busy
node cannot be throttled in the middle of programming the dataplane, but that
also means a runaway agent can affect the node. Set a limit only after
measuring.

### Scaling

Controller memory grows with the number of nodes, peers, Routes, and cached
details. The cluster status cache pre-builds responses and coalesces dirty
signals for two seconds before rebuilding, which keeps repeated dashboard views
cheap at the cost of holding one built response. The detail cache is bounded by
its TTL, not by a maximum entry count, so a large TTL on a large cluster is the
main way to make the controller memory-hungry. Lower
`controller.statusDetailCacheTTL` before you add replicas.

The node agent's process memory grows with the peer set. The eBPF map size is a
separate cost in kernel memory, not in the agent's RSS. The default LPM trie size
of 16384 entries is generous for most clusters. Raising it costs kernel memory on
every node, and lowering it below the number of prefixes you actually program
causes insert failures. Change it only with a measured prefix count.

## Environment Variables

The manifests inject most environment variables from the downward API. The
binaries also read a few variables directly, and those are not always the same
ones the manifests set.

### Controller

| Variable | Source | Used for |
|----------|--------|----------|
| `LOG_LEVEL` | `unbounded-net-config` ConfigMap key | Feeds the `-v=$(LOG_LEVEL)` argument at container start. |
| `POD_NAME` | Downward API, `metadata.name` | Leader election identity and status display. |
| `POD_NAMESPACE` | Downward API, `metadata.namespace` | Lease namespace and default system namespace resolution. |
| `POD_IP` | Downward API, `status.podIP` | EndpointSlice management for the leader's Service endpoint. |
| `POD_UID` | Downward API, `metadata.uid` | Identity checks for the published endpoint. |
| `NODE_NAME` | Downward API, `spec.nodeName` | Leader identity display in the status dashboard. |
| `AZURE_TENANT_ID` | Optional, set by you | Seeds `AzureTenantID` before the file value is applied. The shipped manifest does not set it. |

### Node Agent

| Variable | Source | Used for |
|----------|--------|----------|
| `LOG_LEVEL` | `unbounded-net-config` ConfigMap key | Feeds the `-v=$(LOG_LEVEL)` argument at container start. |
| `NODE_NAME` | Downward API, `spec.nodeName` | Required node name. Startup fails if it is empty and `--node-name` is not set. |
| `POD_NAMESPACE` | Downward API, `metadata.namespace` | Required with `POD_NAME` and `POD_UID` to acknowledge the node's effective Site and GatewayPool assignment on its own Pod. |
| `POD_NAME` | Downward API, `metadata.name` | Required with `POD_NAMESPACE` and `POD_UID` for the same acknowledgment. |
| `POD_UID` | Downward API, `metadata.uid` | Required with `POD_NAME` and `POD_NAMESPACE` for the same acknowledgment. |
| `UNBOUNDED_NET_CONTROLLER_SERVICE_HOST` | Kubernetes service environment | Builds the direct controller URL when no explicit push or WebSocket URL is set. |
| `UNBOUNDED_NET_CONTROLLER_SERVICE_PORT` | Kubernetes service environment | Port for the same URL, defaulting to 9999 when absent. |
| `KUBERNETES_SERVICE_HOST` | Kubernetes service environment | Expanded in the aggregated WebSocket URL template. |

## Tuning Recommendations

### High Availability

Run at least two controller replicas and leave leader election enabled. One
replica holds the lease and does the work; the other serves health and status
endpoints and takes over when the lease expires. With the default timings, a
failed leader is replaced within roughly the renew deadline plus the retry
period, which is under half a minute in the normal case.

Do not disable leader election to "simplify" a single-replica deployment unless
you also accept that a rolling update has a window with no controller. The
leader fence for pod CIDR allocation only exists when leader election is
enabled, and running two replicas without it risks overlapping assignments.

A PodDisruptionBudget with `minAvailable: 1` keeps a voluntary node drain from
taking every controller replica at once. The same idea applies to gateway nodes:
drain them one at a time so the mesh has a healthy gateway to fail over to.

### Large Clusters (1000+ nodes)

For clusters above roughly a thousand nodes, consider these starting points:

| Setting | Default | Large-cluster starting point |
|---------|---------|------------------------------|
| `controller.informerResyncPeriod` | `300s` | `600s` |
| `node.informerResyncPeriod` | `3600s` | `7200s` |
| `controller.statusDetailCacheTTL` | `300s` | `120s` |
| `controller.statusDetailRequestTimeout` | `120s` | `60s` |
| `node.criticalDeltaEvery` | `15s` | `30s` |
| `node.fullSyncEvery` | `2m` | `5m` |

The resync periods dominate API server load, because a resync lists every
watched object. Publication intervals dominate controller CPU and memory,
because every publication decodes and caches. Change one group at a time and
watch the API server's request rate and the controller's memory before changing
the next.

Neighbor tables do not need a size setting. They are computed from the real peer
set, so a large cluster sizes them automatically and a small cluster does not
reserve memory it cannot use. If the computed set is large, the cost is kernel
neighbor memory on each node, which is the correct place for it.

If the controller's memory grows without a matching node count increase, look
first at `controller.statusDetailCacheTTL` and at how often the dashboard
requests details. A long TTL plus frequent views keeps many full snapshots
resident, and the cache bounds retention by time rather than by count.

## Next Steps

- [Custom resources]({{< relref "reference/networking/custom-resources" >}}) for
  the Site, GatewayPool, and peering fields that select protocols, MTUs, and
  health check profiles.
- [Routing flows]({{< relref "reference/networking/routing-flows" >}}) for how
  the settings on this page turn into packet paths.
- [Networking operations]({{< relref "reference/networking/operations" >}}) for
  day-two checks, gateway health, and troubleshooting.
- [CLI reference]({{< relref "reference/cli" >}}) for the command-line surface of
  the other Unbounded binaries.
- [Operations guide]({{< relref "guides/operations" >}}) for rollout and upgrade
  procedures that apply to the whole platform.
