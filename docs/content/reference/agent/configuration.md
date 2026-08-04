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

The same config is used by `unbounded-agent start` and
`unbounded-agent preflight`. Before bootstrap, run preflight on the target host
to validate the loaded config, host prerequisites, API server reachability,
artifact sources, nspawn provisioning paths, and GPU driver readiness when
applicable. See [Agent Preflight]({{< relref "reference/agent/preflight" >}})
for command usage, exit behavior, and the current check list.

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
    "Auth": {
      "BootstrapToken": "abc123.0123456789abcdef"
    },
    "Labels": {
      "unbounded-cloud.io/site": "mysite"
    },
    "RegisterWithTaints": [],
    "Configuration": {
      "maxPods": 250,
      "imageGCHighThresholdPercent": 85,
      "imageGCLowThresholdPercent": 80
    },
    "ImageCredentialProvider": {
      "ConfigPath": "/etc/kubernetes/credential-provider.yaml",
      "BinDir": "/usr/local/lib/kubelet-credential-providers"
    }
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
| `Kubelet.Auth.BootstrapToken` | Token used for TLS bootstrapping (omit when using TPM attestation). |
| `Kubelet.Labels` | Key-value labels applied to the Node on registration. |
| `Kubelet.RegisterWithTaints` | Taints applied to the Node on registration (`key=value:effect`). |
| `Kubelet.Configuration` | *(optional)* JSON object merged over the generated `kubelet.config.k8s.io/v1beta1` `KubeletConfiguration`. Use kubelet configuration field names such as `maxPods`, `podPidsLimit`, and `imageGCHighThresholdPercent`. Setting `apiVersion`, `kind`, `authentication`, `clusterDNS`, `containerRuntimeEndpoint`, `registerWithTaints`, or `rotateCertificates` here is not supported because the agent configures those fields. |
| `Kubelet.ImageCredentialProvider.ConfigPath` | *(optional)* Absolute path inside the nspawn machine to a kubelet exec image credential provider configuration file or supported configuration directory. Must be set together with `BinDir`. |
| `Kubelet.ImageCredentialProvider.BinDir` | *(optional)* Absolute path inside the nspawn machine containing exec image credential provider binaries. The files must be included in the OCI rootfs or exposed through an additional host mount. |
| `OCIImage` | *(optional)* OCI registry reference, `oci-layout://` directory, or HTTPS URL to a tarred OCI image layout. Uses the built-in default image when empty. The agent automatically selects the archive's single tagged image reference. HTTPS URLs may include signed query strings such as Azure Blob SAS parameters. |
| `OfflineArtifacts.Source` | *(optional)* Complete bootstrap artifact source. Accepts an absolute directory, `file://` directory, `oci://` artifact reference, or HTTPS tar/tar.gz archive. HTTPS archives are downloaded and extracted into the host artifact cache, and their URLs may include signed query strings such as Azure Blob SAS parameters. |
| `LocalDNS.Enabled` | *(optional)* Runs a CoreDNS cache inside the nspawn machine and configures machine and ClusterFirst DNS through separate link-local listeners. |
| `LocalDNS.NodeListenerIP` | *(optional)* Listener used by machine services and Default-policy pods. Defaults to `169.254.10.10`. |
| `LocalDNS.ClusterListenerIP` | *(optional)* Listener supplied to kubelet for ClusterFirst pods. Defaults to `169.254.10.11`. |
| `LocalDNS.MetricsAddress` | *(optional)* Native CoreDNS Prometheus bind address. Defaults to the configured IPv4 `Kubelet.NodeIP` on port `9253`; required when no IPv4 node IP is configured. |
| `LocalDNS.CPULimitInMilliCores` | *(optional)* CoreDNS slice CPU quota. Defaults to `2000`. |
| `LocalDNS.MemoryLimitInMB` | *(optional)* CoreDNS slice memory limit. Defaults to `128`. |
| `LocalDNS.RequiredPlugins` | *(optional)* Additional plugins that the selected binary must report through `coredns -plugins`. |
| `LocalDNS.CorefileTemplate` | *(optional)* Full replacement Go template for the Corefile. Empty uses the built-in two-listener template. |
| `Downloads.CoreDNS` | *(optional)* CoreDNS URL, BaseURL, and version override. The offline manifest `versions.coredns` takes precedence when offline artifacts are configured. |
| `Attest.URL` | *(optional)* Base URL of a metalman serve-pxe instance for TPM attestation. |
