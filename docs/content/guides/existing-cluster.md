---
title: "Bring Your Own Cluster"
description: "Attach remote worker nodes to an existing Kubernetes cluster with Unbounded, from installing the operator and creating a Site to joining machines, verifying scheduling, upgrading, and removing them."
weight: 2
---

This guide shows how to add remote worker nodes to a Kubernetes cluster you already run. You keep your existing control plane, your API server, and your scheduler. Unbounded adds a small set of custom resources and controllers, then provisions Linux hosts elsewhere so their kubelets join your cluster as ordinary Nodes.

The guide follows the order you should work in. You install the command line plugin, check that the cluster meets the prerequisites, label the nodes that will act as gateways, install the operator, create a Site, choose the optional components, prepare the hosts, register the first Machine, and verify that workloads schedule onto it. Later sections cover the networking decisions that are hard to change later, upgrades, and removal.

## What You'll Do

1. Install the `kubectl unbounded` plugin and confirm the cluster meets the requirements.
2. Label the cluster nodes that will carry tunnel traffic from remote sites.
3. Install `unbounded-operator` and the custom resource definitions it owns.
4. Create a Site that describes the remote location and its CIDR ranges.
5. Decide which optional components the Site runs.
6. Prepare Linux hosts with SSH access and the packages the agent needs.
7. Register the first Machine and watch it reach the `Ready` phase.
8. Schedule a workload onto the new Node and confirm it runs.
9. Plan upgrades and, when the time comes, remove the Site cleanly.

If you are new to the project, read [Getting Started]({{< relref "guides/getting-started" >}}) first for the end-to-end Azure example, or [Concepts]({{< relref "concepts" >}}) for the architecture.

## What Bringing Your Own Cluster Means

Unbounded does not build or replace a control plane. Your existing cluster stays authoritative: the same API server accepts Node registrations, the same scheduler places Pods, and the same controllers reconcile workloads. What Unbounded adds is a way to produce worker Nodes from hosts that are not part of the cluster's own infrastructure, and a network data plane that connects those Nodes back to the cluster.

The unit of organization is the `Site`. A Site is a cluster-scoped resource that names a location, the CIDR ranges used there, and the optional components enabled for it. `site init` always creates two Sites: one named `cluster` that describes the nodes already attached to your control plane, and one named after the remote location you are adding. When a Node's internal IP falls inside a Site's `spec.nodeCidrs`, the networking controller labels that Node with `unbounded-cloud.io/site` and treats it as a member of that Site.

Each remote host runs the `unbounded-agent`. The agent prepares a node root filesystem, installs a kubelet, and joins the Node using a bootstrap token that belongs to the Site. By default the node stack runs inside a `systemd-nspawn` machine, so the host operating system stays separate from the Kubernetes node. One controller, `machina`, owns the host lifecycle; another, `unbounded-net`, owns the tunnel and routing layer. You configure both through Kubernetes objects rather than by logging into hosts.

The mental model that keeps the rest of this guide simple is this: a `Site` is the boundary for addressing and components, a `Machine` is the desired state of one host and its Node, and a `GatewayPool` is the set of existing cluster nodes that remote Sites dial to reach cluster-internal addresses. Everything else is a detail of how those three objects are reconciled.

## 1. Install the Prerequisites

### Requirements for the Existing Cluster

Before you install anything, check the cluster itself. Most failed first joins trace back to one of these conditions, and each one is cheaper to fix now than after a Site exists.

