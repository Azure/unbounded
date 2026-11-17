---
title: "Configuration"
weight: 1
description: "Complete reference for the unbounded-agent JSON configuration file, including every field, its default, environment variable overrides, and worked examples."
---

## Overview

The unbounded-agent is configured by a single JSON file. That file tells the agent
which cluster to join, how to authenticate, where the node root filesystem comes
from, and how the node stack is isolated from the host. You do not need to write
it by hand for a normal installation: `kubectl unbounded machine manual-bootstrap`,
machina, and metalman all generate it from the `Machine` resource. You do need to
read it when you debug a host, build your own delivery method, or override a
setting the controllers do not expose.

The mental model is simple. The file describes a desired worker node, not a
sequence of steps. On every bootstrap attempt the agent resolves the file into a
goal state together with what it finds on the host: CPU architecture, GPUs, host
kernel, and host distribution. Missing downloads, a wrong CA, or an unreachable
API server show up when the goal state is resolved or when a preflight check runs,
not silently at the end.

This page documents where the file lives, the order in which the agent loads it,
every field and its default, the environment variables that override parts of it,
and complete examples for common deployments.

## Configuration File

### Location

The agent never guesses a path. It reads the file named by the
`UNBOUNDED_AGENT_CONFIG_FILE` environment variable. When that variable is empty,
the agent falls back to the legacy per-field environment variables described
below. The conventional path, used by the cloud-init and ignition payloads, is
`/etc/unbounded/agent/config.json`.

The persistent payloads use that path:

- `kubectl unbounded machine manual-bootstrap --variant cloud-init` writes the
  file with mode `0600` and exports the variable in `runcmd`.
- `--variant ignition` passes the same path to the first-boot unit
  `unbounded-agent-bootstrap.service` through an `Environment=` line.
- The generated install script requires the variable, checks that the file
  exists, then runs `unbounded-agent preflight` and `unbounded-agent start`.

A script-based bootstrap and machina differ only in where they stage the file.
The script variant writes it under `/tmp` with `mktemp`, and machina uploads it to
`/tmp/unbounded-agent.json` and removes it when the run finishes. Both pass the
path through `UNBOUNDED_AGENT_CONFIG_FILE`.

{{< callout type="warning" >}}
The file contains the cluster CA and a bootstrap token or attestation endpoint.
Keep it readable only by root, as the generated cloud-init payload does with mode
`0600`.
{{< /callout >}}

Because the path is an environment variable, you can keep the file anywhere. A
host you are repairing can be started directly with a different file:

```bash
sudo UNBOUNDED_AGENT_CONFIG_FILE=/root/agent-config.json unbounded-agent start
```

### Loading Order

Configuration loading is one fixed pipeline, used by `start`, `preflight`, and
the reprovision helper. The order matters because later steps can change a value
you set earlier.

1. **Select the source.** If `UNBOUNDED_AGENT_CONFIG_FILE` is set and is not
   whitespace, the agent reads and decodes that JSON file. If the variable is
   empty, the agent builds a configuration from the legacy environment
   variables. The two sources are never merged.
2. **Apply command line overrides.** The `--isolation` flag, where the command
   accepts it, replaces `Isolation` after the source is read. A bad value is
   rejected exactly like a bad value in the file.
3. **Normalize.** The agent strips a leading `v` from `Cluster.Version`, trims
   `Kubelet.NodeIP`, and prefixes `https://` to `Kubelet.ApiServer` when no
   scheme is present. It then resolves `MachineName` and `NodeName`.
4. **Validate.** `start` and `preflight` call `Validate` before doing any work.
   This is where required fields, isolation rules, kubelet overlay rules, and
   LocalDNS rules are checked.

Normalization logs when it derives a machine name. A file that names the machine
explicitly produces no extra log line; a file that leaves it out logs the
resolved name and its source at information level.

A missing file fails with `read agent config file "<path>"`, and malformed JSON
fails with `decode agent config file "<path>"`. Both errors name the exact path,
so a wrong environment variable is obvious from the first line of output.

### Where the File Comes From

You have four practical sources, and they all produce the same schema.

`kubectl unbounded machine manual-bootstrap` is the direct one. It builds the
configuration from the `Machine` resource and cluster state, then either writes a
self-contained script or a cloud-init or ignition payload. The script embeds the
JSON in a heredoc, writes it to a temporary file created with `mktemp` under
`/tmp`, and runs the install script with `UNBOUNDED_AGENT_CONFIG_FILE` pointing
at that file.

machina generates the same JSON for hosts it reaches over SSH. It resolves the
cluster CA, the kube-dns service address, the Kubernetes version, provider
labels, and a bootstrap token, then uploads the file to a temporary path and runs
the install script with it.

metalman generates it for PXE installations. Its netboot resolver uses the same
builder as the other paths, so the fields and defaults are identical apart from
the attestation settings a PXE host normally uses.

A hand-authored file is the fourth source. It is best for a lab, a repair, or a
delivery mechanism of your own. Start from a generated file rather than a blank
page: it already contains a working CA, DNS address, version, and token.

### Validation

`Validate` is strict about what the agent owns, and deliberately quiet about
credentials that a later step supplies. In practice it enforces these rules:

- `MachineName` is required after resolution, and `NodeName` must be a valid
  Kubernetes node name.
- `Cluster.ClusterDNS` is required.
- `Kubelet.ApiServer` is required unless you supply `Kubelet.KubeconfigData`,
  and it must be a URL with a scheme and host. The loader adds `https://` when
  you omit the scheme.
- `Isolation` must be one of `nspawn`, `none`, or `container`, and `container`
  requires the experimental opt-in.
- `Kubelet.Configuration` must not set fields the agent owns.
- `LocalDNS` listeners and limits must be internally consistent.
- `ProvisioningFormat`, when set, must be `cloud-init` or `ignition`.

Bootstrap authentication is validated separately from the rest of the file. This
is intentional: on an attested host the token does not exist when the file is
loaded, and the attestation step fills it in before the node starts.

{{< callout type="note" >}}
JSON decoding ignores unknown fields, so a misspelled key does not fail the load.
It silently has no effect. Compare a hand-edited file against the field table
below before you spend time debugging a symptom.
{{< /callout >}}

## Environment Variable Overrides

### File Selection and Identity

`UNBOUNDED_AGENT_CONFIG_FILE` selects the JSON file. It has no default, and when
it is unset the agent uses the legacy variables instead of reading a file.

`AGENT_MACHINE_NAME` supplies a machine name when the file omits `MachineName`. It
is trimmed and lowercased before validation as a Kubernetes node name. This is
what lets one cloud-init payload serve a whole VM scale set or Auto Scaling
group: each instance resolves its own name at startup.

