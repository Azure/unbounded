---
title: "CLI Reference"
weight: 3
description: "Install the kubectl unbounded plugin and use its commands for sites, machines, networking, overrides, and day-2 operations."
---

The Unbounded command line interface is the `kubectl unbounded` plugin. It is the
same program as the standalone `kubectl-unbounded` binary. Examples use the
plugin form; when you invoke the binary directly, replace `kubectl unbounded`
with `kubectl-unbounded`. The plugin groups commands by object family: `site` for
network sites, `machine` for hosts and their operations, `net` for the networking
data plane, `overrides` for component workload patches, and a small set of
top-level commands for installation and version reporting.

The mental model is that the CLI is a thin client. It creates and reads
Kubernetes resources, and controllers do the work asynchronously. That is why
several commands return as soon as a resource is accepted, while others offer a
`--wait` flag that streams status until an operation reaches a terminal phase.
Understanding which commands wait, and which only submit work, prevents most
confusion when you automate the CLI.

## Installing the plugin

The plugin is discovered by `kubectl` through the `kubectl-` prefix on your
`PATH`. A file named `kubectl-unbounded` makes the command `kubectl unbounded`
available. No separate registration step is required.

### Release binaries

Each release publishes archives named after the platform, for example
`kubectl-unbounded-linux-amd64.tar.gz` and
`kubectl-unbounded-darwin-arm64.tar.gz`. Download the archive for your platform
from the GitHub release, verify it against `checksums.txt`, extract it, and put
the binary somewhere on your `PATH`:

```bash
curl -LO https://github.com/Azure/unbounded/releases/download/<version>/kubectl-unbounded-linux-amd64.tar.gz
curl -LO https://github.com/Azure/unbounded/releases/download/<version>/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf kubectl-unbounded-linux-amd64.tar.gz
sudo install -m 0755 kubectl-unbounded /usr/local/bin/kubectl-unbounded
```

Replace `<version>` with the release tag you are installing. The archive
contains the binary plus the project license and notice files. Linux and macOS
builds are published for both amd64 and arm64. The plugin has no runtime
dependency on a separate Unbounded installation and talks directly to the
cluster selected by your kubeconfig.

### Krew

The release also attaches a rendered Krew manifest named `unbounded.yaml`. Krew
can install a plugin from a manifest file, which installs directly from the
release asset without requiring a custom index:

```bash
kubectl krew install --manifest=./unbounded.yaml
```

Download `unbounded.yaml` from the same release as the binary. The manifest
declares the plugin name `unbounded`, selects the archive and checksum for each
supported platform, and installs the `kubectl-unbounded` binary. After Krew
finishes, `kubectl unbounded version` should print the installed version.

### Confirm the installation

```bash
kubectl unbounded version
```

The version command prints a single line in the form
`<version> (commit: <sha>, built: <timestamp>)`. Locally built binaries report
`dev` and `unknown` because those values are injected at build time. The `net`
group also exposes `kubectl unbounded net version` with the same output.

## Command model and global flags

The root command has no persistent flags of its own. Every command declares the
flags it accepts, which is why `--kubeconfig` appears on some commands and not
others. The `net` group is the exception: it installs a kubectl-compatible set
of persistent connection flags and hides them from normal help output. Run
`kubectl unbounded net options` to print that inherited set.

The top-level command tree is intentionally small:

| Command | Purpose |
| --- | --- |
| `install` | Apply the operator manifests and wait for the operator and CRDs. |
| `site init` | Create the cluster and remote Sites, networking objects, and bootstrap token. |
| `machine` | Register hosts, generate bootstrap payloads, manage configurations, and run day-2 operations. |
| `net` | Create networking resources, inspect nodes, read controller status, and manage plugin symlinks. |
| `overrides` | Validate, list, and report the state of component workload overrides. |
| `gantry` | Attach an Azure Container Registry to Gantry. |
| `version` | Print build metadata. |

Several groups and commands have aliases. They are equivalent to the primary
spelling:

| Alias | Primary command |
| --- | --- |
| `machine operations` | `machine operation` |
| `net nodes` | `net node` |
| `net controllers` | `net controller` |
| `net symlinks` | `net symlink` |
| `net create st` | `net create site` |
| `net create gp` | `net create gatewaypool` |
| `net create spr` | `net create sitepeering` |
| `net create sgpa` | `net create sitegatewaypoolassignment` |
| `net create gpp` | `net create gatewaypoolpeering` |

### Kubeconfig resolution

Most commands that accept `--kubeconfig` use the same precedence: the explicit
flag wins, then the `KUBECONFIG` environment variable, then
`~/.kube/config`. This applies to `install`, `site init`, `machine register`,
`machine manual-bootstrap`, the `machine operation` commands, `machine replace`,
the machine operation shortcuts, `overrides`, and `gantry attach-acr`.

Two families behave differently:

- The `machine config` commands and the older `machine reboot`,
  `machine hard-reboot`, `machine soft-reboot`, and `machine repave` commands do
  not accept `--kubeconfig`. They load the ambient controller-runtime
  configuration, which means `KUBECONFIG` or the default kubeconfig location.
- The `net` group uses kubectl's config flags. It accepts `--kubeconfig`,
  `--context`, `--namespace`, `--server`, `--token`, and the rest of the standard
  kubectl connection flags, but hides them from per-command help. Use
  `kubectl unbounded net options` when you need to discover them.

If you work with several clusters, prefer an explicit `--kubeconfig` on the
commands that support it, or set `KUBECONFIG` for the whole shell session. The
ambient-only commands have no flag for choosing a different file, so setting
`KUBECONFIG` in the environment is the only per-invocation option.

### Namespace resolution

Commands that operate on Unbounded components default to the `unbounded-system`
namespace. The Go-side helper honors `POD_NAMESPACE` when it is set, which is how
in-cluster components follow their deployment namespace, but client-side tools
normally see `unbounded-system`. `install`, `site init`, `machine register`,
`gantry attach-acr`, and the `overrides` commands expose `--namespace` where the
namespace is part of the request.

The `net` group has a more forgiving rule. If you pass `--namespace`, it uses
that value directly. Otherwise it tries the current context namespace, then
`unbounded-system`, then the legacy `unbounded-net` and `kube-system`
namespaces, and selects the first one that contains Unbounded networking pods.
This lets the plugin work against clusters installed before the namespace
consolidation without extra flags.

### Output conventions