| Requirement | Why it matters |
| --- | --- |
| A working kubeconfig | `kubectl` must reach the API server. Every command in this guide accepts `--kubeconfig` when you do not use the default context. |
| Cluster-admin equivalent permissions | Installation creates a namespace, RBAC objects, and a Deployment, and the operator then creates the custom resource definitions it owns. |
| An API server endpoint reachable from remote hosts | Remote kubelets and the agent must reach the API server over HTTPS. See [API server reachability](#api-server-reachability). |
| WireGuard support on gateway nodes | Gateway nodes terminate tunnels. The Linux kernel must provide WireGuard, either built in or as a module. |
| UDP 51820-51899 open to gateway nodes | The mesh listens on 51820 and the controller allocates gateway ports starting at 51821. |
| Non-overlapping CIDR ranges | Remote node and Pod CIDRs must not overlap the cluster's own node or Pod CIDRs, or any other Site. The validation webhook rejects overlap. |
| A decision about the CNI | Keep your existing CNI, or let Unbounded Net manage intra-site networking. See [Choosing a CNI mode](#choosing-a-cni-mode). |

The guide assumes a conformant Kubernetes release. There is no separate minimum version check in the plugin: `machine register` reads the API server's Git version and records it as the desired kubelet version, so the agent installs the same minor and patch release as your control plane. If you need a different node binary version, override it at registration time with `--kubernetes-binary-version` or by setting `spec.kubernetes.version` on the `Machine`.

### API server reachability

The agent on each host needs a stable address for your API server. The operator discovers one in two ways. It reads the `kube-public/cluster-info` ConfigMap when your cluster publishes it, which is the usual case for self-managed clusters. On clusters that do not publish `cluster-info`, such as AKS, it falls back to the `KUBERNETES_SERVICE_HOST` fully qualified name.

If neither source produces an address that remote hosts can reach, set the address explicitly when you install the operator:

```bash
kubectl unbounded install --api-server-endpoint https://api.example.com:6443
```

That value is stored in the `unbounded-operator-config` ConfigMap and is preserved across reinstalls, so you only need to set it once. A wrong endpoint shows up as a Machine that provisions but never registers a Node, because the kubelet cannot complete TLS bootstrap.

### Choosing a CNI mode

Unbounded Net can own intra-site pod networking, or it can leave that to a CNI you already run. The choice is recorded on the Site as `spec.manageCniPlugin`, and `site init` writes it from the `--manage-cni-plugin` flag.

When the field is true, the default, the node agent writes CNI configuration and creates tunnel endpoints between nodes in the same Site. When the field is false, the agent creates only the tunnel links to gateway pools for other Sites, and an external CNI such as Cilium or Calico handles pod-to-pod traffic within the Site. The Pod CIDR assignments are still required in both modes, because the controller uses them to route traffic between Sites.

If you keep an existing CNI, make sure its agents run on the remote Nodes as well. A CNI that is only installed on your existing node pool will not configure Pod networking on hosts that join later. Installing the CNI as a DaemonSet that tolerates the remote Node labels is the usual approach.

### Firewall and address planning

Open UDP 51820 through 51899 inbound on the nodes that will act as gateways, and allow the same range outbound from remote hosts. Port 51820 is the mesh listen port; the gateway pool controller assigns each gateway node a unique additional port beginning at 51821, which is why the documented window is wider than a single port.

Remote hosts also need outbound HTTPS to the API server, to the release locations for the agent and node binaries, and to your container registry. If you mirror those artifacts, the `machine register` flags described later let you point each download at your own host.

Plan addresses before you create the Site. Pick a node CIDR for the remote location and a Pod CIDR for it, and confirm that neither overlaps the cluster node CIDR, the cluster Pod CIDR, the Service CIDR, or any other Site you may add later. Overlap is rejected at admission time, and it is much easier to choose correctly now than to renumber a Site with running Nodes.

### Install the CLI

Install the `kubectl unbounded` plugin and put it on your `PATH`. The download archive is named after the operating system and architecture:

| Platform | Command |
| --- | --- |
| Linux amd64 | `curl -sSfL https://get.unbounded.cloud/kubectl-unbounded-linux-amd64.tar.gz \| tar -xzC /usr/local/bin` |
| macOS arm64 | `curl -sSfL https://get.unbounded.cloud/kubectl-unbounded-darwin-arm64.tar.gz \| tar -xzC /usr/local/bin` |

Release archives are also published on the project's GitHub releases with a `checksums.txt` file. The [CLI Reference]({{< relref "reference/cli" >}}) covers the archive layout, checksum verification, and installation through Krew.

Verify that `kubectl` discovers the plugin:

```bash
kubectl unbounded version
```

The command prints the version, commit, and build timestamp. If `kubectl` reports that `unbounded` is not a known command, the binary is not on `PATH` or is not named with the `kubectl-` prefix.

## 2. Prepare Gateway Nodes

Remote Sites reach the rest of the cluster through a gateway pool. A gateway is an ordinary node in your existing cluster that runs the Unbounded Net node agent and has a network path that remote hosts can reach. You select gateways with a label, not by editing a manifest.

Apply the gateway label to each node you want to use. Replace `<node>` with the node name:

```bash
kubectl label node <node> unbounded-cloud.io/unbounded-net-gateway=true
```

`site init` creates a `GatewayPool` named `gw-main` whose selector is exactly that label. The pool has type `External`, which means the controller only includes nodes that publish both an external IP address and a WireGuard public key. The node agent publishes the key once it runs on the node, so a freshly labeled node can take a minute or two to appear in the pool.

Open UDP 51820 through 51899 inbound on each gateway node in your cloud firewall, security group, or network policy. The mesh listens on 51820. The controller allocates each gateway an additional port for gateway-to-gateway and node-to-gateway peering, starting at 51821, so restricting traffic to 51820 alone breaks multi-site paths.

Verify the label and the pool membership:

```bash
kubectl get nodes -l unbounded-cloud.io/unbounded-net-gateway=true
kubectl get gatewaypools
kubectl get gatewaypoolnodes
```

A node that carries the label but never appears in the pool is missing one of the two requirements. Check that the node has an `ExternalIP` address and that the node agent is running in the `unbounded-system` namespace. The agent's status is also summarized by `kubectl unbounded net node list`, which shows each node's Site, gateway pool, and CNI status.

Run at least two gateway nodes in production. A single gateway is a single failure domain for every remote Site that uses it, and the pool does not create capacity that does not exist. Gateways do not need to be dedicated: a normal worker node can carry the label as long as it has the network path and the kernel support.

## 3. Install the Operator

The operator is the component that owns custom resource definitions and reconciles the workloads described by a Site. Install it before you create a Site. `site init` checks API discovery for the types it is about to apply and fails with a clear message when the operator has not been installed.

### Run the installer

```bash
kubectl unbounded install
```

The command applies the operator manifests into the `unbounded-system` namespace. That includes the namespace itself, the operator ServiceAccount and RBAC, a ConfigMap holding the component image registry and API server endpoint, and the operator Deployment. The operator bootstraps the custom resource definitions at startup, then begins reconciling Sites.

By default the command waits for the operator rollout to complete and for every required CRD to become `Established`, with a five minute timeout. A successful exit means the API server now serves the Unbounded resource types.

### Installer flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig` | `~/.kube/config` or `KUBECONFIG` | Path to the kubeconfig used for the installation. |
| `--namespace` | `unbounded-system` | Namespace for the operator and components. Legacy namespace names are refused because the operator's migration reaper deletes them. |
| `--operator-image` | build-time default | Operator image name and tag with no registry, for example `unbounded-operator:v1`. |
| `--image-registry` | build-time default | Full repository prefix, including registry host and organization, used for the operator and every component it deploys. |
| `--api-server-endpoint` | auto-discovered | API server address advertised to provisioned machines. |
| `--wait` | `true` | Wait for the operator rollout and CRDs. |
| `--timeout` | `5m` | Upper bound for those waits. |

`--operator-image` deliberately cannot carry a registry. The registry always comes from `--image-registry` or from the value baked into the binary at build time, which keeps the operator and the components it deploys on the same registry.

### Verify the installation

```bash
kubectl -n unbounded-system rollout status deployment/unbounded-operator
kubectl get crd | grep unbounded-cloud.io
kubectl get sites
```

The last command should succeed and return an empty list, not a "server could not find the requested resource" error. If the installer timed out, inspect the operator pod logs before retrying:

```bash
kubectl -n unbounded-system get pods
kubectl -n unbounded-system logs deployment/unbounded-operator
```

The most common causes are an image that cannot be pulled from your network, insufficient permissions on the installing identity, and a cluster that does not allow the namespace or RBAC objects the installer applies.

## 4. Initialize a Site

A Site organizes remote machines into a virtual data center. It carries the addressing for the location, the components enabled there, and the labels that tie Nodes and Machines back to it. You create it with `kubectl unbounded site init`, which is idempotent: rerunning the same command applies the same objects again.

### Plan the four CIDR ranges

`site init` requires four CIDR flags. Two describe the cluster you already have, and two describe the remote location:

| Flag | Describes | Example |
| --- | --- | --- |
| `--cluster-node-cidr` | Address range of the existing cluster's node IPs. | `10.0.0.0/16` |
| `--cluster-pod-cidr` | Address range the existing cluster allocates to Pods. | `10.0.1.0/16` |
| `--node-cidr` | Address range for node IPs at the remote Site. | `10.244.0.0/16` |
| `--pod-cidr` | Address range to allocate to Pods at the remote Site. | `10.244.1.0/16` |

The pairs must not overlap each other, and the remote pair must not overlap any other Site. The controller matches a Node to a Site by testing the Node's internal IP against `spec.nodeCidrs`, so the `--node-cidr` value must cover the addresses your hosts actually use. If you later add a host outside that range, it will not be recognized as a member of the Site.

The Pod CIDR becomes a `podCidrAssignments` entry. Each Node receives a block from that pool, and the block size is what makes the range usable. When you create the Site by hand you can set `nodeBlockSizes`, for example `ipv4: 24`, to fix the per-node mask. The `site init` template leaves the block size to the controller default, which is a `/24` for IPv4, so start there unless you have a specific reason to change it.

### Create the Site

Run `site init` from a terminal that can reach the cluster. The example below uses four distinct ranges and opts out of CNI management, which is the right choice when your cluster already runs Cilium or Calico:

```bash
kubectl unbounded site init --name my-site \
  --kubeconfig ~/.kube/config \
  --cluster-node-cidr 10.0.0.0/16 \
  --cluster-pod-cidr 10.0.1.0/16 \
  --node-cidr 10.244.0.0/16 \
  --pod-cidr 10.244.1.0/16 \
  --manage-cni-plugin=false
```

If you want Unbounded Net to manage intra-site pod networking, omit `--manage-cni-plugin=false`; the flag defaults to true. If you plan to add bare-metal hosts through PXE, add `--enable-metalman=true`, which enables the Metalman component for the new Site. Metalman is not needed for SSH-provisioned virtual machines or servers.

### Site init flags

| Flag | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--name` | yes | none | Name of the remote Site and the prefix for its Machines. |
| `--cluster-node-cidr` | yes | none | Node CIDR of the existing cluster. |
| `--cluster-pod-cidr` | yes | none | Pod CIDR of the existing cluster. |
| `--node-cidr` | yes | none | Node CIDR for the remote Site. Must be a valid IPv4 CIDR. |
| `--pod-cidr` | yes | none | Pod CIDR for the remote Site. Must be a valid IPv4 CIDR. |
| `--manage-cni-plugin` | no | `true` | Whether Unbounded Net manages CNI configuration and same-site tunnels. |
| `--enable-metalman` | no | `false` | Enable the Metalman PXE controller for the Site. |
| `--kubeconfig` | no | `~/.kube/config` or `KUBECONFIG` | Path to the kubeconfig. |

The command validates every CIDR for IPv4 syntax and readability of the kubeconfig before it changes the cluster. It then verifies that the API server serves the types it is about to apply. If the operator is missing, the error names the missing types and tells you to run `kubectl unbounded install` first.

### What the command creates

`site init` applies a small set of objects and leaves the rest to the operator:

| Object | Name | Purpose |
| --- | --- | --- |
| `Site` | `cluster` | Describes the nodes already attached to your control plane, using the two cluster CIDRs. |
| `Site` | the value of `--name` | Describes the remote location, using the two Site CIDRs. |
| `GatewayPool` | `gw-main` | Selects nodes labeled `unbounded-cloud.io/unbounded-net-gateway=true` and marks the pool as `External`. |
| `SiteGatewayPoolAssignment` | one per Site | Attaches both Sites to `gw-main`. |
| `Secret` | `bootstrap-token-<id>` in `kube-system` | The join credential for the Site, labeled `unbounded-cloud.io/site=<name>`. |

The remote Site that results from the example looks like this. It is shown so you can recognize the shape, not because you need to write it by hand:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: my-site
  labels:
    unbounded-cloud.io/site: my-site
spec:
  components:
    metalman:
      enabled: false
    tokenRefresher:
      enabled: true
  manageCniPlugin: false
  nodeCidrs:
    - 10.244.0.0/16
  podCidrAssignments:
    - cidrBlocks:
        - 10.244.1.0/16
```

Two details are easy to miss. Unbounded Net and Machina do not appear in `spec.components`: they are required cluster singletons that the operator deploys whenever at least one Site exists. And `manageCniPlugin: false` is written only when you pass the flag; when it is absent, the behavior defaults to true.

The gateway pool and its assignment are separate objects because a pool can serve several Sites:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPool
metadata:
  name: gw-main
spec:
  nodeSelector:
    unbounded-cloud.io/unbounded-net-gateway: "true"
  type: External
---
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: my-site
spec:
  sites:
    - my-site
  gatewayPools:
    - gw-main
```

The bootstrap token is the credential a Machine uses to obtain kubelet certificates. `site init` creates one for the Site if none exists, and the token refresher keeps it valid. `machine register` looks up the newest unexpired token labeled with the Site name, so a Site that has no token cannot register Machines.

### Verify the Site

```bash
kubectl get sites
kubectl get site my-site -o yaml
kubectl get gatewaypools
kubectl get sitegatewaypoolassignments
```

`kubectl get sites` prints the node CIDRs, pod CIDR assignments, node count, slice count, and age. With `-o wide` it also prints the Metalman, Gantry, token refresher, and override phases.

The Site status publishes one condition per component. Wait for the components you enabled:

```bash
kubectl wait --for=condition=NetReady site/my-site --timeout=10m
kubectl wait --for=condition=MachinaReady site/my-site --timeout=10m
```

The condition names are `NetReady`, `MachinaReady`, `MetalmanReady`, `MetalmanBootstrapReady`, `GantryReady`, `TokenRefresherReady`, and `RacerReady`, depending on which components are present. A condition that stays false usually means the operator cannot pull a component image or cannot create the workload; `kubectl -n unbounded-system get pods` and the Site's condition message point at which one.

If you need to change a CIDR later, treat it as a planned migration rather than a rerun. Existing Nodes are matched to a Site by internal IP, and the validation webhook rejects overlap with other Sites, so renumbering a Site with running Nodes requires moving the Nodes to addresses in the new range.

## 5. Choose Components

The operator deploys a fixed set of cluster singletons plus optional per-Site components. Two of them are not choices. Unbounded Net provides the tunnel and routing layer, and Machina owns host provisioning; both are deployed as soon as any Site exists and cannot be disabled through the Site spec. The `components.machina` field exists only for compatibility with older manifests and is ignored.

The remaining components are declared in `spec.components`:

| Component | Default | Scope | What it does |
| --- | --- | --- | --- |
| `metalman` | disabled | per Site | Runs the PXE boot and bare-metal lifecycle controller for the Site. |
| `gantry` | enabled | cluster singleton | Runs the peer-to-peer OCI distribution agent that reduces repeated image pulls. |
| `tokenRefresher` | enabled | cluster singleton | Keeps a valid bootstrap token available for each non-cluster Site. |

Because Gantry and the token refresher are cluster-wide singletons, enabling them for one Site enables the singleton; the per-Site field decides whether a Site asks for it. The token refresher ignores the reserved `cluster` Site and only acts for Sites that join machines. Gantry is enabled unless a Site explicitly sets `components.gantry.enabled` to false, so opting out has to be deliberate.

A Site that runs only SSH-provisioned virtual machines or servers does not need Metalman. Add it when you plan to boot physical hosts over the network, and give it a replica count and DHCP interface setting if the defaults do not fit your network:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: my-site
spec:
  nodeCidrs:
    - 10.244.0.0/16
  podCidrAssignments:
    - cidrBlocks:
        - 10.244.1.0/16
  components:
    metalman:
      enabled: true
      replicas: 2
    gantry:
      enabled: true
    tokenRefresher:
      enabled: true
```

`metalman.replicas` defaults to one when omitted. `metalman.dhcpAutoInterface` lets Metalman choose the DHCP interface instead of requiring one to be named. Gantry's only nested setting is `artifactStreaming`, which configures it as the node-local range proxy for AKS ACR Artifact Streaming and requires a node selector that matches nodes with Artifact Streaming enabled. Leave it out unless you are using that feature.

Components install into `unbounded-system` at the operator's own version. Neither the namespace nor the image is configurable through the Site, by design. If you need to adjust resources, node selectors, or tolerations for a component workload, use the cluster-scoped workload overrides ConfigMap described in [Workload Overrides]({{< relref "reference/workload-overrides" >}}). Write access to that ConfigMap is equivalent to cluster-admin, so treat it as a privileged surface.

To confirm what a Site selected:

```bash
kubectl get site my-site -o wide
```

The wide view adds the Metalman, Gantry, and token refresher columns, plus the aggregate override phase. A component that is enabled but not ready shows up in the Site conditions rather than in these columns.

## 6. Prepare Nodes

You can prepare any Linux host that satisfies the requirements below. The agent installs Kubernetes and the container runtime into a node root filesystem, or into host-native paths if you choose the `none` isolation mode, so you do not install either by hand. The agent also installs the CNI plugin binaries, but if you chose an external CNI mode, that CNI's own agents still have to run on the Node.

### Host requirements

| Requirement | Detail |
| --- | --- |
| Architecture | `x86_64` or `aarch64`. The installer maps these to `amd64` and `arm64` and rejects anything else. |
| Operating system | A systemd-based Linux distribution. The agent installs systemd units and, by default, runs the node inside a `systemd-nspawn` machine. |
| Shell and tools | `bash`, `curl`, `tar`, and `sudo`, usable by the SSH account. |
| Root access | The SSH account must run `sudo` without an interactive password prompt. Machina runs the installer in a non-interactive SSH session, so a prompt fails the join. |
| Network path to the API server | Outbound HTTPS to the API server endpoint. |
| Download access | Outbound HTTPS to the agent and node binary sources, or a mirror you point the registration at. |
| Disk | Writable space under `/var/lib/unbounded` for the agent and node runtime, plus space for container images. |
| Package manager or preinstalled tools | `curl`, `nftables`, `util-linux`, and, in the default `nspawn` mode, `systemd-container`. A package-managed host can install them during bootstrap. |

The agent checks these conditions itself through its preflight suite. If a check fails after a join attempt, `sudo unbounded-agent preflight` on the host reproduces the same findings with remediation text. The [Agent Preflight]({{< relref "reference/agent/preflight" >}}) reference lists every check and its severity.

### Remove conflicts

A host that already runs a kubelet, containerd, or Docker is not a clean target. Bootstrap disables active units it finds, but a kubelet that the agent does not own can race with the node stack during the join and leave the Node flapping. Use a host that is not already a member of another cluster. If that is impossible, stop and disable the existing units before you register the host.

The `none` isolation mode is stricter: it refuses a host that has a kubelet or containerd the agent does not own. The default `nspawn` mode runs the node stack in its own machine, which keeps the host's own packages separate, but the agent still disables conflicting services during bootstrap, so do not depend on them.

### SSH access

Machina connects to the host from inside the cluster. The address you pass to `machine register` must be reachable from the `unbounded-system` namespace, either directly or through a bastion host that machina can also reach. A private address that only your laptop can see will leave the Machine in `Pending` with an SSH reachability message.

Confirm the path before registering. From a terminal that can reach the host the same way machina will:

```bash
ssh -i ~/.ssh/my-site_ed25519 -o BatchMode=yes <username>@<host> true
ssh -i ~/.ssh/my-site_ed25519 <username>@<host> sudo -n true
```

`BatchMode=yes` proves that key authentication works without a prompt, and `sudo -n` proves that root commands work without one. If either fails, fix it on the host or in the network before you continue.

When the host is reachable only through a jump host, register both addresses and both credentials. Machina opens the bastion connection first and then connects to the host, and it stores the credentials so later day-two operations reuse them.

### Choose the node name

A Machine's name is prefixed with the Site name. If you pass `--name worker-01` to `--site my-site`, the Machine and the Node are both named `my-site-worker-01`. If you omit `--name`, the name is derived from the host, so `10.0.0.5:2222` becomes `my-site-10.0.0.5-2222` after sanitization.

That name is the Node name the scheduler and `kubectl` will use. Choose a name that is stable and meaningful before the first join, because changing it later means deleting the Node and rejoining. Node labels can be added at registration with the repeatable `--node-label` flag, which is useful for targeting workloads to a site without hard-coding node names.

## 7. Join the First Machine

`machine register` creates a `Machine` resource and, when you supply a key file, the SSH Secret that machina uses to reach the host. It does not wait for provisioning. A successful command means the cluster accepted the desired state; the controller does the rest asynchronously.

### Register the host

The minimal registration needs a Site, a reachable host, and an SSH account with a key:

```bash
kubectl unbounded machine register \
  --site my-site \
  --host <host>[:<port>] \
  --ssh-username <username> \
  --ssh-private-key <path-to-private-key>
```

Add a stable name and labels when you want scheduling to target the machine:

```bash
kubectl unbounded machine register \
  --site my-site \
  --host 10.0.0.5 \
  --name worker-01 \
  --ssh-username ubuntu \
  --ssh-private-key ~/.ssh/my-site_ed25519 \
  --node-label topology.kubernetes.io/region=lab \
  --node-label pool=workers
```

When the host is behind a jump host, supply the bastion address with its port and the credentials machina should use for it:

```bash
kubectl unbounded machine register \
  --site my-site \
  --host 10.0.2.15 \
  --ssh-username ubuntu \
  --ssh-private-key ~/.ssh/my-site_ed25519 \
  --bastion-host 203.0.113.10:2222 \
  --bastion-ssh-username jumpuser \
  --bastion-ssh-private-key ~/.ssh/bastion_ed25519 \
  --bastion-ssh-secret-name bastion-ssh-my-site
```

There is no separate bastion port flag. The port belongs in the `--bastion-host` value, and the host port belongs in `--host`. If you omit the bastion username, key, or secret name, each defaults to the corresponding host value.

### Registration flags

| Flag | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--site` | yes | none | Site that owns the Machine and supplies the bootstrap token. |
| `--host` | yes | none | Host and optional port, for example `10.0.0.5` or `10.0.0.5:2222`. |
| `--ssh-username` | yes | none | Account machina uses to log in. |
| `--ssh-private-key` | unless a bastion is set | none | Path to the private key file. It is stored in a Secret. |
| `--name` | no | derived from host | Machine name without the Site prefix. |
| `--ssh-secret-name` | no | `ssh-<site>` | Secret that holds the SSH key, in the component namespace. |
| `--namespace` | no | `unbounded-system` | Namespace for the SSH Secret and the Machine reference. It must match the namespace machina runs in. |
| `--kubeconfig` | no | current context | Path to the kubeconfig used to create the Machine. |
| `--node-label` | no | none | `key=value` label passed to kubelet. Repeatable. |
| `--bastion-host` | no | none | Bastion address with optional port. |
| `--bastion-ssh-username` | no | `--ssh-username` | Account for the bastion. |
| `--bastion-ssh-private-key` | no | `--ssh-private-key` | Key file for the bastion. |
| `--bastion-ssh-secret-name` | no | `--ssh-secret-name` | Secret for the bastion key. |

The command also accepts download overrides for the agent and for the binaries the agent installs into the node root filesystem. `--agent-version`, `--agent-base-url`, and `--agent-url` control the host-side agent tarball. The `--kubernetes-*`, `--containerd-*`, `--runc-*`, `--cni-*`, and `--crictl-*` families point individual downloads at a mirror. Leave them unset unless you are installing in an environment without access to the public release hosts.

### What happens after registration

The controller writes the desired state and then works through a state machine:

| Phase | Meaning |
| --- | --- |
| `Pending` | The Machine exists but the host is not reachable over SSH yet. The status message carries the probe error. |
| `Provisioning` | Machina is connected and is installing the agent and building the node root filesystem. |
| `Joining` | Provisioning succeeded and the controller is waiting for a Node with the expected name to register. |
| `Ready` | The Node object exists. This phase does not require the Node to be healthy. |
| `Rebooting` | The host is being restarted as part of an operation. |
| `Failed` | Provisioning or a later operation failed. The status message names the cause. |

A `Ready` Machine is not the same as a Ready Node. The controller transitions to `Ready` when the Node object appears, not when its `Ready` condition becomes true. Always confirm the Node separately before you conclude that the join succeeded.

### Watch the join

```bash
kubectl get machines -w
kubectl describe machine my-site-worker-01
```

The Machine printer columns are name, host, phase, Kubernetes version, and age. The `host` column shows the SSH host from the spec, which is useful when the Machine name and the address differ.

Follow the conditions for more detail than the phase gives:

```bash
kubectl get machine my-site-worker-01 \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

The conditions you encounter on a first join are `SSHReachable`, `Provisioning`, `Provisioned`, `AgentBootstrapped`, and, once the node exists, the node lifecycle conditions. A `Provisioned` condition that is true with a `Joining` phase means the host side worked and the problem is now between the kubelet and the API server.

### Common failures

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Stays `Pending` with an SSH error | Address is not reachable from the cluster, key is wrong, or the account needs a password. | Test the same path with `ssh -o BatchMode=yes`, then check firewall rules and the key in the SSH Secret. |
| `Failed` during `Provisioning` | Download failure, unsupported architecture, read-only filesystem, or missing `sudo`. | Read `status.message`; run `unbounded-agent preflight` on the host if you can log in. |
| Stays `Joining` | Kubelet cannot reach the API server, bootstrap token expired, or the Node is not registering under the expected name. | Check the API server endpoint and the token Secret in `kube-system`; inspect kubelet logs on the host. |
| `Ready` but the Node is `NotReady` | Pod networking is not configured, or the CNI agents are not running on the remote node. | Check the CNI DaemonSet and the node's conditions and events. |

A provisioning attempt that exceeds the controller's timeout moves to `Failed` rather than waiting forever, which lets the normal retry path pick it up. If you fix the host condition, the controller retries from the failed state without requiring you to delete the Machine.

## 8. Verify Workloads Schedule

A join is complete when the Node is Ready and a Pod can actually run on it. Check both, because the Machine phase alone does not prove either.

### Confirm the Node

```bash
kubectl get node my-site-worker-01
kubectl wait --for=condition=Ready node/my-site-worker-01 --timeout=15m
kubectl get node my-site-worker-01 -L unbounded-cloud.io/site
```

The last command prints the Site label the networking controller applied. If the label is empty, the Node's internal IP does not match any Site's `spec.nodeCidrs`, and the Node is not receiving Site-scoped networking. That is the single most common reason a Node joins but cannot reach cluster Pods or Services.

Unless you set `spec.kubernetes.registerWithTaints` on the Machine or added taints another way, the Node registers untainted and accepts workloads immediately. If your cluster uses taints for isolation, add tolerations to the workloads you intend to place on remote Nodes rather than removing the taints.

### Run a test workload

Pin a small Deployment to the new Node so the result is unambiguous:

```bash
kubectl create deployment join-check \
  --image=busybox:1.36 \
  --replicas=1 \
  -- /bin/sh -c 'sleep 3600'
kubectl patch deployment join-check --type=merge -p '{
  "spec": {
    "template": {
      "spec": {
        "nodeSelector": {
          "kubernetes.io/hostname": "my-site-worker-01"
        }
      }
    }
  }
}'
kubectl rollout status deployment/join-check --timeout=5m
kubectl get pods -l app=join-check -o wide
```

The Pod should be `Running`, and its IP should fall inside the Site's Pod CIDR. Read the logs to confirm the container started rather than crash-looping:

```bash
kubectl logs deployment/join-check
```

If the Pod stays `Pending`, `kubectl describe pod` names the reason. An unsatisfiable `nodeSelector` or a missing toleration is a scheduling problem, not a networking problem. If the Pod is `Running` but never becomes ready, the container itself is failing.

A `ContainerCreating` Pod that stays there for minutes usually means the CNI has not finished configuring the sandbox, or the image cannot be pulled from the remote location. Check the CNI DaemonSet on the Node and confirm that the registry is reachable from the Site.

### Check cluster connectivity

Scheduling proves the kubelet and scheduler agree. It does not prove that the remote Pod can reach the rest of the cluster. Create a Pod on the remote Node and resolve a cluster Service:

```bash
kubectl run net-check --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"my-site-worker-01"}}}' \
  -- nslookup kubernetes.default.svc.cluster.local
kubectl logs net-check
```

The lookup should return the Service ClusterIP. If it times out, check the Site label on the Node first, then the gateway pool status, then the node agent logs in the `unbounded-system` namespace. A Pod that can reach the API server but not cluster Services is a routing or CNI problem, not a kubelet problem.

### Confirm image distribution

Remote Sites often have worse network paths to a registry than the cluster's own nodes. If image pulls are slow or fail, check whether Gantry is enabled for the Site and whether the image is available at a registry the Site can reach. Gantry is enabled by default and caches layers across peers, but it does not replace registry connectivity on the first pull.

## 9. Networking Considerations

Most of the network configuration is fixed when you create the Site. The decisions below are worth reviewing before the first workload depends on them.

### Address space

Every Site needs a node CIDR that covers the addresses of its hosts and a Pod CIDR pool large enough for its Nodes. The controller allocates a block from the pool to each Node, so the usable Node count is the pool size divided by the block size. A `/16` pool with `/24` blocks supports 256 Nodes, minus the blocks reserved for other purposes.

The node CIDRs of all Sites must be unique, and no Site's node or Pod CIDR may overlap another Site's ranges or the cluster's own ranges. The validating webhook checks overlap when a Site is created or updated, so an overlapping Site is rejected rather than partially applied. Plan for Sites you have not added yet: renumbering later means moving Nodes.

A Node is matched to a Site by its internal IP. If a host has multiple addresses, make sure the kubelet registers the one inside the Site's `nodeCidrs`; a Node that reports an address outside every Site gets no Site label and no routed Pod CIDR.

### Gateways and NAT

`gw-main` is an `External` gateway pool. Its members need an external IP and a WireGuard public key, which the node agent publishes. Remote Nodes then dial a gateway address rather than dialing the cluster's internal network directly.

This is also the part of the path most affected by NAT. A host behind symmetric NAT or carrier-grade NAT can often reach a gateway outbound, but the gateway cannot initiate a connection back to it. Unbounded handles this by letting the remote side initiate and by learning endpoints from incoming packets, and the node agent records a discovered public IP. A discovered address is not proof that the port is reachable through the NAT, so if a Site connects but intermittently loses peers, test the path from the remote host to the gateway's UDP port before changing the Site.

Run the pool with more than one member and make sure every member has the firewall range open. The controller assigns each gateway a unique port and records it in the `net.unbounded-cloud.io/wireguard-port` annotation on the Node, so a firewall rule that only allows 51820 will break gateway-to-gateway paths even though the mesh itself looks healthy.

### Tunnel protocol and MTU

`spec.tunnelProtocol` selects the encapsulation for the Site. The default, `Auto`, uses WireGuard for links with external IPs and GENEVE for links that only have internal IPs. You can force `WireGuard`, `IPIP`, `GENEVE`, `VXLAN`, or `None` when you have a reason to, but the default matches the common case and is easier to operate.

`spec.tunnelMTU` sets the MTU for routes through tunnels, with an allowed range of 576 to 9000. Leave it unset and let the agent derive the MTU from the default route, minus encapsulation overhead. Setting a value larger than the path supports causes fragmentation or silent drops on large packets, which often shows up as a connection that works for small requests and hangs on large responses.

### Same-site and cross-site traffic

When `manageCniPlugin` is true, the node agent writes CNI configuration and makes all Nodes in the same Site tunnel peers. Traffic between two Pods in the same Site goes directly between their Nodes over the tunnel, without leaving the Site through a gateway.

When it is false, an external CNI owns intra-site pod networking, and Unbounded Net only carries traffic that leaves the Site. Your CNI must therefore be present on the remote Nodes, and its Pod CIDR allocation must agree with the Site's `podCidrAssignments`. Running two CNIs that both claim the same Pod range is not a supported configuration.

Cross-site traffic goes through the gateway pool unless you create a `SitePeering` between the Sites, in which case Nodes in the peered Sites can mesh directly. Peering is optional for a single remote Site. Add it when two remote Sites talk to each other frequently and you want to avoid a hairpin through the cluster's gateways.

### Other Site controls

Two optional fields shape what leaves the Site. `spec.nonMasqueradeCIDRs` lists ranges that should not be masqueraded when traffic exits through the default gateway, so packets keep the Pod's real address. `spec.localCidrs` lists ranges that are local to the Site and must never be routed through a gateway pool. Use `localCidrs` for on-premises networks that the remote Site can reach directly, and use `nonMasqueradeCIDRs` when a downstream firewall needs to see the original Pod address.

If you enforce Kubernetes NetworkPolicy, remember that the node agent uses the host network. Allow the underlay ports that carry the tunnels, and check that your CNI enforces policy against the agent's traffic if that matters to you. The [Networking Operations]({{< relref "reference/networking/operations" >}}) reference includes an example policy for the agent namespace.

## 10. Upgrades

Unbounded has four independent upgrade layers. Changing one does not automatically change the others, so identify which layer you are moving before you start.

| Layer | What changes | How to upgrade |
| --- | --- | --- |
| Control plane components | Operator, Unbounded Net, Machina, Gantry, Metalman, token refresher | Install the new plugin and rerun `kubectl unbounded install`. |
| Custom resource definitions | Schemas for Sites, Machines, and networking objects | The new operator bootstraps them at startup, before it reconciles. |
| Node stack | Kubernetes version, containerd, runc, CNI binaries, kubelet configuration | Node recreation after you change the desired configuration. |
| Host-side agent | The `unbounded-agent` binary on each host | An `AgentUpgrade` MachineOperation. |

### Upgrading the operator and components

Install the new `kubectl-unbounded` binary, then run the installer again:

```bash
kubectl unbounded install
kubectl -n unbounded-system rollout status deployment/unbounded-operator
kubectl get sites
```

The installer uses server-side apply, so rerunning it is the supported update path. It replaces the operator Deployment image and updates the component registry ConfigMap, while preserving an API server endpoint you set on a previous run. It then waits for the new operator rollout. The operator reconciles every component at its own version, which is why the operator and its components stay version-matched.

The API groups are alpha and expose a single served version with no conversion webhook. That means a field can change between releases, and a CRD upgrade is what preserves status fields the new controllers write. Upgrade the operator, and therefore the CRDs, before you upgrade machine configurations or agents. If a new operator writes a status field that an older CRD does not preserve, the field is pruned and the symptom looks like a controller bug.

Watch the Site conditions during the rollout. Components restart under the operator's control, and a Site that was Ready before the upgrade should return to Ready without recreating Nodes.

### Upgrading the node stack

A Kubernetes version change is a node recreation, not a reboot. `NodeReboot` restarts the version already installed; it does not install a new one. The supported sequence is to change the desired configuration, then delete the Node so the agent builds a new root filesystem from that configuration.

If your Machines use a `MachineConfiguration`, update it to describe the new Kubernetes version and confirm that a new `MachineConfigurationVersion` exists:

```bash
kubectl unbounded machine config versions <configuration>
kubectl unbounded machine config assign <machine> --config=<configuration> --version=<version>
```

If they do not, update `spec.kubernetes.version` on the Machine instead. That field is what the provisioner resolves into the node configuration, and a Node deletion is what makes the agent apply it.

Then cordon and drain the Node, delete it, and wait for it to return:

```bash
kubectl cordon my-site-worker-01
kubectl drain my-site-worker-01 --ignore-daemonsets --delete-emptydir-data --timeout=10m
kubectl delete node my-site-worker-01
kubectl wait --for=condition=Ready node/my-site-worker-01 --timeout=20m
```

Drain before you delete. Deleting a Node while Pods are still assigned terminates them abruptly instead of evicting them and ignores PodDisruptionBudgets. Upgrade one Node at a time so the Site keeps capacity and a failure does not take the whole fleet down. The [Node Operations]({{< relref "guides/operations/node-operations" >}}) guide covers rollback and the configuration fields that trigger a repave.

### Upgrading the host agent

The host-side agent binary is separate from the node stack. Upgrade it with the `agent-upgrade` convenience command, which creates an `AgentUpgrade` MachineOperation, stages the new binary in a blue-green slot, and rolls back if the restarted daemon does not report healthy:

```bash
kubectl unbounded machine agent-upgrade my-site-worker-01 \
  --download-url https://example.com/unbounded-agent-linux-amd64.tar.gz \
  --sha256 <sha256-digest> \
  --ttl 3600
```

`--download-url` is required. `--sha256` is optional but recommended, because it turns a corrupted or substituted download into a failed operation instead of a broken binary. The command waits for the operation by default. Repeat it per Machine, sequentially, and stop at the first failure. The [Automation Patterns]({{< relref "guides/operations/automation" >}}) guide has a batch loop and the failure codes.

## 11. Removal

Removing a Site is a sequence, not a single delete. The agent keeps a Node alive, the Site protects itself while Nodes are assigned, and the gateway pool may be shared with other Sites. Work from the host inward.

### Remove one Machine

Cordon and drain the Node so workloads move before it disappears:

```bash
kubectl cordon my-site-worker-01
kubectl drain my-site-worker-01 --ignore-daemonsets --delete-emptydir-data --timeout=10m
```

Reset the agent on the host. The `agent-reset` operation removes the agent and every managed node artifact, and it runs through the in-host agent, so it does not need SSH access:

```bash
kubectl unbounded machine agent-reset my-site-worker-01 --force
```

Then delete the Node and the Machine:

```bash
kubectl delete node my-site-worker-01
kubectl delete machine my-site-worker-01
```

Deleting the Machine alone is not enough. The controller has no finalizer on the Machine that stops the host, so a deleted Machine leaves a running agent and a Node that can register again. Reset first, then delete.

If the host is already gone and cannot run the reset, skip the reset and delete the Node and Machine directly, then remove or reimage the host out of band. The next time that host boots with the agent installed, it will try to rejoin, so make sure it is powered off or rebuilt.

The SSH Secret created at registration is not deleted with the Machine. If you are decommissioning the host, delete `ssh-<site>` in the component namespace, or the Secret that `--ssh-secret-name` named. It contains a private key that grants root-equivalent access to the host.

### Remove a Site

Delete the remote Site after its Machines and Nodes are gone:

```bash
kubectl get nodes -l unbounded-cloud.io/site=my-site
kubectl delete site my-site
```

The first command should return no Nodes. A Site with assigned Nodes carries the `net.unbounded-cloud.io/protection` finalizer, so `kubectl delete site` waits instead of removing it while Nodes still match. If the delete appears to hang, list the Nodes with the Site label and remove them first. Do not remove the finalizer by hand unless you have confirmed that no Node or workload depends on the Site.

Delete the Site's gateway pool assignment as well, since it is a separate object:

```bash
kubectl delete sitegatewaypoolassignment my-site
```

Do not delete `gw-main` unless you are removing every Site that uses it. The pool is shared by the `cluster` Site and every remote Site, and deleting it removes gateway connectivity for all of them.

### Remove Unbounded entirely

If you are uninstalling the project rather than removing one location, reset or power off the remaining hosts before you delete their Machines. Deleting a Machine does not stop the agent on the host, and a host that keeps running will try to rejoin and re-register its Node. Delete the remote Nodes as well, or the protection finalizer will hold their Sites. Then delete the remaining Sites and Machines, and finally the operator namespace:

```bash
kubectl delete site --all
kubectl delete machine --all
kubectl delete namespace unbounded-system
```

The namespace contains the operator and all component workloads. Deleting it does not delete the cluster-scoped custom resource definitions, and it does not delete the cluster-scoped custom resources themselves. The operator owns CRD lifecycle, so remove the CRDs only when you intend to remove the API surface as well. Deleting a CRD deletes every custom resource of that type, including any Machine or Site you have not already removed.

## Next Steps

- For a working end-to-end example on AKS, see [Getting Started]({{< relref "guides/getting-started" >}}).
- For host preparation, bastion hosts, and SSH troubleshooting, see the [SSH Guide]({{< relref "guides/ssh" >}}) and [Agent Preflight]({{< relref "reference/agent/preflight" >}}).
- For the provisioning lifecycle and automation, see [Provisioning Lifecycle]({{< relref "guides/operations/automation" >}}).
- For day-two node and host work, see [Node Operations]({{< relref "guides/operations/node-operations" >}}) and [Host Operations]({{< relref "guides/operations/host-operations" >}}).
- For networking architecture and cross-site routing, see [Networking Concepts]({{< relref "concepts/networking" >}}) and [Networking Reference]({{< relref "reference/networking" >}}).
- For every flag and command used here, see the [CLI Reference]({{< relref "reference/cli" >}}).
- For the Machine and Site fields, see the [Machina CRD Reference]({{< relref "reference/machina-crd" >}}).
- For bare-metal installation and PXE, see [PXE Boot]({{< relref "guides/pxe" >}}) and [Bare Metal]({{< relref "concepts/bare-metal" >}}).