The host hostname is the final fallback for `MachineName`, and it is normalized
the same way. If the hostname is not a valid DNS-1123 subdomain, the load fails
and says which value was rejected.

`Kubelet.NodeName` is separate from `MachineName`. Its resolution order is an
explicit value, then the host hostname when it is a valid node name, then
`MachineName`. Node labels, kubelet registration, and host-side watches all use
`NodeName`; the `Machine` resource uses `MachineName`.

### Legacy Per-Field Variables

When no config file is selected, the agent builds the same structure from these
variables. This path exists for deployments created before the JSON file and is
kept working unchanged.

| Variable | Required | Maps to |
| --- | --- | --- |
| `MACHINA_MACHINE_NAME` | No | `MachineName`, before the `AGENT_MACHINE_NAME` and hostname fallbacks |
| `KUBE_VERSION` | Yes | `Cluster.Version` |
| `API_SERVER` | Yes | `Kubelet.ApiServer` |
| `CA_CERT_BASE64` | Yes | `Cluster.CaCertBase64` |
| `CLUSTER_DNS` | Yes | `Cluster.ClusterDNS` |
| `BOOTSTRAP_TOKEN` | Yes, unless `AGENT_ATTEST_URL` is set | `Kubelet.Auth.BootstrapToken` |
| `NODE_LABELS` | No | `Kubelet.Labels`, parsed as `key=value,key=value` |
| `REGISTER_WITH_TAINTS` | No | `Kubelet.RegisterWithTaints`, split on commas |
| `AGENT_ATTEST_URL` | No | `Attest.URL`; when set, a static token is not required |

`NODE_LABELS` rejects an entry without `=`, naming the malformed pair. Taints are
passed through as written because kubelet parses the `key=value:effect` form
itself.

{{< callout type="tip" >}}
The environment fallback cannot express `OCIImage`, `Isolation`, `LocalDNS`,
`Downloads`, `OfflineArtifacts`, or the nspawn settings. Move to a JSON file if
you need any of those.
{{< /callout >}}

### Image Selection

Two variables influence the root filesystem image, and both are consulted only
when `OCIImage` is empty:

- `AGENT_OCI_IMAGE` supplies an image reference instead of the built-in default.
- `AGENT_DISABLE_OCI_IMAGE`, when set to a true boolean value, disables the OCI
  image entirely and logs that the agent falls back to debootstrap.

If `OCIImage` is set in the file, it wins over both variables. That precedence is
deliberate: a configuration that names an image should not change meaning because
of an inherited environment.

### Install Script Variables

The generated install script reads its own variables before it runs the agent.
They control the download of the agent binary, not the node configuration.

| Variable | Default | Effect |
| --- | --- | --- |
| `AGENT_VERSION` | unset | Pins a release tag such as `v0.0.10`; unset tracks the latest published release |
| `AGENT_BASE_URL` | `https://github.com/Azure/unbounded/releases` | Base URL for release assets |
| `AGENT_URL` | unset | Fully qualified tarball URL; overrides both variables above |
| `AGENT_SHA256` | unset | Verifies the tarball before extraction and fails on a mismatch |
| `AGENT_DEBUG` | unset | `1`, `true`, or `yes` adds `--debug` to the agent invocation |
| `AGENT_PREFLIGHT` | enabled | `0`, `false`, or `no` skips the automatic preflight run |

These are also settable through the `Machine` resource as `spec.agent.version`,
`spec.agent.baseURL`, and `spec.agent.url`, which machina and metalman export on
the host before running the script.

## Fields

### Defaults at a Glance

The table below lists every field the file accepts. "Empty" means the JSON field
is absent and the agent applies the behavior in the description. A literal value
in the default column is compiled into the agent rather than written into the
file.

| Field | Type | Default | Purpose |
| --- | --- | --- | --- |
| `MachineName` | string | derived | `Machine` resource name and node identity |
| `NodeName` | string | host hostname, then `MachineName` | Kubernetes Node name used by kubelet |
| `Cluster.CaCertBase64` | string | empty | Base64 cluster CA written into the node |
| `Cluster.ClusterDNS` | string | empty | kube-dns `ClusterIP` |
| `Cluster.Version` | string | empty | Kubernetes version for binaries and offline artifacts |
| `Kubelet.ApiServer` | string | empty | API server endpoint |
| `Kubelet.KubeconfigData` | base64 string | empty | Complete kubeconfig for direct authentication |
| `Kubelet.NodeIP` | string | empty | kubelet `--node-ip`, single IP or dual-stack pair |
| `Kubelet.Auth.BootstrapToken` | string | empty | TLS bootstrap token |
| `Kubelet.Auth.ExecCredential` | object | empty | Exec credential plugin configuration |
| `Kubelet.Labels` | map | empty | Node labels applied at registration |
| `Kubelet.RegisterWithTaints` | array | empty | Taints applied at registration |
| `Kubelet.Configuration` | map | empty | Overlay merged over the baseline `KubeletConfiguration` |
| `Kubelet.ImageCredentialProvider.ConfigPath` | string | empty | In-node provider configuration path |
| `Kubelet.ImageCredentialProvider.BinDir` | string | empty | In-node provider binary directory |
| `CRI.Containerd.Version` | string | `2.1.8` | containerd version |
| `CRI.Containerd.SandboxImage` | string | `mcr.microsoft.com/oss/v2/kubernetes/pause:3.9` | CRI sandbox image |
| `CRI.Runc.Version` | string | `1.5.0` | runc version |
| `CNI.PluginVersion` | string | `1.5.1` | CNI plugin version |
| `Gantry.Disabled` | bool | `false` | Skips the default Gantry `hosts.toml` |
| `LocalDNS.Enabled` | bool | `false` | Runs the machine-local CoreDNS cache |
| `LocalDNS.NodeListenerIP` | string | `169.254.10.10` | Listener for node upstream resolution |
| `LocalDNS.ClusterListenerIP` | string | `169.254.10.11` | Listener forwarding to cluster DNS |
| `LocalDNS.MetricsAddress` | string | derived host IPv4, port `9253` | CoreDNS metrics bind address |
| `LocalDNS.CPULimitInMilliCores` | int | `2000` | CoreDNS CPU quota |
| `LocalDNS.MemoryLimitInMB` | int | `128` | CoreDNS memory limit |
| `LocalDNS.RequiredPlugins` | array | empty | Extra plugins that must exist in the binary |
| `LocalDNS.CorefileTemplate` | string | built-in template | Replacement Corefile template |
| `OCIImage` | string | built-in per host | Root filesystem image or archive |
| `Isolation` | string | `nspawn` | Where the node stack runs |
| `Experimental.ContainerIsolation` | bool | `false` | Opts in to `container` mode |
| `NvidiaToolkit.CTKPath` | string | `/usr/bin/nvidia-ctk` | Toolkit executable on the host |
| `NvidiaToolkit.ContainerRuntimePath` | string | `/usr/bin/nvidia-container-runtime` | Runtime executable on the host |
| `AdditionalHostDevices` | array | empty | Extra device nodes or device groups for nspawn |
| `AdditionalHostMounts` | array | empty | Extra host mounts for nspawn |
| `AdditionalHostMounts[].Source` | string | required | Host path to bind mount |
| `AdditionalHostMounts[].Target` | string | `Source` | Path inside the nspawn machine |
| `AdditionalHostMounts[].ReadOnly` | bool | `false` | Mounts read-only when true |
| `AdditionalHostMounts[].DeviceAllow` | array | empty | Device nodes or groups granted to the machine |
| `NSpawnLifecycle.PreStartCommand` | string | empty | Replaces the pre-start lifecycle helper |
| `NSpawnLifecycle.PostStartCommand` | string | empty | Replaces the post-start lifecycle helper |
| `OfflineArtifacts.Source` | string | empty | Complete offline binary artifact source |
| `ProvisioningFormat` | string | empty | Records `cloud-init` or `ignition` first boot |
| `Attest.URL` | string | empty | metalman `serve-pxe` base URL for TPM attestation |
| `Downloads.<component>.BaseURL` | string | upstream host | Mirror base URL for one artifact |
| `Downloads.<component>.URL` | string | empty | Full URL template for one artifact |
| `Downloads.<component>.Version` | string | component default | Version override for one artifact |

