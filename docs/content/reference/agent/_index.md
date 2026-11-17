---
title: "Agent Reference"
weight: 2
description: "Reference for the Unbounded agent, covering where it runs, how it bootstraps and joins a node, and how the isolation modes differ."
---

The Unbounded agent is the process that turns a booted machine into a Kubernetes
worker node and then keeps that node reconciled with its `Machine` resource. It
runs on the node host as root, installs the node stack, joins the node to the
cluster, and stays resident as a daemon. This page explains the pieces well
enough that you can choose the right child page: what the agent is, where its
files and units live, the three bootstrap phases, how a node joins the cluster,
and what each isolation mode changes about that story.

## What the agent is

The agent is a single binary, `unbounded-agent`. It has two distinct lives. The
first is a short-lived command that performs a bootstrap: `start` runs the host,
root filesystem, and node-start phases in order, then exits. The second is a
long-running daemon, `unbounded-agent daemon`, which is installed as the systemd
unit `unbounded-agent-daemon.service` and watches the `Machine` object for the
node. The other subcommands support those two: `preflight` for non-mutating
checks, `reset` to reverse an installation, `exec` and `logs` to reach the node
stack, and internal lifecycle and upgrade hooks that systemd units call.

The boundary is deliberate. The agent and its daemon always run on the host. They
are not part of the node stack, and they are not replaced when you upgrade the
node. The isolation modes described later change only where containerd, kubelet,
the CNI plugins, and the local DNS service run, and how the agent operates them.
That is why the same configuration format, the same bootstrap phases, and the
same daemon work across all three modes.

The agent is also usable as a library. The `pkg/agent` packages expose the
configuration type, goal-state resolution, the isolation backends, the bootstrap
phases, and reusable daemon controllers. Integrations that maintain their own
configuration schema translate it into the agent configuration and call those
entry points. For most readers, the command and its files are the useful
surface, and this reference is written around them.

## Where the agent runs

The agent owns a small, fixed part of the host filesystem. New installations put
their binaries and helper programs under `/opt/unbounded/agent`, with `bin` and
`libexec` subdirectories. Configuration lives in `/etc/unbounded/agent`, and
installation ownership is recorded in
`/var/lib/unbounded/agent/install-state.json`. Hosts installed by older releases
kept their files under `/usr/local`; the agent detects that layout, links
`/opt/unbounded/agent` at it, and later moves the files when no older release is
left to roll back to. Do not build paths by hand in scripts: the layout is
versioned and can be a link or a moved directory.

The binary is not expected on `PATH`. Units and generated scripts invoke it by
absolute path, and the install script downloads the release archive, places the
seed binary, and then runs `preflight` and `start`. On an immutable host with a
read-only `/usr`, the installation is driven by an Ignition config that installs
`unbounded-agent-bootstrap.service`; that unit runs the same bootstrap on every
boot. The `provisioning-format` child page covers how an installation records
whether cloud-init or Ignition delivered it.

The daemon is the only long-lived process. Its unit has a companion recovery
unit, `unbounded-agent-daemon-recovery.service`, which rolls back to the last
good binary if a newly staged agent fails to start. An agent upgrade or a move of
the host root never asks systemd to restart the daemon; the daemon re-executes the
binary its unit names, in place, so no restart delay or start-limit budget is
spent. If the re-execution fails, it exits and the unit's restart policy takes
over.

## How bootstrap works

Bootstrap is a sequence of three phases, and the phase names are the vocabulary
you will see in logs, preflight reports, and `Machine` condition reasons.

The host phase prepares the operating system. It installs or verifies host
packages, writes and applies the host sysctl tunables Kubernetes needs, disables
swap, removes package-manager guards that would otherwise restart services
mid-bootstrap, and prepares the host-side directories and units the rest of the
run needs. It also takes installation ownership, so two bootstraps cannot change
the same host at the same time.

