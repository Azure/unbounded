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
      "unbounded-kube.io/site": "mysite"
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
