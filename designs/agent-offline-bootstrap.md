# Agent Offline Bootstrap Artifacts

## Goals

- Describe how an operator can mirror or pre-stage agent bootstrap artifacts.
- Propose support for reading binary artifacts from either the local filesystem or an internal OCI artifact bundle.
- Keep the existing agent config shape as much as possible.

## Non-goals

- Changing Kubernetes image pull behavior for workload images.
- Supporting authenticated OCI registries for offline artifact bundles.
- Replacing host OS package management.

## Bootstrap artifact targets

The agent bootstrap path has three artifact classes:

1. The rootfs OCI image.
2. Binary archives and executables installed into the nspawn rootfs.
3. Host OS packages required before the agent can run nspawn and configure the host.

### Rootfs OCI image

The agent uses `OCIImage` to bootstrap the nspawn rootfs. If unset, the agent selects a default image based on host distro and GPU presence.

Current default image targets include:

```text
ghcr.io/azure/agent-ubuntu2404:v20260619
ghcr.io/azure/agent-ubuntu2404-nvidia:v20260619
ghcr.io/azure/agent-ubuntu2604:v20260619
ghcr.io/azure/agent-ubuntu2604-nvidia:v20260619
ghcr.io/azure/agent-azlinux3:v20260619
ghcr.io/azure/agent-azlinux3-nvidia:v20260626
```

Offline environments must either mirror the selected image into an internal registry or pre-stage it in a local format supported by the agent.

### Rootfs binary artifacts

The agent installs the following binaries into the nspawn rootfs during provisioning.

#### Kubernetes binaries

Installed binaries:

```text
kubelet
kubectl
kube-proxy
```

Current default source layout:

```text
https://dl.k8s.io/<kubernetes-version>/bin/linux/<arch>/<binary>
https://dl.k8s.io/<kubernetes-version>/bin/linux/<arch>/<binary>.sha256
```

The `.sha256` files are required because the agent verifies each Kubernetes binary artifact itself. For example, `kubelet.sha256` must contain the SHA256 digest of the `kubelet` binary, not a digest of a bundle or manifest that contains it.

Version selection:

1. `Downloads.Kubernetes.Version`, if set.
2. Otherwise `Cluster.Version` from the agent config.

#### containerd

Current default version:

```text
2.1.8
```

Current default source layout:

```text
https://github.com/containerd/containerd/releases/download/v<version>/containerd-<version>-linux-<arch>.tar.gz
```

#### runc

Current default version:

```text
1.5.0
```

Current default source layout:

```text
https://github.com/opencontainers/runc/releases/download/v<version>/runc.<arch>
```

#### CNI plugins

Current default version:

```text
1.5.1
```

Current default source layout:

```text
https://github.com/containernetworking/plugins/releases/download/v<version>/cni-plugins-linux-<arch>-v<version>.tgz
```

The extracted archive must provide at least:

```text
bridge
host-local
loopback
```

#### crictl

The default crictl version is derived from the Kubernetes version unless explicitly configured.

Version selection:

1. `Downloads.Crictl.Version`, if set.
2. Otherwise derive from the resolved Kubernetes version using the same major/minor and patch `0`.

For example, Kubernetes `v1.33.5` resolves to crictl `1.33.0`.

Current default source layout:

```text
https://github.com/kubernetes-sigs/cri-tools/releases/download/v<version>/crictl-v<version>-<os>-<arch>.tar.gz
```

## Current agent config controls

The agent config already has two relevant controls.

### `OCIImage`

`OCIImage` selects the rootfs image:

```json
{
  "OCIImage": "registry.internal.example.com/unbounded/agent-ubuntu2404:v20260619"
}
```

### `Downloads`

`Downloads` selects the binary artifact sources:

```json
{
  "Downloads": {
    "Kubernetes": {
      "BaseURL": "https://mirror.internal.example.com/k8s",
      "Version": "v1.33.5"
    },
    "Containerd": {
      "BaseURL": "https://mirror.internal.example.com/containerd",
      "Version": "2.1.8"
    },
    "Runc": {
      "BaseURL": "https://mirror.internal.example.com/runc",
      "Version": "1.5.0"
    },
    "CNI": {
      "BaseURL": "https://mirror.internal.example.com/cni",
      "Version": "1.5.1"
    },
    "Crictl": {
      "BaseURL": "https://mirror.internal.example.com/cri-tools",
      "Version": "1.33.0"
    }
  }
}
```

Each download entry supports:

- `BaseURL`: replace the upstream base URL while preserving the default artifact suffix.
- `URL`: replace the full source with a format string.
- `Version`: override the artifact version.

Precedence is:

1. `URL`
2. `BaseURL`
3. built-in upstream default

`Version` is optional. It only overrides the version used to resolve that artifact. If it is omitted, the agent uses its normal resolved version:

| Entry | Version used when `Downloads.*.Version` is omitted |
|---|---|
| `Kubernetes` | `Cluster.Version` from the agent config |
| `Containerd` | `CRI.Containerd.Version` from the agent config, or the built-in default |
| `Runc` | `CRI.Runc.Version` from the agent config, or the built-in default |
| `CNI` | `CNI.PluginVersion` from the agent config, or the built-in default |
| `Crictl` | Derived from the Kubernetes major/minor version, unless explicitly set |

In regular `Downloads` configs, specify `Version` when the mirrored or preloaded artifact set is pinned independently of the cluster or built-in defaults. Otherwise it can be omitted.

### `BaseURL` suffix templating

`BaseURL` is not a complete artifact source. It is a prefix. The agent trims any trailing `/` from `BaseURL`, then appends the built-in suffix for the artifact being downloaded.

For HTTP mirrors, the resolved source is:

```text
<BaseURL>/<artifact-specific-suffix>
```

The artifact-specific suffixes are:

| Entry | Suffix appended to `BaseURL` |
|---|---|
| `Kubernetes` | `v<version>/bin/linux/<arch>/<binary>` |
| `Kubernetes` checksum | `v<version>/bin/linux/<arch>/<binary>.sha256` |
| `Containerd` | `v<version>/containerd-<version>-linux-<arch>.tar.gz` |
| `Runc` | `v<version>/runc.<arch>` |
| `CNI` | `v<version>/cni-plugins-linux-<arch>-v<version>.tgz` |
| `Crictl` | `v<version>/crictl-v<version>-<os>-<arch>.tar.gz` |

For example:

```json
{
  "Downloads": {
    "Runc": {
      "BaseURL": "https://mirror.internal.example.com/runc",
      "Version": "1.5.0"
    }
  }
}
```

resolves to:

```text
https://mirror.internal.example.com/runc/v1.5.0/runc.amd64
```

`URL` is different: it replaces the full source and is formatted directly with the artifact's arguments. Use `URL` when the mirror layout does not match the built-in suffixes.

## Proposal

Add a top-level `OfflineArtifacts` config block for complete offline binary artifact sets. It points at one bundle root, either on the local filesystem or in an internal OCI registry. The bundle is self-describing: it includes a small manifest that tells the agent which binary versions are present.

`OfflineArtifacts.Source` is a Go template string that resolves to the bundle root.

Example filesystem mode:

```json
{
  "OfflineArtifacts": {
    "Source": "file:///opt/unbounded/artifacts/{{ .KubernetesVersion }}"
  }
}
```

Example OCI registry mode:

```json
{
  "OfflineArtifacts": {
    "Source": "oci://registry.internal.example.com/unbounded/bootstrap-artifacts:v0.4.0-k8s-{{ .KubernetesVersion }}"
  }
}
```

When `OfflineArtifacts.Source` is non-empty, the agent treats the offline source as the complete binary artifact source and ignores `Downloads`. This avoids partial offline configuration where some artifacts come from the offline bundle and others fall back to internet defaults. The agent renders the offline source template, reads `manifest.json` from it, and uses the versions declared there when resolving binary artifact paths.