Commands that create resources use a Kubernetes-style `name` output by default.
For example, `net create site` prints `sites/<name> created`, and
`machine operation create` prints `machineoperations/<name> created`. Commands
that support `-o yaml` or `-o json` print the submitted object rendered by the
standard Kubernetes printers.

Long-running commands print progress as indented steps with an arrow marker and
finish with a `ready` line. These lines are written to standard output. Warnings,
including node status warnings from `net node list`, go to standard error so they
do not corrupt machine-readable output.

## Bootstrapping the cluster with `install`

`kubectl unbounded install` bootstraps the cluster so the Unbounded CRDs and the
`unbounded-operator` become available. The operator then reconciles
`Site.spec.components`, which is how component workloads such as unbounded-net,
machina, and metalman are deployed. The command does not deploy those workloads
directly. It applies the operator manifests and, with the default `--wait`, waits
for the operator rollout and for the CRDs that the operator establishes at
startup.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig` | empty | Path to the kubeconfig file. Falls back to `KUBECONFIG` and then `~/.kube/config`. |
| `--namespace` | `unbounded-system` | Namespace for the operator and default components. |
| `--operator-image` | empty | Operator image name and tag without a registry, for example `unbounded-operator:v1`. |
| `--image-registry` | build-time registry | Full image repository prefix for all Unbounded images. |
| `--api-server-endpoint` | empty | API server endpoint advertised to provisioned machines. |
| `--wait` | `true` | Wait for the operator rollout and for the required CRDs to be established. |
| `--timeout` | `5m` | Timeout for those rollout and CRD waits. |

Run it before any other command that expects the Site API to be served:

```bash
kubectl unbounded install --kubeconfig ~/.kube/config
```

The operator discovers the API server endpoint from `kube-public/cluster-info`
at runtime. On clusters that do not publish `cluster-info`, such as some AKS
clusters, it falls back to the `KUBERNETES_SERVICE_HOST` FQDN. Use
`--api-server-endpoint` only when you need to override that discovery, for
example when machines reach the control plane through a private load balancer.

The registry flag and the operator image stay in lockstep. `--operator-image`
must be a name and tag with no registry component, and the registry always comes
from `--image-registry` or from the value baked into the binary at build time.
Passing `registry.example.com/unbounded-operator:v1` to `--operator-image` is
rejected, because accepting it would let the operator and its components resolve
from different registries.

Two failure modes are worth knowing. `install` refuses the legacy namespaces
`unbounded-kube` and `unbounded-net`, because the operator's migration reaper
drains and deletes those namespaces. Choose a different `--namespace`. Also, a
failed rollout returns a timeout error rather than reporting success against the
old operator. The rollout check requires the observed generation to be current
and every replica to be updated, present, and available, so a crash-looping new
pod is detected.

## Initializing a site with `site init`

A site is a network boundary that owns a set of node and pod CIDRs. `site init`
creates two `Site` resources, the cluster site and the named remote site, plus
the `GatewayPool` and `SiteGatewayPoolAssignment` resources that connect them.
It also creates a bootstrap token for the named site, which later machine
registration and manual bootstrap commands consume.

| Flag | Default | Required | Meaning |
| --- | --- | --- | --- |
| `--name` | empty | yes | Site name. Used as a label on machines and secondary resources. |
| `--cluster-node-cidr` | empty | yes | Node CIDR of the cluster site. Must be a valid IPv4 CIDR. |
| `--cluster-pod-cidr` | empty | yes | Pod CIDR of the cluster site. Must be a valid IPv4 CIDR. |
| `--node-cidr` | empty | yes | Node CIDR of the remote site. Must be a valid IPv4 CIDR. |
| `--pod-cidr` | empty | yes | Pod CIDR of the remote site. Must be a valid IPv4 CIDR. |
| `--manage-cni-plugin` | `true` | no | Whether unbounded-net manages the CNI plugin. Set `false` for Cilium, Calico, or another existing CNI. |
| `--enable-metalman` | `false` | no | Enable metalman for the remote site. |
| `--kubeconfig` | empty | no | Path to the kubeconfig file. |

A typical invocation looks like this:

```bash
kubectl unbounded site init \
  --name edge-west \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.10.0.0/16 \
  --pod-cidr 10.245.0.0/16
```

`site init` requires the Unbounded API types it applies to be served by the
cluster. It checks API discovery for the kinds declared by its embedded
templates and fails with a message telling you to run
`kubectl unbounded install` when a required type is missing. A missing operator Deployment or an operator that has not finished
rolling out is only a warning, because the operator will reconcile the Site once
it becomes ready.

The command is declarative and safe to rerun. It applies the rendered manifests
with server-side apply and reuses the existing bootstrap token for the site when
one is already present. The cluster site is always named `cluster`; the remote
site uses the value of `--name`. The cluster site receives the gateway pool,
while the remote site is configured with token refresh enabled and optionally
with metalman.

When your cluster already runs a CNI, pass `--manage-cni-plugin=false`. The
Site is then rendered with CNI management disabled so the existing plugin keeps
handling intra-site networking. The pod CIDRs you supply are still used to plan
inter-site routing.

## Registering a machine

`kubectl unbounded machine register` creates the Kubernetes objects machina
needs to provision a host over SSH. When you provide an SSH private key, it
writes that key into a Secret, then resolves the site bootstrap token, reads the
Kubernetes server version, and applies a `Machine` resource. The controller performs the actual installation
after the object exists, so a successful command means the request was accepted,
not that the node has joined.

| Flag | Default | Required | Meaning |
| --- | --- | --- | --- |
| `--site` | empty | yes | Site that contains the machine. |
| `--host` | empty | yes | Host and optional port, for example `10.0.0.5` or `10.0.0.5:2222`. |
| `--ssh-username` | empty | yes | SSH user for the host. |
| `--name` | derived from host | no | Machine name. The site name is always prefixed, producing `<site>-<name>`. |
| `--ssh-private-key` | empty | conditional | Path to the private key. Required unless a bastion host is configured. |
| `--ssh-secret-name` | `ssh-<site>` | no | Secret that stores the host key. |
| `--namespace` | `unbounded-system` | no | Namespace for the Secrets and their Machine references. Must match the machina namespace. |
| `--bastion-host` | empty | no | Bastion host and optional port. |
| `--bastion-ssh-username` | host username | no | SSH user for the bastion. |
| `--bastion-ssh-private-key` | host key | no | Private key for the bastion. |
| `--bastion-ssh-secret-name` | host secret name | no | Secret for the bastion key. |
| `--node-label` | empty | no | `key=value` label for kubelet. Repeatable. |
| `--kubeconfig` | empty | no | Path to the kubeconfig file. |

The agent download can be overridden without changing the rest of the request:

| Flag | Meaning |
| --- | --- |
| `--agent-version` | Pin the unbounded-agent release tag. |
| `--agent-base-url` | Base URL for release downloads, for mirrors. |
| `--agent-url` | Fully qualified tarball URL. Takes precedence over version and base URL. |

Rootfs binaries can also be mirrored or pinned. Each artifact has the same
three-part shape:

| Artifact | Base URL flag | Full URL flag | Version flag |
| --- | --- | --- | --- |
| Kubernetes binaries | `--kubernetes-base-url` | `--kubernetes-url` | `--kubernetes-binary-version` |
| containerd | `--containerd-base-url` | `--containerd-url` | `--containerd-version` |
| runc | `--runc-base-url` | `--runc-url` | `--runc-version` |
| CNI plugins | `--cni-base-url` | `--cni-url` | `--cni-version` |
| cri-tools | `--crictl-base-url` | `--crictl-url` | `--crictl-version` |

A registration without a bastion looks like this:

```bash
kubectl unbounded machine register \
  --site edge-west \
  --host 10.10.0.21 \
  --ssh-username azureuser \
  --ssh-private-key ~/.ssh/id_ed25519 \
  --node-label role=worker
