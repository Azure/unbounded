---
title: "Reference"
description: "Field-level and component-level reference documentation for the Unbounded Kubernetes architecture, CLI, custom resources, agent, GPU support, networking, Racer, and workload overrides."
---

The reference section is the lookup layer of the Unbounded documentation. Where the concepts pages explain why a subsystem exists and the guides walk you through a task from beginning to end, these pages describe the interfaces you work with directly: command flags, custom resource fields, configuration keys, file formats, isolation modes, and the dataplane behavior that results from them. Use this section when you already know what you want to do and need the exact name, default, accepted value, or side effect.

Reference pages are organized by subsystem rather than by workflow. You can read them in sequence to build a complete picture of the system, but they are written so that each page stands on its own. If you arrive from a search result, read the overview of the page you landed on, then follow the links to the neighboring pages that define the objects or components it mentions. The cross-links are part of the reference: a field in the CRD reference often has a matching section in the agent or networking reference that explains how the controller or node agent consumes it.

## How to use this section

Start with the architecture page if you are new to the project. It names the controllers, binaries, and custom resources that the rest of the reference assumes you recognize, and it shows how provisioning, networking, and node lifecycle fit together. Once you have that mental model, move to the page for the interface you are changing. A change to a host usually starts with the CRD reference, a change to a node image or isolation mode starts with the agent reference, and a change to cross-site traffic starts with the networking reference.

The examples in this section are complete enough to adapt, but they are not a substitute for reading the field description beside them. Defaults matter: an omitted field often selects a behavior that is different from an explicitly empty value, and several fields are mutually exclusive or only take effect when a feature gate is enabled. When a page describes validation, treat the validation as part of the contract. The controllers reject invalid objects rather than repairing them silently, so a manifest that passes a YAML parser can still fail admission or reconciliation.

Code is authoritative for every interface described here. If a page and the implementation disagree, the implementation in this repository is the source of truth, and the page should be treated as a lead to verify. That is especially important for experimental features, whose flags and field names can change between releases. The reference documents the current shape of those interfaces, not a compatibility promise.

## Architecture

[Architecture]({{< relref "reference/architecture" >}}) describes the components that make up an Unbounded deployment, the custom resources they reconcile, and the provisioning pipelines that connect a new host to an existing control plane. It covers the SSH provisioning controller in `cmd/machina`, the bare metal PXE controller in `cmd/metalman`, the `kubectl unbounded` plugin, and the inventory suite, then shows how their objects relate. Read it first when you need to know which component owns a behavior before you look up the behavior itself.

## Command-line interface

The [CLI reference]({{< relref "reference/cli" >}}) documents the `kubectl unbounded` plugin, which is also shipped as the standalone `kubectl-unbounded` binary. It explains installation through release binaries or Krew, how the plugin is discovered on your `PATH`, and how `--kubeconfig`, `KUBECONFIG`, and the default `~/.kube/config` interact. The command sections cover the `site` and `machine` groups and the standalone commands. `kubectl unbounded install` applies the operator manifests and waits for the rollout, `kubectl unbounded site init` creates the cluster and remote `Site` objects plus the net `GatewayPool` resources and a bootstrap token, and `kubectl unbounded machine register` creates the SSH key Secret and the `Machine` object for a host. The `machine manual-bootstrap` command generates bootstrap and cloud-init payloads for a host, while `machine operation` creates and waits on `MachineOperation` resources for day-2 actions. The `overrides` group validates, lists, and reports the applied state of component workload overrides. Each command page lists required and optional flags, validation rules, and worked examples.

## Custom resource definitions

The [CRD reference]({{< relref "reference/machina-crd" >}}) is the field-level guide to the `unbounded.cloud.io` API. It documents the `Machine` resource in `v1alpha3`, including isolation, SSH and bastion settings, netboot and PXE configuration, Kubernetes version and labels, allocation and fencing, and the printer columns you see in `kubectl get machines`. It also covers `MachineOperation` for power and repave actions, the PXE OCI image layout and template data, `MachineClass` and `MachineAllocation` for host allocation, and the relationships between these objects. Use it when you are writing or reviewing a manifest and need to know which field the controller reads.

## Agent reference

The [agent reference]({{< relref "reference/agent" >}}) covers the `unbounded-agent` process that runs on every worker host. It explains how the agent is configured, what it checks before it touches a host, how it builds the node stack, and how the isolation mode you choose changes where that stack runs. The section is split by concern so you can go directly to the part you are debugging.

### Configuration

[Agent configuration]({{< relref "reference/agent/configuration" >}}) documents the single JSON configuration file that tells the agent which cluster to join, how to authenticate, where the node root filesystem comes from, and how the node stack is isolated. It lists every field and its default, explains the loading order and the environment variable overrides that take precedence over the file, and includes worked examples. Normal installations do not require you to write this file by hand, but reading it is the fastest way to understand what a bootstrap actually does.

