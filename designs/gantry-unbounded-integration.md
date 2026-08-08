# Gantry + Unbounded Integration

## Background

Gantry is a P2P OCI image distribution agent that runs as a Kubernetes DaemonSet. Today it
is deployed independently of the unbounded stack. Operators manage two separate install and
upgrade workflows with no coordinated versioning between them.

## Goals

- Gantry becomes an optional component of an unbounded site, it gets installed alongside machina and
  unbounded-net when the operator opts in.
- Gantry ships in the versioned release tarball so all components move together.
- Operators who just want gantry on a plain cluster can still apply the manifests directly -
  no unbounded tooling required.
- Day-2 operations (upgrades, per-node health, rollback) are available through
  `kubectl unbounded gantry`, consistent with how `kubectl unbounded net` works today.

## Out of scope

Running gantry as a host-level systemd service. The engineering cost is high relative to the
benefit; the priority is simpler ops at the Kubernetes layer first.

## Design

### Packaging

Gantry manifests will get parameterized and rendered the same way as unbounded-net today. They will 
ship in the release tarball under a `gantry/` directory alongside machina, machine-ops, and
net. Operators who want to apply them directly without any tooling can still do so - the
files in `deploy/gantry/` remain plain kubectl-apply-able YAML.

### Installation

`Site.spec.components` gains an optional Gantry component. When set, `unbounded-operator`
installs and reconciles Gantry from the declarative Site configuration.

### Day-2 operations

A new `kubectl unbounded gantry` command group mirrors `kubectl unbounded net`. It covers
the three scenarios operators run into most after initial install:

**Status** - a per-node table showing each node's gantry version, readiness, DHT health
score, cache hit count, and storage backend. Useful for confirming a rollout landed cleanly
or spotting a node that fell behind.

**Upgrade** - updates the DaemonSet image and watches the rollout. If the percentage of
unhealthy nodes exceeds a configurable threshold during the rollout, the command exits with
an error and leaves the decision to the operator. Readiness is checked via port-forward so
it works on clusters where pod IPs are not directly routable.

**Rollback** - reverts to the previous DaemonSet revision and confirms recovery using the
same health check as upgrade.

### What stays the same

Gantry's internals are untouched. It continues to run as a DaemonSet, uses Kubernetes pod
annotations for peer discovery, and connects to containerd via the host socket. No changes
to its RBAC, ConfigMap structure, or namespace.

## Phasing

**Phase 1** - packaging and install: parameterized manifests, release tarball inclusion,
`--with-gantry` in `site init`.

**Phase 2** - day-2 tooling: `kubectl unbounded gantry` with status, upgrade, and rollback.

**Later** - a controller-driven model where a CRD holds the desired gantry version and a
reconciler drives rollouts automatically. Not in scope now.
