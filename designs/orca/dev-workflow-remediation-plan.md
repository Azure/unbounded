<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Orca Dev Workflow Remediation Plan

This plan addresses the dev-workflow review findings from the
`phlombar/orcadev-tool` branch after the first consolidation pass.
The goal is to keep the new single-entrypoint model, while making it
reliable on both kind and existing clusters such as AKS.

## Goals

- `./hack/orca/setup-orca.sh` remains the single correct installer.
- `bin/orcadev <verb>` works against a default dev install on kind and
  non-kind clusters without manual `kubectl port-forward` commands.
- The default install stays Azurite origin + LocalStack S3 cachestore.
- Destructive operations do not delete unrelated cluster resources.
- The quickstart examples are executable as written.

## Non-Goals

- Production-grade auth or mTLS changes.
- Reworking Orca's runtime Deployment topology beyond what is needed
  for developer workflow reliability.
- Validating AKS end-to-end in this change; AKS validation remains a
  follow-up owner task.
- Changing `--preset=none` semantics. There are no current users to
  protect; the existing preset behavior is fine for the dev path and
  custom-config users can supply the full set of overrides explicitly.

## 1. Storage Port-Forwards for All orcadev Commands

### Problem

`orcadev` only calls the auto-port-forward path from `roundtrip`,
`bench`, and `scenario`. The first quickstart command,
`bin/orcadev upload --generate ...`, talks directly to
`localhost:30100` without opening `svc/azurite`, and cache commands
talk directly to `localhost:30200` without opening `svc/localstack`.
This works on kind because of host NodePort mappings, but fails on AKS
and other non-kind clusters once those NodePort mappings go away
(see section 2).

### Plan

- Call `ensurePortForwards(ctx, g)` at the top of every subcommand's
  `RunE` (`upload`, `list`, `delete`, `cache list/inspect/clear`,
  plus the existing `roundtrip`/`bench`/`scenario`). Defer the
  returned cleanup as the very next statement so cleanup ordering
  is uniform.
- Do NOT introduce scoped helpers (origin-only, cachestore-only,
  edge-only, all). `derivePortForwardSpecs` already inspects the
  resolved endpoints and dedupes correctly, and probing-first means
  the cost of an unused forward is one TCP probe with a 500ms cap.
  The structural benefit of one canonical call eliminates a class
  of "we forgot to plumb the right helper into the new subcommand"
  bugs.
- Keep TCP-probe-first behavior. If kind NodePorts (none after
  section 2 lands, but the probe still no-ops on any pre-bound
  local port) or a user-managed `kubectl port-forward` already
  bind the local port, the helper returns a no-op cleanup.
- Preserve `--auto-port-forward=false` as a global kill switch.
- Update stale comments in `roundtrip`, `bench`, and `scenario` so
  they no longer say only `svc/orca` is forwarded.
- Document explicitly why this is not done in `PersistentPreRunE`:
  cleanup lifecycle (PreRun returns before the subcommand starts;
  the deferred cleanup belongs in the subcommand frame).

### Tests

- Add a test that exercises `derivePortForwardSpecs` for every
  subcommand entrypoint: confirm the expected spec set is what
  the universal call would produce for that subcommand's resolved
  flags.
- Add a test for `runUpload`/`runList`/`runCacheList` using the
  existing fake clients plus a hook in `ensurePortForwards` (or a
  thin seam) to assert the call is made before any backend
  operation.
- Run `go test ./hack/cmd/orcadev/...`.

### Acceptance Criteria

- `bin/orcadev upload --generate --count 1 --size 1MiB` succeeds on a
  cluster where Azurite is reachable only through `kubectl
  port-forward`.
- `bin/orcadev cache list` succeeds on a cluster where LocalStack is
  reachable only through `kubectl port-forward`.
- No manual `kubectl port-forward` is required for any README command.

## 2. Drop Kind NodePort: ClusterIP Everywhere

### Problem

The dev emulator Service templates render as fixed NodePorts
(`30100`, `30200`). Existing clusters can reject this if the ports
are already allocated or NodePort is restricted by policy. The
kind-only `extraPortMappings` block adds conditional template logic
and divergent install behavior between kind and non-kind clusters.

### Plan

- Change `deploy/orca/dev/01-localstack.yaml.tmpl` and
  `deploy/orca/dev/03-azurite.yaml.tmpl` Services to `type: ClusterIP`
  unconditionally. Remove the `nodePort:` lines and the comment
  paragraphs that justify NodePort.
- Remove `extraPortMappings` from `hack/orca/kind-config.yaml`. The
  cluster becomes a plain 3-worker spec.
- Remove the `AzuriteNodePort` and `LocalstackNodePort` `--set`
  arguments from `hack/orca/setup-orca.sh` (no longer consumed by
  templates).
- Update the README troubleshooting note about NodePort if any
  remains, and reword `kind-config.yaml` and template comments to
  reflect ClusterIP-only.
- Rationale to capture in a comment: orcadev port-forwards, so
  NodePort offers no value once kind users can no longer rely on
  fixed localhost ports for ad-hoc curl. Users who want ad-hoc curl
  on any cluster (kind or otherwise) run `kubectl port-forward
  svc/azurite 10000:10000` themselves.

