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

For Gantry on AKS Flex nodes, deployment and registry authentication are separate.
The cluster creation script installs Gantry without ACR configuration. The
intended attachment workflow requires a compatible plugin/operator release
containing `gantry attach-acr`; the `v0.8.0` deployment baseline does not include
it, and the managed path has not been live-validated:

```bash
# Registry-only attachment; retain customer-owned authentication.
kubectl unbounded gantry attach-acr \
  --acr-resource-id "$ACR_ID"

# Explicit opt-in to shared managed authentication.
kubectl unbounded gantry attach-acr \
  --acr-resource-id "$ACR_ID" --auth-mode aks-identity-binding \
  --cluster-resource-id "$AKS_ID"
```

Both IDs are full Azure resource IDs; `--kubeconfig /path/to/kubeconfig` is
optional. `--auth-mode` defaults to `external`, preserving the existing provider,
authentication policy, and shared credentials without any additional flags.
Use `--auth-mode aks-identity-binding` only to explicitly enable managed authentication.
Neither mode silently replaces customer-owned authentication.

Managed mode uses the stock `acr-credential-provider` in AKS identity-bindings
mode for both Azure and non-Azure Linux Flex nodes. Collective RBAC and kubelet
Pod-bound ServiceAccount tokens authorize a shared pull identity, without changes
to application Pods or ServiceAccounts. There is no custom broker, adapter, or
token-exchange service, no VM/VMSS pull-identity assignments, and no
Azure-privileged ongoing operator. Native AKS nodes are unaffected. Future joined
nodes are configured only when an explicit managed-auth opt-in policy exists.

This requires supported AKS identity bindings, kubelets >= 1.34 with
`KubeletServiceAccountTokenForCredentialProviders` enabled, and a reachable AKS
identity-binding proxy. API-server VNet integration is unsupported under the
[current AKS documentation](https://learn.microsoft.com/en-us/azure/aks/identity-bindings#limitations).
See [ACR attachment and explicit authentication](../../guides/gantry.md#acr-attachment-and-explicit-authentication-on-flex-nodes)
for the intended workflow and limitations.

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
| `Kubelet.Configuration` | *(optional)* JSON object merged over the generated `kubelet.config.k8s.io/v1beta1` `KubeletConfiguration`. Use kubelet configuration field names such as `maxPods`, `podPidsLimit`, and `imageGCHighThresholdPercent`. Setting `apiVersion`, `kind`, `authentication`, `authorization.mode`, `clusterDNS`, `containerRuntimeEndpoint`, `registerWithTaints`, or `rotateCertificates` here is not supported because the agent configures those fields. |
| `Kubelet.ImageCredentialProvider.ConfigPath` | *(optional)* Absolute path inside the nspawn machine to a kubelet exec image credential provider configuration file or supported configuration directory. Must be set together with `BinDir`. |
| `Kubelet.ImageCredentialProvider.BinDir` | *(optional)* Absolute path inside the nspawn machine containing exec image credential provider binaries. The files must be included in the OCI rootfs or exposed through an additional host mount. |
| `OCIImage` | *(optional)* OCI registry reference, `oci-layout://` directory, or HTTPS URL to a tarred OCI image layout. Uses the built-in default image when empty. The agent automatically selects the archive's single tagged image reference. HTTPS URLs may include signed query strings such as Azure Blob SAS parameters. |
| `OfflineArtifacts.Source` | *(optional)* Complete bootstrap artifact source. Accepts an absolute directory, `file://` directory, `oci://` artifact reference, or HTTPS tar/tar.gz archive. HTTPS archives are downloaded and extracted into the host artifact cache, and their URLs may include signed query strings such as Azure Blob SAS parameters. |
| `LocalDNS.Enabled` | *(optional)* Runs a CoreDNS cache inside the nspawn machine and configures machine and ClusterFirst DNS through separate link-local listeners. |
| `LocalDNS.NodeListenerIP` | *(optional)* Listener used by machine services and Default-policy pods. Defaults to `169.254.10.10`. |
| `LocalDNS.ClusterListenerIP` | *(optional)* Listener supplied to kubelet for ClusterFirst pods. Defaults to `169.254.10.11`. |
| `LocalDNS.MetricsAddress` | *(optional)* Native CoreDNS Prometheus bind address. Defaults to port `9253` on the IPv4 node address selected by following kubelet's non-cloud behavior: `Kubelet.NodeIP`, an IP-valued node name, a host-local node-name DNS result, then the host's default-route interface. |
| `LocalDNS.CPULimitInMilliCores` | *(optional)* CoreDNS slice CPU quota. Defaults to `2000`. |
| `LocalDNS.MemoryLimitInMB` | *(optional)* CoreDNS slice memory limit. Defaults to `128`. |
| `LocalDNS.RequiredPlugins` | *(optional)* Additional plugins that the selected binary must report through `coredns -plugins`. |
| `LocalDNS.CorefileTemplate` | *(optional)* Full replacement Go template for the Corefile. Empty uses the built-in two-listener template. |
| `Downloads.CoreDNS` | *(optional)* CoreDNS URL, BaseURL, and version override. The offline manifest `versions.coredns` takes precedence when offline artifacts are configured. |
| `Attest.URL` | *(optional)* Base URL of a metalman serve-pxe instance for TPM attestation. |

LocalDNS requires direct IPv4 nameservers in `/etc/resolv.conf`, or a
`systemd-resolved` stub backed by direct nameservers in
`/run/systemd/resolve/resolv.conf`. Other local caching stubs and split-DNS
routing policies are not supported when LocalDNS is enabled.
