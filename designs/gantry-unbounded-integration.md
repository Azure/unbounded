# Gantry + Unbounded Integration

## Current implementation

The original proposal below is historical. Gantry is now an enabled-by-default,
version-matched cluster singleton managed by `internal/operator/components/gantry`.
The existing `gantry-config` ConfigMap is preserved for registry and runtime
configuration, but it does not select the managed backend. Direct is the default.
Exactly one live user-managed P2PCache annotated
`unbounded-cloud.io/gantry-backing: "true"` selects Racer if its selector is empty,
all nonterminating Nodes have managed Racer coverage, and at least one live Site
enables Gantry. The operator never creates, adopts, changes, or deletes this
cache. Missing/false/removed annotations and selected-cache deletion return to
direct; invalid intent returns to direct with an `InvalidGantryBacking`
diagnostic. Failed API reads instead preserve the deployed configuration and
retry (`internal/operator/components/gantry/racer.go:37`).

Generated backend arguments override legacy ConfigMap backend settings. Gantry
starts its origin without a Racer-readiness gate; parent-directory socket mounts
survive socket replacement. The selected cache UID is stamped on the pod
template, so same-name cache recreation rolls Gantry
(`internal/operator/components/gantry/racer.go:122`, `:139`). Standalone processes
still support backend flags, environment variables, and YAML. The unbounded
agent owns containerd mirror wiring.

Any live P2PCache installs both Racer workloads, including with zero Sites or no
selector matches. Without live caches, each existing workload is maintained
independently with update-only operations. Removing all caches and then deleting
the Deployment and DaemonSet in either order uninstalls the workloads; support
resources alone never reinstall them
(`internal/operator/components/racer/racer.go:41`, `:73`, `:85`, `:115`).

The Site Racer `enabled` and `cacheSize` fields have been removed. Capacity is
the Node `racer.unbounded-cloud.io/cache-size` annotation, then `10Gi` when absent
(`internal/racer/cache_size.go:53`). Before upgrade, copy desired inherited Site
sizes to Node annotations. There is no automatic cache migration or automatic
selection of an old operator-created cache: annotate that cache explicitly to
keep using it. The old `unbounded-cloud.io/gantry-cache` label is ignored.

The supported rollout and limitations are documented in
[the public Gantry guide](../docs/content/guides/gantry.md#optional-racer-backend).
The implementation does not promise atomic fleet-wide cutover, mixed per-Site
backends, or cache-hit authorization isolation between tenants. Racer owns the
64 MiB page cache; containerd remains the committed image store.
Backend switches use normal rolling updates, with transient retries and loss of
warm cache reuse acceptable during convergence.

## Historical proposal: background

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
