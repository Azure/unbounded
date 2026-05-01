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
| `MachineName` | Name of the nspawn machine and the Kubernetes node. |
| `Cluster.CaCertBase64` | Base64-encoded cluster CA certificate. |
| `Cluster.ClusterDNS` | ClusterIP of the kube-dns Service. |
| `Cluster.Version` | Kubernetes version to install (e.g. `1.33.1`). |
| `Kubelet.ApiServer` | Address of the Kubernetes API server. |
| `Kubelet.BootstrapToken` | Token used for TLS bootstrapping (omit when using TPM attestation). |
| `Kubelet.Labels` | Key-value labels applied to the Node on registration. |
| `Kubelet.RegisterWithTaints` | Taints applied to the Node on registration (`key=value:effect`). |
| `OCIImage` | *(optional)* OCI image reference for the rootfs. Falls back to debootstrap when empty. |
| `Attest.URL` | *(optional)* Base URL of a metalman serve-pxe instance for TPM attestation. |
