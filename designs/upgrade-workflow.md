# Control plane upgrade workflow

Status: current behavior as implemented

This document describes how the Unbounded control plane is upgraded: the
`unbounded-operator` and the components it manages. It covers both the
high-level model and the mechanics behind it.

## Summary

The upgrade unit is the operator.

The operator's compiled-in version is the version of every component it manages.
Upgrading a cluster means replacing one image: the `unbounded-operator`
Deployment. The operator then re-applies every component workload at its own
version, and Kubernetes performs the rollouts.

Two consequences follow from that and are worth stating up front:

- **The operator does not upgrade itself.** Something external replaces its
  image. That is `kubectl unbounded install` run from a newer plugin, or a
  direct apply of the released operator manifest. There is no
  `kubectl unbounded upgrade` command.
- **There is no per-component version selection.** The `Site` API exposes no
  version, image, tag, or channel field. Components are upgraded as a set, in
  lockstep with the operator.

## Scope

The operator owns two things: the CRDs, and five component workloads.

| Component | Kind | Workloads |
|-----------|------|-----------|
| `net` | cluster singleton | `Deployment/unbounded-net-controller`, `DaemonSet/unbounded-net-node` |
| `machina` | cluster singleton | `Deployment/machina-controller` |
| `gantry` | cluster singleton | `DaemonSet/gantry`, `DaemonSet/gantry-containerd-config` |
| `metalman` | per-Site | `Deployment/metalman-controller-<site>` |
| `storage` | per-Site | `DaemonSet/unbounded-storage-supervisor-<site>` |

Registered in `operator.DefaultRegistry` (`internal/operator/reconciler.go:66-78`).
Cluster components reconcile from the full set of `Site` objects on every pass;
per-Site components reconcile once per `Site`.

The operator also owns twelve CRDs, six in the `unbounded-cloud.io` group and six
in `net.unbounded-cloud.io` (`internal/operator/bootstrap.go:49-62`).

**Out of scope for this document.** The host-resident `unbounded-agent` binary
and node OS/kubelet changes are *not* part of this model. They are separate
update planes with their own triggers (`MachineOperation` of kind `AgentUpgrade`,
and `MachineConfiguration` repave respectively) and are not driven by an operator
upgrade. See `designs/agent-upgrade.md` and
`designs/lifecycle-operations.md`. Components deployed outside the operator
(`machine-ops-controller`, inventory, orca, playpen) are also out of scope.

## High-level workflow

```
  release tag
      |
      +--> container images pushed as <registry>/azure/<component>:<TAG>
      +--> every Go binary stamped with internal/version.Version = <TAG>
      +--> build/unbounded-operator-<TAG>.yaml published
      |
      v
  kubectl unbounded install        (or kubectl apply -f unbounded-operator-<TAG>.yaml)
      |
      |  updates ONLY the operator Deployment image + its ConfigMap
      v
  operator pod restarts (strategy: Recreate)
      |
      +--> [startup]  apply all 12 CRDs, wait for Established
      +--> [ready]    health server binds, manager starts
      |
      v
  Site reconcile
      |
      +--> for each component: render manifests,
      |      overwrite container images with <registry>/azure/<name>:<TAG>,
      |      stamp config hash, server-side apply
      v
  Kubernetes rolls each workload per its own update strategy
      |
      v
  Site.status.conditions: NetReady / MachinaReady / GantryReady
                          MetalmanReady / StorageReady
```

Nothing in this flow compares an installed version against a desired version.
The operator stamps its own version unconditionally and lets server-side apply
converge. That makes the reconcile idempotent and means a component that drifts
(manual edit, partial failure, restored backup) is corrected on the next pass
rather than only at upgrade time.

## Technical detail

### Where the version comes from

`cmd/unbounded-operator/main.go:206-207` builds the component config with:

```go
Config: operator.Config{
    ImageRegistry:     cfg.imageRegistry,
    ImageTag:          version.Version,
    APIServerEndpoint: cfg.apiServerEndpoint,
}
```