### Preflight

[Preflight]({{< relref "reference/agent/preflight" >}}) documents `unbounded-agent preflight`, a non-mutating command that answers whether a host with a given configuration can complete a node bootstrap. It describes the flag set, the text and JSON output formats, the exit behavior, and the fixed set of host, cluster API, and rootfs checks. Each check returns `ok`, `warning`, or `error`; warnings describe conditions that bootstrap repairs or tolerates, while errors stop bootstrap unless you explicitly ignore them. Run it before a first bootstrap and after any host change that could affect drivers, mounts, or networking.

### Isolation modes

[Isolation modes]({{< relref "reference/agent/isolation" >}}) compares the three ways the agent can place the node stack. The default is [systemd-nspawn]({{< relref "reference/agent/nspawn" >}}), where the agent and daemon remain on the host and systemd, containerd, kubelet, and the CNI binaries run inside a lightweight machine that shares the host kernel and network namespace but has its own filesystem, PID namespace, cgroup subtree, and service manager. The [none mode]({{< relref "reference/agent/isolation-none" >}}) runs the stack directly on the host through systemd units and host-local packages, and is experimental. The [container mode]({{< relref "reference/agent/isolation-container" >}}) runs the node stack in a privileged OCI container and is both experimental and gated behind an explicit enablement flag. Read the mode page for the device, mount, and lifecycle consequences before you select one.

### Host provisioning format

The [host provisioning format]({{< relref "reference/agent/provisioning-format" >}}) page explains how the agent decides whether a host image expects `CloudInit` or `Ignition` data. It covers the limits of inferring the format from an image name or the running distribution, and the explicit `Machine.spec.host.provisioningFormat` declaration that removes the ambiguity. Use it when a provider image does not advertise its format or when a machine boots without applying its provisioning data.

## GPU reference

The [GPU reference]({{< relref "reference/gpu" >}}) explains how Unbounded passes GPUs from a host to the node stack so that the kubelet and the pods it runs can see and use them. Unbounded is not a GPU orchestrator; it connects the driver and devices that already work on the host to the node it manages, and the result is a node that schedules GPU workloads like any other Kubernetes node. The access path depends on the isolation mode, because nspawn needs the device nodes, driver libraries, and a container runtime handler exposed inside the machine, while none mode uses the host driver and container toolkit directly.

### NVIDIA

[NVIDIA GPU support]({{< relref "reference/gpu/nvidia" >}}) is the detailed guide to the NVIDIA path. It covers host driver requirements, CDI specification generation, the runtime handler and bind mounts used in nspawn mode, the differences between isolation modes, and troubleshooting steps for a node that starts but does not advertise GPU resources. Read it before you build a node image or change a driver version on a host that already runs GPU workloads.

### AMD

[AMD GPU support]({{< relref "reference/gpu/amd" >}}) describes the AMD path, which Unbounded builds in so that AMD devices reach node containers without extra configuration. It lists the prerequisites for the host and explains what the agent exposes. Use it together with the NVIDIA page when you run a mixed fleet, since the isolation-mode rules are shared even though the userspace stacks differ.

## Networking reference

The [networking reference]({{< relref "reference/networking" >}}) covers the `unbounded-net` stack that provides pod-to-pod connectivity across multiple Kubernetes sites. It builds an overlay on top of the links you already have, using WireGuard for encrypted tunnels and GENEVE, VXLAN, or IPIP for unencrypted overlays depending on the link type and scope. The dataplane is eBPF-based, routes are held in the kernel as netlink nexthop objects, and ECMP and failover work by deprioritizing unhealthy paths rather than removing routes. The section index summarizes the model and links to the component pages.

### Networking architecture

[Networking architecture]({{< relref "reference/networking/architecture" >}}) describes the controllers, node agents, and dataplane components, and how a packet moves from a pod on one site to a pod on another. It explains per-link-scope encapsulation selection and when the protocol is resolved automatically rather than configured explicitly. Start here when you need to understand which component programs a route or owns a tunnel endpoint.

### Networking custom resources

[Networking custom resources]({{< relref "reference/networking/custom-resources" >}}) is the field-level reference for the `net.unbounded-cloud.io` API and the `Site` resource. It documents `GatewayPool` for labeled sets of forwarding nodes, `SiteGatewayPoolAssignment` for attaching sites to pools, `SitePeering` for direct site meshes, and `GatewayPoolPeering` for linking pools, along with the observational `SiteNodeSlice` and `GatewayPoolNode` objects and the embedded health check settings. The page includes complete manifests, defaults, and validation rules, so use it when you are authoring or reviewing topology objects.

### Networking configuration

