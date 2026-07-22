# Agent Offline Bootstrap Artifacts

## Goals

- Describe how an operator can mirror or pre-stage agent bootstrap artifacts.
- Propose support for reading binary artifacts from the local filesystem, an internal OCI artifact bundle, or an archive downloaded over HTTPS.
- Allow HTTPS to act as a transport for both rootfs OCI layouts and complete offline artifact bundles.
- Keep the existing agent config shape as much as possible.

## Non-goals

- Changing Kubernetes image pull behavior for workload images.
- Supporting authenticated OCI registries for offline artifact bundles.
- Supporting authenticated HTTPS archive URLs, including URL user info or query-string credentials.
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

Offline environments must either mirror the selected image into an internal registry, pre-stage it as a local OCI layout, or make a tarred OCI layout available from an HTTPS endpoint reachable by the host.

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

`OCIImage` selects the rootfs image. It accepts a registry reference, a local OCI layout, or an HTTPS URL to a tarred OCI layout:

```json
{
  "OCIImage": "registry.internal.example.com/unbounded/agent-ubuntu2404:v20260619"
}
```

```json
{
  "OCIImage": "oci-layout:///opt/unbounded/images/agent-ubuntu2404:v20260619"
}
```

```json
{
  "OCIImage": "https://artifacts.internal.example.com/agent-ubuntu2404.oci.tar"
}
```

An HTTPS archive must contain exactly one tagged image reference, which the agent selects automatically. Archives with zero or multiple tagged image references are rejected.

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

Add a top-level `OfflineArtifacts` config block for complete offline binary artifact sets. It points at one bundle root on the local filesystem, one bundle in an internal OCI registry, or one tar archive served over HTTPS. The bundle is self-describing: it includes a small manifest that tells the agent which binary versions are present.

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

Example HTTPS archive mode:

```json
{
  "OfflineArtifacts": {
    "Source": "https://artifacts.internal.example.com/bootstrap-artifacts-v0.4.0-k8s-{{ .KubernetesVersion }}.tar.gz"
  }
}
```

When `OfflineArtifacts.Source` is non-empty, the agent treats the offline source as the complete binary artifact source and ignores `Downloads`. This avoids partial offline configuration where some artifacts come from the offline bundle and others fall back to internet defaults. The agent renders the offline source template and resolves the bundle. For HTTPS mode, resolution first downloads and extracts the archive into a source-specific host cache. The agent then reads `manifest.json` and uses the versions declared there when resolving binary artifact paths.

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
| Container image archive | `container-images/<arch>/<sanitized-image-ref>-<hash>.tar` |
| Container image archive checksum | `container-images/<arch>/<sanitized-image-ref>-<hash>.tar.sha256` |

Container image archive paths are derived from each image ref in `manifest.json` `containerImages`. The filename is stable for a given image ref and includes a sanitized image ref plus a hash to avoid collisions. The archive is a local image archive suitable for `ctr --namespace k8s.io images import`.

The existing `Downloads` block remains the regular per-artifact override mechanism. As a separate compatibility improvement, `Downloads.*.BaseURL` and `Downloads.*.URL` should also support `file://` and `oci://` endpoints for non-offline custom layouts. Those `Downloads` settings are ignored whenever offline artifacts are configured.

The rootfs image remains controlled by `OCIImage`. Even in offline mode, operators must set `OCIImage` to the registry reference, local OCI layout, or HTTPS OCI layout archive they want the agent to use. Offline artifact source settings do not infer or rewrite `OCIImage`.

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

### HTTPS archive mode

In HTTPS archive mode, `OfflineArtifacts.Source` is the URL of one plain tar or gzip-compressed tar archive containing the complete filesystem bundle layout. This mode is useful when operators can host static files but do not want to operate an OCI registry or copy the expanded bundle onto every host.

Example:

```json
{
  "OfflineArtifacts": {
    "Source": "https://artifacts.internal.example.com/bootstrap-artifacts-v0.4.0-k8s-{{ .KubernetesVersion }}.tar.gz"
  }
}
```

The archive may place the bundle directly at its root or under a containing directory. It must contain exactly one `manifest.json`; the directory containing that file becomes the resolved bundle root. All component-prefixed paths are interpreted relative to that directory.

The agent treats HTTPS only as the transport for the archive:

1. Render and validate the HTTPS URL.
2. Download and safely extract the archive into a source-specific host cache under `/var/lib/unbounded/offline-artifacts/`.
3. Resolve the extracted directory using the same filesystem logic as `file://` mode.
4. Validate the manifest, configured versions, required host-architecture artifacts, and required checksum files.
5. Mark the extracted cache ready only after all validation succeeds.
6. Produce `file://` download overrides that point into the extracted cache.

The cache directory name includes a short hash of the rendered source URL. A ready cache can be reused by bootstrap, preflight, repave, and upgrade operations. An incomplete cache is removed and rebuilt on the next resolution attempt.

HTTPS archive URLs must include a host and archive path. URL user info, query parameters, and fragments are rejected. The HTTPS endpoint must use a certificate trusted by the host. Archive extraction rejects absolute or parent-traversing paths, duplicate files, links, and entry types other than directories and regular files.

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
container-images/amd64/mcr.microsoft.com_oss_v2_kubernetes_pause_3.9-<hash>.tar
container-images/amd64/mcr.microsoft.com_oss_v2_kubernetes_pause_3.9-<hash>.tar.sha256
```

### Container image archive staging

Offline container image archives are staged on the host before the nspawn machine starts. The agent downloads archives listed by `manifest.json` `containerImages` from the resolved offline source, verifies each archive with the adjacent `.sha256` artifact, and writes them into a source-specific host cache directory.

The host-side staging layout is:

```text
/var/lib/unbounded/container-images/
  current -> <source-cache-dir>
  <source-cache-dir>/
    image-0.tar
    image-1.tar
    ...
  empty/