`version.Version` is set at link time by `STAMP_LDFLAGS` (`Makefile:196-198`)
from `VERSION`, which defaults to `git describe --tags --always --dirty`
(`Makefile:186-191`). The operator binary therefore carries the fleet version.

The registry is separate and resolves from `--image-registry`, then
`$UNBOUNDED_IMAGE_REGISTRY`, then `ghcr.io` (`cmd/unbounded-operator/main.go:76`).

### Image resolution

`internal/operator/component/env.go:74-76`:

```go
func (c Config) Image(repository string) string {
    return strings.TrimRight(c.ImageRegistry, "/") + "/azure/" + repository + ":" + c.ImageTag
}
```

So registry `ghcr.io`, repository `machina`, version `v0.2.0` yields
`ghcr.io/azure/machina:v0.2.0`. The `azure/` path segment is fixed, and the
registry is operator-wide: every managed component must be published to the same
registry under that path.

Two stamping helpers exist:

- `SetPodSpecImages` (`env.go:79-105`) rewrites every init and main container in
  a pod template. Used by `net`, `machina`, `metalman`, and `storage`.
- `SetNamedContainerImage` (`env.go:112-152`) rewrites only the named container.
  Used by `gantry` (`internal/operator/components/gantry/gantry.go:204`) so its
  pinned busybox init container and the busybox-based
  `gantry-containerd-config` DaemonSet keep their own images.

### Embedded manifests

Most components ship as rendered YAML compiled into the operator binary with
`go:embed` (`deploy/net/embed.go`, `deploy/machina/embed.go`,
`deploy/gantry/embed.go`, `deploy/unbounded-storage-supervisor/embed.go`), and
`unbounded-operator-build` depends on the render targets for all of them
(`Makefile:611`). The operator therefore ships a self-consistent snapshot of
those components' manifests.

The image tag baked into that embedded YAML is effectively a build artifact: it
is overwritten at reconcile by `Config.Image(...)`. To keep that safe,
`TestEmbeddedManifestsHaveNoLatestImageTags`
(`internal/operator/manifests_guard_test.go`) fails `make test` if any embedded
component manifest pins an image to `:latest`, on the grounds that
operator-managed components must be version-matched to the operator's release.

`metalman` is the exception: it has no embedded manifest. Its Deployment is
constructed directly in Go (`internal/operator/components/metalman/metalman.go:98-195`)
because it is per-Site and its shape depends on `Site` fields such as
`dhcpAutoInterface` and `replicas`.

### Apply semantics

All component objects are applied with server-side apply using field manager
`unbounded-operator` and `ForceOwnership`
(`internal/operator/component/env.go:29-32`, `env.go:234`). Force ownership
means the operator reclaims fields another manager has taken, so manual
`kubectl edit` changes to managed fields are reverted on the next reconcile.

### CRD lifecycle

CRDs are handled at operator startup, not in the reconcile loop. Component
reconcilers explicitly skip `CustomResourceDefinition` objects in the embedded
manifests (for example `net.go:117-121`).

`operator.BootstrapCRDs` (`internal/operator/bootstrap.go:77-79`) server-side
applies every CRD and waits for each to become `Established`. It runs before the
manager starts, because the typed `Site` informer cannot sync until the `Site`
CRD is served. The whole operation is bounded by `CRDBootstrapTimeout`, four
minutes (`bootstrap.go:45`), and the operator's health server only binds after
bootstrap completes. The Deployment `startupProbe` budget is deliberately sized
to exceed that window: 54 failures at 5s, 270s
(`deploy/unbounded-operator/04-deployment.yaml.tmpl:85-86`).

`CRDMaintainer` (`bootstrap.go:87-137`) then re-applies the CRDs every 60s under
leader election. Maintenance failures are logged and retried rather than
crashing the manager, because established CRDs stay served by the apiserver
regardless of operator liveness.

The practical effect for upgrades: **CRD schema changes ship with the operator
and land before any component reconciles.** An admin never applies CRDs
separately. `kubectl unbounded install` stopped doing so for exactly this reason
(`cmd/kubectl-unbounded/app/install.go:140-143`).

