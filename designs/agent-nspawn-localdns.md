# Agent nspawn LocalDNS

Status: proposed

## Summary

This document proposes an optional CoreDNS-based local DNS cache that runs as a
systemd service inside the active systemd-nspawn machine. The physical host
keeps its existing resolver configuration and does not depend on the nspawn
machine for DNS.

LocalDNS exposes two stable link-local listener addresses in the network
namespace shared by the host and nspawn machine:

- `169.254.10.10` serves processes inside the nspawn machine and pods using
  `dnsPolicy: Default`. It forwards to DNS servers discovered from the physical
  host resolver configuration.
- `169.254.10.11` serves pods using `dnsPolicy: ClusterFirst`. It forwards to
  the cluster DNS service IP supplied by `Cluster.ClusterDNS`.

When LocalDNS is enabled, the agent writes the nspawn machine's static
`/etc/resolv.conf` to use `169.254.10.10` and configures kubelet with
`--cluster-dns=169.254.10.11`. CoreDNS starts and becomes ready before
containerd and kubelet start.

The initial metrics contract uses the native CoreDNS Prometheus endpoint. It
does not add a separate LocalDNS exporter.

## Background

This design is based on the AKS LocalDNS implementation in Azure AgentBaker at
commit
[`62a783b7a967352bb4726e636d22930c9973f4f7`](https://github.com/Azure/AgentBaker/tree/62a783b7a967352bb4726e636d22930c9973f4f7).
It retains AgentBaker's two-listener model, host-upstream and cluster-DNS
traffic separation, CoreDNS caching and serve-stale behavior, upstream
discovery on managed startup or restart, and native CoreDNS metrics. It adapts
that model to run CoreDNS inside the nspawn machine and intentionally does not
modify the physical host resolver or add AgentBaker's separate metrics exporter
and critical-FQDN hosts refresher.

Unbounded runs kubelet and containerd inside a systemd-nspawn machine. The
machine configuration uses:

```ini
[Network]
VirtualEthernet=no
```

The machine therefore shares the physical host's network namespace. A CoreDNS
process inside the machine can bind addresses on a dummy interface visible to
pods, host-network workloads, and external node scrapers. The process and its
systemd unit still belong to the nspawn machine and are stopped when that
machine stops.

The current rootfs provisioning flow masks `systemd-resolved` inside the
machine and copies the physical host's `/etc/resolv.conf` into the rootfs as a
static file. The current kubelet goal state passes `Cluster.ClusterDNS`
directly as `--cluster-dns`.

This provides two integration points:

1. Replace nameserver entries in the machine's static resolver file with a
   LocalDNS node listener.
2. Preserve the configured cluster DNS service IP as a CoreDNS upstream while
   giving kubelet a LocalDNS cluster listener.

## Goals

- Cache DNS queries for nspawn services and Kubernetes pods.
- Support stale cached responses when an upstream DNS server is temporarily
  unavailable.
- Keep physical host DNS independent from the nspawn machine lifecycle.
- Start and verify LocalDNS before starting containerd and kubelet.
- Continuously monitor both listeners through a systemd watchdog and restart
  unhealthy LocalDNS processes.
- Enforce CPU and memory limits equivalent to the AgentBaker defaults unless
  overridden.
- Preserve the distinction between node/default DNS and ClusterFirst DNS.
- Install CoreDNS through the same online and offline artifact mechanisms used
  for other nspawn rootfs binaries.
- Keep LocalDNS configuration and installation idempotent across bootstrap,
  reboot, repave, and reset.
- Bypass conntrack for TCP and UDP DNS traffic to both LocalDNS listeners,
  matching the AgentBaker dataplane behavior.
- Expose native CoreDNS metrics for node-level Prometheus scraping.
- Define a generic Unbounded capability without embedding AKS-specific profile,
  annotation, or critical-FQDN policy.

## Non-goals

- Changing the physical host's `/etc/resolv.conf` or network-manager DNS
  settings.
- Making physical host services depend on LocalDNS.
- Reproducing the full AgentBaker LocalDNS implementation.
- Implementing an AKS `LocalDnsProfile` API in Unbounded.
- Importing CoreDNS or Caddy parser packages to validate custom Corefiles.
- Maintaining a critical AKS FQDN hosts file.
- Publishing AKS-specific node labels or annotations.
- Adding a separate exporter for service, cgroup, or configured-forward
  metrics.
- Supporting two simultaneously active nspawn machines that bind the same
  LocalDNS addresses.
- Supporting IPv6 listeners or upstreams in the initial implementation.
- Translating host split-DNS or domain-routing policy into CoreDNS configuration.
- Replacing cluster DNS or changing the cluster DNS deployment.

## DNS traffic model

### Nspawn services and Default-policy pods

The nspawn machine's `/etc/resolv.conf` uses the node listener:

```text
nameserver 169.254.10.10
```

The query path is:

```text
nspawn process or dnsPolicy: Default pod
    -> 169.254.10.10
    -> physical host resolver upstreams
```

The upstream addresses are captured from the physical host before the machine
resolver file is rewritten. The physical host resolver itself remains
unchanged.

### ClusterFirst pods

When LocalDNS is enabled, kubelet receives:

```text
--cluster-dns=169.254.10.11
```

The query path is:

```text
dnsPolicy: ClusterFirst pod
    -> 169.254.10.11
    -> Cluster.ClusterDNS
    -> cluster DNS
```

`Cluster.ClusterDNS` remains the actual cluster DNS service IP. LocalDNS changes
the value passed to kubelet, but does not discard the original value needed as
the CoreDNS upstream.

### Physical host

The physical host continues to use its existing resolver path:

```text
physical host process
    -> physical host /etc/resolv.conf
    -> existing resolver
```

Stopping or replacing the nspawn machine cannot break physical host DNS. The
host-side agent can still resolve artifact endpoints and repair or repave the
machine if LocalDNS fails.

## Why the machine resolver must not use the cluster listener

The machine's `/etc/resolv.conf` must not point at `169.254.10.11`. That
listener forwards to the cluster DNS service IP, whose routing may depend on a
running kubelet and initialized Kubernetes dataplane.

Using the cluster listener for kubelet's own DNS could create this dependency
cycle:

```text
kubelet must resolve the API server
    -> LocalDNS cluster listener
    -> cluster DNS service IP
    -> service routing is not initialized
    -> kubelet cannot start
```

The node listener avoids that cycle by forwarding to DNS infrastructure already
available to the physical host.

## Proposed configuration

Add an optional top-level LocalDNS block with a caller-supplied Corefile
template:

```json
{
  "LocalDNS": {
    "Enabled": true,
    "NodeListenerIP": "169.254.10.10",
    "ClusterListenerIP": "169.254.10.11",
    "MetricsAddress": "10.20.1.7:9253",
    "CPULimitInMilliCores": 2000,
    "MemoryLimitInMB": 128,
    "RequiredPlugins": ["nsid", "template"],
    "CorefileTemplate": "<CoreDNS configuration template>"
  },
  "Downloads": {
    "CoreDNS": {
      "Version": "1.12.3"
    }
  }
}
```

The initial API provides defaults for listener addresses. `MetricsAddress`
defaults to `<Kubelet.NodeIP>:9253` when kubelet has a configured IPv4 node IP.
When kubelet node IP is omitted or ambiguous, LocalDNS requires an explicit
metrics address. It does not default to a wildcard bind. An empty
`CorefileTemplate` selects the built-in default template described below.
CoreDNS version and source selection use `Downloads.CoreDNS`, consistent with
other downloaded rootfs components, rather than LocalDNS runtime config. CPU
and memory limits default to the AgentBaker values of 2000 millicores and 128
MB. `RequiredPlugins` adds product-specific plugins to the required baseline
plugin set.

A possible Go shape is:

```go
type AgentLocalDNSConfig struct {
    Enabled              bool     `json:"Enabled"`
    NodeListenerIP       string   `json:"NodeListenerIP,omitempty"`
    ClusterListenerIP    string   `json:"ClusterListenerIP,omitempty"`
    MetricsAddress       string   `json:"MetricsAddress,omitempty"`
    CPULimitInMilliCores *int     `json:"CPULimitInMilliCores,omitempty"`
    MemoryLimitInMB      *int     `json:"MemoryLimitInMB,omitempty"`
    RequiredPlugins      []string `json:"RequiredPlugins,omitempty"`
    CorefileTemplate     string   `json:"CorefileTemplate,omitempty"`
}
```

### Required behavior outside the template

Unbounded owns and enforces the integration settings that do not need to be
expressed as CoreDNS syntax:

- CoreDNS artifact version, source, checksum, and installation path.
- Dummy interface ownership and both listener addresses.
- The nspawn machine resolver pointing at `NodeListenerIP`.
- Kubelet `--cluster-dns` pointing at `ClusterListenerIP`.
- CoreDNS systemd unit installation, ordering, restart policy, and resource
  lifecycle.
- DNS and readiness probes against both configured listener addresses before
  kubelet starts.
- Metrics port conflict checks and the expected native metrics address.
- Reboot, repave, and reset behavior.

The built-in template enforces the default CoreDNS policy, including binds,
forwarding, readiness, cache, serve-stale, loop protection, and native metrics.
A caller-supplied template is a full replacement for the built-in template and
can change or omit those directives. The initial API does not merge fragments
into the default template. Unbounded cannot safely inject mandatory plugin
directives into arbitrary CoreDNS server blocks, so custom templates are
governed by a contract and operational checks rather than by rewriting their
contents.

A custom template must:

- Bind DNS to both configured listener addresses.
- Provide a node-listener root fallback that forwards to
  `NodeUpstreamIPs`.
- Provide a cluster-listener root fallback that forwards to
  `ClusterDNSServiceIP`.
- Expose listener-specific readiness endpoints.
- Serve `health-check.localdns.local` locally through both listeners for ongoing
  watchdog DNS probes.
- Avoid forwarding either listener back to itself.
- Expose native metrics at `MetricsAddress` when metrics are enabled.

Validation must require:

- Distinct, valid IPv4 unicast listener addresses.
- An IPv4 `Cluster.ClusterDNS` service IP distinct from both listeners.
- A valid metrics listen address when metrics are enabled.
- Positive CPU and memory limits within supported systemd ranges.
- Valid, normalized plugin names without duplicates.
- A Corefile template that parses under the strict template context and stays
  within configured input and rendered size limits.

The fixed link-local defaults are conventions, not assumptions embedded in the
runtime implementation. Callers may provide different addresses when their
network environment requires them.

### Product-specific configuration

A consumer such as AKS Flex Node may have a richer product-facing LocalDNS
profile. That consumer should translate its profile into `CorefileTemplate` and
`RequiredPlugins`. Unbounded supplies runtime listener, host-upstream,
cluster-DNS, and metrics values to the template. Unbounded should not own AKS
node annotations, symbolic forward destinations, or critical FQDN lists.

The optional AgentBaker critical-FQDN hosts feature belongs in AKS Flex Node.
Flex can render the `hosts` blocks into its full replacement Corefile template,
add `hosts` to `RequiredPlugins`, install a machine-local refresh service and
timer into each rootfs, and reconcile AKS-specific node annotations through its
host daemon. The refresher should query the direct node upstreams persisted by
Unbounded in `/etc/unbounded/localdns/node-upstreams` rather than the machine
resolver, then validate and atomically replace the machine-local hosts file.
This keeps AKS FQDN policy out of the generic Unbounded API while reusing the
Unbounded CoreDNS runtime.

## Goal-state model

LocalDNS spans both machine rootfs state and host-global network state:

- The CoreDNS binary, Corefile, and systemd unit are machine-specific files.
- The dummy interface and listener addresses exist in the shared host network
  namespace.
- The CoreDNS process belongs to the active nspawn machine.

The resolved goal state should keep the original cluster DNS service IP and the
kubelet-facing DNS address separate. For example:

```go
type LocalDNS struct {
    Enabled              bool
    CoreDNSVersion       string
    CoreDNSBinarySource  artifactsource.Source
    NodeListenerIP       netip.Addr
    ClusterListenerIP    netip.Addr
    NodeUpstreamIPs      []netip.Addr
    ClusterDNSServiceIP  netip.Addr
    MetricsAddress       string
    CPULimitInMilliCores int
    MemoryLimitInMB      int
    RequiredPlugins      []string
    Corefile             []byte
}
```

The rootfs and node-start goal states may each reference the resolved LocalDNS
goal. Goal-state resolution must happen before mutating the rootfs so artifact
sources, listener conflicts, resolver upstreams, and Corefile content are known
up front.

Kubelet resolution should use:

```text
LocalDNS disabled: ClusterDNS = Cluster.ClusterDNS
LocalDNS enabled:  ClusterDNS = LocalDNS.ClusterListenerIP
```

The original `Cluster.ClusterDNS` remains in the LocalDNS goal as the cluster
listener upstream.

## Host upstream discovery

The agent must discover node-listener upstream IPs from the physical host while
the physical host resolver is still unchanged.

The initial implementation supports two resolver layouts:

1. `/etc/resolv.conf` contains direct upstream addresses. The agent reads those
   addresses regardless of which host component generated the file.
2. `/etc/resolv.conf` points to the `systemd-resolved` stub and
   `/run/systemd/resolve/resolv.conf` contains the direct upstream addresses.
   The agent reads the upstream resolver file rather than using the stub as a
   forwarding destination.

For either layout, discovery keeps only valid IPv4 unicast addresses, rejects
loopback addresses and the configured LocalDNS listener addresses, normalizes
and deduplicates the result, and rejects an empty upstream set.

The `systemd-resolved` layout is supported only when its effective DNS policy
can be represented as one default upstream set. The agent checks resolved's
routing-domain state and rejects split-DNS configurations, including per-domain
or VPN-specific resolver routing, rather than flattening them into one CoreDNS
`forward .` destination list. Translating split-DNS policy into domain-specific
CoreDNS server blocks is outside the initial scope.

Other local stubs and resolver layouts are unsupported initially. If
`/etc/resolv.conf` points to dnsmasq, a NetworkManager caching stub, or another
local resolver, preflight fails with the detected layout and the supported
alternatives. The agent does not attempt backend-specific discovery for those
services.

LocalDNS always forwards directly to the discovered upstreams. It does not send
queries through `systemd-resolved` or another local caching stub, avoiding
multiple caching layers and a runtime dependency on the host resolver daemon.

Discovery must not assume Azure DNS or replace a hard-coded Azure DNS address.
Those are product-specific AgentBaker behaviors.

The resolved upstream addresses are written directly into the generated
Corefile. The Corefile must not use the machine's `/etc/resolv.conf` as its
forward source because that file points back to LocalDNS.

## Corefile

The configured Corefile is a strict Go template rendered after host upstream
DNS and all listener settings are resolved:

```go
type LocalDNSCorefileTemplateData struct {
    // NodeListenerIP is the DNS address used by nspawn services and
    // dnsPolicy: Default pods.
    NodeListenerIP string

    // ClusterListenerIP is the DNS address kubelet writes for
    // dnsPolicy: ClusterFirst pods.
    ClusterListenerIP string

    // NodeUpstreamIPs are DNS server addresses discovered from the physical
    // host resolver and used by the node listener.
    NodeUpstreamIPs []string

    // NodeUpstreamIPsJoined contains NodeUpstreamIPs joined by one ASCII space
    // for direct use as the argument list of a CoreDNS forward directive.
    NodeUpstreamIPsJoined string

    // ClusterDNSServiceIP is the original Cluster.ClusterDNS service IP and
    // is used by the cluster listener.
    ClusterDNSServiceIP string

    // MetricsAddress is the CoreDNS native Prometheus listen address on the
    // node InternalIP, such as 10.0.0.4:9253.
    MetricsAddress string
}
```

The fields have these roles:

| Field | Meaning |
|---|---|
| `NodeListenerIP` | Stable DNS endpoint for processes inside the nspawn machine and Default-policy pods. It normally forwards to `NodeUpstreamIPs`. |
| `ClusterListenerIP` | Stable DNS endpoint supplied to kubelet as `--cluster-dns` for ClusterFirst pods. It normally forwards to `ClusterDNSServiceIP`. |
| `NodeUpstreamIPs` | Runtime-discovered physical host DNS upstream addresses. The list remains available for templates that need to iterate over individual addresses. The addresses must not point back to LocalDNS. |
| `NodeUpstreamIPsJoined` | The validated node upstream addresses joined by one ASCII space, ready for direct use in `forward . {{ .NodeUpstreamIPsJoined }}`. |
| `ClusterDNSServiceIP` | The cluster's actual kube-dns/CoreDNS Service IP from `Cluster.ClusterDNS`. LocalDNS preserves it as an upstream even though kubelet receives `ClusterListenerIP`. |
| `MetricsAddress` | Address on which CoreDNS's native Prometheus plugin serves `/metrics`. It controls exposure and must not conflict with another listener. |

Template execution uses `missingkey=error`. `NodeUpstreamIPsJoined` avoids
requiring a template helper for the common `forward` directive. The renderer
does not expose environment access, filesystem access, command execution, or
arbitrary Sprig functions. The input and rendered output have bounded sizes.
The rendered Corefile is written without shell evaluation.

Unbounded verifies the rendered configuration operationally by starting
CoreDNS and probing both DNS and readiness endpoints before kubelet starts.
When `CorefileTemplate` is empty, Unbounded renders this default template:

```corefile
health-check.localdns.local:53 {
    bind {{ .NodeListenerIP }} {{ .ClusterListenerIP }}
    whoami
}

.:53 {
    errors
    bind {{ .NodeListenerIP }}

    forward . {{ .NodeUpstreamIPsJoined }} {
        force_tcp
        policy sequential
        max_concurrent 1000
    }

    ready {{ .NodeListenerIP }}:8181

    cache 30 {
        success 9984
        denial 9984
        serve_stale 3600s verify
        servfail 0
    }

    loop
    prometheus {{ .MetricsAddress }}
}

.:53 {
    errors
    bind {{ .ClusterListenerIP }}

    forward . {{ .ClusterDNSServiceIP }} {
        force_tcp
        policy sequential
        max_concurrent 1000
    }

    ready {{ .ClusterListenerIP }}:8181

    cache 30 {
        success 9984
        denial 9984
        serve_stale 3600s verify
        servfail 0
    }

    loop
}
```

The `prometheus` directive appears in only one server block because its listener
is process-wide and its endpoint exports metrics for the complete CoreDNS
instance, including both DNS server blocks.

The renderer replaces all template fields with validated values. In particular,
`NodeUpstreamIPsJoined` contains discovered physical host upstreams and
`ClusterDNSServiceIP` contains `Cluster.ClusterDNS`; neither value is a
hard-coded Unbounded DNS destination.

The selected CoreDNS binary must include the plugins referenced by the built-in
Corefile. Artifact publishing and installation tests should verify the plugin
set through `coredns -plugins` or an equivalent deterministic check.

## Machine resolver file

The current `DisableResolved` task masks `systemd-resolved` inside the rootfs and
copies the physical host resolver file. When LocalDNS is enabled, the task
should instead:

1. Preserve valid `search`, `domain`, and `options` directives as appropriate.
2. Remove all existing `nameserver` directives.
3. Add one `nameserver` directive for the node listener.
4. Write the result atomically with mode `0644`.

Example:

```text
search example.internal
nameserver 169.254.10.10
options timeout:2 attempts:3
```

When LocalDNS is disabled, existing behavior remains unchanged.

## Network interface

The listener addresses require a stable interface in the shared network
namespace. The agent should reconcile a dummy interface before starting the
nspawn machine:

```text
interface: localdns
addresses:
  169.254.10.10/32
  169.254.10.11/32
state: up
```

The operation must be idempotent:

- Reuse an existing correctly typed interface.
- Add or replace missing listener addresses.
- Reject an existing incompatible interface rather than deleting an interface
  not proven to be agent-owned.
- Tolerate state left by a previously stopped machine.

The interface is host-global even though CoreDNS runs inside nspawn. It is
owned by the host agent lifecycle, not created independently by both `kube1`
and `kube2` systemd units.

To make physical host reboot deterministic, the agent installs an idempotent
host oneshot unit such as `unbounded-localdns-network.service`. Drop-ins for the
active `systemd-nspawn@<machine>.service` require and start after this unit. The
oneshot runs the same interface and conntrack-bypass reconciler used by managed
node start, so the listener addresses and rules exist before the nspawn machine
and its LocalDNS service can start. The unit starts after
`nftables-flush.service`, which otherwise removes the rules during host boot.
This host unit prepares network state only; CoreDNS continues to run inside
nspawn.

A full agent reset stops and removes the ordering drop-ins and host oneshot,
stops all nspawn machines, and then removes the interface. A repave keeps and
reuses the host network state while updating the active machine's ordering.

### Conntrack bypass

Unbounded matches AgentBaker by installing IPv4 raw-table `NOTRACK` rules for
TCP and UDP port 53 in both `OUTPUT` and `PREROUTING`, for both LocalDNS listener
addresses. `OUTPUT` covers node services and host-network pods;
`PREROUTING` covers regular pod traffic. The complete desired set contains eight
rules:

```text
2 chains x 2 listener addresses x 2 protocols = 8 rules
```

Each rule carries the comment `unbounded-localdns: skip conntrack`. The host
network oneshot uses native nftables and owns a dedicated IPv4 table with
`PREROUTING` and `OUTPUT` base chains at raw priority. It atomically reconciles
the complete table to the desired eight `notrack` rules. Dedicated table
ownership allows stale listener addresses to be removed without inspecting or
modifying foreign rules.

Native nftables is required when LocalDNS is enabled; there is no
iptables-compatible fallback. Missing nftables is a fatal preflight error in
offline mode and follows normal host-package remediation in online mode. A host
with nftables installed but without raw-priority `notrack` support fails
preflight.

Host boot orders this reconciliation after `nftables-flush.service` and before
the nspawn machine. Enabled-to-enabled repave keeps the rules. Disable-through-
repave and full reset remove all rules carrying the Unbounded ownership comment
after no active machine depends on LocalDNS.

## Rootfs provisioning

When LocalDNS is enabled, rootfs provisioning adds these operations:

1. Download and verify the CoreDNS artifact and required plugin set.
2. Install the binary at `/usr/local/bin/coredns` with mode `0755`.
3. Install the supervisor at
   `/usr/local/libexec/unbounded-localdns-supervisor` with mode `0755`.
4. Render the Corefile at `/etc/unbounded/localdns/Corefile` with mode `0644`.
5. Write normalized node upstreams, one address per line, to
   `/etc/unbounded/localdns/node-upstreams` with mode `0644`.
6. Render `localdns.slice` with the normalized CPU and memory limits.
7. Render `localdns.service` with the listener addresses and install both units
   in the machine's systemd unit directory.
8. Enable the service for machine boot.
9. Write the machine resolver file with the node listener as its nameserver.

A representative unit is:

```ini
[Unit]
Description=Unbounded LocalDNS
After=network.target
Before=containerd.service
Before=kubelet.service

[Service]
Type=notify
NotifyAccess=all
ExecStart=/usr/local/libexec/unbounded-localdns-supervisor \
    --coredns=/usr/local/bin/coredns \
    --corefile=/etc/unbounded/localdns/Corefile \
    --node-listener=169.254.10.10 \
    --cluster-listener=169.254.10.11 \
    --ready-port=8181
WatchdogSec=60
Restart=on-failure
RestartSec=2
KillMode=mixed
TimeoutStopSec=30
Slice=localdns.slice

DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes

[Install]
WantedBy=multi-user.target
```

The companion slice is:

```ini
[Unit]
Description=Unbounded LocalDNS resource limits

[Slice]
CPUQuota=200%
MemoryMax=128M
```

The rendered values come from `CPULimitInMilliCores` and `MemoryLimitInMB`;
2000 millicores converts to `CPUQuota=200%`. CoreDNS does not need to manage the
dummy interface. The host agent reconciles that state before machine startup.

The initial service sandbox is part of the required unit, not an optional
follow-up. CoreDNS runs as a dynamic unprivileged user with only
`CAP_NET_BIND_SERVICE`; system directories, home directories, kernel tunables,
kernel modules, and cgroups are protected from mutation. Custom Corefiles that
need broader filesystem or kernel access are unsupported by the initial
service-policy contract.

The supervisor starts CoreDNS as its child, probes both readiness endpoints,
and sends `READY=1` only after startup succeeds. Kubelet and containerd receive
systemd drop-ins with `Requires=localdns.service` and
`After=localdns.service`. Systemd does not complete the LocalDNS start job until
the readiness notification arrives, so automatic machine boot cannot start
those dependent services before LocalDNS is ready. The managed node-start path
performs its own readiness check as an additional operation-level guard.

### Ongoing watchdog and graceful shutdown

After startup, the supervisor checks LocalDNS at 20 percent of the systemd
watchdog interval. Each check verifies the HTTP readiness endpoint and performs
a DNS query for `health-check.localdns.local` through both listener addresses.
The probe implementation connects directly to the configured link-local
addresses and does not honor HTTP proxy environment variables.

The supervisor sends `WATCHDOG=1` only when all checks pass. Consecutive failures
therefore let `WatchdogSec=60` restart the unit. Matching AgentBaker, the
supervisor also tracks a sliding window and explicitly triggers restart after 10
failed checks in 10 minutes even when successful checks occur between failures.
Probe failures and restart reasons are written to the machine journal.

On service stop, the supervisor forwards `SIGINT` to CoreDNS and waits for the
child to exit. `TimeoutStopSec=30` and `KillMode=mixed` provide a bounded
fallback if graceful shutdown does not complete. The host-global interface and
`NOTRACK` rules are not removed by a service restart; their lifecycle remains
owned by the host network oneshot, repave, disable, and reset operations.

## Node-start ordering

The current node-start sequence starts the machine, then NVIDIA setup,
containerd, image import, and kubelet. With LocalDNS enabled, the sequence
becomes:

```text
configure containerd, kubelet, and LocalDNS files
    -> reconcile LocalDNS dummy interface
    -> start nspawn machine
    -> start or confirm localdns.service
    -> wait for both LocalDNS readiness endpoints
    -> set up NVIDIA
    -> start containerd
    -> import container images
    -> start kubelet
```

The explicit readiness step is required even though the unit is ordered before
kubelet. The agent should not start kubelet until both listener readiness
endpoints return `OK`.

Readiness verifies that CoreDNS loaded its configuration and opened its
listeners. It does not require the cluster DNS upstream to answer before
kubelet starts, because cluster service routing may not be initialized yet. The
node listener upstream should be reachable at this stage and may be checked
separately.

If LocalDNS fails to start or become ready, node start fails before kubelet
registers. The physical host remains able to resolve DNS and retry or reset the
operation.

## CoreDNS plugin verification

The built-in Corefile requires these compiled-in CoreDNS plugins:

```text
bind
cache
errors
forward
loop
prometheus
ready
whoami
```

`RequiredPlugins` extends this baseline for full replacement templates. For
example, an AKS Flex template may require `hosts`, `nsid`, or `template`.
Plugin names are normalized, deduplicated, and validated as config.

After acquiring the selected binary and before installing it into the rootfs,
the agent executes `coredns -plugins` and compares its output with the complete
required set. A missing plugin fails provisioning before the active machine is
started. Artifact publishing tests perform the same check for the built-in
CoreDNS artifacts. Unbounded does not infer plugin requirements by parsing the
custom Corefile and does not import CoreDNS packages; the caller declares
plugins beyond the baseline explicitly.

## Artifact acquisition

### Online mode

Add `CoreDNS` alongside the existing Kubernetes, containerd, runc, CNI, and
crictl entries in `AgentDownloads`, the Machine API download spec, and
`goalstates.DownloadOverrides`. `Downloads.CoreDNS.Version` selects the target
version; an omitted version uses the agent's compiled-in CoreDNS default. URL
and BaseURL follow the existing download override precedence. The source must
resolve by version, OS, and architecture and must be verified against a trusted
SHA256 value before installation.

The exact upstream archive layout should be encapsulated in artifact path
helpers rather than spread through provisioning code. The installer should
extract only the expected `coredns` executable, reject links and unexpected path
traversal, and install atomically.

### Offline mode

CoreDNS must be part of the same complete bundle selected by
`OfflineArtifacts.Source`. Offline mode must never fall back to an internet
CoreDNS source.

Extend the manifest with a CoreDNS version:

```json
{
  "schemaVersion": 1,
  "versions": {
    "kubernetes": "v1.35.0",
    "containerd": "2.1.8",
    "runc": "1.5.0",
    "cni": "1.5.1",
    "crictl": "1.35.0",
    "coredns": "1.12.3"
  },
  "containerImages": []
}
```

Suggested bundle paths are:

```text
coredns/v1.12.3/bin/linux/amd64/coredns
coredns/v1.12.3/bin/linux/amd64/coredns.sha256
coredns/v1.12.3/bin/linux/arm64/coredns
coredns/v1.12.3/bin/linux/arm64/coredns.sha256
```

Only the selected platform's files need to be present in a platform-specific
OCI manifest. Filesystem and HTTPS bundles may contain one or more supported
architectures.

`versions.coredns` is an additive optional field in schema v1 and does not
require a schema-version bump. Existing schema v1 manifests decode it as absent,
and updated readers continue to accept manifests that omit it. When LocalDNS is
enabled, bundle validation requires a non-empty CoreDNS version plus the binary
and checksum for the host architecture. The manifest version is the source of
truth in offline mode and overrides `Downloads.CoreDNS.Version`, just as the
offline source replaces regular per-artifact download overrides. No online
fallback is allowed. When LocalDNS is disabled, an existing schema v1 bundle
without CoreDNS remains valid. This conditional requirement preserves backward
compatibility for users who do not enable the feature.

`agent-artifacts-builder` should be extended to download, verify, and publish
the selected CoreDNS binary for each requested architecture.

## Metrics and observability

The built-in Corefile enables CoreDNS's native Prometheus plugin. A scraper
connects to:

```text
http://<node-internal-ip>:9253/metrics
```

The metrics endpoint exposes DNS request, response, cache, forwarding, Go
runtime, and process metrics supplied by the selected CoreDNS build.

Unbounded does not add an AgentBaker-style exporter on another port. Prometheus
already publishes `up` for target availability, and native process collectors
cover the common CPU and memory use cases. Product-specific configured-forward
metrics can be added by a product integration if required.

The metrics address binds to the node InternalIP on port `9253`. An in-cluster
Prometheus scraper discovers the Kubernetes Node, obtains the same InternalIP,
and scrapes `http://<node-internal-ip>:9253/metrics`. Binding only to loopback
would not be reachable from a normal Prometheus pod, while a wildcard bind would
unnecessarily expose metrics on every host interface.

When `Kubelet.NodeIP` contains one configured IPv4 address, the resolver derives
`MetricsAddress` from that value. If node IP is omitted, contains no IPv4
address, or cannot identify one intended InternalIP, the caller supplies
`MetricsAddress` explicitly. Preflight verifies that the bind IP is assigned in
the shared network namespace and detects port conflicts. Host firewall policy
and Prometheus target discovery remain deployment concerns.

Logs are collected through the nspawn machine's journal:

```bash
machinectl shell <machine> /usr/bin/journalctl -u localdns.service
```

The agent should include LocalDNS startup and readiness failures in its own
structured operation logs without logging sensitive resolver configuration.

## Reboot and repave behavior

### Node soft reboot

A managed `NodeReboot` stops and starts the existing nspawn machine without
reprovisioning its rootfs. Before stopping the machine, the host agent resolves
the node-start goal state and rediscovers the physical host upstream DNS set.
After stopping the machine and before starting it again, the node-start
configuration phase atomically rewrites the active rootfs Corefile with those
resolved values.

This requires LocalDNS Corefile rendering and installation to be part of the
repeatable node-start configuration path, not only initial rootfs provisioning.
The dummy interface is reconciled before machine start. LocalDNS then starts
before containerd and kubelet, and the agent verifies both listeners before
continuing.

An unmanaged `machinectl reboot` or `systemctl restart localdns` inside the
machine does not invoke host-side goal-state resolution and therefore reuses the
last rendered upstream set.

### Physical host reboot

The nspawn machine is enabled with the host systemd manager and may start
automatically during a physical host reboot. The host LocalDNS network oneshot
and nspawn ordering drop-in recreate the dummy interface before machine start.
Inside the machine, LocalDNS readiness ordering blocks containerd and kubelet
until both listeners are available.

The machine initially uses the last rendered Corefile. Physical host reboot does
not rediscover upstreams in the initial design; a subsequent managed node soft
reboot or repave does.

### Repave

The current lifecycle stops the old machine before starting the new machine.
The sequence is:

```text
stop old machine and its CoreDNS
    -> keep host-global dummy interface
    -> provision new rootfs and LocalDNS files
    -> start new machine and CoreDNS
    -> verify LocalDNS
    -> start containerd and kubelet
```

There is a LocalDNS outage while no worker machine is active, but there are no
running workloads on that worker during the replacement. The physical host
retains working DNS throughout the operation.

Future make-before-break repave would attempt to run two CoreDNS processes in
the shared network namespace and cause listener conflicts. Supporting
simultaneously active old and new machines requires a different ownership
model, unique per-machine listeners, or a host-level singleton. It is explicitly
outside the initial design.

### Reset

A full reset:

1. Stops both nspawn machines.
2. Removes machine rootfs state, including CoreDNS binaries and units.
3. Removes all agent-owned LocalDNS `NOTRACK` rules.
4. Removes the agent-owned LocalDNS dummy interface, host network oneshot, and
   nspawn ordering drop-ins.

Reset does not modify the physical host resolver configuration because this
design never changes it.

### Disable LocalDNS through repave

Changing `LocalDNS.Enabled` from true to false is applied through repave rather
than by modifying the active rootfs in place. The replacement rootfs is built
without the CoreDNS binary, Corefile, LocalDNS unit, or service dependency
drop-ins. Its machine resolver is copied from the physical host resolver, and
kubelet receives the original `Cluster.ClusterDNS` value.

The repave sequence keeps the host-global LocalDNS network state until the
replacement node is healthy:

1. Stop the old machine and its LocalDNS process while retaining its rootfs and
   the host-global LocalDNS network state for rollback.
2. Provision the replacement rootfs with LocalDNS disabled.
3. Start the replacement machine with the normal resolver and cluster DNS
   settings.
4. Verify containerd, kubelet, and node health.
5. Clean up the old rootfs and its LocalDNS files.
6. Remove the old nspawn-ordering drop-in, disable and remove the host LocalDNS
   network oneshot, remove the agent-owned `NOTRACK` rules, and remove the dummy
   interface.

Deferring host-global cleanup until the replacement is healthy preserves the
ability to restart the old machine during rollback. A managed node soft reboot
does not change the LocalDNS enabled state; callers must request repave to apply
that configuration transition.

## Failure handling

### CoreDNS artifact unavailable

Preflight and goal-state resolution fail before rootfs mutation. In offline
mode, a missing CoreDNS artifact is fatal and does not trigger an online
fallback.

### CoreDNS configuration invalid

LocalDNS fails before kubelet starts. The agent reports the service and
readiness failure. The physical host remains usable for correction and retry.

### CoreDNS crashes after node start

Systemd restarts the service. During the restart, nspawn service DNS and pod DNS
may fail. CoreDNS cache and serve-stale behavior reduce failures caused by
upstream outages but cannot serve while the process itself is down.

Kubelet and containerd are intentionally dependent on the node listener after
startup. If this coupling proves operationally unsafe, a later configuration
option may leave the machine resolver on the physical host resolver while using
LocalDNS only for ClusterFirst pods.

### Host upstream changes and refresh

The initial design does not include a refresh timer. Physical host DNS settings
are normally stable, the reviewed AgentBaker implementation resolves upstreams
during managed service lifecycle rather than through a timer, and Unbounded
never points the physical host resolver at LocalDNS. A stale LocalDNS upstream
therefore cannot prevent the host agent from performing recovery.

The Corefile keeps the node upstreams discovered during the most recent managed
provisioning, node soft reboot, or repave. If the physical host upstream set
changes while the node continues running, LocalDNS may use stale destinations
until one of those operations rerenders the Corefile. The physical host remains
unaffected because its resolver configuration is unchanged.

When settings need to be refreshed, an operator or controller uses a managed
node soft reboot or repave. Both operations resolve the current physical host
upstreams, rerender `CorefileTemplate`, install the resulting Corefile, and
verify LocalDNS before starting kubelet. Node soft reboot reuses the existing
rootfs; repave installs the settings into the replacement rootfs. This provides
an explicit recovery mechanism without a polling loop, resolver-backend watcher,
or concurrent refresh and repave coordination.

Periodic reconciliation can be added later if supported host resolver backends
require it. It would be an availability improvement over AgentBaker behavior
rather than a parity requirement.

### Listener or metrics port conflict

Preflight fails when an address or port is already owned by an unrelated
process or incompatible interface. The agent must not delete unknown network
state to force LocalDNS startup.

## Preflight

Add LocalDNS checks near the phases they validate:

| Check | Purpose |
|---|---|
| `localdns-config` | Validate the complete LocalDNS config, apply defaults, and render the Corefile template with discovered runtime values. |
| `localdns-artifact` | Validate the online artifact or required offline bundle entries for the host architecture and verify required plugins when the selected binary is locally available. |
| `localdns-interface` | Detect an incompatible existing interface or address ownership conflict. |
| `localdns-conntrack` | Validate native nftables support for raw-priority `notrack` rules. |
| `localdns-ports` | Detect DNS, readiness, and metrics listener conflicts. |
| `localdns-upstreams` | Confirm the host uses a supported direct or systemd-resolved layout, reject split-DNS policy, and require at least one usable direct node upstream. |
| `localdns-rootfs` | Confirm LocalDNS files and unit paths can be written into the target rootfs. |

`localdns-config` runs only when LocalDNS is enabled and reports a fatal error
for invalid configuration. It validates listener and cluster DNS IPv4
addresses, listener uniqueness, metrics address syntax, CPU and memory limits,
required plugin names, Corefile template size, strict template parsing and
execution, non-empty rendered output, usable host upstreams, and listener-loop
rejection. The upstream check also rejects unsupported local stubs and resolver
policies that cannot be represented by one default upstream set. The same
normalization and validation functions are used by preflight and goal-state
resolution so a configuration accepted by preflight is interpreted identically
during bootstrap.

Preflight remains non-mutating. It does not create the interface, install the
binary, write the Corefile, bind ports, or start CoreDNS. Unbounded does not
import CoreDNS or Caddy parser packages for this check. A successfully rendered
custom Corefile is still trusted caller input; without running the selected
CoreDNS binary, preflight cannot prove every CoreDNS directive is valid or that
the template satisfies every forwarding contract. Activation and readiness
checks remain the final validation before kubelet starts.

Interface and port checks distinguish conflicting third-party ownership from
matching state owned by the active LocalDNS deployment. This allows preflight
and repave goal-state resolution to run while the old machine's LocalDNS process
still owns the desired ports.

Offline mode treats a missing required CoreDNS artifact as an error. Regular
online mode may report a source reachability failure according to the existing
artifact preflight policy.

## Security considerations

- CoreDNS runs inside a privileged nspawn machine that shares the host network
  namespace, but `localdns.service` itself uses `DynamicUser=yes`, a
  `CAP_NET_BIND_SERVICE`-only capability set, `NoNewPrivileges=yes`, and the
  filesystem and kernel protections shown in the required unit.
- The built-in Corefile binds DNS only to the configured listener addresses. A
  caller-supplied Corefile is trusted configuration and may declare additional
  binds, ports, file reads, or network destinations that Unbounded cannot infer
  from template rendering alone. Product adapters must validate or constrain
  untrusted product input before producing a template.
- Metrics bind to the node InternalIP rather than loopback or a wildcard
  address. Host firewall policy still controls which cluster networks may
  scrape the endpoint.
- Artifact checksums are mandatory in online and offline modes.
- Resolver discovery and logs must not expose signed artifact URLs or other
  credentials.
- The interface and conntrack reconcilers must not remove or replace network
  objects or firewall rules they cannot prove are agent-owned.
- `CorefileTemplate` has bounded input and rendered sizes and does not allow
  shell interpolation. Rendering uses a strict context and a minimal allowlist
  of pure template functions. These controls protect template rendering but do
  not sandbox valid CoreDNS directives in the rendered file.

## Remaining AgentBaker parity gaps

This design now covers the two-listener dataplane, machine and pod resolver
wiring, CoreDNS artifact and plugin validation, resource limits, watchdog,
graceful shutdown, native metrics, and `NOTRACK` rules. The following
differences remain relative to the reviewed AgentBaker implementation.

| Area | AgentBaker | Unbounded and Flex disposition |
|---|---|---|
| Physical host DNS | Points the physical host resolver at the node listener. | Intentionally different. Only the nspawn resolver uses LocalDNS so host recovery never depends on the worker machine. |
| Critical-FQDN hosts plugin | Generates base and hosts-enabled Corefiles, refreshes critical AKS FQDNs, preserves last-known-good values, and hot-reloads the hosts file. | Product work in AKS Flex Node. Flex supplies the template, required plugin, hosts file, refresh service and timer, and FQDN policy. |
| Hosts-plugin node annotation | Reconciles `kubernetes.azure.com/localdns-hosts-plugin=enabled`. | Product work in the AKS Flex host daemon after the node and hosts data are ready. |
| AKS-specific Corefile policy | Generates VNetDNS and KubeDNS domain overrides, `nsid`, and AKS-specific template responses. | Product work in the AKS Flex profile adapter and full replacement `CorefileTemplate`; not part of the generic built-in template. |
| Custom metrics exporter | Serves service, cgroup CPU and memory, timestamp, and configured-forward metrics on port `9353`. | Intentionally omitted from Unbounded. Native CoreDNS metrics use the node InternalIP on port `9253`. Exact AgentBaker metric-name compatibility requires a Flex-owned exporter or agent metrics endpoint. |
| Exporter discovery label | Adds `kubernetes.azure.com/localdns-exporter=enabled`. | Product work if AKS monitoring requires this contract. Generic Unbounded leaves target discovery to the deployment. |
| Ordinary LocalDNS service restart | The AgentBaker wrapper rediscovers upstreams and regenerates the Corefile whenever the service starts. | The nspawn supervisor restarts CoreDNS with the last rendered Corefile. Upstreams are rediscovered by managed node soft reboot or repave. |
| Physical host reboot upstream discovery | LocalDNS starts on the host and rediscovers upstreams during boot. | The host network oneshot restores interface and `NOTRACK` state, but the nspawn machine initially uses the last rendered Corefile. A managed node soft reboot or repave refreshes it. |
| Upstream discovery backend | Reads the systemd-resolved upstream resolver file on the controlled AKS image. | Supports direct nameservers in `/etc/resolv.conf` and the systemd-resolved upstream resolver file when there is no split-DNS policy. Other local stubs and split-DNS layouts fail preflight. |
| Binary delivery | Extracts CoreDNS from an image cached in the AKS VHD. | Intentionally different but functionally equivalent. Unbounded uses `Downloads.CoreDNS` or the complete offline bundle with checksum and plugin verification. |

The physical-host DNS scope and restart-time upstream discovery differences are
consequences of running CoreDNS inside nspawn and keeping physical host DNS
independent. They should not be removed solely in pursuit of literal host-level
parity. End-to-end AKS parity also depends on the Flex-owned profile adapter,
hosts plugin, annotations, and any required monitoring contract being
implemented and tested with Unbounded.

## Compatibility

LocalDNS is disabled by default. Existing configurations, rootfs resolver
behavior, kubelet `--cluster-dns`, artifact bundles, and node-start ordering are
unchanged unless the feature is enabled.

Offline manifest schema v1 remains unchanged. Enabling LocalDNS conditionally
requires `versions.coredns` and the matching bundle content; disabling LocalDNS
continues to accept existing schema v1 manifests that omit CoreDNS.

Consumers that already replace `Cluster.ClusterDNS` with another node-local
listener should not enable this feature without migrating that integration to
the new LocalDNS goal state.

Enabling LocalDNS also requires a supported host resolver layout. Hosts that use
an unsupported local caching stub or split-DNS routing continue to work with
LocalDNS disabled, but LocalDNS preflight rejects the configuration.

## Test strategy

### Unit tests

- Config defaults and validation.
- Host upstream discovery from direct `/etc/resolv.conf` nameservers and the
  systemd-resolved upstream resolver file.
- Rejection of empty upstreams, listener loops, unsupported local stubs, and
  systemd-resolved split-DNS policy.
- Corefile rendering for IPv4 and multiple node upstreams.
- Upstream normalization and rediscovery during managed node soft reboot and
  repave.
- Machine resolver rendering and preservation of search/options directives.
- Kubelet cluster DNS selection with LocalDNS enabled and disabled.
- Interface reconciliation for absent, matching, incomplete, and conflicting
  state.
- Exact `notrack` rule generation, idempotent native nftables reconciliation,
  stale owned-rule removal, and foreign-rule preservation.
- Online `Downloads.CoreDNS` source and version resolution and checksum
  verification.
- Offline `versions.coredns` precedence, conditional bundle requirements, and
  prohibition of online fallback.
- Metrics and readiness address rendering.
- CPU and memory defaulting, validation, conversion, and slice rendering.
- Watchdog success, consecutive failure, sliding-window failure, child exit,
  graceful shutdown, and shutdown timeout behavior.
- Required systemd sandbox directives and CoreDNS startup under the dynamic
  service user.
- Baseline and product-specific plugin-set verification from `coredns -plugins`
  output.

### Integration tests

- Start CoreDNS in an nspawn machine and resolve through both listeners.
- Verify the node listener can resolve the API server before kubelet starts.
- Verify the cluster listener forwards after cluster service routing is ready.
- Verify `dnsPolicy: Default` and `dnsPolicy: ClusterFirst` pods receive the
  intended nameservers.
- Stop CoreDNS and confirm the physical host resolver continues to work.
- Make each watchdog probe fail independently and confirm systemd restarts
  LocalDNS according to consecutive and sliding-window policy.
- Restart CoreDNS and confirm DNS recovers.
- Change the physical host upstream set, perform a managed node soft reboot,
  and confirm the new Corefile uses the changed set while the machine resolver
  address remains unchanged.
- Scrape native metrics from the configured node address.

### Lifecycle tests

- Re-run provisioning without duplicating interfaces, addresses, units, or
  files.
- Reboot the host, verify the host network oneshot recreates the dummy interface
  and all eight `NOTRACK` rules after nftables flush and before nspawn starts,
  and verify LocalDNS becomes ready before kubelet.
- Repave from `kube1` to `kube2` with LocalDNS enabled and verify the interface
  is reused.
- Repave from enabled to disabled, verify the replacement uses normal DNS, and
  verify LocalDNS host-global state is removed only after replacement health.
- Reset and verify the `NOTRACK` rules, dummy interface, and machine-local
  artifacts are removed.
- Run the same flows from filesystem, OCI, and HTTPS offline bundles.

## Alternatives considered

### Run LocalDNS on the physical host

A host-level singleton survives nspawn repave and can serve physical host DNS.
It also requires host-level binary, systemd, resolver, and cleanup ownership.
This design does not need host DNS and prefers to keep the service with the
worker rootfs.

A host singleton may become preferable if Unbounded adopts overlapping
make-before-break machines or requires uninterrupted DNS while no nspawn worker
is active.

### Use only the cluster listener

A single `169.254.10.11` listener is sufficient for ClusterFirst pods and is the
smallest implementation. It does not cache DNS for kubelet, containerd, nspawn
services, or Default-policy pods. This design includes the node listener because
machine-local DNS caching is an explicit requirement.

### Point the machine resolver at the cluster listener

This creates a bootstrap dependency on cluster service routing and is rejected.

### Run LocalDNS as a Kubernetes DaemonSet

A DaemonSet follows standard Kubernetes NodeLocal DNSCache patterns but starts
after kubelet and containerd and cannot support machine services during node
bootstrap. Cluster deployment policy is also outside the standalone agent's
control.

### Extract CoreDNS from a container image archive

The offline bootstrap path already supports container image archives, but
extracting a host executable from image layers adds format and ordering
complexity. A directly checksummed binary is simpler and matches other rootfs
binary artifacts.

### Add an AgentBaker-compatible custom exporter

The native CoreDNS endpoint provides the DNS, cache, forwarding, process, and
runtime metrics needed for the initial feature. A second exporter and port are
not justified without a compatibility requirement for AgentBaker-specific
metric names.