### Tests

- Update `internal/orca/manifests` tests so the dev render
  expectations cover ClusterIP output.
- Verify `setup-orca.sh --no-wait` renders no `nodePort:` entries.
- Run `go test ./internal/orca/manifests/...` and
  `go test ./hack/cmd/orcadev/...`.

### Acceptance Criteria

- Default `setup-orca.sh --context <any>` applies emulator Services
  with `type: ClusterIP` and no `nodePort:` field.
- `make orca-kind-up` works without any host port mapping.
- `bin/orcadev` works on kind without depending on a previously
  bound `localhost:30100` / `localhost:30200`.

## 3. Make Uninstall Non-Destructive by Default

### Problem

`setup-orca.sh --uninstall` deletes the entire namespace. On an
existing dev cluster, the default namespace `unbounded-kube` may
also contain other Unbounded components. This can delete unrelated
resources.

### Plan

- Switch `--uninstall` to label-selector deletion using labels that
  are already set in the manifests:
  - `app.kubernetes.io/name=orca` (Orca's own resources).
  - `app.kubernetes.io/part-of=orca-dev` (the dev emulators).
- Delete by label across the resource kinds we know we own
  (`deployment,service,configmap,secret,serviceaccount`).
- Do NOT delete the namespace by default.
- Add `--delete-namespace` as an explicit option. When set, print a
  warning that every resource in the namespace will be removed and
  perform `kubectl delete namespace`.
- Update README teardown examples:
  - Existing cluster: `setup-orca.sh --uninstall`.
  - Disposable kind cluster: `make orca-kind-down`.
  - Explicit namespace cleanup: `setup-orca.sh --uninstall
    --delete-namespace`.
- The label selector approach is more robust than the previous
  name-based enumeration and absorbs future renames or additions
  automatically.

### Tests

- Add a kind smoke step that (a) creates a sentinel ConfigMap in
  `unbounded-kube`, (b) runs `setup-orca.sh --uninstall`, (c) asserts
  the sentinel still exists.
- Run `bash -n hack/orca/setup-orca.sh` for syntax.

### Acceptance Criteria

- `setup-orca.sh --uninstall` does not delete unrelated resources in
  `unbounded-kube`.
- `setup-orca.sh --uninstall --delete-namespace` is the only path
  that removes the namespace.

## 4. README Command Accuracy

### Problem

The README advertises scenarios that are not accepted by the current
CLI, and cache examples use the awss3 origin bucket (`orca-origin`)
even though the default install is azureblob/Azurite (`orca-test`).
The orcadev package docstring has the same scenario drift.

### Plan

- Limit the scenario list to commands accepted by `runScenario`:
  - `cold-warm`.
  - `range-stress`.
  - `empty-object`.
  - `etag-change`.
- Remove `multi-object` and `range-large` from both the README and
  the orcadev package docstring (`hack/cmd/orcadev/orcadev/orcadev.go`).
  Filing as future work is fine; they are not part of this change.
- Change default cache examples to use `orca-test` (the default
  azureblob container):
  - `bin/orcadev cache inspect --bucket orca-test --key orca-test.bin`.
  - `bin/orcadev cache clear --object orca-test/orca-test.bin --yes`.
- Add an awss3 appendix note showing `orca-origin` only after
  `setup-orca.sh --origin awss3`.

### Tests

- Manually run every README quickstart command on kind after the doc
  fixes. The kind smoke section in the validation matrix below
  encodes this.

### Acceptance Criteria

- Every command in the happy-path README quickstart is accepted by
  the CLI.
- Default cache examples target `orca-test`.

## 5. Relax Anti-Affinity for Non-Kind Installs via Template Knob

### Problem

The default install uses three Orca replicas, and the Deployment
requires one Orca pod per node through hard pod anti-affinity. A
single-node or two-node existing cluster will time out during
rollout. The README's existing-cluster path does not mention this.

### Plan

- Add a `RequireAntiAffinity` template knob to
  `deploy/orca/04-deployment.yaml.tmpl`:
  - When `true`, emit the existing
    `requiredDuringSchedulingIgnoredDuringExecution` block.
  - When `false`, emit
    `preferredDuringSchedulingIgnoredDuringExecution` with the same
    selector. This lets small clusters schedule multiple replicas
    on one node when nothing better is available, while still
    preferring spread when room exists.
- Update `setup-orca.sh`:
  - Default `RequireAntiAffinity=true` when the target context is
    `kind-*`.
  - Default `RequireAntiAffinity=false` otherwise.
  - Expose `--require-anti-affinity` and `--no-require-anti-affinity`
    flags only if needed; otherwise the auto-default is fine.
- Update the README existing-cluster path with one sentence saying
  the dev install will schedule Orca on whatever nodes are available
  on non-kind clusters; for production-shape spread, pass a config
  with the strict anti-affinity restored.
- Keep kind installs strict so the dev harness mirrors the production
  topology shape and the existing inttest coverage stays meaningful.
- Skipping the alternative "preflight count nodes from bash" approach
  because counting schedulable nodes correctly across taints and
  cordons is fiddly and the user experience of "install fails, here's
  a flag" is worse than "install just works."

### Tests

- Extend `internal/orca/manifests` render coverage for both
  `RequireAntiAffinity=true` and `false`.
- Manual smoke:
  - kind 3-node path renders `requiredDuringScheduling...`.
  - Single-node minikube-ish render path produces
    `preferredDuringScheduling...`.

### Acceptance Criteria

- Default kind install keeps strict anti-affinity.
- Default non-kind install uses preferred anti-affinity and rolls
  out on clusters with fewer than 3 nodes.

## 6. Clean Up Tempdir Traps

### Problem

`setup-orca.sh` installs one `trap` for the kind image archive
tempdir and later overwrites it with the rendered manifest tempdir
trap. This can leak a temporary image archive directory on
`--kind-load` runs.

### Plan

- Replace ad hoc `trap` calls with a single cleanup stack:
  - `cleanup_paths=()`.
  - `cleanup() { rm -rf "${cleanup_paths[@]}"; }`.
  - `trap cleanup EXIT` once near the top of the script.
  - Append each tempdir to `cleanup_paths` after `mktemp -d`.

### Tests

- `bash -n hack/orca/setup-orca.sh`.
- Manual run with and without `--kind-load`; ensure both paths exit
  cleanly and leave no stale tempdirs in `/tmp`.

### Acceptance Criteria

- One `trap` owns all tempdir cleanup.
- No tempdir is leaked in normal success or failure paths.

## 7. `make orca-install` and `--build` Safety Rails

### Problem

`make orca-install` always passes `ORCA_DEV_IMAGE=ghcr.io/azure/orca:dev`
to `setup-orca.sh` against whatever the current kubectl context is.
On non-kind clusters that image is not in any registry the cluster
can pull, so pods enter `ImagePullBackOff`. Separately,
`setup-orca.sh --build` without `--kind-load` happily builds an image
that no cluster consumes.

### Plan

- Update root `Makefile` `orca-install`:
  - If `ORCA_DEV_IMAGE` is the default (`ghcr.io/azure/orca:dev`)
    and the current kubectl context is not `kind-*`, error with a
    message telling the user to either switch to a kind context or
    pass `ORCA_DEV_IMAGE=my-registry/orca:tag`.
  - Implementation: a small shell block in the recipe that inspects
    `kubectl config current-context` before invoking the script.
- Update `setup-orca.sh`:
  - If `--build` is passed without `--kind-load`, error with a
    message saying the built image must be loaded somewhere
    (pass `--kind-load` for kind, or push the image manually for
    other clusters).
  - Keep `--build --kind-load` as the only supported combination.

### Tests

- `bash -n hack/orca/setup-orca.sh`.
- Manual check that `make orca-install` against a non-kind context
  with no `ORCA_DEV_IMAGE` override prints the explanatory error
  rather than applying a broken Deployment.

### Acceptance Criteria

- `make orca-install` cannot silently install a broken image into a
  non-kind cluster.
- `setup-orca.sh --build` alone fails fast with a clear message.

## Suggested Implementation Order

1. Drop kind NodePort, switch emulator Services to ClusterIP
   (section 2). Pure correctness fix in the install pipeline.
2. Hardened uninstall (section 3). Removes the namespace-deletion
   foot-gun before more people pick up the script.
3. Universal `ensurePortForwards` in every orcadev subcommand
   (section 1). Becomes essential once section 2 removes the kind
   NodePort short-circuit.
4. README accuracy and package docstring fix (section 4).
5. Relax anti-affinity for non-kind installs (section 5).
6. Tempdir trap cleanup (section 6).
7. `make orca-install` and `--build` safety rails (section 7).

## Final Validation Matrix

- `go test ./hack/cmd/orcadev/...`.
- `go test ./internal/orca/manifests/...`.
- `go test ./internal/orca/...` (full Orca test surface).
- `golangci-lint run ./...`.
- `gofumpt -d <changed Go files>`.
- `bash -n hack/orca/setup-orca.sh hack/orca/kind-up.sh hack/orca/kind-down.sh`.
- `make orca-inttest`.
- kind smoke:
  - `make orca-kind-up`.
  - `bin/orcadev upload --generate --count 1 --size 1MiB`.
  - `bin/orcadev list`.
  - `bin/orcadev roundtrip --file /tmp/orca-test.bin --cleanup`.
  - `bin/orcadev scenario cold-warm`.
  - `bin/orcadev cache list`.
  - `bin/orcadev cache inspect --bucket orca-test --key orca-test.bin`.
- Non-kind design validation:
  - Rendered emulator Services are ClusterIP by default with no
    `nodePort:` field.
  - Rendered non-kind Deployment uses
    `preferredDuringSchedulingIgnoredDuringExecution`.
  - orcadev opens port-forwards before upload/list/cache operations.
  - `setup-orca.sh --uninstall` leaves unrelated namespace resources
    intact; namespace stays.
  - `setup-orca.sh --uninstall --delete-namespace` removes the
    namespace.
  - `make orca-install` against a non-kind context with default
    image errors out instead of applying a broken Deployment.