### Component configuration is preserved across upgrades

Component ConfigMaps are create-only. Each component's `ensureConfig` creates the
embedded default only when no ConfigMap exists, and otherwise leaves the existing
object untouched (`net.go:154-187`, `gantry.go:248-282`, `storage.go:111-140`).
The apply mutator then skips that ConfigMap so the manifest apply cannot
overwrite it either (`net.go:123-127`).

Operator upgrades therefore do not clobber operator-tuned component config. The
trade-off is that new default keys introduced by a release are not merged into an
existing ConfigMap; they have to be added deliberately.

`machina` is the one exception. It patches `apiServerEndpoint` into the existing
`config.yaml` when the operator-resolved endpoint differs, and only that key
(`machina.go:183-202`).

### Config changes trigger rollouts

Each component computes a SHA-256 over the complete ConfigMap payload
(`internal/operator/component/configmap.go:17-31`) and stamps it as a pod
template annotation, for example `unbounded-cloud.io/net-config-hash`
(`net.go:31`, stamped at `net.go:144`). Editing a component ConfigMap changes
the pod template hash, which rolls the workload.

This is a separate rollout trigger from an image change, and it applies outside
upgrades: a config edit on a stable version rolls the affected workload the same
way.

### Retention

No component is auto-deleted when its enabling `Site` goes away. `net`,
`machina`, and `gantry` each check for existing resources and keep reconciling
them rather than uninstalling (`net.go:49-62`, `machina.go:57-69`,
`gantry.go:95-107`). The reasoning is sharpest for `net`: deleting
`unbounded-net-node` would break pod networking cluster-wide.

The consequence for upgrades is that a retained component continues to be
version-matched to the operator even with no `Site` enabling it. Removal
requires an explicit uninstall flow, which does not exist yet.

## Rollout behavior per workload

Once the operator applies the new images, Kubernetes drives each rollout using
that workload's own strategy. These differ meaningfully.

| Workload | Strategy | Availability during upgrade |
|----------|----------|-----------------------------|
| `unbounded-operator` | `Recreate` | Old pod terminates before the new one starts. Components keep running; only reconciliation pauses. |
| `unbounded-net-controller` | `RollingUpdate`, maxSurge 0, maxUnavailable 1 | Genuine zero-available window. |
| `metalman-controller-<site>` | `RollingUpdate`, maxSurge 0, maxUnavailable 1 | Genuine zero-available window. |
| `unbounded-net-node` | `RollingUpdate`, maxUnavailable 100% | All nodes update simultaneously. |
| `unbounded-storage-supervisor-<site>` | `RollingUpdate`, maxUnavailable 100% | All nodes update simultaneously. |
| `gantry` | `RollingUpdate`, maxUnavailable 10% | Gradual, deliberately limited blast radius. |

The `maxSurge: 0` settings are not accidental. Both `unbounded-net-controller`
and `metalman` run `hostNetwork` and bind host ports, so a surge pod cannot start
while the old pod holds them. Allowing surge would deadlock the rollout. The
reasoning is recorded at `deploy/net/controller/03-deployment.yaml.tmpl:17-24`
and `internal/operator/components/metalman/metalman.go:138-142`.

Both hostNetwork Deployments and `unbounded-operator` itself use
`imagePullPolicy: Always`, so a pull failure at the new tag surfaces as a stuck
rollout rather than a silent no-op against a stale cached image. `gantry` uses
`IfNotPresent`.

## Verification

### Operator rollout

`kubectl unbounded install --wait` blocks on the operator rollout, then on CRD
establishment (`install.go:152-160`).

The rollout check is stricter than usual and intentionally so
(`install.go:521-531`):

```go
return deploy.Status.ObservedGeneration >= deploy.Generation &&
    deploy.Status.UpdatedReplicas == desired &&
    deploy.Status.Replicas == desired &&
    deploy.Status.AvailableReplicas == desired
```