`OfflineArtifacts.Source` is rendered before reading `manifest.json`, using values derived from the agent config. This lets one config be reused across clusters on different Kubernetes versions.

Supported template data:

| Field | Example value | Description |
|---|---|---|
| `.KubernetesVersion` | `v1.34.2` | Full Kubernetes version with `v` prefix |
| `.KubernetesVersionNoV` | `1.34.2` | Full Kubernetes version without `v` prefix |

The agent should always provide both variables after normalizing `Cluster.Version`. For example, if `Cluster.Version` is `1.34.2`, `.KubernetesVersion` is `v1.34.2` and `.KubernetesVersionNoV` is `1.34.2`. Template execution should use strict missing-key behavior so typos fail preflight instead of rendering as empty strings.

For example:

```json
{
  "OfflineArtifacts": {
    "Source": "oci://registry.internal.example.com/unbounded/bootstrap-artifacts:v0.4.0-k8s-{{ .KubernetesVersion }}"
  }
}
```

resolves to:

```text
oci://registry.internal.example.com/unbounded/bootstrap-artifacts:v0.4.0-k8s-v1.34.2
```

If `OfflineArtifacts.Source` references Kubernetes version fields and the agent config does not contain a usable Kubernetes version, preflight should fail.

Offline artifact paths are component-prefixed to avoid ambiguity when different components share the same version string. The v1 offline layout is:

| Entry | Offline artifact path under the resolved offline source |
|---|---|
| `Kubernetes` | `kubernetes/<kubernetes-version>/bin/linux/<arch>/<binary>` |
| `Kubernetes` checksum | `kubernetes/<kubernetes-version>/bin/linux/<arch>/<binary>.sha256` |
| `Containerd` | `containerd/v<version>/containerd-<version>-linux-<arch>.tar.gz` |
| `Runc` | `runc/v<version>/runc.<arch>` |
| `CNI` | `cni/v<version>/cni-plugins-linux-<arch>-v<version>.tgz` |
| `Crictl` | `crictl/v<version>/crictl-v<version>-<os>-<arch>.tar.gz` |

The existing `Downloads` block remains the regular per-artifact override mechanism. As a separate compatibility improvement, `Downloads.*.BaseURL` and `Downloads.*.URL` should also support `file://` and `oci://` endpoints for non-offline custom layouts. Those `Downloads` settings are ignored whenever offline artifacts are configured.

The rootfs image remains controlled by `OCIImage`. Even in offline mode, operators must set `OCIImage` to the full image reference or local image source they want the agent to use. Offline artifact source settings do not infer or rewrite `OCIImage`.

### Offline artifact manifest

Every offline artifact source must include a `manifest.json` file at the bundle root. If `schemaVersion` is omitted, the agent treats the manifest as v1.

Minimal manifest:

```json
{
  "versions": {
    "kubernetes": "v1.34.2",
    "containerd": "2.1.8",
    "runc": "1.5.0",
    "cni": "1.5.1",
    "crictl": "1.34.0"
  },
  "containerImages": [
    "mcr.microsoft.com/oss/v2/kubernetes/kube-proxy:v1.34.2",
    "mcr.microsoft.com/oss/v2/kubernetes/pause:3.9"
  ]
}
```

The manifest is the source of truth for binary versions and included container images in offline mode. This prevents a newer agent binary from resolving newer built-in runtime defaults against an older offline bundle. For example, if the agent's built-in containerd default changes from `2.1.8` to `2.1.9`, but the offline manifest declares `2.1.8`, the agent resolves and installs the `2.1.8` artifact from the bundle. Similarly, `containerImages` lists image tags that should be available in containerd after bootstrap imports the bundled image archives, including the pause and kube-proxy images. The initial artifact builder supports exporting publicly pullable container images; private image registry authentication is out of scope for the initial implementation.

The offline bundle should be versioned by Kubernetes version because Kubernetes is the primary compatibility axis. Example OCI tags:

