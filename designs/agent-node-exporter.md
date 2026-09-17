# Agent nspawn Node Exporter

## Summary

This document proposes an optional Prometheus node exporter that runs as a
systemd service inside the active systemd-nspawn machine. The exporter reads the
Linux interfaces visible to that machine and exposes Prometheus metrics on a
caller-configurable node address.

Unbounded owns artifact acquisition, rootfs installation, systemd lifecycle,
offline bundle integration, and local endpoint validation. Product integrations
own their scrape configuration, firewall policy, collector profile, and
certificate issuance. In particular, an AKS integration can select port `19100`
and AgentBaker-compatible collector arguments without making those choices
generic Unbounded defaults.

The initial feature is disabled by default. Enabling it does not install a
Prometheus scraper and does not push metrics to a control plane. A scraper must
discover and pull the endpoint separately.

## Background

The agent configuration already models optional machine services and a complete
offline artifact source. `AgentConfig` carries both optional service
configuration and `OfflineArtifacts` (`pkg/agent/config/config.go:50-74`).
Download overrides already have an optional CoreDNS entry
(`pkg/agent/goalstates/downloads.go:21-22`), and the offline resolver converts
bundle entries into those overrides
(`pkg/agent/goalstates/offline_artifacts.go:193-242`).

Machine-specific state is split between rootfs and node-start goal states
(`pkg/agent/goalstates/rootfs.go:11-23` and
`pkg/agent/goalstates/nodestart.go:6-23`). Rootfs provisioning installs optional
machine assets before startup (`pkg/agent/phases/rootfs/provision.go:19-29`),
while node start configures services and starts the nspawn machine
(`pkg/agent/phases/nodestart/start.go:22-35`). Node exporter should use these
same extension points instead of adding a separate host lifecycle.

The nspawn machine shares the physical host network namespace. Node exporter can
therefore bind a node address that is reachable outside the machine, while the
process and service remain owned by the active machine. Process, mount, and
cgroup visibility are not equivalent to network visibility, so the metric
contract must be validated separately rather than inferred from shared
networking.