```

The resulting Machine is named `edge-west-10-10-0-21` unless you pass `--name`.
The SSH Secret is named `ssh-edge-west`. The Machine references the site
bootstrap token, so run `site init` first. If the token cannot be found, the
command reports that and tells you to initialize the site.

With a bastion, the bastion username defaults to the host username, the bastion
key defaults to the host key, and the bastion Secret defaults to the host
Secret. Override them only when the two hops use different credentials. The
Machine records a private key reference for the bastion only when the bastion
Secret differs from the host Secret.

## Generating a bootstrap payload

`kubectl unbounded machine manual-bootstrap` produces a self-contained payload
that installs the agent and provisions a host as a worker node. It covers the
cases where machina cannot reach the host over SSH, or where you provision hosts
through cloud-init, Ignition, or an autoscaling group. The output is written to
standard output and is not a Kubernetes object.

The positional `NAME` is optional. When you omit it, the agent resolves the
machine name at startup from `AGENT_MACHINE_NAME`, falling back to the host
hostname. That makes one payload reusable across many instances. The resolved
name is logged by the agent.

| Flag | Default | Required | Meaning |
| --- | --- | --- | --- |
| `--site` | empty | yes | Site whose bootstrap token is embedded and whose name labels the node. |
| `--variant` | `script` | no | Output format: `script`, `cloud-init`, or `ignition`. |
| `--kubeconfig` | empty | no | Path to the kubeconfig file. |
| `--node-label` | empty | no | `key=value` label for kubelet. Repeatable. |
| `--register-with-taint` | empty | no | Taint to register on the node. Repeatable. |
| `--node-ip` | empty | no | IP address passed to kubelet. |
| `--local-dns` | `false` | no | Enable the nspawn-local CoreDNS cache. |
| `--oci-image` | empty | no | OCI image source for the agent rootfs. |
| `--isolation` | nspawn | no | `nspawn`, `none`, or experimental `container`. |
| `--experimental-container-isolation` | `false` | no | Allow container isolation. |
| `--sandbox-image` | empty | no | Containerd CRI sandbox image reference. |
| `--offline-artifacts-source` | empty | no | Offline rootfs artifact source: absolute path, `file://`, HTTPS archive, or `oci://` reference. |
| `--additional-host-mount` | empty | no | Extra bind mount in `source[:target][:ro]` form. Repeatable. |
| `--additional-host-device` | empty | no | Extra host device or systemd device group. Repeatable. |
| `--kubernetes-version` | auto-detected | no | Override the Kubernetes version. |
| `--agent-version` | latest release | no | Pin the agent release tag. |
| `--agent-url` | empty | conditional | Agent tarball URL. With Ignition it must name the bare binary. |
| `--agent-sha256` | empty | conditional | Agent binary digest. Required with Ignition. |
| `--agent-base-url` | public releases | no | Base URL for agent release downloads. |

The same rootfs mirror and version flags that `machine register` exposes are
also available here: `--kubernetes-base-url`, `--kubernetes-url`,
`--kubernetes-binary-version`, `--containerd-base-url`, `--containerd-url`,
`--containerd-version`, `--runc-base-url`, `--runc-url`, `--runc-version`,
`--cni-base-url`, `--cni-url`, `--cni-version`, `--crictl-base-url`,
`--crictl-url`, and `--crictl-version`.

### Script variant

The script variant is the default. It prints a bash script that installs the
agent on the target host:

```bash
kubectl unbounded machine manual-bootstrap my-node --site edge-west | ssh root@10.10.0.21 bash
```

If you log in as a non-root user with passwordless sudo, pipe to `sudo bash`
instead. The script embeds the agent JSON configuration and the install script
for the target architecture, so the host does not need a copy of the plugin.

### Cloud-init variant

The cloud-init variant emits a user-data document suitable for a VM provisioning
API:

```bash
kubectl unbounded machine manual-bootstrap my-node --site edge-west \
  --variant cloud-init > user-data.yaml
```

Omit the name to build one payload for an autoscaling group. Each instance then
derives its own machine name from its hostname.

### Ignition variant

Ignition requires a bare agent binary and a digest, because the generated config
references the binary directly:

```bash
kubectl unbounded machine manual-bootstrap my-node --site edge-west \
  --variant ignition \
  --agent-url https://example.com/downloads/unbounded-agent-linux-amd64 \
  --agent-sha256 <sha256>
```

The `--agent-url` value must name the binary, not the tarball, and
`--agent-sha256` is mandatory in this variant. This is the path used for
image-based hosts such as Azure Container Linux.

## Day-2 machine operations

Every day-2 action is represented by a `MachineOperation` resource. The CLI
offers a generic `machine operation create` command for full control and a set of
shortcuts that fill in the operation kind, target, and wait behavior. The
controllers execute the operation and update its status. The CLI only submits
the object and, when asked, watches status.

### Operation kinds