```text
bootstrap-artifacts:v0.4.0-k8s-v1.34.2
bootstrap-artifacts:v0.4.0-k8s-v1.35.0
```

The tag is for operator clarity. The agent should validate against `manifest.json`, not parse the tag.

In offline mode, the agent should fail preflight if:

- `manifest.json` is missing or invalid.
- the cluster Kubernetes version does not match `versions.kubernetes`.
- an explicit runtime version in agent config conflicts with the manifest version.
- a required artifact path derived from the manifest is missing.


### Filesystem mode

In filesystem mode, operators download the pre-created artifacts from the official Unbounded release process, copy them onto each target host, and set `OfflineArtifacts.Source` to the local artifact root. This is useful for small host counts, disconnected bring-up, and environments where a local registry is not available.

Supported source forms must use absolute paths:

```text
file:///opt/unbounded/artifacts
/opt/unbounded/artifacts
```

Relative paths are rejected.

The agent resolves each binary artifact by appending the component-prefixed offline artifact path to the filesystem source root, using versions from `manifest.json`. For example:

```text
file:///opt/unbounded/artifacts/v1.34.2/manifest.json
file:///opt/unbounded/artifacts/v1.34.2/runc/v1.5.0/runc.amd64
file:///opt/unbounded/artifacts/v1.34.2/kubernetes/v1.34.2/bin/linux/amd64/kubelet
file:///opt/unbounded/artifacts/v1.34.2/kubernetes/v1.34.2/bin/linux/amd64/kubelet.sha256
```

Kubernetes checksum resolution remains unchanged. The agent appends `.sha256` to the resolved Kubernetes binary source, so the local directory must also contain the checksum files.

### OCI registry mode

In OCI registry mode, operators download the same pre-created artifacts from the official Unbounded release process, publish them into an internal OCI registry as an artifact bundle, and set `OfflineArtifacts.Source` to that bundle. This is useful for datacenter-local serving and larger fleets where copying artifacts to every host is less practical.

The initial implementation only supports OCI registries that are reachable without authentication from the target host. Authenticated OCI registry access is unsupported and out of scope for this design.

The resolved OCI offline source uses the same fragment-selector convention discussed for OCI `BaseURL` support:

```text
resolved artifact source = <resolved-offline-source>#<component-prefixed-offline-artifact-path>
```

The component-prefixed offline artifact path becomes the OCI blob title selector, using versions from `manifest.json`. The OCI artifact must also contain a blob titled `manifest.json`.

Published OCI bundles should use a single tag per Kubernetes version and an OCI image index to distinguish host architectures. Each platform manifest in the index should set `platform.os` to `linux` and `platform.architecture` to the supported host architecture, for example `amd64` or `arm64`. The agent should select the platform manifest matching `RootFS.HostArch` before building the title-to-descriptor map.

The agent should resolve and fetch the OCI artifact manifest once per resolved `OfflineArtifacts.Source` and selected platform, then cache its descriptor map for the provisioning run. Each binary artifact is fetched as an individual blob selected by title. The agent should not copy or download the entire OCI artifact bundle for each binary.

With this config:

```json
{
  "OfflineArtifacts": {
    "Source": "oci://registry.internal.example.com/unbounded/bootstrap-artifacts:v0.4.0-k8s-v1.34.2"
  }
}
```

the selected `linux/amd64` platform manifest should contain blobs titled like:

```text
manifest.json
kubernetes/v1.34.2/bin/linux/amd64/kubelet
kubernetes/v1.34.2/bin/linux/amd64/kubelet.sha256
kubernetes/v1.34.2/bin/linux/amd64/kubectl
kubernetes/v1.34.2/bin/linux/amd64/kubectl.sha256
kubernetes/v1.34.2/bin/linux/amd64/kube-proxy
kubernetes/v1.34.2/bin/linux/amd64/kube-proxy.sha256
containerd/v2.1.8/containerd-2.1.8-linux-amd64.tar.gz
runc/v1.5.0/runc.amd64
cni/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz
crictl/v1.34.0/crictl-v1.34.0-linux-amd64.tar.gz
```