This proposal is motivated by node-level observability requirements such as
[AKSFlexNode issue 296](https://github.com/Azure/AKSFlexNode/issues/296). The
AKS profile referenced by that issue is based on the AgentBaker node exporter
assets at commit
[`8b2add778eb4a67902abdf1dd0a1aa455a9a4cdf`](https://github.com/Azure/AgentBaker/tree/8b2add778eb4a67902abdf1dd0a1aa455a9a4cdf/parts/linux/cloud-init/artifacts/node-exporter).
The generic feature does not promise literal AgentBaker policy unless a caller
supplies that profile.

## Goals

- Run node exporter as a systemd service inside the active nspawn machine.
- Expose CPU, memory, filesystem, network, and other enabled node exporter
  collectors through a Prometheus `/metrics` endpoint.
- Bind to one explicit node IP instead of a wildcard address.
- Keep the feature opt-in and cloud-neutral.
- Let product integrations select the port and collector arguments.
- Install the binary through the same online and offline mechanisms as other
  rootfs artifacts.
- Keep bootstrap, managed restart, physical host reboot, repave, and reset
  behavior idempotent.
- Support native node exporter TLS with caller-managed certificate files.
- Verify locally that the configured endpoint is serving metrics.
- Preserve existing behavior when the feature is disabled.

## Non-goals

- Running Prometheus or another scraper on the node.
- Pushing or remotely writing metrics to a control plane.
- Defining Kubernetes ServiceMonitor, PodMonitor, or scrape discovery policy.
- Opening host firewall ports or changing external routing.
- Issuing, approving, or rotating TLS certificates.
- Reusing kubelet certificates automatically.
- Defining an AKS-specific port, collector set, certificate policy, label, or
  annotation in the generic API.
- Guaranteeing that every physical-host metric is visible through the nspawn
  mount, PID, and cgroup namespaces.
- Supporting two simultaneously active nspawn machines that bind the same
  address and port.
- Adding a node exporter textfile collector producer.
- Replacing kubelet, container runtime, or application metrics endpoints.

## Metrics flow

Node exporter is a pull endpoint:

```text
Linux /proc, /sys, mounts, and network interfaces visible in nspawn
    -> node-exporter.service
    -> http(s)://<node-address>/metrics
    -> deployment-owned Prometheus-compatible scraper
    -> deployment-owned metrics storage
```

Node exporter does not need Kubernetes API or kubelet credentials. The scraper
must be able to route to the configured address and must be configured to trust
and authenticate to the endpoint when TLS is enabled.

## Proposed configuration

Add an optional `NodeExporter` block to `config.AgentConfig`:

```json
{
  "NodeExporter": {
    "Enabled": true,
    "ListenAddress": "10.20.1.7:9100",
    "ExtraArgs": [
      "--no-collector.wifi",
      "--no-collector.hwmon"
    ]
  },
  "Downloads": {
    "NodeExporter": {
      "Version": "<pinned-version>"
    }
  }
}
```

A possible intermediate representation is:

```go
type AgentNodeExporterConfig struct {
    Enabled       bool                   `json:"Enabled"`
    ListenAddress string                 `json:"ListenAddress,omitempty"`
    ExtraArgs     []string               `json:"ExtraArgs,omitempty"`
    TLS           *NodeExporterTLSConfig `json:"TLS,omitempty"`
}

type NodeExporterTLSConfig struct {
    Enabled         bool   `json:"Enabled"`
    CertificateFile string `json:"CertificateFile,omitempty"`
    PrivateKeyFile  string `json:"PrivateKeyFile,omitempty"`
    ClientCAFile    string `json:"ClientCAFile,omitempty"`
}
```

`Downloads.NodeExporter` uses the existing download source shape with
`BaseURL`, `URL`, and `Version`. The selected implementation must pin a compiled
in default version rather than following an unversioned latest release.

### Defaults

When enabled:

- `ListenAddress` defaults to the IPv4 node address selected by the existing
  kubelet non-cloud address resolution followed by port `9100`.
- If no IPv4 node address can be selected, goal-state resolution fails and the
  caller must provide `ListenAddress`.
- The default collector set is the upstream node exporter default.
- `ExtraArgs` is empty.
- TLS is disabled.
- The version comes from `Downloads.NodeExporter.Version` or the compiled-in
  default. It is not derived from `Cluster.Version`.

The generic port is `9100`, the conventional upstream node exporter port. A
product profile can explicitly choose another port. For example, an AKS adapter
can bind the Kubernetes Node InternalIP on port `19100`.

The resolver must not use the first address returned by `hostname -I`. Interface
ordering is unstable on multi-homed and overlay-networked hosts. An explicit
`Kubelet.NodeIP` wins, followed by the same deterministic node address rules
used by other agent metrics listeners.

### Extra argument contract

`ExtraArgs` permits product-specific collector selection without growing the
Unbounded API for every upstream flag. Validation must:

- require each entry to be one argument rather than a shell fragment;
- reject NUL bytes, newlines, and other control characters;
- bound the argument count and aggregate size;
- reject `--web.listen-address` and `--web.config.file`, which are owned by the
  agent;
- reject arguments that change paths owned by another nspawn machine unless a
  future explicit API supports those paths.

The rootfs phase renders arguments as distinct, correctly escaped systemd
`ExecStart` tokens. It must not invoke a shell and must not concatenate caller
input into an `EnvironmentFile` that is later reparsed as shell syntax.

### Product-specific configuration

An AKS-facing adapter can translate its product policy into generic config:

```json
{
  "NodeExporter": {
    "Enabled": true,
    "ListenAddress": "10.20.1.7:19100",
    "ExtraArgs": [
      "--no-collector.wifi",
      "--no-collector.hwmon",
      "--collector.cpu.info",
      "--collector.filesystem.mount-points-exclude=^/(dev|proc|sys|run/containerd/.+|var/lib/docker/.+|var/lib/kubelet/.+)($|/)",
      "--collector.netclass.ignored-devices=^(azv.*|veth.*|[a-f0-9]{15})$",
      "--collector.netclass.netlink",
      "--collector.netdev.device-exclude=^(azv.*|veth.*|[a-f0-9]{15})$",
      "--no-collector.arp.netlink"
    ]
  }
}
```

This translation belongs to the product adapter. Unbounded validates and runs
the resulting profile but does not attach AKS meaning to these values.

## Goal-state model

Add a resolved node exporter goal to both rootfs and node-start state:

```go
type NodeExporter struct {
    Enabled       bool
    Version       string
    ListenAddress string
    ExtraArgs     []string
    TLS           NodeExporterTLS
}

type NodeExporterTLS struct {
    Enabled         bool
    CertificateFile string
    PrivateKeyFile  string
    ClientCAFile    string
}
```

The rootfs goal uses `RootFS.Downloads.NodeExporter` together with the resolved
version, matching the existing download-override pattern. The node-start goal
uses the listen address, scheme, and TLS settings for service validation after
machine startup.

Resolution must happen before rootfs mutation. It validates config, selects the
version, resolves online or offline sources, and checks address conflicts that
can be detected without binding the port.

Mutable slices must be copied when config and goal states are copied. A caller
must not be able to modify `ExtraArgs` after goal-state resolution.

## Address and port validation

`ListenAddress` must:

- use `IP:port` syntax;
- contain a unicast IPv4 address in the initial implementation;
- use a port from 1 through 65535;
- refer to an address assigned in the shared network namespace;
- not use wildcard or multicast addresses;
- not conflict with another configured Unbounded listener.

Preflight checks the live port. A matching listener owned by the active node
exporter is accepted so preflight and restart remain idempotent. A listener
owned by an unrelated process is an error. Ownership checks must not kill or
reconfigure foreign processes.

IPv6 can be added after node address selection, scraping, and certificate SAN
behavior are validated end to end.

## Rootfs provisioning

When enabled, rootfs provisioning performs these operations:

1. Acquire and verify the architecture-specific release archive.
2. Extract exactly the expected `node_exporter` regular file.
3. Install it atomically at `/usr/local/bin/node_exporter` with mode `0755`.
4. Run `node_exporter --version` and require the configured version.
5. Write `/etc/unbounded/node-exporter/web-config.yml` when TLS is enabled.
6. Write `node-exporter.service` into the machine systemd unit directory.
7. Enable `node-exporter.service` for `multi-user.target`.

Archive extraction rejects absolute paths, parent traversal, links, duplicate
candidate binaries, and unexpected executable names. The installer never runs
an executable from the archive before it has been written to its final
agent-owned path and verified.

The operation is idempotent. An existing binary with the requested version can
be reused. Config and unit files are atomically replaced only when desired
content differs.

### Systemd service

A representative HTTP unit is:

```ini
[Unit]
Description=Prometheus Node Exporter
Documentation=https://github.com/prometheus/node_exporter
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/node_exporter \
    --web.listen-address=10.20.1.7:9100
Restart=on-failure
RestartSec=10
NoNewPrivileges=yes
ProtectHome=yes
ProtectSystem=strict
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

The final sandbox must be tested against the complete supported default
collector set. Options that hide `/proc`, `/sys`, devices, network interfaces,
or cgroups must not be enabled merely to improve a systemd security score. Node
exporter should run without capabilities and as an unprivileged dynamic user if
integration tests show that the required collectors remain accurate. If they
do not, the design must document the minimal additional access rather than
defaulting silently to unrestricted root.

The service is enabled in the machine rootfs, so normal nspawn startup starts
it on physical host reboot as well as managed start. It has no ordering or
runtime dependency on kubelet or containerd.

## Node-start ordering

The managed sequence is:

```text
use the node exporter service and TLS web config installed in the rootfs
    -> start the nspawn machine and its enabled node exporter service
    -> start containerd
    -> import container images
    -> start kubelet
    -> validate the node exporter service
```

Node exporter does not need containerd or kubelet, so machine systemd may start
it as soon as the nspawn machine reaches `multi-user.target`. Endpoint
validation runs after the worker startup tasks so an observability failure does
not prevent kubelet from being launched.

For HTTP, the managed node-start path checks:

```text
http://<listen-address>/metrics
```

Success requires a 2xx response, a bounded response body, and Prometheus text
containing `node_exporter_build_info`. The probe does not use a configured HTTP
proxy.

For TLS, Unbounded verifies that `node-exporter.service` is active. The user is
responsible for validating the complete HTTPS or mutual TLS scrape path because
Unbounded does not own the scraper trust or client credentials.

Node exporter is an observability service, not a kubelet prerequisite. The
systemd unit does not add `Requires=node-exporter.service` to kubelet. A managed
start with the feature enabled still reports an error if node exporter service
validation fails, but the failure does not stop an already started kubelet. Physical host reboot relies on systemd restart policy and
monitoring alerts rather than blocking the worker boot indefinitely.

## TLS

TLS is optional and disabled by default. A user enables it by providing paths
to a serving certificate and private key inside the nspawn machine:

```json
{
  "NodeExporter": {
    "Enabled": true,
    "TLS": {
      "Enabled": true,
      "CertificateFile": "/etc/node-exporter/tls/tls.crt",
      "PrivateKeyFile": "/etc/node-exporter/tls/tls.key"
    }
  }
}
```

Unbounded renders the native node exporter web configuration and starts node
exporter with:

```text
--web.config.file=/etc/unbounded/node-exporter/web-config.yml
```

For server TLS, the generated configuration is:

```yaml
tls_server_config:
  cert_file: /etc/node-exporter/tls/tls.crt
  key_file: /etc/node-exporter/tls/tls.key
  client_auth_type: NoClientCert
```

A user can require mutual TLS by also providing `ClientCAFile`. Unbounded then
sets `client_auth_type: RequireAndVerifyClientCert` and `client_ca_file` in the
web configuration.

The configured paths must be absolute, must exist inside the machine, and must
be readable by `node-exporter.service`. The serving certificate must be valid
for the address or DNS name used by the scraper. The scraper must trust the
serving certificate issuer and, for mutual TLS, present a client certificate
trusted by `ClientCAFile`.

The user owns certificate and CA provisioning, permissions, renewal, and
rotation. After replacing a certificate, key, or CA file, the user is
responsible for restarting `node-exporter.service` so the new files are used.
Unbounded does not issue certificates, watch certificate files, or automate
rotation.

If TLS is enabled and a required file is missing or invalid, the service fails.
Unbounded never falls back to HTTP. Certificate and key contents are not stored
in agent config or written to logs.

## Artifact acquisition

### Online mode

Add `NodeExporter` to:

- the internal agent downloads configuration;
- `goalstates.DownloadOverrides`;
- the Machine API download spec;
- conversion and deep-copy paths;
- artifact reachability preflight.

The default source follows the upstream Prometheus release layout:

```text
https://github.com/prometheus/node_exporter/releases/download/v<version>/node_exporter-<version>.linux-<arch>.tar.gz
```

Upstream publishes release checksums in a checksum manifest rather than as an
adjacent checksum for every archive. Online resolution downloads the bounded
checksum manifest, selects the exact archive filename, validates the digest
format, and verifies the archive before extraction. The default checksum source
is `sha256sums.txt` in the selected release directory. `BaseURL` preserves that
layout. A full `URL` override uses an adjacent `.sha256` checksum file.

No download path may use an unversioned `latest` URL.

### Offline mode

Node exporter is included in the same complete bundle selected by
`OfflineArtifacts.Source`. Offline mode never falls back to GitHub.

Extend manifest schema v1 with an optional version:

```json
{
  "schemaVersion": 1,
  "versions": {
    "kubernetes": "v1.35.0",
    "containerd": "2.1.8",
    "runc": "1.5.0",
    "cni": "1.5.1",
    "crictl": "1.35.0",
    "nodeExporter": "<pinned-version>"
  },
  "containerImages": []
}
```

Suggested paths are:

```text
node-exporter/v<version>/node_exporter-<version>.linux-amd64.tar.gz
node-exporter/v<version>/node_exporter-<version>.linux-amd64.tar.gz.sha256
node-exporter/v<version>/node_exporter-<version>.linux-arm64.tar.gz
node-exporter/v<version>/node_exporter-<version>.linux-arm64.tar.gz.sha256
```

Only the selected platform is required in a platform-specific OCI manifest.
Filesystem and HTTPS bundles may include multiple architectures.

`versions.nodeExporter` remains optional in schema v1 for backward
compatibility. If `NodeExporter.Enabled` is true, offline resolution requires a
non-empty version and the archive plus checksum for the host architecture. If
the feature is disabled, existing manifests without node exporter remain valid.
If a manifest declares a node exporter version, bundle validation treats the
corresponding archive and checksum as required content.

The manifest version is authoritative in offline mode. A conflicting explicit
`Downloads.NodeExporter.Version` is ignored along with other regular download
overrides because `OfflineArtifacts.Source` represents the complete source.
The resolved offline override carries the manifest version into goal-state
resolution. Although offline bundles are grouped by Kubernetes version, node
exporter remains independently versioned and the same node exporter release may
appear in bundles for multiple Kubernetes versions.

Extend `agent-artifacts-builder` and publishing workflows to acquire, verify,
and publish node exporter for every supported architecture. The builder writes
an adjacent checksum artifact into the bundle even though the upstream release
uses one checksum manifest. Published filesystem, HTTPS, and OCI forms contain
the same logical paths.

The existing optional CoreDNS pattern demonstrates that schema v1 can add an
optional component version and conditionally include its files
(`pkg/agent/bootstrapartifacts/manifest.go:23-48` and
`pkg/agent/bootstrapartifacts/paths.go:53-73`). Node exporter should follow the
same backward-compatible pattern.

## Metrics and observability

Running inside nspawn intentionally measures the Linux environment visible to
the worker machine. Shared networking alone does not prove physical-host metric
parity.

The implementation must validate at least:

- `node_cpu_seconds_total` against physical-host `/proc/stat`;
- `node_memory_MemTotal_bytes` against physical-host `/proc/meminfo`;
- `node_filesystem_*` against the intended machine and workload mounts;
- `node_network_*` against interfaces in the shared network namespace;
- build and process metrics for the exporter itself.

Expected differences must be documented. In particular, filesystem metrics may
reflect the nspawn mount namespace rather than every physical-host mount, and
process or cgroup collectors may be scoped by nspawn isolation. Collector errors
must be observable in logs and scrape output.

If required CPU, memory, filesystem, or network metrics are materially incorrect
inside nspawn, the implementation must stop and revisit service placement. A
host-level exporter is the fallback design for physical-host semantics, but it
has separate installation, upgrade, reset, and blue-green ownership costs.

### Scraper integration

Unbounded only exposes the endpoint. A product integration must separately
ensure:

- the Kubernetes Node advertises the same reachable address;
- routing and firewall policy permit scraper traffic;
- target discovery includes these nodes and the selected port;
- the scrape scheme matches HTTP or HTTPS;
- the scraper trusts the serving CA and presents a client identity when mTLS is
  enabled;
- the remote metrics backend retains the expected series.

## Reboot and repave behavior

### Initial bootstrap

Rootfs provisioning installs the verified binary, config, unit, and enablement
link. Starting the nspawn machine starts the service. After the worker services
have started, the managed start path checks the local endpoint in HTTP mode or
checks that the service is active in TLS mode. It reports failure if the enabled
exporter is unavailable.

### Managed node restart

The existing rootfs and unit are reused. The service starts through normal
systemd enablement and is checked again. Changes to node exporter configuration
are applied through repave so the replacement rootfs receives the new service
content.

### Physical host reboot

The enabled nspawn machine starts through host systemd. Its machine systemd
starts node exporter from the persisted rootfs. No online artifact lookup is
required during reboot. TLS uses the certificate and CA files currently
available at the user-configured paths.

### Repave

The replacement rootfs receives its own binary and unit. The current
break-before-make lifecycle stops the old machine before starting the
replacement, avoiding a shared-port conflict. The old rootfs is retained until
the replacement reaches the lifecycle commit point, preserving rollback.

A future make-before-break lifecycle cannot start two exporters on the same
shared address. It must either assign per-machine ports, move exporter ownership
to a host singleton, or delay the replacement exporter until the old machine
stops.

### Disable through repave

Changing `Enabled` from true to false is applied through a replacement rootfs.
The replacement contains no node exporter binary, web config, service unit, or
enablement link. Cleanup of the old machine removes its assets with the old
rootfs.

### Reset

Reset stops and removes nspawn machines and their rootfs state. Because this
design installs no host-level node exporter binary or unit, normal machine
cleanup removes the service. Any product-owned host certificate source or
firewall rule remains owned by that product and is not removed by generic reset.

## Failure handling

### Artifact unavailable

Goal-state resolution or preflight fails before rootfs mutation when possible.
A checksum mismatch or malformed archive fails installation. Offline mode does
not attempt an online source.

### Port conflict

Preflight or startup fails without stopping the conflicting process. The error
identifies the logical listen address but does not expose unrelated process
command lines or environment.

### Exporter crash

Systemd restarts the service. Kubelet and workloads continue running. Monitoring
should alert on scrape failure.

### Missing or invalid TLS files

The service fails closed and retries according to systemd policy. Plain HTTP is
never substituted. The user corrects or replaces the files and restarts the
service.

### Scraper unavailable

The local exporter remains healthy. Unbounded does not consider a remote
Prometheus outage a node lifecycle failure because it has no generic scraper
identity or endpoint.

## Preflight

Add checks near the phases they predict:

| Check | Purpose |
|---|---|
| `node-exporter-config` | Validate enablement, address, port, argument bounds, reserved flags, TLS mode, and clean certificate paths. |
| `node-exporter-artifact` | Validate the online archive and checksum source, or required offline manifest entries for the host architecture. |
| `node-exporter-port` | Detect a conflicting listener while accepting a matching active deployment. |
| `node-exporter-certificates` | When files should already exist, validate certificate/key parsing, key match, expiry, server usage, SAN coverage, and optional client CA parsing. |

Preflight is non-mutating. It does not install the binary, create certificate
files, bind the port, start a service, or change a firewall.

Certificate files provisioned only during rootfs creation cannot be fully
validated by host preflight. In that case preflight reports that validation is
deferred, and rootfs installation or service activation performs the fatal
check. A missing or invalid TLS file never causes an HTTP fallback.

In offline mode, missing node exporter manifest data or files is fatal whenever
the feature is enabled. The check must not probe an online source as a fallback.

## Security considerations

- Bind one configured node IP, never `0.0.0.0` by default.
- Leave firewall and network authorization to the deployment that understands
  scraper source networks.
- Use TLS and mTLS when the deployment scraper supports them and the network is
  not otherwise trusted.
- Treat private keys as product-managed files. Do not include key bytes in
  agent config, logs, status, or goal-state diagnostics.
- Require checksum verification for online and offline archives.
- Reject shell interpretation of collector arguments.
- Run with no capabilities and an unprivileged identity when collector tests
  permit it.
- Do not enable systemd sandbox settings that silently falsify required metrics.
- Bound readiness responses and checksum manifests to prevent memory exhaustion.
- Redact signed artifact URL query strings from errors and logs.
- The `/metrics` endpoint can reveal host names, interfaces, mount paths, kernel
  details, and capacity. Plain HTTP exposure must be an explicit deployment
  decision, even when it is needed for compatibility with an existing scraper.

## Compatibility

Node exporter is disabled by default. Existing configs, rootfs contents,
service ordering, ports, and offline bundles remain valid.

Adding optional `versions.nodeExporter` to schema v1 is backward compatible.
Updated readers accept old manifests when the feature is disabled. Updated
bundle publishers may include node exporter, but consumers that do not enable
the feature do not install or run it.

The Machine API changes are additive. Generated deepcopy and CRD artifacts must
be regenerated through the repository generation workflow rather than edited by
hand.

## Test strategy

- Unit tests cover configuration, address selection, download resolution,
  archive verification, systemd rendering, TLS configuration, and idempotency.
- Offline tests require the node exporter version, archive, and checksum when
  enabled and verify that no online fallback occurs.
- Integration tests start node exporter inside nspawn and scrape CPU, memory,
  filesystem, and network metrics from the configured node address.
- TLS integration tests cover server TLS, optional mutual TLS, missing files,
  and user-managed certificate replacement followed by service restart.
- Lifecycle tests cover initial bootstrap, repeated execution, machine restart,
  physical host reboot, repave, disablement, and reset.
- Online and filesystem, HTTPS, and OCI offline artifact sources are exercised
  for supported architectures.
- Product integrations validate their own scraper discovery, network policy,
  and remote metrics delivery.

## Alternatives considered

### Run node exporter on the physical host

A host singleton provides the clearest physical-host metric semantics and avoids
shared-port conflicts between nspawn machines. It also introduces host binary
installation, host service upgrades, rollback, and reset ownership separate
from worker rootfs lifecycle. This remains the fallback if nspawn metric
visibility does not satisfy the required contract.

### Run node exporter as a Kubernetes DaemonSet

A DaemonSet is conventional and integrates naturally with Kubernetes scrape
objects. It starts only after kubelet, containerd, networking, scheduling, and
image pulling work. It cannot provide bootstrap-time observability, and generic
Unbounded does not own cluster workload deployment policy.

### Run a node-local scraper and remote-write metrics

This avoids inbound scrape routing but adds credentials, buffering, remote-write
configuration, and another managed process. The motivating requirement is an
exporter endpoint compatible with an existing scraper, so a local scraper is
outside scope.

### Reuse kubelet serving certificates automatically

Automatic reuse appears convenient but couples exporter availability to serving
CSR approval, file layout, private-key permissions, SAN policy, and rotation
behavior that differ among Kubernetes distributions. Generic Unbounded accepts
explicit certificate paths but leaves that integration to the product.

### Enable TLS by default

Unbounded does not own a universal CA or scraper configuration. A TLS default
would create an endpoint that many existing scrapers cannot use. The secure
path is explicit coordinated enablement of serving certificates, trust, client
identity, and scrape scheme.