| Kind | Effect |
| --- | --- |
| `NodeReboot` | Restart the Kubernetes node stack without rebooting the host. |
| `HostHardReboot` | Power cycle the host out of band through the BMC or cloud provider. |
| `HostSoftReboot` | Reboot the host operating system from inside the agent. |
| `HostPowerOff` | Power off the host. |
| `HostPowerOn` | Power on the host. |
| `HostReplace` | Replace the host VM through the external provider, destroying OS disk state. |
| `AgentUpgrade` | Upgrade the host-side unbounded-agent. |
| `AgentReset` | Remove the unbounded-agent and its managed resources from the host. |

The CLI accepts exactly these eight kinds. The API also defines `HostAllocate`
and `HostRelease`, but they are not valid values for `machine operation create`.

### Generic create

`machine operation create NAME` builds the resource from flags. The name must be
a valid DNS-1123 subdomain. You must target either one machine with `--machine`
or a set of machines with `--selector`; supplying both or neither is an error.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kind` | empty | Operation kind from the table above. Required in practice. |
| `--machine` | empty | Target Machine name. |
| `--selector`, `-l` | empty | Machine label selector. |
| `--param` | empty | Operation parameter as `key=value`. Repeatable. |
| `--ttl` | unset | Seconds after completion before cleanup. `0` keeps the operation indefinitely. |
| `--wait` | `false` | Stream status until the operation reaches Complete or Failed. |
| `--timeout` | `0` | Wait timeout. `0` waits indefinitely. |
| `--output`, `-o` | `name` | Output format: `name`, `yaml`, or `json`. |
| `--dry-run` | `none` | `none`, `client`, or `server`. |
| `--field-manager` | `kubectl-unbounded` | Field manager for the create request. |
| `--kubeconfig` | empty | Path to the kubeconfig file. |

Create a hard reboot for a single machine:

```bash
kubectl unbounded machine operation create reboot-worker-01 \
  --kind HostHardReboot --machine worker-01
```

Create a node reboot for every machine with a label and wait for completion:

```bash
kubectl unbounded machine operation create reboot-gpu \
  --kind NodeReboot --selector role=gpu --wait
```

Upgrade the agent with an explicit download URL:

```bash
kubectl unbounded machine operation create upgrade-worker-01 \
  --kind AgentUpgrade --machine worker-01 \
  --param downloadURL=https://example.com/unbounded-agent-linux-amd64.tar.gz
```

`AgentUpgrade` requires the `downloadURL` parameter. The `.tar.gz` URL should
point at an agent release archive. Other operation parameters are passed through
to the controller without schema validation by the CLI, so use descriptive names
and quote values that contain spaces.

Several combinations are rejected before the request is sent. `--wait` cannot be
combined with a client or server dry run, because a dry run never creates the
object. `--wait` also cannot be combined with `-o yaml` or `-o json`, because
progress output is not machine-readable. Use `name` output when you wait.

### Waiting on an operation

`machine operation wait NAME` watches an existing operation without creating
one. It takes `--timeout`, which defaults to `0` for an indefinite wait, and
`--kubeconfig`.

```bash
kubectl unbounded machine operation wait reboot-worker-01
```

The wait prints a step when the phase enters `InProgress`, when execution
reaches stages such as powering the host off or waiting for the node to join,
and when conditions change. A failed operation returns a non-zero exit with the
failure message. A successful operation ends with a `ready` line.

### Shortcut commands

The shortcuts target one machine and create the operation with an owner
reference to that Machine's UID. They wait by default, use a 300 second TTL after
completion, and generate a name of the form
`<machine>-<operation>-<UTC timestamp>`. Each accepts `--operation-name`,
`--ttl`, `--wait`, `--timeout`, and `--kubeconfig`.

| Command | Kind | Notes |
| --- | --- | --- |
| `machine node-reboot NAME` | `NodeReboot` | Restarts the node stack only. |
| `machine host-hard-reboot NAME` | `HostHardReboot` | Out-of-band power cycle. |
| `machine host-soft-reboot NAME` | `HostSoftReboot` | Reboots the host OS from the agent. |
| `machine power-off NAME` | `HostPowerOff` | Powers the host off. |
| `machine power-on NAME` | `HostPowerOn` | Powers the host on. |
| `machine agent-upgrade NAME` | `AgentUpgrade` | Requires `--download-url`; optional `--sha256`. |
| `machine agent-reset NAME` | `AgentReset` | Prompts unless `--force` is set. |
| `machine replace NAME` | `HostReplace` | Prompts unless `--force` is set. |

```bash
kubectl unbounded machine node-reboot worker-01
kubectl unbounded machine host-soft-reboot worker-01 --timeout 10m
kubectl unbounded machine agent-upgrade worker-01 \
  --download-url https://example.com/unbounded-agent-linux-amd64.tar.gz \
  --sha256 <sha256>
```

`agent-reset` and `replace` are destructive, so they ask for confirmation. When
standard input is not a terminal, they refuse and tell you to rerun with
`--force`. The confirmation prompt requires typing the exact machine name. In
automation, pass `--force` only after you have made the same check in your
pipeline.

`replace` deletes and recreates the host VM with fresh bootstrap data. Host-local
OS disk state is destroyed. Replacing a machine is not the same as repaving it
with the legacy command: `replace` prompts for confirmation and wires the
operation to the Machine with an owner reference, while `machine repave` is the
older non-interactive wrapper around the same `HostReplace` kind.

### Legacy reboot commands

Four older commands remain available. They use the ambient kubeconfig, accept
only `--ttl`, and always wait for the operation to finish.

| Command | Behavior |
| --- | --- |
| `machine reboot NAME` | Creates a `HostHardReboot` operation. Requires Redfish configuration and fails before creating the operation when it is absent. |
| `machine hard-reboot NAME` | Creates a `HostHardReboot` operation without the Redfish precondition. |
| `machine soft-reboot NAME` | Creates a `HostSoftReboot` operation. |
| `machine repave NAME` | Creates a `HostReplace` operation. |

The `host-hard-reboot`, `host-soft-reboot`, and `replace` shortcuts are the
preferred forms because they accept `--kubeconfig`, expose `--operation-name` and
`--timeout`, and use a generated DNS-safe operation name. Use the legacy forms
when you need the older behavior or are following an existing runbook.

## Machine configurations

A `MachineConfiguration` is a profile for a class of machines. It carries the
Kubernetes version, agent image, node labels, taints, update strategy, and a
priority used when several configurations could apply. The machina controller
turns the profile into versioned `MachineConfigurationVersion` objects. Assign a
configuration to a Machine by name, and optionally pin a version.

The `machine config` commands use the ambient kubeconfig. They do not accept
`--kubeconfig`, so select the cluster with `KUBECONFIG` or the current default
context before running them.

### `machine config create`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubernetes-version` | empty | Kubernetes version, for example `v1.34.0`. |
| `--agent-image` | empty | Agent OCI image reference. |
| `--node-labels` | empty | Comma-separated `key=value` labels. |
| `--taints` | empty | Comma-separated `key=value:Effect` taints. |
| `--update-strategy` | `OnDelete` | `OnDelete` or `RollingUpdate`. |
| `--priority` | `0` | Priority for machine selector ordering. |

