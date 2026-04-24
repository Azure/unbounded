---
title: "Agent"
weight: 3
description: "How the unbounded-agent bootstraps a host into a Kubernetes worker node."
---

The unbounded-agent is a single binary that turns a Linux host into a
Kubernetes worker node. It is the final convergence point for every
provisioning path: regardless of how the host was delivered (SSH, cloud-init,
PXE), the agent runs the same bootstrap sequence. Typically the agent is
invoked by a controller (machina, metalman, or Karpenter), but it can also be
run manually to bootstrap a machine and join it as a worker node.

## How It Works

When `unbounded-agent start` is invoked, the agent runs through three phases
in sequence:

1. **Host preparation** (`host`) -- installs required packages, configures
   kernel parameters for Kubernetes networking, and disables services that
   conflict with kubelet (Docker, swap).
2. **Rootfs preparation** (`rootfs`) -- detects host settings such as CPU
   architecture and NVIDIA GPU devices, creates a
   [systemd-nspawn]({{< relref "reference/agent/nspawn" >}}) machine rootfs
   (from an OCI image), and downloads containerd, runc, CNI
   plugins, and Kubernetes binaries.
3. **Node start** (`node-start`) -- boots the nspawn machine and starts
   containerd and kubelet inside it. Kubelet performs TLS bootstrapping against
   the API server using the configured bootstrap token.

## Configuration

The agent reads a JSON config file whose path is set through the
`UNBOUNDED_AGENT_CONFIG_FILE` environment variable. The `manual-bootstrap`
command generates this config automatically, but it can also be authored by
hand. A minimal example:

```json
{
  "MachineName": "mysite-worker-01",
  "Cluster": {
    "CaCertBase64": "<base64-encoded CA certificate>",
    "ClusterDNS": "10.0.0.10",
    "Version": "1.33.1"
  },
  "Kubelet": {
    "ApiServer": "https://api.example.com:6443",
    "BootstrapToken": "abc123.0123456789abcdef",
    "Labels": {
      "unbounded-cloud.io/site": "mysite"
    },
    "RegisterWithTaints": []
  }
}
```

See the
[Configuration reference]({{< relref "reference/agent/configuration" >}}) for
the full field specification.

## Running the Agent

### What You'll Do

Join an existing Ubuntu VM to your Kubernetes cluster as a worker node by
manually running the unbounded-agent on the target host.

### Prerequisites

- The [`kubectl-unbounded` plugin]({{< relref "reference/cli" >}}) installed
  and on your `PATH`.
- A site already created in the cluster. See the
  [Getting Started]({{< relref "guides/getting-started" >}}) guide.

### Steps

1. Generate and run the bootstrap script on the target VM:

```bash
kubectl unbounded machine manual-bootstrap my-node --site mysite | ssh user@host sudo bash
```

{{< callout type="note" >}}
To inspect the script before running it, save it to a file first:

```bash
kubectl unbounded machine manual-bootstrap my-node --site mysite > bootstrap.sh
```
{{< /callout >}}

### Customizing the agent download

By default the bootstrap script downloads the latest published
`unbounded-agent` release directly from GitHub. This means new releases are
picked up automatically without editing scripts or docs. The download can be
customized when generating the bootstrap payload or at runtime via
environment variables:

- `--agent-version` / `AGENT_VERSION`: pin to a specific release tag instead
  of tracking latest. Example: `--agent-version v0.0.10`.
- `--agent-base-url` / `AGENT_BASE_URL`: point at a self-hosted mirror of the
  release assets. The layout under the base URL must match the GitHub
  releases layout (`<base>/latest/download/<asset>` and
  `<base>/download/<tag>/<asset>`). Useful for air-gapped environments.
- `--agent-url` / `AGENT_URL`: fully qualified download URL for the agent
  tarball. Overrides the version / base URL resolution entirely.

Examples:

```bash
# Pin the agent to a specific release:
kubectl unbounded machine manual-bootstrap my-node --site mysite \
    --agent-version v0.0.10 | ssh user@host sudo bash

# Self-host the release assets under an internal mirror:
kubectl unbounded machine manual-bootstrap my-node --site mysite \
    --agent-base-url https://releases.internal.example.com/unbounded \
    | ssh user@host sudo bash
```

The same overrides can also be exported on the target host before running a
previously generated bootstrap script:

