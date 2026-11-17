---
title: "Cloud Bootstrapping (SSH)"
weight: 3
description: "Turn an existing Linux host into an Unbounded worker node by giving the machina controller SSH access and letting it install and start the node agent."
---

SSH bootstrapping is the path for a machine that already exists and already
runs Linux. You do not image it, PXE boot it, or ask a cloud provider to create
it. Instead, you publish the host address and an SSH key to the cluster, and the
machina controller connects to the host, copies the unbounded-agent install
script and a generated agent configuration, runs the installer, and waits for
the resulting Node to register.

The mental model is a three step handoff:

1. `kubectl unbounded install` puts the CRDs and the unbounded-operator on the
   cluster. The operator then deploys the shared `unbounded-net` and machina
   workloads.
2. `kubectl unbounded site init` declares the site and issues a bootstrap token
   that machines in that site may use to join.
3. `kubectl unbounded machine register` writes the SSH credentials as a Secret
   and creates a `Machine` custom resource. From then on, machina owns the host
   lifecycle.

![Diagram showing the machina controller connecting over SSH to a host, uploading the install script and agent config, and the host joining the cluster as a Node](../../img/architecture-ssh-provisioning.svg)

This guide walks through each step in order, explains the `Machine` fields that
control the connection, and ends with the checks and log commands you need when
a machine does not reach `Ready`.

## When SSH bootstrapping applies

Use SSH bootstrapping when the host already exists and you can reach it from the
cluster over the network. Typical cases are an on-premises server with a stable
address, a virtual machine in another cloud, a GPU rental instance, or a host
that your infrastructure tooling created but does not manage as a Kubernetes
Node.

SSH bootstrapping is a good fit when all of these are true:

- The host runs Linux on `x86_64` or `aarch64`.
- The host has a stable IP address or DNS name that the machina controller can
  reach, directly or through a bastion.
- You have an SSH account on the host that can run commands as root through
  `sudo`.
- The host can reach the Kubernetes API server and the download locations for
  the agent and node binaries.