```bash
kubectl unbounded machine config create gpu-worker \
  --kubernetes-version v1.34.0 \
  --agent-image ghcr.io/azure/unbounded-agent:latest \
  --node-labels=env=prod,tier=worker \
  --taints=dedicated=gpu:NoSchedule
```

The command creates only the `MachineConfiguration`. The controller creates
version 1 from the template and reports it in status. If you set
`--update-strategy` to an unsupported value, the API rejects the object.

### `machine config get`

With no argument, `get` lists configurations in a table with the columns `Name`,
`Latest Version`, `Strategy`, `Priority`, and `Age`. With a name, it prints a
detail view:

```bash
kubectl unbounded machine config get
kubectl unbounded machine config get gpu-worker
```

The detail view includes `latest-version`, `current-version`,
`update-strategy`, `priority`, any Kubernetes version and node labels, taints,
and the agent image. An empty list prints `No MachineConfigurations found`.

### `machine config versions`

`versions NAME` lists the versions that belong to one configuration, ordered as
the API returns them. The table columns are `Name`, `Version`, `Deployed`,
`Machines`, `K8s Version`, `Agent Image`, and `Age`.

```bash
kubectl unbounded machine config versions gpu-worker
```

`Machines` is the number of machines the operator reports as deployed on that
version. `Deployed` reports whether the version is the one currently deployed.
An unknown configuration name produces an empty result rather than an error,
because the lookup is label-based.

### `machine config assign`

`assign MACHINE_NAME` sets `spec.configurationRef` on a Machine. `--config` is
required. When `--version` is omitted, only the configuration name is stored and
the controller selects the latest version. When `--version` is present, the
command verifies that the named `MachineConfigurationVersion` exists before
updating the Machine.

```bash
kubectl unbounded machine config assign edge-west-10-10-0-21 --config gpu-worker
kubectl unbounded machine config assign edge-west-10-10-0-21 --config gpu-worker --version 3
```

The output confirms the configuration, the machine, and whether a version was
pinned or the controller will choose the latest. A missing configuration or
version fails before the Machine is modified.

## Networking commands

The `net` group manages the unbounded-net data plane. It creates the networking
custom resources, reads controller and node status, gives you access to node
agent pods, and creates plugin symlinks. Use the networking reference pages for
the resource model and routing behavior; this section documents the command
surface.

### Shared `net` flags

Every `net` subcommand inherits a hidden set of kubectl connection flags,
including `--kubeconfig`, `--context`, `--namespace`, `--server`, `--token`, and
`--request-timeout`. They are hidden to keep per-command help short, but they
still work. Run `kubectl unbounded net options` to list them.

The group also inherits interface naming flags. They affect how route output is
classified, not how the cluster is configured, so set them when your node agents
use non-default interface names.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--wireguard-interface-prefix` | `wg` | WireGuard interface name prefix. |
| `--geneve-interface` | `geneve0` | GENEVE interface name. |
| `--vxlan-interface` | `vxlan0` | VXLAN interface name. |
| `--ipip-interface` | `ipip0` | IPIP interface name. |

### Creating networking resources

`kubectl unbounded net create` has one subcommand per resource. All of them accept
the same output and request flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--output`, `-o` | `name` | Output format: `name`, `yaml`, or `json`. |
| `--dry-run` | `none` | `none`, `client`, or `server`. |
| `--field-manager` | `kubectl-unbounded` | Field manager for the create request. |
| `--health-check-enabled` | unset | Enable UDP health probes over tunnels. |
| `--health-check-detect-multiplier` | unset | Missed probes before a peer is marked down. Node default is 3. |
| `--health-check-receive-interval` | unset | Expected interval between received probes. Node default is 15s. |
| `--health-check-transmit-interval` | unset | Interval between transmitted probes. Node default is 15s. |
| `--tunnel-mtu` | unset | MTU for tunnel interfaces in this scope. |
| `--tunnel-protocol` | unset | `WireGuard`, `GENEVE`, or `Auto`. |

Health and tunnel values are included in the resource only when you set the flag.
Leaving them unset lets the node agent defaults apply, which is different from
explicitly setting `--health-check-enabled=false`. The down timeout is the detect
multiplier times the larger of the receive and transmit intervals.

| Subcommand | Alias | Resource | Required flags |
| --- | --- | --- | --- |
| `create site NAME` | `st` | `Site` in `unbounded-cloud.io/v1alpha3` | `--node-cidr`, `--pod-cidr-block` |
| `create gatewaypool NAME` | `gp` | `GatewayPool` in `net.unbounded-cloud.io/v1alpha1` | `--node-selector` |
| `create sitepeering NAME` | `spr` | `SitePeering` | none |
| `create sitegatewaypoolassignment NAME` | `sgpa` | `SiteGatewayPoolAssignment` | none |
| `create gatewaypoolpeering NAME` | `gpp` | `GatewayPoolPeering` | none |

The `Site` command is the one exception to the `net.unbounded-cloud.io` group.
Sites are part of the machina API group, so the CLI creates
`unbounded-cloud.io/v1alpha3` objects for them.

#### Sites