### Naming and Cluster

The first three fields a new file must get right are the machine identity and the
two cluster addresses.

```json
{
  "MachineName": "node-01",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  }
}
```

`MachineName` is the name of the `Machine` resource the host belongs to. The
agent also uses it for install-state ownership. It must be a valid Kubernetes DNS
subdomain because it becomes an object name.

`Cluster.CaCertBase64` is the base64 PEM of the cluster CA. The agent decodes it
and writes it into the node as the API server client CA. An empty value is not
rejected by validation, but a node without it cannot verify the API server
certificate, so TLS bootstrap fails. On an attested host the attestation response
can replace it.

`Cluster.ClusterDNS` is the `ClusterIP` of the `kube-dns` service. It becomes
kubelet's `clusterDNS` setting and, when LocalDNS is enabled, the upstream for the
cluster listener. This value is required.

`Cluster.Version` is the Kubernetes version used for node binaries and offline
artifact resolution. A leading `v` is stripped during loading, so `v1.34.0` and
`1.34.0` load identically. The controllers fill it from the `Machine` spec, which
falls back to the cluster's own version.

### Kubelet

`Kubelet.ApiServer` is the endpoint kubelet talks to. If the value has no scheme,
the agent prefixes `https://`; `api.example.com:443` becomes
`https://api.example.com:443`. A value that already includes a scheme is left
unchanged.

`Kubelet.KubeconfigData` is a complete kubeconfig supplied verbatim for direct
kubelet authentication. It is mutually exclusive with `Auth`. The kubeconfig must
have a current context, an HTTPS cluster server, and at least one credential that
does not reference a file path: a token, client certificate and key data, basic
authentication, an exec plugin, or an auth provider. Paths for certificate
authorities, client certificates, client keys, and token files are rejected
because the file is written into the node root, not the host. When it is present,
kubelet does not rotate certificates and `Kubelet.ApiServer` is not required.

`Kubelet.NodeIP` overrides what kubelet advertises. It accepts a single IPv4 or
IPv6 address, or a comma-separated dual-stack pair such as
`10.0.0.15,fd00::15`. Leaving it empty lets kubelet select the address from the
node's default route, which is usually what you want. Set it when the host has
several interfaces and the default route is not the network the control plane
uses.

`Kubelet.Auth.BootstrapToken` is a Kubernetes bootstrap token in
`<token-id>.<token-secret>` form. It is used for TLS bootstrapping: kubelet
starts with a bootstrap kubeconfig, submits a certificate signing request, and
writes the issued certificate to its kubeconfig. Exactly one of `BootstrapToken`
or `ExecCredential` must be set, unless a kubeconfig is supplied or attestation
provides the token later.

`Kubelet.Auth.ExecCredential` configures kubelet to authenticate through a
`client.authentication.k8s.io` exec credential plugin instead of a bootstrap
token. The command field is required. The plugin's kubeconfig is written directly
as kubelet's kubeconfig, so no TLS bootstrap is performed; the plugin is expected
to provide renewable credentials.

`Kubelet.Labels` is a map of labels applied at node registration. The
configuration builders give this map a defined priority: user labels from the
`Machine` come first, common unbounded labels are applied unconditionally, and
cloud provider labels override both. If you hand-author the file, you are writing
the final map yourself, so include `unbounded-cloud.io/site` and any provider
labels your cluster expects.

`Kubelet.RegisterWithTaints` is a list of taints in `key=value:effect` form. The
agent sorts the list and joins it for kubelet's `--register-with-taints` flag.
Kubelet treats taints as part of the first registration, which is why a later
configuration change to this list requires a repave rather than a kubelet
restart.

`Kubelet.Configuration` is a JSON object merged over the agent's baseline
`KubeletConfiguration`. Use it for fields such as `maxPods`, `kubeReserved`, or
`evictionHard`. The merged result is rendered with the agent's own connectivity
and authentication fields applied after your overlay. For that reason these keys
are rejected if you set them:

- `apiVersion` and `kind`
- `authentication`
- `authorization.mode`
- `clusterDNS`
- `containerRuntimeEndpoint`
- `registerWithTaints`
- `rotateCertificates`

The baseline already sets values such as `cgroupDriver: systemd`,
`protectKernelDefaults: true`, `readOnlyPort: 0`, a `staticPodPath`, and a
restricted TLS cipher list. The merge is a top-level copy: each key you set
replaces the baseline value for that key, including nested maps such as
`kubeReserved`, which is replaced as a whole.

#### Image Credential Provider

`Kubelet.ImageCredentialProvider` points kubelet at an exec image credential
provider inside the node. Both fields are required when the object is present,
and both must be clean absolute paths without whitespace, control characters,
quotes, backslashes, percent signs, or dollar signs, because they are rendered
into a systemd argument.