[Networking configuration]({{< relref "reference/networking/configuration" >}}) documents the shared YAML configuration consumed by the controllers and node agents. By default it is read from the `unbounded-net-config` ConfigMap at `/etc/unbounded-net/config.yaml`, and `--config-file` overrides the path. The page explains the fail-fast behavior on a missing or invalid file, the CLI flags that act as explicit overrides, and the settings that control link behavior. Use it when a node agent starts with the wrong protocol or cannot find its configuration.

### Routing flows

[Routing flows]({{< relref "reference/networking/routing-flows" >}}) follows packets through the eBPF dataplane. It documents the single TC egress program `unbounded_encap`, the `unbounded0` dummy interface that attracts kernel routes, the BPF LPM trie that steers traffic into tunnels, and the path a packet takes for cross-site and same-site destinations. It also covers symmetric routing and the gateway path used when traffic crosses sites. Read it when you are debugging connectivity that works in one direction but not the other.

### Networking operations

[Networking operations]({{< relref "reference/networking/operations" >}}) covers deployment prerequisites, including the required kernel eBPF and TC support and the WireGuard kernel module for encrypted tunnels, and the operational tasks that keep a network healthy. It describes how to inspect gateway health, read the metrics and status that the controllers publish, and diagnose common failures. Use it after a deployment when traffic is not flowing or a gateway is not being selected.

## Racer reference

The Racer pages document the caching dataplane used for object storage. [Racer reference]({{< relref "reference/racer" >}}) describes the `ClusterCache` resource in the `racer.unbounded-cloud.io/v1alpha1` API group: a cluster-scoped object that provisions a cache for an Azure Blob Storage container. It explains the optional `spec.origin`, `spec.priority`, `spec.passthrough`, and `spec.nodeSelector` fields, the naming limits, and the guarantees the cache does and does not provide. A `ClusterCache` is not a PersistentVolume or PersistentVolumeClaim; it provides relative capacity guarantees and no promise of data durability.

[racer-object reference]({{< relref "reference/racer-object" >}}) documents the `racer-object` sidecar, which exposes Racer-managed objects through a read-only S3-compatible or Azure Blob API. It is a cache-aware proxy, not a durable storage layer, so it has no upstream credentials, fallbacks, or write and delete operations. The page lists the flags for cache selection, API flavor, listen port, request timeout, connection limits, workers, and debugging, along with the upstream requirements for block blob storage and strong quoted ETags. Read it when you are connecting a workload to a `ClusterCache` and the client needs object API semantics rather than the Racer socket protocol.

## Workload overrides

[Workload overrides]({{< relref "reference/workload-overrides" >}}) describes how to customize the workloads that the operator manages without forking the manifests. You supply strategic merge patches as independent documents in the `unbounded-component-overrides` ConfigMap in the operator namespace, which is typically `unbounded`. The operator does not create the ConfigMap and treats its absence as no overrides. Overrides are supported for a restricted set of components and kinds, including net, machina, gantry, and metalman, and the operator rejects unsupported components, unknown kinds, and workload names that do not exist in its management model. Use it for environment-specific settings such as resource requests, node selectors, or tolerations, and keep each document scoped to one concern so multiple teams can contribute without conflict.

## Conventions in these pages

Most reference pages follow the same layout. An overview states what the page covers and the mental model behind it. Field and flag tables give the name, accepted values, default, and effect. YAML examples show the smallest manifest that demonstrates the feature, and command examples use `<placeholders>` for values you must supply. Validation sections describe what the controllers reject and why, and related links point to the pages that explain the surrounding behavior.

Two conventions are worth calling out. First, an unset field and a field set to an empty value are not always equivalent; read the description rather than assuming the default. Second, experimental features are labeled as such because their interfaces can change. The agent container isolation mode, the none isolation mode, and parts of the CLI surface fall into that category. Do not build automation on an experimental interface without checking the current code.

{{< callout type="note" >}}
The reference describes the interfaces in this repository at the time of writing. If a flag, field, or default here disagrees with the code, the code wins. Verify the behavior in `cmd/`, `internal/`, and `api/` before you depend on it.
{{< /callout >}}

## Related documentation

If you are new to the project, read [Concepts]({{< relref "concepts" >}}) before this section, then follow [Getting Started]({{< relref "guides/getting-started" >}}) to create a first cluster. For task-oriented instructions that use the interfaces documented here, see the [Guides]({{< relref "guides" >}}), including the [agent guide]({{< relref "guides/agent" >}}), the [existing cluster guide]({{< relref "guides/existing-cluster" >}}), and the [operations guides]({{< relref "guides/operations" >}}). The [networking concepts]({{< relref "concepts/networking" >}}) page and the [Racer concepts]({{< relref "concepts/racer" >}}) page explain the problems these reference pages solve.
