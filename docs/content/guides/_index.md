---
title: "Guides"
description: "Task-oriented guides for standing up Unbounded, provisioning worker machines, connecting networks, running day-2 operations, and serving advanced workloads."
---

The guides are the practical half of the Unbounded documentation. Each page takes a
single job, such as attaching the first remote node or replacing a failed host, and
walks it from prerequisites to a verified result. The pages assume you have a
Kubernetes control plane or intend to create one, and they favor commands you can run
over descriptions of how something works internally.

Read them in the order your own work demands. A first-time reader should follow the
path in [Recommended Reading Path](#recommended-reading-path), which moves from a
working cluster to provisioning, then networking, then day-2 operations, and finally
the integrations that change how images and model weights reach your nodes. An
operator who already has a cluster can jump straight to the section that matches the
task in front of them.

## Who These Guides Are For

These pages are written for the engineer who operates the cluster rather than the one
who develops Unbounded itself. That includes platform engineers building an internal
development cluster, site reliability engineers running production worker fleets
across providers, and infrastructure engineers who need to attach hardware in a
colocation facility or at the edge. If you can read a Kubernetes manifest, use
`kubectl`, and reason about IP addressing, you have enough background to follow any
guide here.

The guides assume a working knowledge of a few Kubernetes ideas: Nodes, DaemonSets,
custom resources, and the difference between the control plane and the kubelet. They
do not assume you have used Unbounded before. Every page introduces the Unbounded
resources it touches, and most of them repeat the small set of checks that prove the
step worked.

If you are still building the mental model, start with
[Concepts]({{< relref "concepts/overview" >}}) and the
[architecture reference]({{< relref "reference/architecture" >}}) first. The concepts
section explains why Unbounded separates the control plane from the machines that run
workloads, and it introduces the `Site`, `Machine`, and agent vocabulary that the
guides use without redefining. Come back here when you want to do something rather
than understand something.

The guides also serve as a reference for unusual situations. Troubleshooting sections
at the end of the longer pages describe the failures operators actually hit: a machine
that never leaves `Pending`, a node that joins under the wrong name, a tunnel that
comes up but carries no pod traffic, or a pull that stalls behind a slow origin. Those
sections are written to be read on their own when something is broken.

## How the Guides Are Organized

Most guides follow the same shape. An overview states the outcome and the model behind
it, a prerequisites section lists what must already exist, and numbered steps carry
the work. Verification comes before troubleshooting, because the fastest way to debug
a failure is to know which step should have proved the state you expected. Pages that
describe an API rather than a workflow, such as the operations guides, replace the
numbered steps with resource examples and status tables.

Commands are shown as you would type them, apart from values in angle brackets such as
`<site-name>` or `<machine-name>`. Replace those with your own values; the surrounding
text explains what each one controls. Fenced blocks carry a language tag so that
copying a manifest or a shell command preserves its quoting.

Several pages depend on the `kubectl unbounded` plugin. The plugin wraps the
installer, site initialization, machine registration, and bootstrap payload
generation, so you rarely need to assemble those objects by hand. If a command in a
guide is not found, install or upgrade the plugin as described in the prerequisites of
the page you are following.

## Recommended Reading Path

The sequence below is the shortest route from an empty subscription or an existing
cluster to a fleet you can operate. You do not have to read every page to finish a
step, but skipping ahead usually means returning for a prerequisite you did not know
you needed.

### 1. Get a Control Plane and a First Remote Node

Start with exactly one of the two onboarding guides. They answer the same question
from opposite starting points, and reading both before choosing is usually wasted
effort.

[Getting Started]({{< relref "guides/getting-started" >}}) is the fastest supported
path. It builds an Azure Kubernetes Service cluster with the `aks-quickstart.sh`
script, which pre-configures gateways and the Unbounded components, then adds a remote
node over SSH and verifies pod networking through the tunnel. Use it when you want a
working example to inspect and you are free to create new Azure resources.

[Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}}) is the path for a
cluster you already run and intend to keep. It installs the plugin and the operator,
labels gateway nodes, initializes a `Site` with planned address ranges, prepares the
hosts, registers the first `Machine`, and checks that workloads schedule onto the
resulting Node. Use it when the control plane, its CNI choice, and its address plan
are already decided.

