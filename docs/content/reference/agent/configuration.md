---
title: "Configuration"
weight: 1
description: "JSON configuration file specification for the unbounded-agent."
---

The agent reads a JSON config file whose path is set through the
`UNBOUNDED_AGENT_CONFIG_FILE` environment variable. This config can be
generated from the cluster using the
[`kubectl unbounded machine manual-bootstrap`]({{< relref "reference/cli" >}})
command, or authored by hand.

## Example

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

## Fields

| Field | Description |
|---|---|
| `MachineName` | *(optional)* Name of the Kubernetes `Machine` and node. When omitted, the agent resolves it at startup from the `AGENT_MACHINE_NAME` environment variable, falling back to the host hostname. |
| `Cluster.CaCertBase64` | Base64-encoded cluster CA certificate. |
| `Cluster.ClusterDNS` | ClusterIP of the kube-dns Service. |
| `Cluster.Version` | Kubernetes version to install (e.g. `1.33.1`). |
| `Kubelet.ApiServer` | Address of the Kubernetes API server. |
| `Kubelet.BootstrapToken` | Token used for TLS bootstrapping (omit when using TPM attestation). |
| `Kubelet.Labels` | Key-value labels applied to the Node on registration. |
| `Kubelet.RegisterWithTaints` | Taints applied to the Node on registration (`key=value:effect`). |
| `OCIImage` | *(optional)* OCI image reference for the rootfs. Falls back to debootstrap when empty. |
| `Downloads` | *(optional)* Rootfs binary download overrides for Kubernetes binaries, containerd, runc, CNI plugins, and crictl. |
| `PackageSources.APT.MirrorURL` | *(optional)* Ubuntu apt mirror URL used by debootstrap fallback and the rootfs `sources.list`. |
| `Attest.URL` | *(optional)* Base URL of a metalman serve-pxe instance for TPM attestation. |