`net create site` defines a network boundary. Nodes join the site when their IP
addresses match one of the site's node CIDRs. Pod CIDR blocks define the pool
from which per-node pod subnets are allocated, and they remain required even when
pod assignment or CNI management is disabled because they describe the address
space used for inter-site routing.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--node-cidr` | empty | CIDR that identifies the site's nodes. Repeatable; required. |
| `--pod-cidr-block` | empty | Pod CIDR pool for per-node subnets. Repeatable; required. |
| `--non-masquerade-cidr` | empty | CIDR that should not be SNATed when leaving the site. Repeatable. |
| `--local-cidr` | empty | CIDR considered local and not tunneled. Repeatable. |
| `--no-manage-cni-plugin` | `false` | Disable CNI configuration and same-site tunnel peers. |
| `--pod-assignment-enabled` | `true` | Set `assignmentEnabled` on the pod CIDR assignment rule. |
| `--pod-assignment-priority` | `100` | Priority for the pod CIDR assignment rule. |
| `--pod-node-regex` | empty | Node name regex for the assignment rule. Repeatable. |
| `--pod-node-block-size-ipv4` | `0` | Per-node IPv4 mask size. |
| `--pod-node-block-size-ipv6` | `0` | Per-node IPv6 mask size. |

```bash
kubectl unbounded net create site edge-west \
  --node-cidr 10.10.0.0/16 \
  --pod-cidr-block 10.245.0.0/16
```

For a cluster with an existing CNI, disable both management paths:

```bash
kubectl unbounded net create site edge-west \
  --node-cidr 10.10.0.0/16 \
  --pod-cidr-block 10.245.0.0/16 \
  --no-manage-cni-plugin \
  --pod-assignment-enabled=false
```

#### Gateway pools

A gateway pool selects nodes by label and turns them into tunnel endpoints.
`External` pools connect to networks outside the cluster; `Internal` pools connect
sites inside it. Omitting `--type` leaves the field unset so the API default
applies. `--routed-cidr` is repeatable and describes networks reachable through
the pool.

```bash
kubectl unbounded net create gatewaypool edge-gw \
  --type External \
  --node-selector role=gateway \
  --routed-cidr 0.0.0.0/0
```

`--node-selector` accepts repeated `key=value` pairs and is required.

#### Peerings and assignments

A `SitePeering` connects sites, optionally as a full node mesh. A
`SiteGatewayPoolAssignment` routes traffic from sites through a gateway pool. A
`GatewayPoolPeering` connects two gateway pools. All three accept `--enabled`,
whose flag default is true and which is written to the spec only when you pass
it, along with the health and tunnel flags above.

```bash
kubectl unbounded net create sitepeering edge-to-hub \
  --site edge-west --site hub --mesh-nodes

kubectl unbounded net create sitegatewaypoolassignment edge-west-gw \
  --site edge-west --gateway-pool edge-gw

kubectl unbounded net create gatewaypoolpeering edge-to-core \
  --gateway-pool edge-gw --gateway-pool core-gw
```

`SitePeering` and `SiteGatewayPoolAssignment` accept repeatable `--site`.
`SiteGatewayPoolAssignment` and `GatewayPoolPeering` accept repeatable
`--gateway-pool`. The CLI does not require a minimum number of sites or pools;
the API validation and the controller decide whether the reference set is
complete. Always check the resulting status before assuming a peering is active.

### Listing nodes

`kubectl unbounded net node list` asks the controller for its cluster summary and
renders one row per node. The default table columns are `NAME`, `TYPE`, `SITE`,
`POOL`, `PEERS`, `GATEWAYS`, `K8S STATUS`, `UN STATUS`, `CNI`, and
`LAST UPDATE`. The wide form adds `INTERNAL IPS`, `EXTERNAL IPS`, and
`POD CIDRS`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--output`, `-o` | `table` | `table`, `wide`, or `json`. |
| `--color`, `-C` | `auto` | `auto`, `always`, or `never`. `-C` alone means `always`. |
| `--watch`, `-w` | `false` | Stream updates over a WebSocket. |
| `--suppress-warnings` | `false` | Hide warnings from output. |

```bash
kubectl unbounded net node list
kubectl unbounded net node list -o wide
kubectl unbounded net node list -o json
kubectl unbounded net node list --watch
```

Warnings are written to standard error, so `-o json` remains valid JSON when a
warning is present. The fetch flags below are persistent on the `node` group and
apply to `list` and `show`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--controller-service` | `unbounded-net-controller` | Controller Service used for status. |
| `--controller-selector` | `app.kubernetes.io/name=unbounded-net-controller` | Controller pod selector for the port-forward fallback. |
| `--controller-deployment` | `unbounded-net-controller` | Controller Deployment for the port-forward fallback. |
| `--controller-port` | `9999` | Controller Service port. |
| `--timeout` | `15s` | Request timeout. |

The client first tries the Service proxy. If that fails, it port-forwards to a
controller pod and retries. The error message names both attempts so you can tell
whether the controller is missing or merely unreachable through the Service.

### Node diagnostics and access

`node show` loads the expiring diagnostic record for one node. The first
positional argument is always the node name. The optional second argument selects
a view:

| Mode | Output |
| --- | --- |
| omitted | Node information pane with identity, status, addresses, build, and placement. |
| `peer` or `peers` | Peering table. A third argument limits output to one peer. |
| `route` or `routes` | Route table classified using the interface name flags. |
| `bpf` | BPF entries for the node. |
| `json` | Raw diagnostic JSON. |

```bash
kubectl unbounded net node show worker-01
kubectl unbounded net node show worker-01 peers
kubectl unbounded net node show worker-01 route
kubectl unbounded net node show worker-01 json
```

Diagnostics are cached until they expire. Pass `--refresh` to collect a fresh
snapshot. `--color` controls colorization and `--watch` is rejected because
diagnostics are not streamed; use `node list --watch` for a live overview.

`node logs NODE_NAME` resolves the node agent pod for that node and streams its
logs. It accepts the same log selection flags as the controller logs command:
`--selector`, `--container` (default `node`), `--follow`, `--previous`, `--tail`,
`--since`, `--since-time`, and `--timestamps`.

```bash
kubectl unbounded net node logs worker-01 --tail 200
kubectl unbounded net node logs worker-01 --follow
```

`node exec NODE_NAME` runs a command in the node agent pod. If you pass extra
arguments, the command must follow `--`; otherwise the CLI rejects the arguments
rather than guessing. With no command at all, it opens `/bin/sh`, and anything
after `--` must be non-empty. Use `-i` for stdin and `-t` for a TTY.

```bash
kubectl unbounded net node exec worker-01 -ti -- /bin/sh
kubectl unbounded net node exec worker-01 -- crictl ps
```

Both commands resolve the target pod with the selector
`app.kubernetes.io/name=unbounded-net-node` and the `node` container by default.
Override `--selector` or `--container` only when your deployment uses different
labels or container names.

### Controller operations

The `controller` group talks to the unbounded-net controller.

`controller logs` selects controller pods and prints logs from the first pod in
name order. It accepts `--selector`, `--container`, `--follow`, `--previous`,
`--tail`, `--since`, `--since-time`, and `--timestamps`.

```bash
kubectl unbounded net controller logs --tail 100
kubectl unbounded net controller logs --follow
```

`controller status-json` prints the controller's cluster overview. It accepts the
controller fetch flags and `--pretty`, which defaults to true.

```bash
kubectl unbounded net controller status-json
kubectl unbounded net controller status-json --pretty=false
```

The output preserves controller metadata and summary fields and removes legacy
per-node diagnostic arrays, so it is safe to store or diff. For raw per-node
diagnostics, use `net node show NODE_NAME json`.

`controller proxy` starts a local HTTP server that proxies to the controller's
HTTPS endpoint through a port-forward. It fetches the controller CA from the
`unbounded-net-serving-ca` ConfigMap and verifies the controller certificate, so
browsers and `curl` do not see TLS errors.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--deployment` | `unbounded-net-controller` | Controller Deployment name. |
| `--local-port` | `9999` | Local HTTP port. |
| `--remote-port` | `9999` | Controller HTTPS port. |
| `--address` | `127.0.0.1` | Listen address. Repeatable. |
| `--no-browser` | `true` | Do not open the status page. |