Both pages end with the same proof: a pod runs on a remote node and can reach the
cluster network. Once you reach that point, the remaining guides are independent
tasks rather than a strict sequence.

### 2. Choose How the Next Machine Arrives

Provisioning is the part of Unbounded that varies most between environments, because
the right mechanism depends on what already exists. The four provisioning guides cover
the four cases, and you can mix them in one cluster.

| Starting point | Guide | What it assumes |
| --- | --- | --- |
| A Linux host you can reach over SSH | [Cloud Bootstrapping (SSH)]({{< relref "guides/ssh" >}}) | An installed OS, working SSH access, and a reachable API server |
| A host you can log into and run a payload on | [Agent]({{< relref "guides/agent" >}}) | An installed OS and a way to deliver and run a script |
| A provider that can create instances | [Cloud API Provisioning]({{< relref "guides/cloud-api" >}}) | A provider adapter and credentials that can launch capacity |
| A physical server with no OS you control | [Bare Metal (PXE)]({{< relref "guides/pxe" >}}) | Network boot firmware, a provisioning network, and usually a BMC |

Read [Cloud Bootstrapping (SSH)]({{< relref "guides/ssh" >}}) first if you only want
one of them. It shows the full lifecycle of a `Machine` from registration through the
`Node` join, including the `MachineClass` fields and the credential handling, and the
later pages reuse that vocabulary. The agent guide then explains the payload that SSH
provisioning runs, which is useful when you need to bootstrap a host through a
different channel such as cloud-init, Ignition, or a configuration management system.

[Cloud API Provisioning]({{< relref "guides/cloud-api" >}}) moves the decision to a
provider. A `MachineClass` describes the capacity, a `MachineAllocation` requests one
machine, and a provider adapter launches it and returns the bootstrap material. The
page also covers the lifecycle conditions and the Karpenter path, so read it when you
want capacity to follow workload demand automatically.

[Bare Metal (PXE)]({{< relref "guides/pxe" >}}) is the longest of the four because it
has the most moving parts. The `metalman` controller runs DHCP, TFTP, HTTP artifact
serving, and Redfish in one process, then drives a server through network boot, an OS
install, first boot, and node join. Read it when you cannot assume an operating system
exists yet.

### 3. Connect the Network

Networking is where Unbounded deployments most often diverge from a stock cluster.
The default is an encrypted WireGuard mesh that Unbounded builds between sites, and it
works without help from your network team as long as nodes can reach each other over
the public internet.

[L3 Connectivity (Azure VPN + Ubiquiti)]({{< relref "guides/l3-connectivity" >}}) covers
the alternative. When a private Layer 3 path already exists, you peer the sites over
that path and set the `SitePeering` tunnel protocol to `None`, so Unbounded routes pod
traffic without building its own tunnel. The page plans the topology and address
space, creates the Azure VPN gateway and the Ubiquiti site-to-site connection, creates
gateway pools, and verifies the data path in both directions.

Read this guide before you commit to an address plan, not after. The ranges for pods,
services, and site-internal networks are difficult to change once machines have
joined. The [networking reference]({{< relref "reference/networking/configuration" >}})
explains the protocol choices and the custom resources in more depth once the topology
is clear.

### 4. Operate the Fleet

Day-2 work on Unbounded is expressed as resources, not as logins. A `MachineOperation`
names a target, an operation kind, and optional parameters; the component that owns
that boundary carries the work out and records the result in the operation's status.
[Run Machine Operations]({{< relref "guides/operations" >}}) is the entry point. It
draws the line between the host, the node, and the agent, explains who executes each
kind, and shows how to create, target, watch, and clean up operations safely.