The rootfs phase resolves the machine goal state and prepares the node
filesystem. Goal-state resolution probes the host: CPU architecture, kernel,
distribution, and GPU hardware, then combines those observations with the agent
configuration to decide which root filesystem image, which downloads, and which
device or mount grants apply. The phase then acquires the root filesystem from
an OCI registry, a local OCI layout, or an HTTPS archive, and stages the node
binaries and images the node stack needs.

The node-start phase configures and starts the node stack. It writes the
containerd, kubelet, CNI, and local DNS configuration, starts the services in
dependency order, and waits for kubelet to report active. Kubelet joins the
cluster with TLS bootstrap: the agent writes a bootstrap kubeconfig containing
the API server URL, the cluster CA, and the bootstrap token, and kubelet uses it
to request a client certificate. The agent polls for the generated client
kubeconfig, so a rejected or expired token, a CA mismatch, or an unreachable API
server fails this wait rather than surfacing much later. On success it records
the applied configuration under `/etc/unbounded/agent`, enables the daemon, and
reports the `AgentBootstrapped` condition as true.

Bootstrap is resumable and single-owner. The install-state record stores the
machine name, a fingerprint of the cluster version, the root filesystem image,
the API server, and a non-default isolation mode, alongside an installation ID
and phase. A retry with the same identity either resumes unfinished work or
reports that the installation is already complete; a retry that changes the
machine, version, image, endpoint, or recorded mode is refused until you run
`reset`, because building a second node stack beside the first is not a safe
repair. Credentials and artifact locations may be refreshed between attempts
without changing the fingerprint. If a bootstrap is interrupted, the daemon
declines to start and exits with a deferred status instead of fighting the
unfinished installation for ownership.

## The daemon and day-two work

Once the node is up, the daemon reconciles it. It authenticates with the
bootstrap credentials only long enough to obtain a daemon-controller client
certificate, then watches the `Machine` named by the agent configuration and the
`MachineOperation` objects that request host-local actions. When the desired
configuration changes, it regenerates the node configuration and restarts the
services whose configuration changed, leaving an already correct node alone. The
same controller writes agent-owned status: the applied isolation mode, the
isolation change status, and the `IsolationDrift` condition that compares the
recorded mode with `spec.isolation`.

The daemon also owns the operations that must run on the host rather than in the
control plane: node reboots, agent upgrades, agent resets, and host soft reboots.
It defers other work while a host operation that replaces or powers the machine
is in flight, so it never races an out-of-band power or replacement action.
`unbounded-agent exec` and `unbounded-agent logs` resolve the active node from
the applied configuration and route commands and journal output through the
backend of the mode that config records, so they work the same way in every
mode.

Self-registration is a bootstrap convenience, not the normal path. If the agent
configuration carries a bootstrap token and no `Machine` with the configured name
exists, the agent creates a minimal one and records what it observes. If the
`Machine` already exists, which is the usual case for controller-provisioned
hosts, the agent only reports status. An allocation-backed host is a deliberate
exception: the cluster creates its `Machine` with an allocation reference, and
the agent never fabricates one, because a self-created `Machine` could never
authorize the host.

## Isolation modes

An isolation mode answers one question: where does the node stack run? The agent
and daemon are always on the host, and `unbounded-net-node`, gantry, and the rest
of the host-side agents are unaffected. The mode is explicit. There is no
auto-detection: an empty value means `nspawn`, an unrecognized value is rejected
at every layer, and a host with an installed node refuses a mode change until it
is repaved or reset.

`nspawn` is the default and the most managed option. The node stack runs inside a
systemd-nspawn machine whose root filesystem lives under
`/var/lib/machines/<machine>`, with its own filesystem, PID namespace, cgroup
subtree, and service manager. The machine shares the host kernel and network
namespace, so pod networking, ports, and firewall rules behave as they do on a
host-installed node. The agent provisions the root filesystem from an OCI image
and alternates between the `kube1` and `kube2` slots so an upgrade can build and
verify the new node before removing the old one.

