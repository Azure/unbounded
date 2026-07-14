# Gantry deployment artifacts

This directory contains the Kubernetes manifests and node configuration template
used to deploy Gantry.

For prerequisites, installation, private-registry authentication, verification,
production hardening, upgrades, and troubleshooting, use the canonical
[Distribute Container Images with Gantry](../../docs/content/guides/gantry.md)
guide.

## Files

| Path | Purpose |
| --- | --- |
| `serviceaccount.yaml` | Namespace, ServiceAccount, RBAC, and PriorityClass. |
| `configmap.yaml` | Gantry runtime configuration and documented defaults. |
| `daemonset.yaml` | One Gantry agent per Kubernetes node. |
| `node-config.yaml` | Standalone node configurator for containerd's default Gantry mirror. |
| `hosts.toml.template` | Per-registry containerd mirror configuration. |
| `examples/networkpolicy.yaml` | Production hardening template; edit all site-specific CIDRs before applying. |
| `examples/registry-secret.example.yaml` | Optional shared-registry-identity Secret template. |

## Development

Build the Gantry image locally:

```bash
make image-gantry-local
```

Build and push it to a registry:

```bash
make image-gantry-push \
    CONTAINER_REGISTRY=ghcr.io/your-org \
    VERSION=<version>
```

The image is built from `images/gantry/Containerfile`. Contributor architecture
and protocol details are documented in
[`designs/gantry-detailed-design.md`](../../designs/gantry-detailed-design.md),
and the end-to-end test workflow is documented in
[`e2e/gantry/README.md`](../../e2e/gantry/README.md).