From there, read the page for the boundary you are changing. [Host Operations]({{< relref "guides/operations/host-operations" >}})
covers power, reboot, and replace actions that talk to the provider or the BMC.
[Node Operations]({{< relref "guides/operations/node-operations" >}}) covers restarting
or rebuilding the Kubernetes node stack on a host without touching the host itself,
including the repave workflow and node upgrades. [Agent Operations]({{< relref "guides/operations/agent-operations" >}})
covers `AgentUpgrade` and `AgentReset`, the two kinds that act on the agent
installation, with the drain, activation, and rollback rules that keep an upgrade from
stranding a node.

Finish with [Automation Patterns]({{< relref "guides/operations/automation" >}}) when
you are ready to stop creating operations by hand. It shows how label selectors,
watches, GitOps, and CI jobs drive the same API, and it is the right page for building
a rolling upgrade or an automatic scale-in policy.

Image distribution belongs in this phase too. [Distribute Container Images with Gantry]({{< relref "guides/gantry" >}})
installs the peer-to-peer distribution agent and configures upstream registries, so
nodes share layers instead of each pulling the same bytes. Operate it with
[Trace Gantry Pulls]({{< relref "guides/gantry-tracing" >}}) when a pull is slow or you
need to know which source served a layer, and consult
[Gantry Benchmark]({{< relref "guides/gantry-benchmark" >}}) when you want to
understand the scale the design targets and how the measurements are produced.

### 5. Take On Advanced Workloads

The last group changes where bytes come from for workloads that read a lot of them.
These guides are independent of each other and of the provisioning path, but they
assume a healthy cluster with working pod networking.

[Cache Azure Blob Storage with Racer]({{< relref "guides/azure-blob-cache" >}}) points a
`ClusterCache` at an Azure Blob Storage container and injects the `racer-object`
sidecar into pods, so an application reads objects from a loopback endpoint at local
latency without holding storage credentials. [Load vLLM Models from Racer]({{< relref "guides/racer-vllm" >}})
applies the same machinery to model weights, so vLLM replicas stream safetensors
through the node-local cache and peers instead of each one reading the full checkpoint
from object storage.

Read the Azure Blob page before the vLLM page even if you only care about model
serving. It explains the cache object, the origin configuration, the injection
annotations, and the first-read versus cached-read behavior that the vLLM guide builds
on. The vLLM page then adds the GPU-specific parts: the dataplane mode check, the model
layout the loader expects, and the pod contract that mounts the cache.

## Guide Catalog

The catalog below lists every guide in this section, grouped by the job it performs.
The description under each entry is a summary, not a substitute for the page's
prerequisites.

### Getting Started

- [Getting Started]({{< relref "guides/getting-started" >}}): Builds an AKS cluster with the quickstart script, connects a remote node over SSH, and verifies pod networking through the tunnel.
- [Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}}): Adds remote worker nodes to a cluster you already run, from installing the operator and creating a `Site` through joining a `Machine`, verifying workloads, upgrading, and removing nodes.

### Provisioning

- [Agent]({{< relref "guides/agent" >}}): Installs the agent on a host, generates and inspects a bootstrap payload, runs it with `sudo`, verifies that the host joins its site, and troubleshoots the common failures.
- [Cloud Bootstrapping (SSH)]({{< relref "guides/ssh" >}}): Gives the machina controller SSH access to an existing Linux host so it can install and start the node agent and wait for the `Node` to register.
- [Cloud API Provisioning]({{< relref "guides/cloud-api" >}}): Drives a provider API to launch worker machines through `MachineClass` and `MachineAllocation`, join them to a site, and release their infrastructure.
- [Bare Metal (PXE)]({{< relref "guides/pxe" >}}): Provisions physical servers with `metalman` over PXE, UEFI HTTP boot, or virtual media, from the `Machine` declaration to a joined `Node`.