`ConfigPath` is the provider configuration file or directory supported by the
kubelet version you run. `BinDir` is the directory holding the provider
binaries. The paths are checked syntactically, not for existence, because the
files can arrive from the OCI root filesystem image or from an
`AdditionalHostMounts` bind mount at start time.

A node that pulls from a private registry through an operator-installed provider
names those in-node paths here. Note that this field is not set by ACR attachment
itself: `kubectl unbounded gantry attach-acr --auth-mode aks-identity-binding`
enables the operator-managed registry authentication DaemonSet, which installs the
stock ACR provider and its kubelet drop-in on Flex nodes. A configuration that
also declares `ImageCredentialProvider` must point at the paths that installation
uses.

### Isolation

`Isolation` selects where the node stack runs. The agent itself always runs on
the host; the mode decides where containerd, kubelet, the CNI plugins, and
CoreDNS live.

| Value | Behavior |
| --- | --- |
| `nspawn` | Runs the node stack in a `systemd-nspawn` machine with a rootfs from an OCI image |
| `none` | Runs the node stack as agent-owned units directly on the host |
| `container` | Runs the node stack in a privileged OCI container (experimental) |

An empty value means `nspawn`. That is also how every configuration written before
the field existed behaves, so old files keep loading the same way. `ParseMode`
rejects unknown values rather than treating them as nspawn: an older agent must
never silently run a newer mode as the default.

`container` is refused unless the configuration opts in with
`Experimental.ContainerIsolation: true`. The error names the missing setting, so
the fix is visible in the message. There are four ways that opt-in reaches a
host, and they all set the same configuration field:

- machina reads `experimentalContainerIsolation: true` from its own YAML
  configuration and includes the opt-in for every `Machine` it provisions.
- metalman accepts `serve-pxe --experimental-container-isolation` and renders it
  into every agent configuration its resolver produces.
- `machine-ops-controller --experimental-container-isolation` covers host
  replacement, so a replacement host of a container-mode `Machine` gets the
  opt-in.
- `kubectl unbounded machine manual-bootstrap --experimental-container-isolation`
  covers hand-run bootstraps.

The mode is part of the installation identity. Changing it is a different kind of
change from editing a label: the daemon treats it as an isolation change and
either migrates in place or reprovisions the host at its next repave. The agent
also refuses a direct `start` in a different mode on a host already installed in
one, instead of building a second node stack beside the first. `nspawn` and an
unset value hash to the same identity, so existing installations are unaffected.

### Image and Artifacts

`OCIImage` is the root filesystem image for the node stack. It accepts three
forms:

- a registry reference such as `ghcr.io/azure/agent-ubuntu2404:v20261031`
- an `oci-layout://` reference to a local OCI layout directory, optionally with a
  tag, such as `oci-layout:///opt/unbounded/images/agent-ubuntu2404:v20260619`
- an `https://` URL to a tarred OCI image layout

An HTTPS archive must contain exactly one tagged image reference. The agent
selects it automatically, and the URL may carry signed query parameters. Those
parameters are stripped when the agent computes installation identity, so
refreshing an expiring signature does not look like a different image.

When `OCIImage` is empty, the agent selects a built-in default based on the host
distribution and whether an NVIDIA GPU is present:

| Host | CPU default | GPU default |
| --- | --- | --- |
| Ubuntu 24.04 and other distributions | `ghcr.io/azure/agent-ubuntu2404:v20261031` | `ghcr.io/azure/agent-ubuntu2404-nvidia:v20261031` |
| Ubuntu 26.04 | `ghcr.io/azure/agent-ubuntu2604:v20261031` | `ghcr.io/azure/agent-ubuntu2604-nvidia:v20261031` |
| Azure Linux 3 and Azure Container Linux | `ghcr.io/azure/agent-azlinux3:v20261031` | `ghcr.io/azure/agent-azlinux3-nvidia:v20261031` |

`OfflineArtifacts.Source` names a complete offline binary artifact source. It
takes precedence over `Downloads` when both are present. The value is rendered as
a strict Go template with `.KubernetesVersion` and `.KubernetesVersionNoV`, then
resolved as an absolute filesystem path, a `file://` URL, an HTTPS archive, or an
`oci://` artifact reference. HTTPS archives are downloaded under
`/var/lib/unbounded/offline-artifacts` and extracted on the host. The bundle must
contain a `manifest.json` whose `versions.kubernetes` matches `Cluster.Version`,
and when LocalDNS is enabled it must also carry `versions.coredns`.

`NvidiaToolkit` overrides where the NVIDIA Container Toolkit tools live on a host
that installs them somewhere other than `/usr/bin`. It is valid only with
`Isolation: none`, because that is the one mode that runs the toolkit from the
host; nspawn uses the copies in the rootfs image. `CTKPath` defaults to
`/usr/bin/nvidia-ctk` and `ContainerRuntimePath` defaults to
`/usr/bin/nvidia-container-runtime`. A path that needs cleaning is refused rather
than rewritten, so the value you wrote is the value the agent runs.

The paths for the node stack itself are derived by the agent rather than set
here. In nspawn mode the stack runs in the alternating slot machine `kube1` or
`kube2`: its root filesystem is unpacked under `/var/lib/machines/<slot>` and its
machine configuration is written to `/etc/systemd/nspawn/<slot>.nspawn`. Applied
agent configuration is kept under `/etc/unbounded/agent`.

### LocalDNS

`LocalDNS` enables a CoreDNS instance inside the node stack that caches DNS for
the node. It is off by default. When enabled, it creates a dummy `localdns`
interface on the host, assigns both listener addresses to it as `/32` addresses,
and installs nftables rules that skip conntrack for DNS traffic to those
addresses. The node's resolver is then pointed at the node listener, so ordinary
name lookups are answered locally and only cache misses leave the host.

`LocalDNS.NodeListenerIP` defaults to `169.254.10.10` and receives the machine's
own queries, forwarding them to the host's upstream nameservers.
`LocalDNS.ClusterListenerIP` defaults to `169.254.10.11` and forwards to
`Cluster.ClusterDNS`, which is how in-cluster service names resolve. Both must be
unicast IPv4 addresses, must differ from each other, and must differ from
`Cluster.ClusterDNS`.

`LocalDNS.MetricsAddress` is the bind address for the Prometheus endpoint. When
it is empty the agent derives a host address using the same order kubelet uses
for a non-cloud node: an IPv4 value from `Kubelet.NodeIP`, then an IP-valued node
name, then the IPv4 addresses the node name resolves to, then the host's default
route address. The port is `9253`. A value you set yourself must use `IP:port`
syntax with a unicast IPv4 host and a port between 1 and 65535.