```bash
export AGENT_BASE_URL=https://releases.internal.example.com/unbounded
bash bootstrap.sh
```

The same agent-download flags are also accepted by `kubectl unbounded
machine register`, which creates a `Machine` CRD. The machina controller
then SSH's into the host and exports the same `AGENT_*` environment
variables when running the install script, so pinning an agent version or
pointing at a mirror works uniformly across all three provisioning paths
(`manual-bootstrap`, `register`, and the machina reconciler):

```bash
kubectl unbounded machine register \
    --site mysite \
    --host 10.0.0.5 \
    --ssh-username azureuser \
    --ssh-private-key ~/.ssh/id_ed25519 \
    --agent-version v0.0.10 \
    --agent-base-url https://releases.internal.example.com/unbounded
```

These values are persisted on the `Machine` object under
`spec.agent.{version,baseURL,url}` so the controller reproduces the same
override on every reconcile:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: mysite-worker-01
spec:
  agent:
    version: v0.0.10
    baseURL: https://releases.internal.example.com/unbounded
  ssh:
    host: 10.0.0.5
    # ...
```

### Mirroring the rootfs downloads (air-gapped deployments)

After installing the `unbounded-agent`, the agent downloads five additional
binaries into the nspawn machine rootfs:

| Artifact | Upstream default |
|---|---|
| kubelet / kubectl / kube-proxy | `https://dl.k8s.io` |
| containerd | `https://github.com/containerd/containerd/releases/download` |
| runc | `https://github.com/opencontainers/runc/releases/download` |
| CNI plugins | `https://github.com/containernetworking/plugins/releases/download` |
| crictl (cri-tools) | `https://github.com/kubernetes-sigs/cri-tools/releases/download` |

Each default can be overridden at registration time. Every artifact
exposes three flags, mirroring the `--agent-*` flags above:

- `--<artifact>-base-url`: replaces the upstream host + path prefix.
  Mirrors must preserve the same file layout as the upstream project
  (e.g. `<base>/v<version>/<asset>`).
- `--<artifact>-url`: full URL template; `%s` fmt placeholders are
  substituted with version / arch fields in the same order as the
  upstream default.
- `--<artifact>-version` (or `--kubernetes-binary-version`): pin the
  artifact to a specific version independently of the cluster
  Kubernetes version. The default crictl minor-alignment behavior
  (Kubernetes vX.Y.Z -> cri-tools vX.Y.0) is preserved when no crictl
  version is set.

The flags are available on both `manual-bootstrap` and `register`, and
the machina controller forwards them through the agent JSON config
(`Downloads` block) so an air-gapped operator can register a node
against an internal mirror without touching source:

```bash
kubectl unbounded machine register \
    --site mysite \
    --host 10.0.0.5 \
    --ssh-username azureuser \
    --ssh-private-key ~/.ssh/id_ed25519 \
    --agent-base-url      https://mirror.internal/unbounded \
    --kubernetes-base-url https://mirror.internal/k8s \
    --containerd-base-url https://mirror.internal/containerd/releases/download \
    --runc-base-url       https://mirror.internal/runc/releases/download \
    --cni-base-url        https://mirror.internal/plugins/releases/download \
    --crictl-base-url     https://mirror.internal/cri-tools/releases/download
```

The resulting `Machine` persists the overrides under
`spec.agent.downloads` so the controller reproduces them on every
reconcile:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: mysite-worker-01
spec:
  agent:
    baseURL: https://mirror.internal/unbounded
    downloads:
      kubernetes:
        baseURL: https://mirror.internal/k8s
      containerd:
        baseURL: https://mirror.internal/containerd/releases/download
      runc:
        baseURL: https://mirror.internal/runc/releases/download
      cni:
        baseURL: https://mirror.internal/plugins/releases/download
      crictl:
        baseURL: https://mirror.internal/cri-tools/releases/download
  ssh:
    host: 10.0.0.5
    # ...
