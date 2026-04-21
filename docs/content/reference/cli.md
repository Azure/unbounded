---
title: "CLI Reference"
weight: 3
description: "Complete reference for the kubectl-unbounded plugin commands."
---

## Overview

`kubectl-unbounded` is a kubectl plugin that extends `kubectl` with commands for
managing Unbounded Kube sites. Once installed, commands are available as:

```bash
kubectl unbounded <command>
```

The plugin binary can also be invoked directly as `kubectl-unbounded`.

## Installation

Download the plugin binary from the
[GitHub releases](https://github.com/Azure/unbounded-kube/releases)
page and place it on your `PATH`. kubectl automatically discovers plugins named
`kubectl-<name>`.

You can also install via [Krew](https://krew.sigs.k8s.io/):

```bash
kubectl krew install unbounded
```

## Global Behavior

All commands that interact with the cluster accept a `--kubeconfig` flag. If
omitted, the plugin falls back to the `KUBECONFIG` environment variable. If
neither is set, the default kubeconfig location (`~/.kube/config`) is used.

---

## Commands

### `kubectl unbounded site`

Manage unbounded-kube sites.

This is a command group with no action of its own. Use one of the subcommands
below.

---

### `kubectl unbounded site init`

Initialize a new unbounded-kube site. This command:

1. Validates inputs and checks that `kubectl` is on your PATH.
2. Verifies that at least one node is labeled as a gateway
   (`unbounded-kube.io/unbounded-net-gateway=true`).
3. Installs the unbounded-net CNI plugin (downloads from the release URL or
   uses local manifests).
4. Creates unbounded-net Site and GatewayPool resources.
5. Creates a bootstrap token for the site.
6. Installs the machina controller in the `unbounded-kube` namespace.

#### Required Flags

| Flag | Type | Description |
|------|------|-------------|
| `--name` | `string` | Name of the site |
| `--cluster-node-cidr` | `string` | Node CIDR of the control-plane cluster (e.g. `10.224.0.0/16`) |
| `--cluster-pod-cidr` | `string` | Pod CIDR of the control-plane cluster (e.g. `10.244.0.0/16`) |
| `--node-cidr` | `string` | Node CIDR for the new site (e.g. `10.100.0.0/24`) |
| `--pod-cidr` | `string` | Pod CIDR for the new site (e.g. `10.101.0.0/24`) |

#### Optional Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--cni-manifests` | `string` | *(embedded manifests)* | Path to a local file/directory or HTTPS URL for CNI manifests |
| `--machina-manifests` | `string` | *(embedded manifests)* | Path to a local file/directory or HTTPS URL for machina manifests |
| `--kubeconfig` | `string` | `$KUBECONFIG` or default | Path to kubeconfig file |

#### Validation

- All CIDR values must be valid IPv4 CIDR notation.
- `--cni-manifests`, when provided, must be either a valid HTTPS URL or an
  existing local file/directory path. If omitted, the manifests embedded in
  the kubectl plugin are used.
- `kubectl` must be available on `PATH`.

#### Example

```bash
kubectl unbounded site init \
  --name my-edge-site \
  --cluster-node-cidr 10.224.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.100.0.0/24 \
  --pod-cidr 10.101.0.0/24
```

With local manifests:

```bash
kubectl unbounded site init \
  --name dc2 \
  --cluster-node-cidr 10.224.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.200.0.0/24 \
  --pod-cidr 10.201.0.0/24 \
  --cni-manifests ./my-cni-manifests/ \
  --kubeconfig ~/.kube/config
```

---

### `kubectl unbounded machine`

Manage unbounded-kube machines.

This is a command group with no action of its own. Use one of the subcommands
below.

---

### `kubectl unbounded machine register`

Register a machine to an existing site. This command:

1. Creates a Kubernetes Secret containing the SSH private key.
2. Optionally creates a separate Secret for bastion SSH credentials.
3. Resolves the bootstrap token created by `site init`.
4. Detects the cluster's Kubernetes version.
5. Creates a `Machine` custom resource with SSH and Kubernetes configuration.

#### Required Flags

| Flag | Type | Description |
|------|------|-------------|
| `--site` | `string` | Name of the site (must match a site created by `site init`) |
| `--host` | `string` | Host address of the machine, optionally with port (e.g. `10.0.0.5` or `10.0.0.5:2222`) |
| `--ssh-username` | `string` | SSH username for connecting to the machine |

#### Optional Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--name` | `string` | *derived from `--host`* | Name for the machine (final name is `{site}-{name}`) |
| `--ssh-private-key` | `string` | *(none)* | Path to SSH private key file. **Required** when no bastion flags are set. |
| `--ssh-secret-name` | `string` | `ssh-{site}` | Name of the Kubernetes Secret for SSH credentials |
| `--bastion-host` | `string` | *(none)* | Host address of the bastion, optionally with port |
| `--bastion-ssh-username` | `string` | value of `--ssh-username` | SSH username for the bastion |
| `--bastion-ssh-private-key` | `string` | value of `--ssh-private-key` | Path to SSH private key for the bastion |
| `--bastion-ssh-secret-name` | `string` | value of `--ssh-secret-name` | Name of the Kubernetes Secret for bastion credentials |
| `--kubeconfig` | `string` | `$KUBECONFIG` or default | Path to kubeconfig file |

#### Default Derivation

- **Machine name:** If `--name` is omitted, it is derived from `--host` by
  replacing dots and colons with hyphens, then prefixed with the site name.
  For example, `--site dc1 --host 10.0.0.5:2222` produces the machine name
  `dc1-10-0-0-5-2222`.
- **Bastion credentials:** Bastion SSH username, key, and secret name all
  default to the corresponding non-bastion values.

#### Validation

- `--site`, `--host`, and `--ssh-username` must be non-empty.
- `--ssh-private-key` is required when no bastion flags are specified.
- Key files (both `--ssh-private-key` and `--bastion-ssh-private-key`) must be
  readable if provided.

#### Examples

**Direct SSH:**

```bash
kubectl unbounded machine register \
  --site dc1 \
  --host 10.0.0.5 \
  --ssh-username admin \
  --ssh-private-key ~/.ssh/id_ed25519
```

**With explicit machine name:**

```bash
kubectl unbounded machine register \
  --site dc1 \
  --name worker-1 \
  --host 10.0.0.5 \
  --ssh-username admin \
  --ssh-private-key ~/.ssh/id_ed25519
```

**With bastion (shared credentials):**

```bash
kubectl unbounded machine register \
  --site dc1 \
  --host 10.0.0.5:2222 \
  --ssh-username admin \
  --ssh-private-key ~/.ssh/id_ed25519 \
  --bastion-host 5.6.7.8
```

**With bastion (separate credentials):**

```bash
kubectl unbounded machine register \
  --site dc1 \
  --host 10.0.0.5 \
  --ssh-username admin \
  --ssh-private-key ~/.ssh/host_key \
  --bastion-host 5.6.7.8 \
  --bastion-ssh-username bastion-user \
  --bastion-ssh-private-key ~/.ssh/bastion_key \
  --bastion-ssh-secret-name bastion-ssh
```

---

## Environment Variables

| Variable | Description |
|----------|-------------|
| `KUBECONFIG` | Fallback kubeconfig path when `--kubeconfig` is not provided |

## See Also

- **[Getting Started]({{< relref "guides/getting-started" >}})** -- Walks
  through `site init` and `machine register` step by step.
- **[SSH Guide]({{< relref "guides/ssh" >}})** -- Detailed SSH provisioning
  walkthrough with examples.
- **[CRD Reference]({{< relref "reference/machina-crd" >}})** -- Full Machine
  and Image API specification.