`none` runs the node stack as agent-owned systemd units directly on the host.
There is no machine and no private root filesystem. The agent still writes its
own containerd and kubelet units rather than relying on distribution packages,
but the services use host-local binaries and configuration and the host's package
database is part of the node. This mode suits hosts that already have the
packages a node needs, or hosts where a machine or container is undesirable. It
is experimental and not validated on every host.

`container` runs the node stack in a privileged OCI container supervised by an
agent-owned systemd unit. It is experimental and gated: the agent refuses to
bootstrap in this mode, or to change a running node to it, unless the agent
configuration sets `Experimental.ContainerIsolation` to true. The other
components that can request the mode have matching switches, but the agent's own
setting is the deciding gate. Treat this mode as an evaluation and development
tool rather than a production choice.

A mode change is a separate, documented operation rather than a side effect of
editing `spec.isolation`. Moving from `nspawn` to `container` can be done in
place by building the target beside the running node and rolling back on failure,
while changing `none` requires a reprovision: the host is reset and bootstrapped
again in the target mode, which needs the reprovision annotation on the
`Machine`. The daemon records progress in `status.isolationChange` and reports a
refusal with a reason such as `UnsupportedMode`, `ExperimentalNotEnabled`, or
`ReprovisionNotAllowed` instead of starting a change it cannot finish.

## Choosing the provisioning format

The agent records how an installation was first provisioned, because the answer
changes what a later replacement host must be given. An explicit
`ProvisioningFormat` of `cloud-init` or `ignition` in the agent configuration is
reported in `Machine` status; an empty value stays unobserved for legacy
installations and is never inferred from the running distribution. The
`provisioning-format` child page documents how the `Machine`,
`MachineConfiguration`, and host replacement paths resolve that value. Read it
when you build images yourself or when a replacement host is rejected for a
format mismatch.

## How to read this reference

Start with the configuration page. It is the contract for every other page: it
lists every field, its default, the loading order, the environment variables, and
complete examples. The preflight page comes next, because it is the cheapest way
to find out whether a host and a configuration can complete a bootstrap.

The isolation page is the map of the three modes. Read it before the mode-specific
pages, because it compares filesystem isolation, node stack sourcing, supervising
units, upgrade behavior, host requirements, and the shared host-global state that
every mode must handle. The `nspawn` page then explains the default in depth:
root filesystem layout, host-side configuration, devices, GPUs, and lifecycle.
The `isolation-none` and `isolation-container` pages cover the two experiments,
including what they do not install for you.

The table below is the reading order that matches those dependencies.

| Page | What it answers |
| --- | --- |
| [Configuration]({{< relref "reference/agent/configuration" >}}) | Which file the agent reads, every field, defaults, overrides, and examples |
| [Preflight]({{< relref "reference/agent/preflight" >}}) | Which non-mutating checks run, what each status means, and how to read a report |
| [Isolation]({{< relref "reference/agent/isolation" >}}) | How the three modes compare, how a mode is selected, and how changes and drift work |
| [nspawn]({{< relref "reference/agent/nspawn" >}}) | The default mode: machine rootfs, host configuration, grants, and lifecycle |
| [none Mode]({{< relref "reference/agent/isolation-none" >}}) | The host-native node stack, its units, and its limits |
| [container Mode]({{< relref "reference/agent/isolation-container" >}}) | The experimental OCI container stack, its gate, and its requirements |
| [Host provisioning format]({{< relref "reference/agent/provisioning-format" >}}) | How cloud-init and Ignition installations are declared and resolved |

If you are trying to get a node running rather than understand the internals,
read the [agent guide]({{< relref "guides/agent" >}}) first and return here when a
step fails or when you need to change a setting the controllers do not expose.
Day-two work, including upgrades, reboots, and recovery, is described in
[agent operations]({{< relref "guides/operations/agent-operations" >}}). The
command-line surface, including the agent subcommands and their flags, is listed
in the [CLI reference]({{< relref "reference/cli" >}}).