```

`<source-cache-dir>` is derived from the resolved `OfflineArtifacts.Source` and includes a short hash of that source, so different Kubernetes versions or registries do not share one cache directory. The stable `current` symlink points at the cache directory for the most recently resolved offline source. When offline artifacts are not configured, the staging target is the `empty/` directory.

The nspawn machine bind-mounts the stable host path read-only:

```text
host:    /var/lib/unbounded/container-images/current
machine: /var/lib/unbounded/container-images
mode:    read-only
```

Inside the running machine, node-start imports every staged `.tar` file visible at `/var/lib/unbounded/container-images` with:

```bash
ctr --namespace k8s.io images import /var/lib/unbounded/container-images/image-<n>.tar
```

The staging cache is host-level instead of machine-rootfs-specific. This lets alternating nspawn machines, such as `kube1` and `kube2`, share already downloaded image archives across initial bootstrap and repave. Node restart does not redownload archives because it reuses the existing rootfs and host staging state.

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

`OCIImage` should support three source modes:

1. Normal OCI registry image references, including internal registry mirrors.
2. Local OCI layout directories.
3. Tarred OCI layout archives downloaded over HTTPS.

Local layout example:

```text
oci-layout:///opt/unbounded/images/agent-ubuntu2404:v20260619
```

HTTPS archive example:

```text
https://artifacts.internal.example.com/agent-ubuntu2404.oci.tar
```

The HTTPS object may be a plain tar or gzip-compressed tar archive. After download, the archive must extract to an OCI image layout containing `oci-layout`, `index.json`, and `blobs/`. It may contain those files at the archive root or under one containing directory. The archive must contain exactly one tagged image reference, which the agent selects automatically; archives with zero or multiple tagged references are rejected.

The agent probes the HTTPS object during preflight without downloading its full contents. During rootfs provisioning, it downloads and safely extracts the archive into a temporary directory, locates the single OCI layout and image reference, and reuses the same OCI unpack path as `oci-layout://`. The temporary archive contents are removed after provisioning. URL user info, query parameters, and fragments are rejected.

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

Offline operators mirror these images into their target environment by copying them to an internal registry, exporting them as local OCI layout directories, or packaging those layouts as tar archives for HTTPS delivery. An HTTPS rootfs archive should contain the same OCI layout content that filesystem mode consumes, with exactly one tagged image reference. For `agent-*` images, the container image workflow uses `agent-artifacts-builder archive-oci-image` to export the pushed image and uploads the resulting `.oci.tar.gz` archive and adjacent `.sha256` file as GitHub Actions artifacts.

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

The OCI representation should use one OCI blob per target artifact, not one tarball layer containing all artifacts. This lets the agent fetch only the artifacts it needs and allows registries to deduplicate unchanged blobs across bundle tags.

For HTTPS archive mode, `agent-artifacts-builder` should also package the expanded filesystem bundle as a gzip-compressed tar archive and write an adjacent `.sha256` file. The archive contains `manifest.json` and the same component-prefixed paths as filesystem mode. It is intentionally a complete bundle because static HTTPS servers cannot provide OCI title-based blob selection. The version-group publishing flow writes archives named like `bootstrap-artifacts-<tag-prefix>-k8s-<kubernetes-version>.tar.gz` alongside the expanded bundles uploaded by the workflow.

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

For HTTPS archive mode, operators publish a tarred OCI layout and a complete offline artifact archive on an HTTPS server trusted by the target hosts, then configure:

```json
{
  "OCIImage": "https://artifacts.internal.example.com/agent-ubuntu2404.oci.tar",
  "OfflineArtifacts": {
    "Source": "https://artifacts.internal.example.com/bootstrap-artifacts-v0.4.0-k8s-{{ .KubernetesVersion }}.tar.gz"
  }
}
```

The OCI layout archive in this example contains one tagged rootfs image reference, which the agent selects automatically. Archives with zero or multiple tagged references are rejected.

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

HTTPS archive mode supports the same version-template pattern as OCI registry mode. Operators should publish immutable, versioned archive URLs for every supported Kubernetes version. The source-specific extracted cache lets preflight, bootstrap, and repave reuse a bundle after it has been downloaded and validated.

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

For filesystem mode, existence checks should verify regular files under the resolved source root. For OCI registry mode, existence checks should resolve the OCI artifact manifest once, build a title-to-descriptor map, and verify that each required artifact title exists. Preflight should not download large OCI artifact blobs just to check availability; descriptor presence is enough. Kubernetes checksum blobs are required artifacts and must be checked just like the binaries.

For HTTPS archive mode, source resolution downloads and safely extracts the complete archive before validating it with the filesystem checks. A cache is only marked ready after the manifest, versions, required artifact paths, and required checksum files validate as present. Artifact contents are verified through the normal installation path. This is intentionally different from OCI registry mode because a static archive does not expose per-file descriptors before download.

Preflight should also validate `OCIImage` independently because `OfflineArtifacts.Source` does not control the rootfs image. For registry image references, preflight should check that the image reference is syntactically valid and reachable. For local `oci-layout://` references, preflight should check that the OCI layout directory exists and contains the requested reference. For HTTPS OCI layout archives, preflight should probe the archive URL without downloading it; provisioning performs full download, safe extraction, layout validation, and unpacking.

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