```

Leaving any artifact unset preserves the upstream default, so you can
mirror a subset of the artifacts and let the rest fall back to the
public CDN.

A successful run looks like this (timestamps shortened for readability):

```
[I] starting unbounded-agent version=dev commit=unknown
[I] [install-packages] started
[I] [install-packages] completed duration=7.35s status=ok
[I] [parallel(configure-os, configure-nftables, disable-docker, disable-swap)] started
[I] [disable-swap] started
[I] [configure-os] started
[I] [configure-nftables] started
[I] [disable-docker] started
[I] [disable-swap] completed status=ok duration=3.86ms
[I] [configure-os] completed duration=18.04ms status=ok
[I] [configure-nftables] completed duration=1.07s status=ok
[I] [disable-docker] completed status=ok duration=1.09s
[I] [parallel(configure-os, configure-nftables, disable-docker, disable-swap)] completed duration=1.09s status=ok
[I] [apply-attestation] started
[I] attestation not configured, skipping
[I] [apply-attestation] completed duration=12.81µs status=ok
[I] [ensure-nspawn-workspace] started
[I] [oci-download-rootfs] started
[I] pulling OCI image image=ghcr.io/azure/agent-ubuntu2404:... dest=/var/lib/machines/my-node
[I] OCI image extraction complete dest=/var/lib/machines/my-node
[I] [oci-download-rootfs] completed duration=2.10s status=ok
[I] [ensure-nspawn-workspace] completed duration=2.23s status=ok
[I] [parallel(download-kube-binaries, download-cri-binaries, download-cni-binaries, configure-os, disable-resolved)] started
[I] [download-kube-binaries] started
[I] [download-cri-binaries] started
[I] [download-cni-binaries] started
[I] [download-kube-binaries] completed status=ok duration=2.06s
[I] [download-cni-binaries] completed duration=3.21s status=ok
[I] [download-cri-binaries] completed duration=3.32s status=ok
[I] [parallel(download-kube-binaries, download-cri-binaries, download-cni-binaries, configure-os, disable-resolved)] completed duration=3.33s status=ok
[I] [parallel(configure-containerd, configure-kubelet)] started
[I] [configure-containerd] completed duration=5.57ms status=ok
[I] [configure-kubelet] completed duration=13.31ms status=ok
[I] [parallel(configure-containerd, configure-kubelet)] completed duration=13.46ms status=ok
[I] [start-nspawn-machine] started
[I] [start-nspawn-machine] completed duration=654.85ms status=ok
[I] [setup-nvidia] started
[I] NVIDIA runtime not enabled or no host libraries found, skipping
[I] [setup-nvidia] completed status=ok duration=18.40µs
[I] [start-containerd] started
[I] [start-containerd] completed status=ok duration=319.89ms
[I] [start-kubelet] started
[I] [start-kubelet] completed duration=180.11ms status=ok
```

2. Verify the node has joined:

```bash
kubectl get nodes -o wide
```

```
NAME       STATUS   ROLES    AGE   VERSION   INTERNAL-IP      EXTERNAL-IP   OS-IMAGE               KERNEL-VERSION      CONTAINER-RUNTIME
my-node    Ready    <none>   20s   v1.33.1   192.168.100.10   <none>        Ubuntu 24.04.4 LTS     6.8.0-106-generic   containerd://2.0.4
```

## GPU Support

When NVIDIA GPUs are detected on the host, the agent automatically:

- Bind-mounts GPU devices and driver libraries into the nspawn machine.
- Generates a CDI spec and registers the NVIDIA container runtime with
  containerd.

No additional configuration is required. Both `amd64` and `arm64`
architectures are supported. See the
[GPU reference]({{< relref "reference/gpu" >}}) for details.

## Troubleshooting

**Agent fails to start** -- Verify that the config file path is correct and
contains valid JSON. Check that all required fields are present.

**Node not joining the cluster** -- Ensure the host has HTTPS connectivity to
the API server and that the bootstrap token has not expired. Inspect the
kubelet logs inside the nspawn machine:

```bash
machinectl shell <machine-name> /bin/journalctl -u kubelet
```

**GPU not detected** -- Confirm that the NVIDIA driver is installed on the host
and that GPU devices are visible under `/dev/nvidia*`.

## See Also

- **[Project Overview]({{< relref "concepts/overview" >}})** -- How the agent
  fits into the broader system.
- **[Architecture Reference]({{< relref "reference/architecture" >}})** -- Deep
  dive into component internals.
- **[nspawn Isolation]({{< relref "reference/agent/nspawn" >}})** -- How the
  agent uses systemd-nspawn to isolate worker node components.