```bash
kubectl unbounded net controller proxy
```

`net dashboard` is the same proxy with browser opening enabled by default. It is
a convenience alias, not a separate server implementation.

### Plugin symlinks

`net symlink create` creates kubectl plugin symlinks next to the running binary.
It creates a completion symlink for `kubectl_complete-unbounded-net` and
shortcut names for the create commands, such as `kubectl-create-site`,
`kubectl-create-gp`, and `kubectl-create-gpp`. This lets you type familiar
commands such as `kubectl create site` without spelling out the full plugin path.

```bash
kubectl unbounded net symlink create
```

Existing symlinks are skipped unless you pass `--force`, which replaces their
targets. A dangling symlink is replaced automatically. A regular file with the
same name is never replaced, even with `--force`, because the command only
manages symlinks.

## Workload overrides

The `overrides` group inspects the component workload overrides that the operator
reads from the `unbounded-component-overrides` ConfigMap. Overrides customize the
Deployments and DaemonSets the operator generates for Unbounded components.

Write access to that ConfigMap is equivalent to root on every node in every
affected site. The workloads it patches already run privileged and host
networked, so changing a container image, its arguments, or adding a sidecar is
arbitrary code execution. Treat write access like a `ClusterRoleBinding`, not
like ordinary configuration. The [workload overrides reference]({{< relref
"reference/workload-overrides" >}}) documents the document schema and allowlist.

### `overrides validate`

`validate` checks override documents without applying them. It reports parsing
errors, schema errors, bad `apiVersion` values, disallowed or protected paths,
invalid strategic merge directives, and explicit nulls. It does not check whether
a container or volume named by a patch exists, because that depends on the
workload the running operator renders.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--filename`, `-f` | empty | Document to validate, or `-` for standard input. Repeatable. |
| `--namespace` | `unbounded-system` | Namespace holding the ConfigMap when no file is given. |
| `--kubeconfig` | empty | Path to the kubeconfig file. |

```bash
kubectl unbounded overrides validate -f overrides.yaml
kubectl unbounded overrides validate -f component-overrides.yaml
kubectl unbounded overrides validate
```

With `-f`, validation is offline. A file may be a bare overrides document or a
ConfigMap manifest. A ConfigMap is unwrapped and each data key is validated
separately, matching how the operator reads it. Every input file must supply
unique ConfigMap keys; two files with the same base name are rejected rather
than silently keeping the last one. Each input is limited to 4 MiB. Directories
are rejected, and `-` reads standard input.

With no `-f`, the command reads the cluster ConfigMap. If the ConfigMap does not
exist, it prints a message naming the namespace and suggests `--namespace` when
the operator is installed elsewhere. An empty input is an error, because a clean
result for zero entries would be a false positive.

A valid document prints a line of the form
`ok: <n> entries, syntax and allowlist valid`, followed by a note that container
and volume resolution happens in the operator. Any problem produces a non-zero
exit and the collected diagnostics.

### `overrides list`

`list` shows the entries the operator reads, as authored. It answers what was
asked for, not what was applied. Use `overrides status` for the applied state.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--namespace` | `unbounded-system` | Namespace holding the overrides ConfigMap. |
| `--kubeconfig` | empty | Path to the kubeconfig file. |

```bash
kubectl unbounded overrides list
```

The table columns are `Source`, `Component`, `Kind`, `Sites`, and `Changes`.
`Source` identifies the ConfigMap key and the entry position, such as
`overrides.yaml[2]`, or the document field for image entries. `Sites` prints
`(all)` when the entry has no site selector. `Changes` summarizes the entry as
one or more of `patch`,
`extraArgs(<containers>)`, `add(<containers>)`, `addInit(<containers>)`, and
`kubeProxyImage(<image>)`.

The command prints the observed ConfigMap resource version and reminds you to run
`overrides status`. Site selectors that match no existing Site are reported as a
warning, because they are inert rather than invalid: a document may be written
before its Site exists. Validation problems still fail the command with a
non-zero exit, even though the table was printed.

### `overrides status`

`status` reads the override state that the operator recorded on each Site. Every
value it shows was computed by the running operator, so the answer stays correct
when the plugin and the operator are different versions. It accepts only
`--kubeconfig`.

```bash
kubectl unbounded overrides status
```

The table columns are `Site`, `Phase`, `Workload`, `Applied`, and `Drift`.
`Workload` uses the form `<Kind>/<Name>`. `Applied` is one of `yes`, `pending`,
`withheld`, `failed`, `no override`, `not applied`, or `stale`. `Drift` describes
how an image override diverges from the operator's own image.

When every reporting Site carries the same row for a workload, those rows are
collapsed into one `(all sites)` entry. With only one Site, rows are not
collapsed, because calling them all-sites would hide the actual site name.

If Sites observed different ConfigMap versions, the command prints a note. That
state is transient: a newer version always triggers another reconcile. Degraded
Sites are listed with their operator messages, and the command exits non-zero
when any Site reports overrides as Degraded. Version drift is called out
separately, because a pinned image will not be updated by an operator upgrade.
If no Site has overrides, the command prints `No overrides are in effect on any
Site.` and exits successfully. If the cluster has no Sites at all, it prints
`No Sites found.` instead.