`LocalDNS.CPULimitInMilliCores` defaults to `2000` and `LocalDNS.MemoryLimitInMB`
defaults to `128`. Both apply as the CoreDNS slice's CPU quota and memory limit
and must be positive when set.

`LocalDNS.RequiredPlugins` adds to the plugins the agent expects the CoreDNS
binary to provide. The baseline list is `bind`, `cache`, `errors`, `forward`,
`loop`, `prometheus`, `ready`, and `whoami`. Entries are lowercased and must
match `^[a-z][a-z0-9_-]*$`; duplicates are rejected. At install time the agent
runs the downloaded `coredns -plugins` and fails if a required plugin is absent,
which catches a custom CoreDNS build before the node starts.

`LocalDNS.CorefileTemplate` replaces the built-in Corefile. It is a Go template
with `missingkey=error`, so a misspelled field fails instead of rendering an empty
string. These fields are available:

| Field | Value |
| --- | --- |
| `NodeListenerIP` | Resolved node listener address |
| `ClusterListenerIP` | Resolved cluster listener address |
| `NodeUpstreamIPs` | Slice of host upstream nameserver addresses |
| `NodeUpstreamIPsJoined` | The same list joined with spaces |
| `ClusterDNSServiceIP` | `Cluster.ClusterDNS` |
| `MetricsAddress` | Resolved metrics bind address |

The template is limited to 256 KiB and the rendered Corefile to 512 KiB, and an
empty render is rejected.

### Downloads and Attestation

`Downloads` overrides where the agent fetches the binaries it installs into the
node root filesystem. Each component entry is optional; an unset entry falls back
to the upstream default. `BaseURL` replaces the host and path prefix while
keeping the upstream layout, and `URL` replaces the entire URL template. `Version`
pins the component version and takes precedence over the version the agent would
otherwise derive.

| Component | Default base URL | Default version |
| --- | --- | --- |
| `Kubernetes` | `https://dl.k8s.io` | `Cluster.Version` |
| `Containerd` | `https://github.com/containerd/containerd/releases/download` | `2.1.8` |
| `Runc` | `https://github.com/opencontainers/runc/releases/download` | `1.5.0` |
| `CNI` | `https://github.com/containernetworking/plugins/releases/download` | `1.5.1` |
| `Crictl` | `https://github.com/kubernetes-sigs/cri-tools/releases/download` | Kubernetes minor, as `v<major>.<minor>.0` |
| `CoreDNS` | `https://github.com/coredns/coredns/releases/download` | `1.12.3` |

`URL` is a plain template, not a format string. Each `%s` is replaced in order
with the values listed below, `%%` stays literal, and an unmatched placeholder is
left as `%s` rather than producing a formatting error.

| Component | Placeholder order |
| --- | --- |
| `Kubernetes` | version, architecture, binary name |
| `Containerd` | version, version, architecture |
| `Runc` | version, architecture |
| `CNI` | version, architecture, version |
| `Crictl` | version, version, operating system, architecture |
| `CoreDNS` | version, architecture |

`Downloads.CoreDNS.Version` also selects the CoreDNS version LocalDNS installs.
Use it together with `LocalDNS.RequiredPlugins` when you need a CoreDNS build
that carries extra plugins. When `OfflineArtifacts` is configured, its manifest
wins and `Downloads` is ignored.

`Attest.URL` is the base URL of a metalman `serve-pxe` instance. When it is set,
the agent requests a bootstrap token by TPM attestation instead of using a static
token from the file. The agent appends `/attest` to the URL, opens
`/dev/tpmrm0`, creates an endorsement key and storage root key, and posts their
public parts to the server. The server returns an encrypted token and,
optionally, the cluster CA. On success those values replace
`Kubelet.Auth.BootstrapToken` and `Cluster.CaCertBase64` for the node start.

Attestation makes up to five attempts when the connection fails, with exponential
backoff, and caps the response body at 1 MiB. A non-200 response fails
immediately. If no URL is configured the step is skipped. A host that attests
still needs a working TPM and network access to the `serve-pxe` endpoint; a
failure there stops the bootstrap with an attestation error rather than falling
back to an empty token.

### Runtime, Gantry, and nspawn Settings

`CRI` pins the container runtime versions. `CRI.Containerd.Version` defaults to
`2.1.8`, `CRI.Containerd.SandboxImage` defaults to
`mcr.microsoft.com/oss/v2/kubernetes/pause:3.9`, and `CRI.Runc.Version` defaults
to `1.5.0`. `CNI.PluginVersion` defaults to `1.5.1`. If you use offline
artifacts, a containerd, runc, or CNI version you set here must match the version
in the bundle manifest, or resolution fails.

`Gantry.Disabled` skips writing the default containerd `hosts.toml` that points
registry traffic at Gantry. It defaults to `false` and exists as a breakglass
setting for environments that manage containerd registry routing themselves.

`AdditionalHostDevices` lists extra host device nodes under `/dev`, or systemd
device group specifiers such as `char-infiniband_verbs`, that the nspawn machine
should receive in addition to automatically discovered devices. Entries must be
clean absolute paths under `/dev` or valid `char-` or `block-` specifiers, with
no whitespace or colon.

`AdditionalHostMounts` bind-mounts extra host paths into the nspawn machine. Each
entry has a required `Source`, an optional `Target` that defaults to `Source`, a
`ReadOnly` flag, and an optional `DeviceAllow` list. Paths must be clean and
absolute, with no whitespace, control characters, or colon. Mounting a directory
that contains device nodes does not by itself make them usable; `DeviceAllow`
grants the machine access to those nodes, including nodes created after it
starts.

`NSpawnLifecycle` replaces the lifecycle helper commands the machine's systemd
hooks run. A nil value runs the helper the agent installs. Each command must be a
single systemd command whose first token is an absolute path. It must not contain
control characters, a lone `;`, or end in a backslash, because any of those would
change how systemd parses the unit. `%i` expands to the machine name, and each
command must run its phase with the host's view of the filesystem and fail when
the phase fails.

`ProvisioningFormat` records how the host was first booted. The accepted values
are `cloud-init` and `ignition`; an empty value means the installation predates
the field. The agent does not infer it from the host distribution. The daemon
reports an explicit value in `Machine` status.

## Bootstrap and Join Settings

The fields that decide whether a host joins the cluster are the cluster block,
the kubelet endpoint, and one authentication method. Everything else tunes the
node after it is a member.

