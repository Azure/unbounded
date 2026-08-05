---
title: "CLI Reference"
weight: 3
description: "Complete reference for the kubectl-unbounded plugin commands."
---

## Overview

`kubectl-unbounded` is a kubectl plugin that extends `kubectl` with commands for
managing Unbounded sites. Once installed, commands are available as:

```bash
kubectl unbounded <command>
```

The plugin binary can also be invoked directly as `kubectl-unbounded`.

## Installation

Download the plugin binary from the
[GitHub releases](https://github.com/Azure/unbounded/releases)
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

Manage Unbounded sites.

This is a command group with no action of its own. Use one of the subcommands
below.

---

### `kubectl unbounded install`

Bootstrap a cluster with the Unbounded CRDs and `unbounded-operator`. This
command applies the operator manifests and waits for the operator to roll out.
The operator itself installs and upgrades the machina and net CRDs at startup, so
`install` no longer applies CRDs directly. It does not deploy component workloads
directly; the operator reconciles those from `Site.spec.components` after Sites
are created or updated.

```bash
kubectl unbounded install
```

Optional flags:

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--kubeconfig` | `string` | `$KUBECONFIG` or default | Path to kubeconfig file |
| `--namespace` | `string` | `unbounded-system` | Namespace for the operator and default components |
| `--wait` | `bool` | `true` | Wait for the operator rollout and CRD establishment |
| `--timeout` | `duration` | `5m0s` | Timeout for rollout waits |
| `--api-server-endpoint` | `string` | auto-discovered | Override the API server endpoint advertised to provisioned machines; by default the operator discovers it from `kube-public/cluster-info`, or the `KUBERNETES_SERVICE_HOST` FQDN on clusters (e.g. AKS) that do not publish cluster-info |
| `--image-registry` | `string` | `ghcr.io` | Registry prefix used by the operator for version-matched first-party component images |

> **Breaking change:** the `--skip-crds` flag has been removed. CRDs are now owned
> and installed by the operator at startup (`operator.BootstrapCRDs`), so there is
> nothing for `install` to skip. Automation passing `--skip-crds` must drop it.

`--operator-image` overrides the operator image. `--image-registry` configures
the registry for components; their image tag always matches the operator's
compiled version.

---

### `kubectl unbounded site init`

Initialize a new Unbounded site. This command:

1. Validates inputs and kubeconfig access.
2. Verifies `unbounded-operator` is installed: the required CRDs
   (`sites.unbounded-cloud.io`, `gatewaypools.net.unbounded-cloud.io`,
   `sitegatewaypoolassignments.net.unbounded-cloud.io`) must be established.
3. Creates a cluster `Site`, a remote `Site`, and related net `GatewayPool` resources.
4. Records component choices in `Site.spec.components` for `unbounded-operator`.
5. Creates a bootstrap token for the remote site.

> **Prerequisite:** `site init` no longer bootstraps `unbounded-operator`. Run
> [`kubectl unbounded install`](#kubectl-unbounded-install) first. If the operator
> CRDs are not present, `site init` fails with guidance to run `install`. If the
> CRDs exist but the operator Deployment is missing or not yet rolled out,
> `site init` warns and proceeds (the operator reconciles the Site once ready).

Global components (`unbounded-net`, `machina`, and `unbounded-storage`) are
enabled on the cluster Site. `metalman` is per-site and is enabled on the remote
Site when `--enable-metalman` is set.

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
| `--kubeconfig` | `string` | `$KUBECONFIG` or default | Path to kubeconfig file |
| `--manage-cni-plugin` | `bool` | `true` | Whether unbounded-net manages the CNI plugin |
| `--enable-machina` | `bool` | `true` | Enable machina in `Site.spec.components` |
| `--enable-metalman` | `bool` | `false` | Enable metalman in `Site.spec.components` |
| `--enable-storage` | `bool` | `false` | Enable unbounded-storage in `Site.spec.components` |

> **Breaking change:** the `--skip-install` and `--install-timeout` flags have
> been removed. `site init` no longer installs the operator, so there is nothing
> to skip or time out. Run `kubectl unbounded install` first. Automation passing
> `--skip-install` must drop it.

#### Validation

- All CIDR values must be valid IPv4 CIDR notation.
- The kubeconfig must be readable.
- `unbounded-operator` must be installed (its CRDs must be established).

#### Example

```bash
kubectl unbounded install

kubectl unbounded site init \
  --name my-edge-site \
  --cluster-node-cidr 10.224.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.100.0.0/24 \
  --pod-cidr 10.101.0.0/24
```

With optional components:

```bash
kubectl unbounded site init \
  --name dc2 \
  --cluster-node-cidr 10.224.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.200.0.0/24 \
  --pod-cidr 10.201.0.0/24 \
  --enable-metalman \
  --enable-storage \
  --kubeconfig ~/.kube/config
```

---

### `kubectl unbounded machine`

Manage Unbounded machines.

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

### `kubectl unbounded machine operation`

Create and watch `MachineOperation` resources.

This command group is the resource-oriented surface for day-2 machine
operations. It creates the same `MachineOperation` CRD you can manage directly
with `kubectl get mop`, `kubectl describe mop`, and `kubectl delete mop`.

---

### `kubectl unbounded machine operation create`

Create a `MachineOperation`.

#### Required Arguments and Flags

| Name | Type | Description |
|------|------|-------------|
| `NAME` | argument | Name of the `MachineOperation` resource |
| `--kind` | string | Operation kind: `NodeReboot`, `AgentUpgrade`, `AgentReset`, `HostReboot`, `HostPowerOff`, `HostPowerOn`, or `HostReplace` |
| `--machine` or `--selector` | string | Target one Machine by name or select Machines by label selector |

`AgentUpgrade` also requires `--param downloadURL=<url>`.

#### Optional Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--param` | `key=value` | none | Operation parameter. Repeat for multiple parameters. |
| `--ttl` | `int32` | unset | Seconds after completion before cleanup. `0` keeps the operation indefinitely. |
| `--wait` | `bool` | `false` | Wait until the operation reaches `Complete` or `Failed`. |
| `--timeout` | duration | `0` | Client-side wait timeout. `0` waits indefinitely. |
| `--dry-run` | string | `none` | `none`, `client`, or `server`. |
| `-o`, `--output` | string | `name` | `name`, `yaml`, or `json`. |
| `--field-manager` | string | `kubectl-unbounded` | Field manager used on create. |
| `--kubeconfig` | string | `$KUBECONFIG` or default | Path to kubeconfig file. |

`--wait` streams progress output and can only be used with the default
`-o name` output.

#### Examples

Create a host reboot operation:

```bash
kubectl unbounded machine operation create reboot-worker-01 \
  --kind HostReboot \
  --machine worker-01
```

Create and wait for a node reboot operation selected by labels:

```bash
kubectl unbounded machine operation create reboot-gpu \
  --kind NodeReboot \
  --selector role=gpu \
  --wait
```

Generate YAML without creating the operation:

```bash
kubectl unbounded machine operation create poweroff-worker-01 \
  --kind HostPowerOff \
  --machine worker-01 \
  --dry-run=client \
  -o yaml
```

Upgrade the agent:

```bash
kubectl unbounded machine operation create upgrade-worker-01 \
  --kind AgentUpgrade \
  --machine worker-01 \
  --param downloadURL=https://example.com/unbounded-agent-linux-amd64.tar.gz
```

Selector support is implemented at the CRD level. Agent operations support
selectors. Metalman bare-metal host operations support selectors when the
selector includes `unbounded-cloud.io/site=<site>`. Cloud VM host operations
currently require one operation per Machine.

---

### `kubectl unbounded machine operation wait`

Wait for an existing `MachineOperation` to reach `Complete` or `Failed`.

```bash
kubectl unbounded machine operation wait reboot-worker-01 --timeout 5m
```

Use native kubectl commands for listing and inspection:

```bash
kubectl get mop
kubectl describe mop reboot-worker-01
kubectl get mop reboot-worker-01 -o yaml
```

---

### Machine Operation Convenience Commands

Convenience commands create a `MachineOperation` with a generated operation
name, default `--ttl 300`, and `--wait=true`.

| Command | Operation kind | Notes |
|---------|----------------|-------|
| `kubectl unbounded machine node-reboot NAME` | `NodeReboot` | Restarts the nspawn-backed Kubernetes node. |
| `kubectl unbounded machine host-reboot NAME` | `HostReboot` | Reboots or power-cycles the host through the owning backend. |
| `kubectl unbounded machine power-off NAME` | `HostPowerOff` | Powers off the host. |
| `kubectl unbounded machine power-on NAME` | `HostPowerOn` | Powers on the host. |
| `kubectl unbounded machine agent-upgrade NAME --download-url URL` | `AgentUpgrade` | Upgrades the host-side agent binary. |
| `kubectl unbounded machine agent-reset NAME --force` | `AgentReset` | Removes the agent and managed resources from the host. Requires confirmation unless `--force` is set. |
| `kubectl unbounded machine replace NAME --force` | `HostReplace` | Destructively replaces the host. Requires confirmation unless `--force` is set. |

Common flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--operation-name` | generated | Explicit `MachineOperation` name. |
| `--ttl` | `300` | Seconds after completion before cleanup. `0` keeps indefinitely. |
| `--wait` | `true` | Wait for terminal operation phase. |
| `--timeout` | `0` | Client-side wait timeout. |
| `--kubeconfig` | `$KUBECONFIG` or default | Path to kubeconfig file. |

---

### Legacy Machine Operation Commands

The older counter-backed commands remain available for compatibility:

| Command | Mechanism |
|---------|-----------|
| `kubectl unbounded machine reboot NAME` | Patches `Machine.spec.operations.rebootCounter`. |
| `kubectl unbounded machine repave NAME` | Patches `Machine.spec.operations.repaveCounter` and `rebootCounter`. |

These commands do not create `MachineOperation` resources.

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