It is not the right tool when you cannot get a shell on the host, when the host
must be created from a provider API, or when the machine has no outbound network
access at all. For provider-driven capacity, use `MachineClass` and
`MachineAllocation` instead, which are described under
[Machine and MachineClass](#machine-and-machineclass). For image or network boot
flows, see [PXE Boot]({{< relref "guides/pxe" >}}).

The connection details also outlive the first join. They stay on the `Machine`
spec, so later day-two actions reuse the same stored credentials instead of
asking you to hand them to each operation.

## Prerequisites

The prerequisites split into what the host must provide, what the cluster must
provide, and what the SSH connection itself must allow. Check all three before
you register anything, because a mismatch usually shows up as a Machine that
stays in `Pending` or `Failed`.

### Host requirements

| Requirement | Detail |
| -- | -- |
| Architecture | `x86_64` or `aarch64`. The install script maps these to `amd64` and `arm64` and rejects anything else. |
| Shell and tools | `bash`, `curl`, `tar`, and `sudo` must be present and usable by the SSH account. |
| Root access | The account must be able to run `sudo` without an interactive password prompt, because machina runs the installer in a non-interactive SSH session. |
| Operating system | A systemd-based Linux distribution. The agent installs and manages a systemd service. |
| Network | The host must reach the API server over HTTPS, and must be able to download the agent tarball and node binaries. |
| Disk | Writable space under `/var/lib/unbounded`, where the install script stages the agent, plus space for the node runtime. |

The install script runs from a temporary directory under
`/var/lib/unbounded`, not `/tmp`, because hardened hosts commonly mount `/tmp`
with `noexec`. If `/var/lib/unbounded` is read-only on your image, the install
fails before the agent starts.

### Cluster requirements

| Requirement | Detail |
| -- | -- |
| Working kubeconfig | `kubectl` must reach the control plane. Every command in this guide accepts `--kubeconfig` if you do not use the default. |
| `kubectl-unbounded` | The plugin must be installed and on your `PATH`. |
| Operator | `unbounded-operator` must be installed with `kubectl unbounded install`. |
| Site | The site must exist before you register machines, because registration reads the site bootstrap token. |
| Gateway | For cross-site traffic, at least one node labeled `unbounded-cloud.io/unbounded-net-gateway=true`. |

The gateway requirement comes from the `GatewayPool` manifest that `site init`
creates. It selects nodes with that label, and the pool is what other sites dial
when they route traffic back. If you only need a node to join and run local
workloads, a gateway is not required for the join itself, but cross-site pod
networking will not work without one.

For the gateway nodes, allow UDP ports `51820` through `51899` between sites.
The gateway pool controller assigns each gateway node its own WireGuard port
starting at `51821`, and the forge tooling in this repository opens exactly the
`51820` to `51899` UDP range on gateway node pools. If you build the cluster by
hand, open the same range in your firewall or network security group.

### SSH access requirements

The controller opens an SSH connection from the machina pod, not from your
laptop. That distinction matters for firewalls and for key placement.

- The machina controller pod must be able to reach the host on the SSH port.
- If the host is only reachable from a jump host, configure a bastion as
  described in [Bastion hosts](#bastion-hosts).
- The SSH server must accept public key authentication for the account you
  specify.
- The key must be an unencrypted private key in a format that Go's SSH library
  can parse, such as Ed25519, RSA, or ECDSA. Passphrase-protected keys are not
  supported because the controller has no passphrase to supply.

If the host is behind a NAT gateway or a load balancer, register the address the
controller can actually reach. The host field accepts an IP address, a DNS name,
and an optional port, for example `10.0.0.5`, `10.0.0.5:2222`, or
`host.example.com`. When you omit the port, machina uses `22`.

## Cluster setup

Cluster setup has two commands. The first installs the operator and the custom
resource definitions. The second declares the site and its bootstrap token.
Neither command provisions a machine; they prepare the cluster to accept
machines and to deploy the networking components those machines need.

### Install the operator

Run `kubectl unbounded install` against the cluster. The command applies the
Unbounded CRDs and deploys `unbounded-operator` into the system namespace,
`unbounded-system` by default.

```bash
kubectl unbounded install
```

The operator is the component that reconciles `Site.spec.components`. The
`unbounded-net` controller, the machina controller, gantry, the token refresher,
and the metalman bootstrap workload are cluster singletons deployed by the
operator, not by the command. That is why the command is safe to run again: it
converges the operator, and the operator converges the rest.

Useful flags for this step:

| Flag | Default | Purpose |
| -- | -- | -- |
| `--namespace` | `unbounded-system` | Namespace for the operator and the default components. |
| `--kubeconfig` | current context | Path to the kubeconfig file. |
| `--operator-image` | built-in | Operator image name and tag without a registry. |
| `--image-registry` | built-in | Image repository prefix applied to all Unbounded images. |
| `--api-server-endpoint` | auto-detected | Endpoint advertised to provisioned machines. |
| `--wait` | `true` | Wait for the operator rollout. |
| `--timeout` | built-in default | Timeout for the rollout wait. |

If the operator cannot discover the API server endpoint from
`kube-public/cluster-info`, set `--api-server-endpoint` explicitly. A machine
with an empty or wrong endpoint can still install the agent, but it cannot
register its Node, because the agent on the host has nowhere to send its kubelet
credentials.

Confirm the operator is running before you continue:

```bash
kubectl -n unbounded-system rollout status deploy/unbounded-operator
```

### Initialize the site

A site is the unit that groups machines and the network ranges they use. Create
it with `kubectl unbounded site init`. The command requires a name and four
CIDRs.

```bash
kubectl unbounded site init \
  --name dc-east \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 192.168.50.0/24 \
  --pod-cidr 10.245.0.0/16
```

The four CIDR flags are all required and are validated as IPv4 CIDRs:

| Flag | Meaning |
| -- | -- |
| `--cluster-node-cidr` | The range the existing cluster uses for node IPs. |
| `--cluster-pod-cidr` | The range the existing cluster uses for pod IPs. |
| `--node-cidr` | The range for node IPs at the new site. |
| `--pod-cidr` | The range for pod IPs at the new site. The value is written to the site's pod CIDR assignment. |

The optional flags are:

| Flag | Default | Meaning |
| -- | -- | -- |
| `--manage-cni-plugin` | `true` | When false, `unbounded-net` leaves CNI configuration to an existing plugin such as Cilium or Calico and only builds tunnel links to gateway pools. |
| `--enable-metalman` | `false` | Enables the metalman PXE controller for this site. |
| `--kubeconfig` | current context | Path to the kubeconfig file. |

The CIDR ranges must not overlap. If the new site's node or pod range collides
with the cluster ranges, routing between the sites is ambiguous and the site
controller cannot build a coherent route plan.

### What site init creates

`site init` applies manifests instead of running an imperative installer, so
the result is visible and editable. It creates two `Site` resources:

- A cluster site named `cluster`, which describes the nodes that already belong
  to the control plane.
- A site named after `--name`, which describes machines you register later.

It also creates a `GatewayPool` named `gw-main` that selects nodes labeled
`unbounded-cloud.io/unbounded-net-gateway=true`, and a
`SiteGatewayPoolAssignment` that points both sites at that pool. The cluster
site is the only one that owns the gateway pool manifest; the remote site gets
its own assignment.

The bootstrap token is a Kubernetes bootstrap token Secret in `kube-system`,
labeled `unbounded-cloud.io/site=<name>`. The token ID is random, the Secret is
named `bootstrap-token-<id>`, and the token expires 24 hours after it is issued.
The operator's token refresher keeps tokens current for sites that enable it.
The remote site created by `site init` enables the refresher, which is why a
machine registered much later than the site can still receive a usable token.

`site init` checks the cluster before it writes anything. If the API server does
not serve the kinds that the embedded manifests need, the command stops and
tells you to run `kubectl unbounded install` first. If the operator Deployment is
missing or not fully rolled out, the command only warns, because the Site
resources are still valid and the operator reconciles them once it is ready.

Inspect the result:

```bash
kubectl get site
kubectl get gatewaypool
kubectl get sitegatewaypoolassignment
kubectl -n kube-system get secrets -l unbounded-cloud.io/site=dc-east
```

### Site components

`Site.spec.components` is how a site opts into optional components. The field
exists on both sites, but the two sites use it differently.

For the remote site, `site init` writes `components.tokenRefresher.enabled:
true` and writes `components.metalman.enabled` from `--enable-metalman`.
Machina is always required and its component setting is a deprecated
compatibility field that is ignored. Gantry defaults to enabled through the CRD
default.

For the cluster site, the token refresher is explicitly disabled and metalman
is left off. The cluster site describes existing nodes, so it does not need
tokens for joining.

You rarely edit this block by hand. If you do, remember that the operator
deploys components into its own namespace at its own version. Component
workloads are customized through workload overrides, not through this field.

## Machine and MachineClass

Two resource kinds look similar at first glance and serve different purposes.
`Machine` is the object that owns one host and, in the SSH path, holds the
connection details. `MachineClass` is a template for provider-driven capacity,
where a provider adapter creates the host and a `MachineAllocation` consumes it.
SSH bootstrapping creates `Machine` objects and never creates a `MachineClass`.

### Machine fields

A `Machine` is cluster-scoped and has the short name `mach`. The fields you care
about for SSH bootstrapping are all under `spec`.

| Field | Required | Meaning |
| -- | -- | -- |
| `spec.ssh.host` | yes | Hostname or IP address, optionally with a port. Port 22 is assumed when omitted. |
| `spec.ssh.username` | no | SSH username. Defaults to `azureuser` when omitted. |
| `spec.ssh.privateKeyRef` | yes | Secret key selector with `name`, `namespace`, and `key`. |
| `spec.ssh.bastion` | no | Optional jump host. Its structure mirrors the SSH block without a nested bastion. |
| `spec.kubernetes.version` | no | Kubernetes version to install. When empty, the controller uses the cluster version. |
| `spec.kubernetes.nodeRef` | no | The Node that corresponds to this Machine. Written by the controller when the Node appears. |
| `spec.kubernetes.nodeLabels` | no | Labels passed to kubelet's `--node-labels` flag. |
| `spec.kubernetes.registerWithTaints` | no | Taints passed to kubelet's `--register-with-taints` flag. |
| `spec.kubernetes.bootstrapTokenRef` | no | Reference to the bootstrap token Secret in `kube-system`. Required for provisioning to succeed. |
| `spec.agent` | no | Agent settings: `version`, `baseURL`, `url`, `image`, `downloads`, and `localDNS`. |
| `spec.isolation` | no | Where the node stack runs: `nspawn` (default), `none`, or `container` (experimental, needs an operator opt-in). |

The `privateKeyRef` selector has three fields. `name` is the Secret name,
`namespace` is required by the API, and `key` defaults to `ssh-privatekey` when
omitted. Note the difference from the key the CLI writes: `machine register`
stores the key under `ssh-private-key`. If you author a Machine by hand and
reuse a Secret created by the CLI, set `key: ssh-private-key` explicitly or
machina will look for the wrong entry.

### What MachineClass adds

A `MachineClass` is a provider-neutral template. It does not describe SSH
credentials. Instead it names the provider adapter, the provider configuration,
the MachineConfiguration version, and optional first-boot artifacts. Capacity
consumers create `MachineAllocation` objects that reference a class by UID, and
the provider adapter creates the host.

| Field | Meaning |
| -- | -- |
| `spec.siteRef.name` | Site that allocated machines join. Immutable. |
| `spec.provider` | Registered provider adapter name. Immutable. |
| `spec.providerConfigRef` | Reference to a namespaced `ConfigMap` interpreted by the adapter. |
| `spec.configurationRef` | MachineConfiguration version frozen into each allocation. |
| `spec.bootstrap.filesRef` | ConfigMap of first-boot files. Its `run` key is the script run as root before the agent installer. |
| `spec.bootstrap.agentArtifact` | Digest-pinned agent tarball URL and SHA-256. |

The class status publishes resolved offerings, the resolved revision, and the
object UIDs used to detect drift. That machinery exists so allocations can be
reproducible. It has nothing to do with whether a host is reachable over SSH,
which is why the register command does not touch it.

If you are deciding between the two, use `Machine` when the host exists and you
control its SSH access. Use `MachineClass` plus `MachineAllocation` when a
provider should create the host and you want the class to pin configuration and
artifacts.

### Credentials and key handling

SSH private keys are stored as Kubernetes Secrets in the machina namespace,
`unbounded-system` by default. `machine register` reads the key file from your
workstation and writes it under the `ssh-private-key` key of a Secret named
`ssh-<site>`, creating or updating the Secret with server-side apply.

That design has two consequences worth knowing.

First, the Secret is not tied to a single machine. Every machine registered in
the same site without an explicit `--ssh-secret-name` shares `ssh-<site>`. If
you rotate the key file and rerun register for one machine, the shared Secret
changes and the other machines in the site pick up the new key on their next
reconcile. Use `--ssh-secret-name` when machines need different credentials.

Second, machina reads the Secret from its own namespace. The controller resolves
the reference by name in the namespace it runs in, so a Secret placed in another
namespace will not be found even if `privateKeyRef.namespace` names it. Keep
`--namespace` aligned with the machina installation namespace.

The bootstrap token is a separate credential. `machine register` does not create
it. It looks up the newest unexpired token labeled with the site name in
`kube-system` and writes the token Secret name into
`spec.kubernetes.bootstrapTokenRef.name`. Machina then reads `token-id` and
`token-secret` from that Secret and passes the combined token to the agent. The
agent uses it to obtain kubelet credentials through the normal TLS bootstrap
flow, with the `system:bootstrappers:unbounded-agent-daemons` group.

Treat both Secrets as sensitive. The SSH private key grants root-equivalent
access to the host, and the bootstrap token grants the ability to join a Node to
the cluster. The machina RBAC in this repository grants the controller read
access to Secrets and no write access, so the controller can consume these
credentials but not create or modify them.

## Registering a machine

`kubectl unbounded machine register` is the command that creates the `Machine`.
It applies the SSH Secret and the Machine in one run, so a successful command
means the cluster has everything machina needs to start connecting.

```bash
kubectl unbounded machine register \
  --site dc-east \
  --host 192.168.50.21 \
  --ssh-username azureuser \
  --ssh-private-key ~/.ssh/dc-east_ed25519
```

The required flags are:

| Flag | Meaning |
| -- | -- |
| `--site` | Site that will own the machine. Selects the bootstrap token and prefixes the Machine name. |
| `--host` | Host and optional port, for example `10.0.0.5` or `10.0.0.5:2222`. |
| `--ssh-username` | Account machina uses to log in to the host. |

A key file is required unless you configure a bastion. Registration refuses to
run without `--ssh-private-key` when no bastion flags are present, because
there would be no way for machina to authenticate.

The optional flags you are most likely to use:

| Flag | Default | Meaning |
| -- | -- | -- |
| `--name` | derived from host | Machine name without the site prefix. |
| `--ssh-secret-name` | `ssh-<site>` | Secret that holds the SSH key. |
| `--namespace` | `unbounded-system` | Namespace for the Secret and the Machine reference. Must match the machina namespace. |
| `--kubeconfig` | current context | Path to the kubeconfig file. |
| `--node-label` | none | `key=value` label passed to kubelet. Repeatable. |
| `--agent-version` | latest release | Pin the agent release tag. |
| `--agent-base-url` | GitHub releases | Mirror or self-host the agent downloads. |
| `--agent-url` | none | Fully qualified agent tarball URL. Overrides the version and base URL. |

The command also accepts download overrides for the binaries the agent installs
into the rootfs: `--kubernetes-base-url`, `--kubernetes-url`,
`--kubernetes-binary-version`, `--containerd-base-url`, `--containerd-url`,
`--containerd-version`, `--runc-base-url`, `--runc-url`, `--runc-version`,
`--cni-base-url`, `--cni-url`, `--cni-version`, `--crictl-base-url`,
`--crictl-url`, and `--crictl-version`. These are the escape hatch for hosts
that cannot reach the public mirrors.

### Name derivation

Machine names are always prefixed with the site name. The prefix keeps names
unique across sites and makes it easy to list a site's machines with a label
selector. When `--name` is omitted, the suffix is derived from `--host` by
lowercasing it, replacing characters that are not valid in a Kubernetes name
with hyphens, collapsing repeated hyphens, and trimming leading and trailing
hyphens. A host of `192.168.50.21` in site `dc-east` therefore becomes
`dc-east-192.168.50.21`, and `10.0.0.5:2222` becomes `dc-east-10-0-0-5-2222`.

When you pass `--name`, the value is still prefixed. `--name node-01` in site
`dc-east` produces `dc-east-node-01`. Choose the name carefully if you plan to
manage the Node by name, because the Machine name is also the default Node name
when `spec.kubernetes.nodeRef` is empty.

The Machine is labeled `unbounded-cloud.io/site=<site>`, which is the canonical
site membership label. The net controller uses the same label on Nodes to scope
routing to a site.

### Example Machine manifest

The command is a convenience wrapper. If you manage manifests with Git, write
the `Machine` directly and apply it with `kubectl apply`. The shape below is what
the command produces, with a Secret named `ssh-dc-east` that holds the private
key under `ssh-private-key`.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: dc-east-node-01
  labels:
    unbounded-cloud.io/site: dc-east
spec:
  ssh:
    host: 192.168.50.21
    username: azureuser
    privateKeyRef:
      name: ssh-dc-east
      namespace: unbounded-system
      key: ssh-private-key
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

Two details in that example are worth calling out.

`spec.kubernetes.version` is optional. When it is set, the agent installs that
version. When it is empty, the controller fills the agent configuration from the
cluster's own version. The register command always writes the version it
discovered from the API server, so a manifest that omits it behaves slightly
differently from a command-created Machine but reaches the same result on a
cluster whose version is stable.

`spec.kubernetes.bootstrapTokenRef` is a reference, not a token value. The
Secret it names must exist in `kube-system`. The field is optional in the CRD,
but provisioning fails without it, so always set it.

You can also let machina write `nodeRef` back to the Machine. After the Node
appears, the controller sets `spec.kubernetes.nodeRef.name` to the Node name if
the field was empty, which makes the Machine to Node relationship explicit for
later operations.

### Bastion hosts

A bastion, or jump host, lets machina reach a machine that is not directly
reachable from the cluster. The SSH connection goes to the bastion first, then
opens a TCP tunnel to the target, then performs the SSH handshake with the
target through that tunnel. Both hops use public key authentication.

```bash
kubectl unbounded machine register \
  --site dc-east \
  --host 10.20.0.21 \
  --ssh-username ubuntu \
  --ssh-private-key ~/.ssh/target_ed25519 \
  --bastion-host 203.0.113.10:2222 \
  --bastion-ssh-username jumpuser \
  --bastion-ssh-private-key ~/.ssh/bastion_ed25519 \
  --bastion-ssh-secret-name ssh-dc-east-bastion
```

| Flag | Default | Meaning |
| -- | -- | -- |
| `--bastion-host` | none | Bastion host and optional port. Enables the bastion path. |
| `--bastion-ssh-username` | `--ssh-username` | Account for the bastion. |
| `--bastion-ssh-private-key` | `--ssh-private-key` | Key file for the bastion. |
| `--bastion-ssh-secret-name` | `--ssh-secret-name` | Secret for the bastion key. |

The defaults exist so a single key and account can be reused for both hops. If
the bastion uses the same credentials as the target, pass `--bastion-host` and
the same `--ssh-private-key`; the bastion username and key inherit the target
values. If the bastion needs different credentials, pass a different key file
and a different secret name. The command only writes a separate bastion Secret
when the bastion key file is set and the secret name differs from the host
secret; otherwise the Machine's bastion block omits `privateKeyRef` and machina
falls back to the parent key.

In the manifest, the bastion block mirrors the SSH block without a nested
bastion:

```yaml
spec:
  ssh:
    host: 10.20.0.21
    username: ubuntu
    privateKeyRef:
      name: ssh-dc-east
      namespace: unbounded-system
      key: ssh-private-key
    bastion:
      host: 203.0.113.10:2222
      username: jumpuser
      privateKeyRef:
        name: ssh-dc-east-bastion
        namespace: unbounded-system
        key: ssh-private-key
```

Reachability checks follow the same path. When a bastion is configured, the TCP
probe is dialed through the bastion's SSH tunnel rather than directly from the
controller pod, so a machine that is only reachable through the jump host can
still become `Ready`.

## Watching provisioning

After you register a Machine, machina reconciles it on a loop. You can follow
the whole sequence from the cluster with a watch.

```bash
watch 'kubectl get mach'
```

The `Phase` column is the quickest signal. The phases for an SSH machine are:

| Phase | Meaning |
| -- | -- |
| `Pending` | The machine is not reachable, or it was just created. The controller retries every 30 seconds. |
| `Provisioning` | SSH succeeded and the agent install script is running on the host. |
| `Joining` | The installer finished and the controller is waiting for a Node with the expected name. The controller checks every 30 seconds. |
| `Ready` | The Node exists. The controller rechecks every 5 minutes. |
| `Failed` | The last provisioning attempt failed or timed out. The controller retries every 1 minute. |
| `Rebooting` | Defined in the phase enum for hosts being restarted. SSH provisioning does not produce it. |

The transitions are driven by status conditions as well as phase. The most
useful conditions during a first join are `SSHReachable`, `Provisioning`,
`Provisioned`, and `AgentBootstrapped`. You can read them directly:

```bash
kubectl get mach dc-east-node-01 -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

The same information in a table is easier to scan while you wait:

```bash
kubectl get mach dc-east-node-01 \
  -o custom-columns='PHASE:.status.phase,MESSAGE:.status.message'
```

### What happens on the host

When the machine is reachable and has Kubernetes configuration, machina opens an
SSH session and performs four steps:

1. It copies the embedded installer to `/tmp/machina-agent-install.sh` and marks
   it executable.
2. It builds the agent configuration from the Machine, the cluster endpoint,
   and the bootstrap token, and uploads it to `/tmp/unbounded-agent.json`.
3. It runs the installer as root, with optional agent environment overrides
   prefixed to the command:
   `UNBOUNDED_AGENT_CONFIG_FILE="/tmp/unbounded-agent.json" sudo -E bash /tmp/machina-agent-install.sh`.
4. It removes both temporary files, whether the installer succeeds or fails.

The installer checks the architecture, downloads the agent tarball from the
release location, verifies the SHA-256 digest when one is supplied, extracts
the binary, runs `unbounded-agent preflight`, and then runs
`unbounded-agent start`. The preflight step is enabled by default and can be
disabled through the `AGENT_PREFLIGHT` environment variable, which the register
command does not expose. The important consequence for troubleshooting is that
the installer runs in the foreground of the SSH session, so its exit code is
what moves the Machine to `Joining` or `Failed`.

If the host needs a custom agent build or a mirror, the Machine's `spec.agent`
fields flow into the install script. `version` pins a release tag, `baseURL`
changes the release host, and `url` replaces the download URL entirely. When no
version is set, the script tracks the latest published release, so pin
`version` when you need repeatable installs.

### Provisioning timeout

The controller treats a provisioning attempt older than 5 minutes as timed out.
This matters when a reconcile pass starts an install and the controller restarts
before the install finishes: the Machine is left in `Provisioning` with a
`Provisioning` condition set to `True`. On the next reconcile the controller
compares the condition's last transition time with the timeout. If the attempt
is older than the timeout, the Machine moves to `Failed` with a message that the
provisioning timed out, and the normal retry loop takes over.

The timeout keeps a Machine whose controller restarted mid-install from staying
in `Provisioning` forever. It is a machina controller setting, not a Machine
field: the key is `provisioningTimeout` in the `machina-config` ConfigMap. If
your hosts are slow to download the agent, pin the download location to a local
mirror before you raise the timeout, because a long timeout also delays the
retry after a genuinely stuck install.

## Verifying the node joined

A Machine in `Joining` is waiting for a Node whose name matches
`spec.kubernetes.nodeRef.name`, or the Machine name when `nodeRef` is empty. The
register command does not pin a different node name, so the Node is normally
named exactly like the Machine, site prefix included.

Check the Node directly:

```bash
kubectl get node dc-east-node-01
```

The Machine writes the Node reference back when it finds the Node. A quick way
to confirm that the controller observed the join is:

```bash
kubectl get mach dc-east-node-01 \
  -o jsonpath='{.status.phase}{"\n"}{.spec.kubernetes.nodeRef.name}{"\n"}'
```

The first line should be `Ready`, and the second should match the Node name. Then
confirm that the Node is schedulable and carries the labels you expect:

```bash
kubectl get node dc-east-node-01 --show-labels
kubectl describe node dc-east-node-01
```

Labels you set with `--node-label` appear on the Node, along with provider
defaults and the site label applied by the networking controller. If a label is
missing, check that it was passed in `key=value` form and that the key was not
duplicated, because the register command rejects duplicate keys and malformed
pairs before it applies anything.

Finally, check that the Node is part of the site's networking. The net
controller labels Nodes with `unbounded-cloud.io/site`, and the node agent uses
that label to find its site assignment:

```bash
kubectl get node dc-east-node-01 -L unbounded-cloud.io/site
kubectl get site dc-east
```

If the Node is `Ready` but the site label is absent, the machine joined the
Kubernetes API but the networking agent has not completed its assignment. That
is a networking problem, not an SSH problem, and the networking operations
reference is the right next step.

## Troubleshooting

Most failures fall into a small set of buckets: the controller cannot reach the
host, it can reach the host but cannot authenticate, the installer fails on the
host, or the Node never registers. Work from the Machine status first, then the
controller log, then the host.

Start with the status message. It is written by the controller and usually names
the failing step:

```bash
kubectl get mach dc-east-node-01 -o jsonpath='{.status.phase}: {.status.message}{"\n"}'
```

Then read the controller log. Machina runs as `machina-controller` in the
system namespace:

```bash
kubectl -n unbounded-system logs deploy/machina-controller --tail=200
```

Add `-f` to follow a join in progress. The log lines include the machine name
and host, so you can filter if you run many machines:

```bash
kubectl -n unbounded-system logs deploy/machina-controller -f \
  | grep dc-east-node-01
```

### Machine stuck in Pending

A Machine in `Pending` is not reachable. The status message contains the dial
error, and the `SSHReachable` condition is `False` with reason `Unreachable`.
The controller retries every 30 seconds, so you can fix the network path and
watch the next attempt without touching the Machine.

Work through these causes in order:

- **Wrong address.** The address in `spec.ssh.host` must be reachable from the
  machina pod, not from your workstation. If the cluster uses a private network,
  a public address may route differently from the cluster than from your laptop.
- **Wrong port.** If SSH listens on a nonstandard port, include it as
  `host:port`. Without a port, machina dials 22.
- **Firewall or security group.** Allow inbound TCP from the cluster's pod or
  node network to the SSH port on the host.
- **Bastion missing.** If the host is only reachable through a jump host, the
  Machine needs `spec.ssh.bastion`. A direct dial will fail even though your
  interactive SSH works through the bastion.
- **Controller cannot resolve the name.** Use an IP address or a name that the
  cluster's DNS resolves.

Test the path from inside the cluster rather than from your machine:

```bash
kubectl -n unbounded-system run ssh-probe --rm -it --restart=Never \
  --image=busybox -- nc -w 3 192.168.50.21 22
```

A successful probe prints the host's SSH banner. A timeout or a `Connection
refused` error means the problem is network reachability, and no amount of key
or token work will help.

### When SSH is not possible

If the network path cannot be opened at all, SSH bootstrapping is the wrong
transport for that host. `kubectl unbounded machine manual-bootstrap` generates a
self-contained payload that you run on the host yourself, with the agent config
embedded. It is not a fix for a broken SSH path, but it keeps the same site,
token, and Node registration flow:

```bash
kubectl unbounded machine manual-bootstrap --site dc-east --variant script \
  > bootstrap.sh
```

Copy the script to the host and run it as root. Use `--variant cloud-init` or
`--variant ignition` when your provisioning system expects those formats.

### Machine stuck in Failed

`Failed` means an attempt to provision the machine returned an error. The
failure can happen before the SSH session opens, for example while resolving the
key Secret or the bootstrap token, or during the install itself. The status
message usually includes the specific error, and for install failures it
includes the installer's stderr. The controller retries every minute, so a
transient download failure often clears on its own.

The common causes are:

| Symptom in the message | Likely cause | Fix |
| -- | -- | -- |
| `get secret ... not found` | The SSH Secret is not in the machina namespace. | Rerun register with the correct `--namespace`, or move the Secret. |
| `key ... not found in secret` | The Secret exists but the key name does not match. | Set `key: ssh-private-key` in `privateKeyRef` when reusing a CLI-created Secret. |
| `parse SSH private key` | The key is encrypted, corrupt, or in an unsupported format. | Use an unencrypted Ed25519, RSA, or ECDSA private key. |
| `SSH dial` with an authentication error | The username or key does not match the account. | Check `spec.ssh.username` and that the public half of the key is in `authorized_keys`. |
| `sudo: a password is required` | The account cannot run sudo non-interactively. | Grant passwordless sudo for the account, or use an account that has it. |
| `unsupported architecture` | The host reports an architecture other than `x86_64` or `aarch64`. | Use a supported host. |
| `curl: command not found` or `tar: command not found` | The host image is missing a required tool. | Install `curl` and `tar`. |
| `Provisioning timed out after 5m0s` | The attempt outlived the 5 minute timeout. | Check host egress and mirror reachability, then let the retry run. |
| `Failed to get bootstrap token` | The token Secret is missing or expired. | Rerun `kubectl unbounded site init` for the site. |

Provisioning failures are retried, but a deterministic failure such as a bad
key will fail every time. Fix the input, then rerun the register command (or
patch the Machine) and let the next retry use the corrected values.

### Machine stuck in Joining

`Joining` means the installer exited successfully and machina is waiting for a
Node. The controller looks the Node up by name, using
`spec.kubernetes.nodeRef.name` when it is set and the Machine name otherwise.
A name mismatch is the most common reason a Node exists but the Machine never
notices.

Check for a Node with the expected name:

```bash
kubectl get node dc-east-node-01
kubectl get nodes -l unbounded-cloud.io/site=dc-east
```

If the Node is missing entirely, the agent did not finish its kubelet setup.
The agent has a command that reaches the node stack regardless of isolation
mode, so you do not have to guess whether kubelet runs on the host or inside an
nspawn machine. Run it as root on the host:

```bash
sudo unbounded-agent logs --unit kubelet --lines 200
```

Look for TLS bootstrap errors, token rejection, or an unreachable API server.
Other useful checks on the host:

```bash
sudo systemctl status unbounded-agent-daemon
sudo unbounded-agent preflight
```

The preflight command re-runs the same checks the installer performs. It is the
fastest way to see whether the host can reach the API server, whether the
required binaries exist, and whether the rootfs layout is usable.

If the Node exists but the Machine stays in `Joining`, check the name. A Machine
named `dc-east-node-01` with `nodeRef` set to something else will wait for the
`nodeRef` name even though a Node with the Machine name exists. Remove the
mismatched `nodeRef` or fix the Node name.

### Bootstrap token expiry

Bootstrap tokens are issued with a 24 hour expiration. The token refresher
component keeps the site token current for sites that enable it, which
`site init` does for the remote site. If a Machine still fails with a token
error, inspect the token Secret:

```bash
kubectl -n kube-system get secret bootstrap-token-abc123 \
  -o jsonpath='{.data.expiration}' | base64 -d
```

A missing or past expiration makes the token invalid. Rerunning
`kubectl unbounded site init` with the same flags is safe and issues a fresh
token when the site has no usable one. The token refresher then patches
`spec.kubernetes.bootstrapTokenRef` on the site's Machines to point at the new
Secret, so you normally do not need to re-register. If the site does not enable
the refresher, rerun `machine register` after `site init` so the Machine picks
up the new reference. Machines that already joined do not need the token
anymore, because the agent exchanges it for kubelet credentials during
bootstrap.

### API server connectivity

If the installer completes but the Node never registers, the host may not be
able to reach the API server. The agent configuration contains the API server
endpoint and the cluster CA, and the host uses them directly. A host that can
reach the internet but not the control plane's private endpoint will download
the agent and then fail at join.

Check from the host:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://<api-server-endpoint>/healthz
```

A `401` or `403` still proves connectivity. A timeout or DNS failure points at
routing, DNS, or a firewall. The endpoint comes from the machina controller
configuration, not from the Machine. If it was wrong when the operator was
installed, run `kubectl unbounded install --api-server-endpoint <endpoint>` with
the correct value and let the Machine retry.

### SSH host keys and key security

Machina disables SSH host key verification. There is no `known_hosts` entry to
populate and a host key change will not block provisioning. That tradeoff keeps
bootstrap self-contained, but it means the controller trusts the network path to
the host. If you need host identity verification, place the host behind a
bastion you control and restrict who can reach the SSH port.

Private keys are stored as Kubernetes Secrets, so anyone who can read Secrets in
the machina namespace can read them. Protect that namespace with RBAC, avoid
sharing one key across unrelated sites, and rotate `ssh-<site>` when a key is
exposed. Rerunning register after rotation updates the Secret for every Machine
that references it.

Passphrase-protected keys do not work, because the controller has no way to
supply the passphrase. Generate a dedicated unencrypted key for machina:

```bash
ssh-keygen -t ed25519 -N '' -f ~/.ssh/dc-east_ed25519
ssh-copy-id -i ~/.ssh/dc-east_ed25519.pub azureuser@192.168.50.21
```

Then register with the private key file. The public key must be in the target
account's `authorized_keys`, and the account must be allowed by the SSH server's
configuration.

## Related

- [Getting Started]({{< relref "guides/getting-started" >}}) for the end-to-end
  quickstart that includes an SSH node.
- [Existing Cluster]({{< relref "guides/existing-cluster" >}}) for attaching
  Unbounded to a control plane you already run.
- [Networking Concepts]({{< relref "concepts/networking" >}}) for how sites,
  gateways, and tunnels fit together.
- [Node Operations]({{< relref "guides/operations/node-operations" >}}) for
  day-two actions such as reboots, repaves, and agent upgrades.
- [Machine CRD Reference]({{< relref "reference/machina-crd" >}}) for every
  field on `Machine`, `MachineClass`, and the related resources.
- [CLI Reference]({{< relref "reference/cli" >}}) for the full flag list on
  `install`, `site init`, and `machine register`.
- [Architecture]({{< relref "reference/architecture" >}}) for how the
  controller, operator, and node agent interact.
- [Agent Configuration]({{< relref "reference/agent/configuration" >}}) for the
  agent configuration the controller generates.