The agent runs three phases in order: host preparation, root filesystem and
artifact preparation, and node start. The configuration is read once, before the
first phase, and the resolved goal state is passed through the rest of the run.
`unbounded-agent start` performs all three; see
[Agent]({{< relref "guides/agent" >}}) for the operational walkthrough.

### Authentication Choices

A node uses one of these methods. The shared configuration validation allows an
empty `Kubelet.Auth` because attestation can supply the token after the file is
loaded.

| Method | Fields | Behavior |
| --- | --- | --- |
| Bootstrap token | `Kubelet.Auth.BootstrapToken` | kubelet submits a CSR and receives a client certificate |
| Exec credential | `Kubelet.Auth.ExecCredential` | kubelet runs a plugin that returns credentials; no TLS bootstrap |
| Direct kubeconfig | `Kubelet.KubeconfigData` | kubelet uses the supplied kubeconfig verbatim |
| TPM attestation | `Attest.URL` | the agent obtains a token and optionally the CA from `serve-pxe` |

A bootstrap token is the common case. The token must be valid, unexpired, and
allowed to authenticate and request certificates. A token that was issued for a
different cluster, or one whose secret was rotated after the file was generated,
fails at the certificate signing step with an authorization error. The agent
reports a node-start failure, and the `Machine` condition reason is
`KubeletBootstrapFailed` when the failure comes from starting kubelet or waiting
for it to register.

A CA mismatch looks different from a token problem. The agent reaches the API
server, but kubelet cannot verify its certificate, so the request fails before
authorization is evaluated. If the file came from a different cluster, regenerate
it instead of editing the CA by hand.

`Kubelet.Labels` and `Kubelet.RegisterWithTaints` are applied when the node first
registers. kubelet does not revise labels on restart, which is why the daemon
compares labels and taints against the applied configuration and repaves the node
when they differ. Editing the file on disk does not change a node that has
already joined; the desired configuration comes from the applied record and the
`Machine` resource.

`NodeName` is the name that appears in `kubectl get nodes`. If you leave it empty,
the host hostname is used when it is valid, and `MachineName` otherwise. If neither
is a valid DNS-1123 subdomain, the load fails with an explicit error instead of
registering an invalid name.

### Checking a Join

`unbounded-agent preflight` loads the same file and performs the non-mutating
checks first, which is the fastest way to catch a bad CA, an invalid token, or an
unreachable API server. See
[Preflight]({{< relref "reference/agent/preflight" >}}) for the check list and
exit behavior.

After a start, confirm the node registered:

```bash
kubectl get nodes -o wide
kubectl get machine <machine-name> -o yaml
```

The `Machine` status reports the bootstrap credentials and conditions that the
agent publishes during the run.

## Networking Settings

The agent configuration covers the node's connection to the control plane and
its local DNS behavior. It does not configure the cluster network. Pod networking,
site peering, and gateway selection belong to the networking controllers and the
`unbounded-net` node agent; see
[Networking Configuration]({{< relref "reference/networking/configuration" >}}).

The network-related fields in this file are:

- `Kubelet.ApiServer`, the control plane endpoint. It must be reachable from the
  host, including DNS resolution and a TCP connection to the endpoint, or
  bootstrap stops at the API server check.
- `Cluster.ClusterDNS`, the in-cluster DNS service address used by kubelet and by
  the LocalDNS cluster listener.
- `Kubelet.NodeIP`, which selects the address kubelet advertises when the default
  route is not the right interface.
- `LocalDNS`, which adds the local caching resolver described above.
- `AdditionalHostDevices` and `AdditionalHostMounts`, which expose host network
  devices and paths such as InfiniBand device nodes to an nspawn node.

Ports that appear in preflight and in the node stack are fixed rather than
configurable here: kubelet binds `0.0.0.0:10250`, containerd exposes metrics on
`0.0.0.0:10257`, LocalDNS listens on port 53 on its two listener addresses, its
readiness endpoint is port 8181, and its metrics endpoint defaults to port 9253.
Preflight rejects a host where the kubelet or containerd metrics bind address is
already occupied.

## LocalDNS Requirements

LocalDNS reads the host resolver configuration to decide where to forward cache
misses. That makes the host's resolver setup part of the configuration contract.
The agent reads `/etc/resolv.conf` and requires at least one usable direct IPv4
nameserver.

Two resolver layouts are supported:

1. `/etc/resolv.conf` lists direct IPv4 nameservers and no loopback address. The
   agent uses those addresses as upstreams.
2. `/etc/resolv.conf` points only at the systemd-resolved stub, meaning
   `127.0.0.53` or `127.0.0.54`. The agent checks that resolved has no split-DNS
   routing domains, then reads `/run/systemd/resolve/resolv.conf` for the real
   upstream addresses.

A missing `/etc/resolv.conf` fails with a read error naming the host resolver
configuration.

The unsupported cases fail with a message rather than a degraded resolver:

- A local caching stub other than the systemd-resolved stub, such as a
  container runtime's own resolver, is rejected with "host resolver uses an
  unsupported local caching stub".
- Any systemd-resolved routing domain other than `~.` is rejected as split DNS.
  Per-link or per-domain routing would make a single cache answer incorrectly.
- A resolver file with no usable direct IPv4 nameserver is rejected. Loopback
  addresses and the LocalDNS listener addresses themselves are not usable
  upstreams.

On an nspawn or container node, the resolver inside the node is rewritten to
point at the node listener. On a host-native node the host's own resolver is left
alone; only kubelet's dedicated resolver file points at LocalDNS.

## Logging

The agent writes its logs to standard error. The level and format come from root
flags:

- `--debug` raises the level to debug. The install script sets it when
  `AGENT_DEBUG` is `1`, `true`, or `yes`.
- `--log-format` selects `text` (the default) or `json`. JSON output is the right
  choice when logs are collected by a structured logging pipeline; text output is
  more readable in a terminal.

The preflight report is the exception: it goes to standard output so it can be
parsed or redirected. Under systemd, the journal captures both streams. For an
ignition installation the bootstrap unit is `unbounded-agent-bootstrap.service`:

```bash
journalctl -u unbounded-agent-bootstrap.service -f
```

For a cloud-init installation, the script output is part of the cloud-init log.
After bootstrap, the long-running daemon runs as
`unbounded-agent-daemon.service` and logs there.

Configuration problems are reported before any phase starts, and the process
exits with status 1. A message that begins with `read agent config file` is a
path problem, `decode agent config file` is a JSON syntax problem, and preflight
prefixes field or cross-field rule failures with `validate agent config`.

## Worked Examples

The examples below are complete files. Placeholders appear in angle brackets
inside JSON strings and are explained after each example. Every other value is
literal.

