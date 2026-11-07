---
title: "Isolation Modes"
weight: 3
description: "Where the unbounded-agent runs the Kubernetes node stack, how a mode is declared, and how drift from the declared mode is reported."
---

The unbounded-agent turns a host into a Kubernetes worker. The agent and its
daemon always run on the host. An **isolation mode** decides only where the
node stack runs: containerd, kubelet, CNI plugins, and CoreDNS.

| Mode | Node stack runs | Status |
|---|---|---|
| [`nspawn`]({{< relref "reference/agent/nspawn" >}}) | In a systemd-nspawn machine | Default. The only mode in this release. |

Hosts with no recorded mode are `nspawn` hosts, and their behavior does not
change.

## Selecting a mode

Set the mode at provisioning time with any of:

- `spec.isolation` on the `Machine`.
- `--isolation` on `kubectl unbounded machine manual-bootstrap`.
- `Isolation` in the [agent config]({{< relref "reference/agent/configuration" >}}).

An empty value is `nspawn`. Each of these accepts only the modes this release
can run, which today is `nspawn`: the Machine CRD rejects any other
`spec.isolation`, `kubectl` rejects any other `--isolation` before it contacts
the cluster, and the agent rejects any other `Isolation` with a clear error
rather than running it as `nspawn`.

A host records the mode it was installed in. For `nspawn` the record is the
same whether the mode is unset or set to `nspawn`, so declaring the default on
an existing host changes nothing.

## Changing the mode

`spec.isolation` declares the mode a node moves to at its next repave, which the
agent performs when the Node is deleted. Setting it never moves a running node.

The agent in this release cannot change the mode of a running node. If a repave
finds a different mode declared (for example from a newer Machine CRD), the
agent refuses the change, reports it in `status.isolationChange` as `Failed`
with reason `UnsupportedMode`, and repaves in the mode the node already runs so
version and configuration rollouts continue.

A change that resets and bootstraps the host again (a `Reprovision`) also needs
an explicit opt-in separate from `spec.isolation`: the annotation
`unbounded-cloud.io/allow-reprovision: "true"` on the Machine. Without it, an
agent that supports such a change refuses it with reason
`ReprovisionNotAllowed` and repaves in the current mode.

## Drift

The agent records the mode it applied in `status.isolation` on the `Machine`,
and its latest attempt to change the mode in `status.isolationChange`. Only the
agent writes these fields. Machina compares `status.isolation` with
`spec.isolation` and sets the `IsolationDrift` condition:

| Reason | Status | Meaning |
|---|---|---|
| `NoModeDeclared` | `False` | `spec.isolation` is empty; a repave leaves the node as it is |
| `AppliedModeMatches` | `False` | The declared mode runs |
| `AppliedModeDiffers` | `True` | A different mode is declared; it applies at the next repave |
| `ChangeInProgress` | `True` | The repave is changing the mode now |
| `ChangeStale` | `True` | The change has been `InProgress` for more than two hours without a new report; check the agent on the host |
| `ChangeFailed` | `True` | The last attempt at the declared mode failed; it is tried again at the next repave |
| `ChangeRefused` | `True` | The agent refused the change: it cannot run the mode (`UnsupportedMode`), or the change needs a reprovision and the Machine lacks the `unbounded-cloud.io/allow-reprovision` annotation (`ReprovisionNotAllowed`). The message says what to do |

The condition is absent until the agent has reported a mode. Nothing moves on
drift. The agent never writes `spec` on an existing Machine: the declaration is
yours, so a mismatch you create stays visible until a repave applies it or you
withdraw it. The only time the agent sets `spec.isolation` is when it creates
the Machine itself at self-registration, where it records the mode it runs. See
[Machina CRDs]({{< relref "reference/machina-crd" >}}).

Machina does not check the agent version. An agent that predates isolation
modes does not know the `Isolation` config key and ignores it, so it runs the
node as `nspawn`. Such an agent also does not report `status.isolation`, so
`IsolationDrift` stays absent.

## See also

- [systemd-nspawn Isolation]({{< relref "reference/agent/nspawn" >}})
- [Agent Configuration]({{< relref "reference/agent/configuration" >}})