### Regular `Downloads` endpoint support

Outside offline mode, `Downloads` should continue to support per-artifact overrides. Extend it to accept filesystem and OCI endpoints in addition to HTTP endpoints.

Example custom filesystem override:

```json
{
  "Downloads": {
    "Runc": {
      "URL": "file:///opt/unbounded/artifacts/custom/runc/%s/runc.%s"
    }
  }
}
```

Example custom OCI override:

```json
{
  "Downloads": {
    "Runc": {
      "URL": "oci://registry.internal.example.com/unbounded/bootstrap-artifacts:v1#runtime/runc/%s/runc.%s"
    }
  }
}
```

These custom `Downloads` endpoints are useful for testing, bespoke layouts, and partial mirrors, but they are not used when offline artifacts are configured.

### Rootfs image sources

`OCIImage` should continue to support normal registry image references, including internal registry mirrors. In filesystem mode, add support for local OCI image sources so an airgapped node can bootstrap without a registry pull.

Proposed source form:

```text
oci-layout:///opt/unbounded/images/agent-ubuntu2404:v20260619
```

The agent can reuse the existing OCI unpack path after resolving the local OCI layout directory.

## Artifact publishing process

Unbounded should own and publish the artifacts required for offline agent bootstrap:

1. Rootfs OCI images.
2. Kubernetes-versioned binary artifact bundles.

### Rootfs image publishing

Unbounded should continue publishing the rootfs OCI images used by `OCIImage`. These images are the source of truth for the nspawn rootfs and should be published as normal OCI images under the Unbounded-owned registry namespace.

Examples:

```text
ghcr.io/azure/agent-ubuntu2404:v20260619
ghcr.io/azure/agent-ubuntu2404-nvidia:v20260619
ghcr.io/azure/agent-azlinux3:v20260619
ghcr.io/azure/agent-azlinux3-nvidia:v20260626
```

Offline operators mirror these images into their target environment, either by copying them to an internal registry or by exporting them as local OCI layout directories for filesystem-mode bootstrap.

### Binary artifact bundle publishing

Unbounded should publish offline binary artifact bundles as OCI artifacts. Each bundle is scoped to one Kubernetes version and one Unbounded release or build. The Kubernetes version is included in the tag for operator clarity, while `manifest.json` inside the bundle remains the source of truth for the agent.

Example tags:

```text
ghcr.io/azure/unbounded/bootstrap-artifacts:v0.4.0-k8s-v1.34.2
ghcr.io/azure/unbounded/bootstrap-artifacts:v0.4.0-k8s-v1.35.0
```

Each bundle should contain:

- `manifest.json`.
- Kubernetes binaries and `.sha256` files for the declared Kubernetes version.
- `containerd`, `runc`, CNI plugin, and `crictl` artifacts for the versions declared in `manifest.json`.
- Included container images declared by `manifest.json`, starting with the pause image.
- Container image archives under `container-images/`, which bootstrap should import before validating the listed `containerImages` tags.
- Artifacts for each supported host architecture.

OCI bundles should be published as multi-platform OCI indexes under the single Kubernetes-versioned tag. Each index entry points to a platform-specific artifact manifest, and each platform-specific manifest contains only that architecture's blobs plus `manifest.json` and platform-specific image archives under `container-images/`. For example, pulling `--platform linux/amd64` should return only `amd64` binaries and `amd64` image archives, and pulling `--platform linux/arm64` should return only `arm64` binaries and `arm64` image archives. The tag remains architecture-neutral.

The bundle should use one OCI blob per target artifact, not one tarball containing all artifacts. This lets the agent fetch only the artifacts it needs and allows registries to deduplicate unchanged blobs across bundle tags.

### Operator mirroring workflow

A connected environment downloads or copies the official Unbounded artifacts, verifies them, and imports them into the target environment.

For OCI registry mode, operators mirror:

```text
ghcr.io/azure/unbounded/bootstrap-artifacts:v0.4.0-k8s-v1.34.2
ghcr.io/azure/agent-ubuntu2404:v20260619
```

into an internal unauthenticated registry reachable by target hosts, then configure:

```json
{
  "OCIImage": "registry.internal.example.com/unbounded/agent-ubuntu2404:v20260619",
  "OfflineArtifacts": {
    "Source": "oci://registry.internal.example.com/unbounded/bootstrap-artifacts:v0.4.0-k8s-{{ .KubernetesVersion }}"
  }
}
```

For filesystem mode, operators export the official rootfs image as a local OCI layout directory and copy the binary artifact bundle contents to the host filesystem, then configure:

```json
{
  "OCIImage": "oci-layout:///opt/unbounded/images/agent-ubuntu2404:v20260619",
  "OfflineArtifacts": {
    "Source": "file:///opt/unbounded/artifacts/{{ .KubernetesVersion }}"
  }
}
```

## Repave and upgrade behavior

Repave and rootfs upgrade operations rebuild or reprovision the nspawn rootfs, so they need the same offline artifacts as initial bootstrap. `OfflineArtifacts` should be treated as durable agent config and used for these operations whenever it is set.

For environments that need Kubernetes version upgrade support, the recommended setup is OCI registry mode with a Kubernetes-version template in `OfflineArtifacts.Source`:

```json
{
  "OfflineArtifacts": {
    "Source": "oci://registry.internal.example.com/unbounded/bootstrap-artifacts:v0.4.0-k8s-{{ .KubernetesVersion }}"
  }
}
```

With this pattern, the same agent config can resolve different offline bundles as `Cluster.Version` changes, as long as the internal registry contains a matching bundle tag for each supported Kubernetes version. Operators should keep old bundle tags available for as long as nodes may need to repave, roll back, or recover to those versions.

Filesystem mode can also support upgrades, but each target host must already have the matching artifact directory for every Kubernetes version it may need. This is simpler for small fleets but harder to manage for broad upgrades.

`OfflineArtifacts` does not cover the `unbounded-agent` binary itself. Agent binary upgrades still need their own offline-capable source.

## Preflight checks

Preflight should make offline bootstrap failures actionable before the agent starts mutating the host or rootfs.

When `OfflineArtifacts.Source` is configured, preflight should:

1. Render `OfflineArtifacts.Source` using the normalized Kubernetes version from the agent config.
2. Load `manifest.json` from the resolved offline source.
3. Validate that the cluster Kubernetes version matches `versions.kubernetes`.
4. Validate that any explicit runtime versions in the agent config match the manifest versions.
5. Resolve every required artifact path for the host architecture from the manifest versions.
6. Verify every required artifact exists in the offline source.

For filesystem mode, existence checks should verify regular files under the resolved source root. For OCI registry mode, existence checks should resolve the OCI artifact manifest once, build a title-to-descriptor map, and verify that each required artifact title exists. Preflight should not download large artifact blobs just to check availability; descriptor presence is enough. Kubernetes checksum blobs are required artifacts and must be checked just like the binaries.

Preflight should also validate `OCIImage` independently because `OfflineArtifacts.Source` does not control the rootfs image. For registry image references, preflight should check that the image reference is syntactically valid and reachable. For local `oci-layout://` references, preflight should check that the OCI layout directory exists and contains the requested reference.

## Host package behavior and preflight

`Downloads` and `OfflineArtifacts` only cover binaries installed into the nspawn rootfs. They do not cover host OS packages.

The agent may require host packages such as:

```text
systemd-container
curl
nftables
util-linux
```

If these are missing in an offline or airgapped environment, package manager operations such as `apt-get`, `dnf`, or `tdnf` may fail because no package repository is reachable.

Offline deployments should preinstall the required host packages or configure an internal OS package repository before running the agent.

The agent preflight check should expose missing host packages clearly before bootstrap proceeds. In an airgapped environment, a missing package should be reported as an actionable preflight failure rather than surfacing later as a generic package manager download error.