### A Standard Join

This is the shape the controllers generate for a host that joins with a bootstrap
token, with a site label, an explicit empty taint list, and a few kubelet
overrides added.

```json
{
  "MachineName": "node-01",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  },
  "Kubelet": {
    "ApiServer": "https://my-cluster-dns.hcp.eastus.azmk8s.io:443",
    "Auth": {
      "BootstrapToken": "<token-id>.<token-secret>"
    },
    "Labels": {
      "unbounded-cloud.io/site": "site-eastus",
      "kubernetes.azure.com/managed": "false"
    },
    "RegisterWithTaints": [],
    "Configuration": {
      "maxPods": 110,
      "kubeReserved": {
        "cpu": "100m",
        "memory": "1Gi"
      },
      "evictionHard": {
        "memory.available": "100Mi"
      }
    }
  }
}
```

`MachineName` becomes the `Machine` resource name and the node identity.
`Cluster.CaCertBase64` is the base64 PEM of the cluster CA; a file generated for
a different cluster will not join. `Cluster.ClusterDNS` is the `kube-dns`
`ClusterIP` and `Cluster.Version` is the Kubernetes version for the node binaries.

`Kubelet.ApiServer` includes its scheme. `Kubelet.Auth.BootstrapToken` uses the
`<token-id>.<token-secret>` form. The site label associates the node with its
site; `kubernetes.azure.com/managed: "false"` is a common provider label that
marks the node as self-managed. The empty `RegisterWithTaints` list is explicit:
the node registers schedulable.

`Kubelet.Configuration` shows the overlay pattern. `maxPods` replaces the single
value, and `kubeReserved` and `evictionHard` replace those nested maps as whole
objects. Do not put `clusterDNS`, `authentication`, `authorization.mode`, or the
other agent-owned keys in this object.

`Isolation` is absent, so the node stack runs in an nspawn machine. `OCIImage` is
absent, so the built-in image for the host distribution and GPU state is used.

### An Attested PXE Join with LocalDNS

A PXE host can join without a static token. The `serve-pxe` endpoint returns one
after TPM attestation, and LocalDNS is enabled to cache cluster DNS locally.

```json
{
  "MachineName": "edge-01",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  },
  "Kubelet": {
    "ApiServer": "https://edge-api.example.com:6443",
    "Auth": {},
    "Labels": {
      "unbounded-cloud.io/site": "site-edge"
    },
    "RegisterWithTaints": [
      "dedicated=edge:NoSchedule"
    ]
  },
  "Attest": {
    "URL": "http://10.0.0.1:8880"
  },
  "LocalDNS": {
    "Enabled": true
  }
}
```

The empty `Auth` object is intentional. The agent posts to
`http://10.0.0.1:8880/attest`, receives the token and, when the server includes
it, the cluster CA. Both are used for the node start. A static `BootstrapToken`
is not required and should not be stored on a diskless host.

The taint keeps ordinary workloads off the edge host until a matching toleration
is applied. LocalDNS uses its default listeners and derives the metrics address
from the node address, falling back to the host's default route.

### An Air-Gapped Host with Offline Artifacts

This file points at a complete artifact bundle and lets the cluster version drive
the path.

```json
{
  "MachineName": "gpu-07",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  },
  "Kubelet": {
    "ApiServer": "https://api.example.com:6443",
    "Auth": {
      "BootstrapToken": "<token-id>.<token-secret>"
    },
    "Labels": {
      "unbounded-cloud.io/site": "site-lab"
    },
    "RegisterWithTaints": []
  },
  "OfflineArtifacts": {
    "Source": "https://artifacts.example.com/unbounded/{{ .KubernetesVersion }}.tar.gz?sig=<signature>"
  },
  "LocalDNS": {
    "Enabled": true,
    "RequiredPlugins": [
      "hosts"
    ],
    "CPULimitInMilliCores": 1000,
    "MemoryLimitInMB": 256
  }
}
```

The source template renders to
`https://artifacts.example.com/unbounded/v1.34.0.tar.gz?sig=<signature>`. Signed
query parameters are accepted, and they are stripped when the agent computes the
installation identity. The archive is staged under
`/var/lib/unbounded/offline-artifacts`, its `manifest.json` must name the same
Kubernetes version, and because LocalDNS is enabled it must also carry a
`versions.coredns` entry. Any `Downloads` block in the same file is ignored.

`RequiredPlugins` adds `hosts` to the baseline plugin list. The agent verifies
that the bundled CoreDNS binary reports it before the node starts. The CPU and
memory limits replace the `2000` millicore and `128` MB defaults.

### Mirrored Downloads

If a mirror replaces upstream hosts but you do not have a complete offline
bundle, use `Downloads`. The upstream layout must be preserved.

```json
{
  "MachineName": "node-02",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  },
  "Kubelet": {
    "ApiServer": "https://api.example.com:6443",
    "Auth": {
      "BootstrapToken": "<token-id>.<token-secret>"
    },
    "Labels": {
      "unbounded-cloud.io/site": "site-lab"
    },
    "RegisterWithTaints": []
  },
  "Downloads": {
    "Kubernetes": {
      "BaseURL": "https://mirror.example.com/kubernetes"
    },
    "Containerd": {
      "BaseURL": "https://mirror.example.com/containerd"
    },
    "CNI": {
      "BaseURL": "https://mirror.example.com/cni"
    },
    "CoreDNS": {
      "Version": "1.12.3"
    }
  }
}
```

Kubernetes binaries resolve under
`https://mirror.example.com/kubernetes/v1.34.0/bin/linux/<arch>/<binary>`.
containerd and CNI follow their own upstream layouts under the mirror base. The
components you do not name continue to use their upstream defaults. Pinning
`Downloads.CoreDNS.Version` also fixes the CoreDNS version LocalDNS installs,
whatever version the cluster itself uses.

### None Mode with a Host Toolkit and a Credential Provider

On a host that runs the node stack directly, the NVIDIA toolkit comes from the
host, so its paths are configurable.

```json
{
  "MachineName": "gpu-host-03",
  "NodeName": "gpu-host-03",
  "Cluster": {
    "CaCertBase64": "<base64-encoded cluster CA>",
    "ClusterDNS": "10.96.0.10",
    "Version": "v1.34.0"
  },
  "Kubelet": {
    "ApiServer": "https://api.example.com:6443",
    "Auth": {
      "BootstrapToken": "<token-id>.<token-secret>"
    },
    "Labels": {
      "unbounded-cloud.io/site": "site-lab",
      "accelerator": "nvidia"
    },
    "RegisterWithTaints": [],
    "ImageCredentialProvider": {
      "ConfigPath": "/etc/kubernetes/credential-provider.yaml",
      "BinDir": "/usr/local/lib/kubelet-credential-providers"
    }
  },
  "Isolation": "none",
  "NvidiaToolkit": {
    "CTKPath": "/usr/local/nvidia/toolkit/nvidia-ctk",
    "ContainerRuntimePath": "/usr/local/nvidia/toolkit/nvidia-container-runtime"
  },
  "ProvisioningFormat": "ignition"
}
```