### Networking

- [L3 Connectivity (Azure VPN + Ubiquiti)]({{< relref "guides/l3-connectivity" >}}): Connects an on-premises site to an AKS cluster over an IPsec VPN and routes pod traffic with `SitePeering` and gateway pools set to the `None` tunnel protocol.

### Operations

- [Machine Operations]({{< relref "guides/operations" >}}): Explains the boundary between host, node, and agent operations and how to run, target, observe, and clean up `MachineOperation` resources.
- [Host Operations]({{< relref "guides/operations/host-operations" >}}): Runs power, reboot, replace, and other host-level actions on machines through the Unbounded API and the provider or BMC behind it.
- [Node Operations]({{< relref "guides/operations/node-operations" >}}): Restarts, rebuilds, and upgrades the Kubernetes node stack on a host without rebooting the host itself, including repave and upgrade choices.
- [Agent Operations]({{< relref "guides/operations/agent-operations" >}}): Stages, activates, verifies, and rolls back host agent upgrades, and removes an agent installation safely with `AgentReset`.
- [Automation Patterns]({{< relref "guides/operations/automation" >}}): Automates machine lifecycle work with the `MachineOperation` API, label selectors, watches, and GitOps or CI workflows.

### Image Distribution and Data Caching

- [Distribute Container Images with Gantry]({{< relref "guides/gantry" >}}): Runs the Gantry peer-to-peer OCI distribution agent so nodes share image layers instead of every node pulling them from the upstream registry.
- [Gantry Benchmark]({{< relref "guides/gantry-benchmark" >}}): Documents how the Gantry benchmarks are run and what the cold-pull, fleet, single-layer, and Artifact Streaming measurements show.
- [Trace Gantry Pulls]({{< relref "guides/gantry-tracing" >}}): Enables OpenTelemetry tracing for Gantry and uses spans, attributes, logs, and metrics to follow a pull and diagnose slow transfers.
- [Cache Azure Blob Storage with Racer]({{< relref "guides/azure-blob-cache" >}}): Serves objects from a public or private Azure Blob Storage container to pods at local read latency with a `ClusterCache` and the `racer-object` sidecar.
- [Load vLLM Models from Racer]({{< relref "guides/racer-vllm" >}}): Serves a vLLM model from a `ClusterCache` so weights stream from node-local cache and peers instead of being pulled from object storage by every replica.

## Conventions Used in These Guides

Placeholders appear in angle brackets and are always explained in the surrounding
text. A command that contains `<machine-name>` will not run until you replace that
value. Cluster-scoped resources such as `Machine` and `MachineOperation` have no
namespace, while workloads and caches generally do, and the examples show the
`metadata.namespace` where it matters.

Verification steps are not optional. Each one targets a specific failure: a `Site`
that never becomes ready, a bootstrap token that expired before the host used it, a
gateway label that no pool selected, or a cache dataplane that is running but not
serving. When a check fails, read the troubleshooting section of that page before
changing anything, because several of the apparent failures are expected states that
resolve on their own.

The guides describe behavior in the default configuration. Where an alternative
exists, such as the `none` and `container` agent isolation modes or the Racer backend
for Gantry, the page names it and links to the reference that covers it in full.

## Where to Go Next

When a guide refers to a resource field, a CLI flag, or a status value and you need
the complete list, use the [CLI reference]({{< relref "reference/cli" >}}) and the
[Machine CRD reference]({{< relref "reference/machina-crd" >}}). For networking
objects, the [custom resource reference]({{< relref "reference/networking/custom-resources" >}})
collects the fields that the guides use one at a time.

If you are deciding whether Unbounded fits a deployment rather than learning how to
operate it, return to [Concepts]({{< relref "concepts/overview" >}}) and read the
overview and networking pages before continuing through the guides. The concepts
material explains the tradeoffs; the guides assume you have already accepted them.