## Attaching an ACR to Gantry

`kubectl unbounded gantry attach-acr` adds an Azure Container Registry upstream
to Gantry without replacing unrelated Gantry configuration. Authentication is
externally managed by default, which preserves the existing setup. Managed
authentication is opt-in.

| Flag | Default | Required | Meaning |
| --- | --- | --- | --- |
| `--acr-resource-id` | empty | yes | Existing ACR Azure resource ID. |
| `--auth-mode` | `external` | no | `external` or `aks-identity-binding`. |
| `--cluster-resource-id` | empty | conditional | AKS resource ID. Required for `aks-identity-binding`. |
| `--namespace` | `unbounded-system` | no | Operator and Gantry namespace. |
| `--kubeconfig` | empty | no | Kubeconfig path. |
| `--timeout` | `10m` | no | Setup and node configuration timeout. |

```bash
kubectl unbounded gantry attach-acr \
  --acr-resource-id /subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.ContainerRegistry/registries/<name>
```

To enable the stock ACR credential provider on current and future Linux Flex
nodes:

```bash
kubectl unbounded gantry attach-acr \
  --acr-resource-id /subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.ContainerRegistry/registries/<name> \
  --cluster-resource-id /subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.ContainerService/managedClusters/<name> \
  --auth-mode aks-identity-binding
```

The identity-binding mode prints a warning that managed ACR authentication may
restart kubelets on all targeted Linux Flex nodes concurrently. Nodes are not
drained automatically, so you are responsible for the impact on running
workloads. The mode requires the selected kubeconfig to match the requested AKS
cluster, the cluster to have finished provisioning, and the nodes to run Linux
with kubelet 1.34 or later. AKS API server VNet integration is not supported in
this mode. The ACR and AKS cluster may be in different subscriptions if they are
in the same tenant, and the Azure CLI caller needs permissions in both
subscriptions.

The command requires the `gantry-config` ConfigMap, so install operator-managed
Gantry first. It uses `az` to read ACR and AKS metadata. In identity-binding
mode, it refuses to replace an existing authentication policy unless the policy
already matches the requested mode and registry, and it detects concurrent
changes to the policy before writing. A successful run prints the attached
registry and whether existing authentication was left unchanged or the stock
credential provider was configured. Verify a private image pull and Gantry
acceleration separately.

## Inventory and inspection

There is no inventory subcommand in `kubectl unbounded`. The repository builds
separate `inventory-agent`, `inventory-aggregator`, `inventory-inspector`, and
`inventory-viewer` binaries, but they are not plugin commands. Within the CLI,
inventory comes from Kubernetes list operations and from the status endpoints
behind `net`. The following set covers the objects most operators need:

```bash
kubectl get sites
kubectl get machines -L unbounded-cloud.io/site
kubectl get machineconfigurations
kubectl get gatewaypools,sitepeerings,sitegatewaypoolassignments,gatewaypoolpeerings
kubectl unbounded net node list -o wide
```

`kubectl get sites` shows the network boundaries created by `site init`.
`kubectl get machines -L unbounded-cloud.io/site` adds a site column by reading
the canonical site label. `kubectl get machineconfigurations` shows the profiles,
while `machine config versions NAME` shows the version history and how many
machines each version has. The networking resources are ordinary custom
resources, so `kubectl get` and `kubectl describe` work on them like any other
object.

For a machine-readable node inventory, use `net node list -o json` or
`net controller status-json`. The former returns the row objects the table would
render; the latter returns the controller's cluster summary. For deeper
per-node facts, `net node show NODE_NAME json` returns the diagnostic record.

Workload override state is another form of inventory. `overrides list` shows
what was asked for, and `overrides status` shows what the operator actually
applied, including drift and degraded sites.

## Exit status and failure behavior

The binary exits with status 1 when a command returns an error and status 0 when
it succeeds. The distinction between "the request was accepted" and "the work is
done" matters:

- `site init`, `machine register`, `net create`, and the non-waiting operation
  commands return success after the API accepts the object. Controllers act
  later.
- Operation shortcuts wait by default, so a failed operation returns a non-zero
  exit with its failure message. The generic `machine operation create` does not
  wait unless you pass `--wait`.
- `overrides validate` and `overrides list` return non-zero when they find
  validation problems. `overrides status` returns non-zero when any Site reports
  overrides as Degraded.
- `install` returns an error when the operator rollout or CRD establishment does
  not finish before `--timeout`.
- `gantry attach-acr` returns an error when the kubeconfig does not match the
  requested AKS cluster, when required Azure permissions are missing, or when an
  existing authentication policy conflicts with the requested mode.

Some commands print useful output before failing. `overrides list` is the main
example: it prints the table and then returns the validation error, so a pipeline
that only checks the exit code still fails, while a human still sees the entries.

A dry run never proves that the controller will accept the request. For
`machine operation create` and `net create`, `--dry-run=client` prints the object
without contacting the API server, and `--dry-run=server` asks the API server to
validate it. Neither waits for a controller to reconcile the result.

## Related documentation

- [Getting started]({{< relref "guides/getting-started" >}}) for a first cluster
  and a first site.
- [Existing clusters]({{< relref "guides/existing-cluster" >}}) for bringing
  Unbounded to a control plane you already run.
- [Machina CRD reference]({{< relref "reference/machina-crd" >}}) for the
  `Machine`, `MachineOperation`, and `MachineConfiguration` schemas.
- [Networking reference]({{< relref "reference/networking" >}}) and
  [networking custom resources]({{< relref "reference/networking/custom-resources" >}})
  for the objects created by `net create`.
- [Networking configuration]({{< relref "reference/networking/configuration" >}})
  for tunnel protocols and health check settings.
- [Workload overrides]({{< relref "reference/workload-overrides" >}}) for the
  override document format and allowlist.
- [Agent provisioning format]({{< relref "reference/agent/provisioning-format" >}})
  and [agent isolation]({{< relref "reference/agent/isolation" >}}) for the
  payloads produced by `machine manual-bootstrap`.
- [Gantry guide]({{< relref "guides/gantry" >}}) for the registry acceleration
  that `gantry attach-acr` configures.
- [Node operations]({{< relref "guides/operations/node-operations" >}}) and
  [host operations]({{< relref "guides/operations/host-operations" >}}) for the
  procedures behind the day-2 commands.