`Isolation: none` is what makes `NvidiaToolkit` valid. In any other mode the
toolkit comes from the rootfs image and the field is rejected with an error that
names the mode.

`ImageCredentialProvider` names a provider configuration and binary directory
that must already exist in the node, either in the image or through a bind mount.
A node that uses the operator-managed ACR path on AKS Flex nodes does not set
these fields: `kubectl unbounded gantry attach-acr --auth-mode
aks-identity-binding` installs the stock provider and its kubelet drop-in at
`/opt/unbounded/registry-auth` instead.

`ProvisioningFormat: ignition` records that the host first booted from an
Ignition config. It does not change behavior at bootstrap time; the daemon
reports it in `Machine` status.

## Changing Configuration on a Running Host

A bootstrap reads the file once. After the node is up, the long-running daemon
does not poll that path. It reads the applied configuration recorded at bootstrap
under `/etc/unbounded/agent`, in a file named `<stem>-applied-config.json`. The
stem is `kube1` or `kube2` for the nspawn slot, and `none-kube1` or
`container-kube1` in the other modes. It builds a desired configuration from that
record plus the `Machine` fields it owns, and acts on the difference at the next
repave, which runs when the Node is deleted.

The values the daemon compares are the isolation mode, `Cluster.Version`,
`OCIImage`, `Kubelet.ApiServer`, `Cluster.CaCertBase64`, `Cluster.ClusterDNS`,
`Kubelet.Auth.BootstrapToken`, `Kubelet.Labels`,
`Kubelet.RegisterWithTaints`, `Kubelet.Configuration`,
`Kubelet.ImageCredentialProvider`, `Gantry.Disabled`, and `LocalDNS`. A difference
in one of those is drift, and drift is what makes the repave rebuild the node.

Several values are not part of that comparison: `Kubelet.NodeIP`, the `CRI` and
`CNI` versions, `AdditionalHostDevices`, `AdditionalHostMounts`,
`NSpawnLifecycle`, `OfflineArtifacts`, and `ProvisioningFormat`. A change to them
does not by itself cause a repave. They take effect the next time the node is
built for another reason.

`Downloads` is a special case. It is not stored in the applied configuration, so
the daemon reads it from `spec.agent.downloads` on the `Machine` every time. That
is deliberate: a repave on a host that can reach only a mirror must not fall back
to upstream hosts.

Some changes are more than drift. `MachineName`, `Cluster.Version`, `OCIImage`,
`Kubelet.ApiServer`, and a non-default `Isolation` are combined into the
installation fingerprint. Changing one of them means the host is a different
installation, and the agent requires a `reset` before it will bootstrap it again
rather than building a second node stack beside the first.

The supported way to change these values on a running host is to edit the
`Machine` resource, not the file on disk. Machina, metalman, and the daemon all
derive the next configuration from the resource. A hand-edited file is useful for
a reset-and-reinstall or a lab host, not for a live drift change.

## Troubleshooting

Most configuration failures are reported before the first phase, and the message
names the field. The table below maps the common ones to a fix.

| Message contains | Cause | Fix |
| --- | --- | --- |
| `read agent config file` | Wrong path, missing file, or unreadable permissions | Check `UNBOUNDED_AGENT_CONFIG_FILE` and file mode |
| `decode agent config file` | Invalid JSON | Remove comments and trailing commas; validate the file |
| `resolve machine name` or `machine name ... is not a valid Kubernetes node name` | Explicit name is not a DNS-1123 subdomain | Use lowercase letters, digits, `-`, and `.` |
| `Cluster.ClusterDNS is required` | Empty DNS address | Set the `kube-dns` `ClusterIP` |
| `Kubelet.ApiServer is required` or `Kubelet.ApiServer is invalid` | Missing endpoint or a URL without scheme and host | Set a full `https://host:port` endpoint |
| `setting Kubelet.Configuration.<field> is not supported` | The overlay sets an agent-owned key | Remove the key; the agent renders it |
| `isolation mode "container" is experimental` | `Isolation: container` without the opt-in | Add `Experimental.ContainerIsolation: true` |
| `invalid Isolation` or `invalid --isolation` | Unknown mode name | Use `nspawn`, `none`, or `container` |
| `LocalDNS listener IPs must be distinct` | Both listeners share an address | Keep the two defaults or choose two addresses |
| `Cluster.ClusterDNS must be distinct from LocalDNS listeners` | DNS service IP collides with a listener | Choose different listener addresses |
| `LocalDNS.NodeListenerIP: must be a unicast IPv4 address` | IPv6, multicast, or unspecified address | Use a unicast IPv4 address |
| `LocalDNS.MetricsAddress: must use IP:port syntax` | Missing port or invalid port | Use `IP:port` with a port from 1 to 65535 |
| `parse LocalDNS.CorefileTemplate` | Template syntax error | Check the Go template, including field names |
| `offline artifacts Kubernetes version ... does not match Cluster.Version` | Bundle built for another release | Use the matching bundle or version |
| `NvidiaToolkit is only supported with Isolation "none"` | Toolkit paths set in nspawn or container mode | Remove the block or switch to `none` |
| `BootstrapToken and ExecCredential are mutually exclusive` | Two auth methods in one file | Keep one |

Authentication failures do not surface as configuration errors. A rejected or
expired token, a wrong CA, or an unreachable API server show up during node
start, after the file has validated. Run `unbounded-agent preflight` first when
you are unsure which layer is failing.

## Related

- [Preflight]({{< relref "reference/agent/preflight" >}}) for the checks that run
  against a loaded configuration.
- [Isolation]({{< relref "reference/agent/isolation" >}}) for the modes and their
  host requirements.
- [Agent]({{< relref "guides/agent" >}}) for the bootstrap walkthrough and repair
  procedures.
- [Machina CRD]({{< relref "reference/machina-crd" >}}) for the `Machine` fields
  that generate this file.
- [Networking Configuration]({{< relref "reference/networking/configuration" >}})
  for the node networking configuration that this file does not cover.
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}}) for
  upgrade, reset, and recovery tasks.