Requiring `Replicas == UpdatedReplicas == AvailableReplicas` matters during an
upgrade specifically. A weaker check can report success while the *old* operator
pod is still the available one, which would let a caller proceed against the
previous version.

### Component readiness

The reconciler publishes one condition per component on each `Site`, in registry
order (`internal/operator/reconciler.go:144-192`), so readiness is waitable:

```
kubectl wait --for=condition=NetReady      site/<name>
kubectl wait --for=condition=MachinaReady  site/<name>
kubectl wait --for=condition=GantryReady   site/<name>
kubectl wait --for=condition=MetalmanReady site/<name>
kubectl wait --for=condition=StorageReady  site/<name>
```

Conditions belonging to components no longer in the registry are pruned, so the
condition set tracks the operator version rather than accumulating history.

### Automated gates

Upgrade is covered by CI, not only by unit tests:

- `.github/workflows/release-upgrade.yaml` runs after the release build,
  verifies the signed release assets, decides `init` vs `upgrade` against the
  target cluster, runs `kubectl unbounded install`, waits for every managed
  workload's rollout, runs smoke tests, and only then publishes the draft
  release.
- `.github/workflows/operator-upgrade-e2e.yaml` and
  `hack/operator-upgrade-e2e/e2e.py` install a previously released version and
  upgrade to the current tree, asserting migration and CRD-repair behavior.
- `internal/operator/manifests_guard_test.go` blocks `:latest` in embedded
  component manifests as part of `make test`.

## Release artifacts

A release tag produces the inputs this workflow consumes (`Makefile:1387-1425`):

- `build/unbounded-operator-<VERSION>.yaml`, the rendered operator manifests
  concatenated into one directly appliable file.
- `build/unbounded-manifests-<VERSION>.tar.gz`, per-component rendered manifests
  plus a `VERSION` file, for inspection and for installs that do not use the
  plugin.
- `build/unbounded-release-bom-<VERSION>.json`, a digest-pinned bill of
  materials for the release images.

## Design characteristics and constraints

These are properties of the current model, recorded so they are considered
deliberately rather than discovered during an upgrade.

**Lockstep by design.** `SiteComponentSpec` carries only `Enabled`; the API
comment states it directly: components install at the operator's own version, so
neither namespace nor image is configurable per component
(`api/machina/v1alpha3/site_types.go:141-148`). This buys guaranteed version
consistency across components that share CRDs and wire formats, and it removes a
version-skew matrix. It costs the ability to upgrade or pin one component
independently, including to pick up a single hotfix.

**No canary or staged rollout across components.** A single reconcile pass
applies every component. Ordering is registry order, cluster components before
per-Site ones, but there is no gating: the operator does not wait for `net` to
become healthy before applying `machina`. Staging has to happen at the cluster
level, by upgrading clusters in sequence.

**Rollback means deploying an older operator, and is not symmetric.** Component
workloads revert cleanly, since their images are re-stamped from the older
operator's version. CRDs do not: `BootstrapCRDs` applies, it never removes or
reverts fields, so a downgrade leaves the newer CRD schema in place. Downgrades
across a CRD schema change need to be reasoned about individually.

**Two components have a real zero-available window.** `maxSurge: 0` on
`unbounded-net-controller` and `metalman` is forced by hostNetwork port binding.
For `net` this pauses CIDR allocation, gateway pool, and peering reconciliation
until the new pod is ready; the dataplane (`unbounded-net-node`) is unaffected by
the controller gap, but is itself rolled at `maxUnavailable: 100%`.

**Config defaults do not migrate.** Because component ConfigMaps are create-only,
a release that adds a new default config key does not add it to clusters that
already have the ConfigMap. New keys must either be optional with a sane in-code
default, or called out in release notes.

**Single registry.** `Config.Image` hardcodes the `azure/` path segment and uses
one registry for all components. Mirroring for an air-gapped or
restricted-registry install means mirroring every component to a single registry
under that path, and setting `UNBOUNDED_IMAGE_REGISTRY` accordingly.
