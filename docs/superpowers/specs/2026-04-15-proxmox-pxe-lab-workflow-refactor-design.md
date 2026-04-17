# Proxmox PXE Lab Workflow Refactor Design

## Goal

Refactor `hack/cmd/proxmox-pxe-lab` from a monolithic scenario runner into a GitHub Actions-friendly Proxmox lab helper with focused subcommands.

The tool should prepare and describe the Proxmox PXE environment in a way that lets workflows own the actual test intent, such as when to create `Machine` objects, when to patch them, and which validations to run.

## Success Criteria

- `proxmox-pxe-lab` exposes focused subcommands instead of centering the workflow around a single `--mode` state machine.
- A `setup` command can prepare the Proxmox PXE environment and emit stable artifacts for later workflow steps.
- `setup` writes both:
  - normalized inventory
  - a structured environment artifact containing the defaults later steps need
- A `render-machines` command can render `Machine` manifests from the emitted artifacts without re-deriving Proxmox-specific defaults.
- A `verify-ready` command can validate node readiness from inventory without reprovisioning.
- Proxmox-specific knowledge is concentrated in setup-oriented code paths rather than spread through all commands.
- The resulting command surface maps cleanly to separate GitHub Actions steps.

## Non-Goals

- Creating a generic backend-agnostic `pxe-ci` abstraction in this pass.
- Replacing GitHub Actions workflow logic with a new monolithic orchestration layer.
- Changing the underlying `Machine` API shape.
- Removing working Proxmox-specific assumptions that are genuinely fixture-specific and still needed.
- Solving all possible future lab backends in this design.

## Problem Statement

The current tool mixes three different concerns in one command flow:

- Proxmox environment setup
- `Machine` manifest generation
- end-to-end scenario execution and validation

That shape was useful for quickly proving the PXE golden path, but it is awkward for CI workflows.

In GitHub Actions, test logic naturally wants to be split across multiple explicit steps. For example:

- prepare the Proxmox fixture environment
- render or apply `Machine` objects
- wait for nodes to become ready
- patch a `Machine` to trigger reboot or repave
- validate the post-action state
- collect logs and artifacts on failure

The current `--mode` flow obscures those boundaries by bundling provisioning, rendering, apply, readiness waiting, reboot validation, and summary writing into one command.

## Recommended Approach

Keep `proxmox-pxe-lab` intentionally Proxmox-specific, but refactor it into focused subcommands that align with workflow steps.

This is preferred over extracting a new generic `pxe-ci` tool because the actual hard-coded behavior is still largely about provisioning and describing the Proxmox fixture environment. The immediate need is not backend abstraction; it is composability for CI.

## Proposed Command Surface

### 1. `setup`

Purpose:

- validate prerequisites
- optionally start or restart `metalman serve-pxe`
- optionally provision fresh Proxmox VMs
- retrieve and normalize inventory
- write artifacts consumed by later workflow steps

Primary outputs:

- `inventory.yaml`
- `env.yaml`
- run summary

This command becomes the primary Proxmox boundary.

### 2. `render-machines`

Purpose:

- read normalized inventory and environment defaults
- render `Machine` manifests deterministically
- allow explicit overrides where needed

Primary output:

- `machines.yaml`

This command should not provision infrastructure or wait for nodes.

### 3. `verify-ready`

Purpose:

- read inventory
- wait for listed nodes to become `Ready`
- emit a summary result

This command remains narrowly focused on readiness validation.

### 4. `reset`

Purpose:

- clean up or reset the Proxmox fixture state when a workflow or debug session needs a known baseline

Expected responsibilities may include:

- deleting relevant `Machine` resources
- deleting relevant `Node` resources
- stopping and destroying PXE VMs

This command is useful but can be implemented after the first three if needed.

### Deferred commands

Possible later additions:

- `collect-artifacts`
- `verify-reboot`
- `verify-repave`

These are intentionally deferred so the first refactor stays small and centered on workflow composition.

## Artifact Model

### Inventory artifact

The inventory artifact should remain the normalized list of provisioned VMs used for subsequent steps.

It should contain, at minimum:

- machine name
- VMID
- MAC
- IPv4

Normalization rules that are currently buried in rendering logic should be moved to a single setup-time or inventory-load path.

### Environment artifact

`setup` should write a structured YAML artifact, not a shell snippet.

Suggested contents:

- `site`
- `proxmoxHost`
- `kubeconfigPath`
- `pxeImage`
- `bootstrapTokenName`
- `redfish`
  - `url`
  - `username`
  - `secretName`
  - `secretNamespace`
  - optional shared `secretKey`
- `network`
  - `subnetMask`
  - `gateway`
  - `dns`
- `artifacts`
  - inventory path
  - machine manifest path
  - summary path

The point of `env.yaml` is to make later workflow steps consume explicit, versioned inputs instead of reconstructing defaults from CLI flags or Proxmox assumptions.

## Command Responsibilities

### `setup` responsibilities

Owns:

- Proxmox SSH validation
- host-side prerequisite checks
- optional GHCR validation for the PXE image
- optional `metalman` startup
- optional fresh VM provisioning
- inventory retrieval from the Proxmox host
- inventory normalization
- writing `inventory.yaml`, `env.yaml`, and summary output

Knows about:

- `/root/create-stretch-pxe-vms.sh`
- `/root/stretch-pxe-inventory.yaml`
- `/root/metalman`
- host-side log paths
- Proxmox/Redfish endpoint conventions

### `render-machines` responsibilities

Owns:

- reading `inventory.yaml`
- reading `env.yaml`
- validating required rendering inputs
- rendering deterministic `Machine` YAML
- optional overrides for bootstrap token, PXE image, or BMC secret key when explicitly requested

Does not own:

- starting services
- provisioning VMs
- applying manifests
- waiting for node readiness

### `verify-ready` responsibilities

Owns:

- reading inventory
- ensuring the expected node count matches inventory
- waiting for each node to become `Ready`
- writing success/failure summary

Does not own:

- reprovisioning
- rendering manifests
- applying manifests

### `reset` responsibilities

Owns:

- removing fixture state that would contaminate a future run

This should be explicit about scope so it does not accidentally become a destructive catch-all.

## GitHub Actions Workflow Shape

The intended workflow composition becomes:

1. `proxmox-pxe-lab setup ...`
2. `proxmox-pxe-lab render-machines --inventory ... --env ... --out ...`
3. `kubectl apply -f machines.yaml`
4. `proxmox-pxe-lab verify-ready --inventory ... --kubeconfig ...`
5. direct workflow-owned test actions
   - patch `Machine` objects
   - inspect `Machine` status
   - assert intermediate state
6. optional follow-up validation or artifact collection

This preserves visibility of test intent in the workflow itself rather than burying it in a binary.

## Configuration Model

The current single `Config` struct should be split into:

- shared/common options
- subcommand-specific config structs

Examples:

- `SetupConfig`
- `RenderMachinesConfig`
- `VerifyReadyConfig`
- `ResetConfig`

This avoids carrying irrelevant fields such as `BootstrapTokenName` or `MachineManifestOut` through commands that do not use them.

## Internal Refactor Boundaries

The refactor should separate these internal concerns:

### 1. Artifact I/O

Helpers for:

- reading and writing inventory
- reading and writing environment artifacts
- reading and writing summaries

### 2. Proxmox fixture operations

Helpers for:

- SSH preflight
- starting `metalman`
- provisioning VMs
- retrieving inventory
- optional cleanup/reset actions

### 3. Machine rendering

Helpers for:

- turning inventory + environment defaults into `Machine` manifests

### 4. Validation helpers

Helpers for:

- waiting for node readiness
- reading and patching `Machine.spec.operations` counters where needed later

This keeps setup-oriented Proxmox behavior separate from render and verification behavior.

## Backward Compatibility Strategy

The old `--mode` entrypoint should not be preserved indefinitely, but a short migration window is acceptable if it reduces churn.

Recommended approach:

- introduce the new subcommands first
- keep the existing mode-based path only temporarily if needed to avoid breaking active users mid-refactor
- remove the old mode flow once workflow callers are updated

The long-term supported surface should be subcommands, not modes.

## Error Handling

Each subcommand should fail within its own boundary and write a summary that reflects the failing phase.

Examples:

- `setup` failures should clearly identify preflight, PXE runtime startup, provisioning, or inventory retrieval
- `render-machines` failures should clearly identify malformed inventory or missing environment inputs
- `verify-ready` failures should clearly identify node timeout vs. missing inventory

This makes CI failures easier to attribute to either fixture setup, manifest generation, or cluster convergence.

## Testing Strategy

Add or update tests to cover:

- subcommand parsing and validation
- setup artifact writing
- environment artifact schema round-trip
- rendering from normalized inventory + env artifact
- verify-ready behavior using preexisting inventory
- reset command argument validation and safe targeting

The tests should prefer focused command behavior over broad end-to-end command-state-machine tests.

## Risks

### Risk: command surface churn

Changing from modes to subcommands will require workflow updates.

Mitigation:

- stage the migration
- keep artifact names and behavior predictable

### Risk: duplicated defaults across setup and render

If defaults are computed in both commands, drift will appear.

Mitigation:

- compute defaults once
- persist them in `env.yaml`
- have render consume the artifact rather than re-derive values

### Risk: reset becomes too destructive

Cleanup logic can easily become unsafe.

Mitigation:

- scope reset to explicit site/nodes/inventory
- require clear inputs
- avoid broad host cleanup beyond the fixture-owned resources

## Open Decisions Resolved In This Design

- Keep the tool Proxmox-specific: yes
- Optimize for GitHub Actions composition: yes
- Use subcommands instead of `--mode`: yes
- Have `setup` emit both inventory and a structured environment artifact: yes
- Keep `kubectl apply` and test intent outside `setup`: yes

## Summary

This refactor keeps `proxmox-pxe-lab` focused on what it actually knows best: preparing and describing the Proxmox PXE fixture environment.

Instead of owning the full scenario, it becomes a workflow-friendly helper that:

- sets up the fixture
- emits stable artifacts
- renders manifests deterministically
- provides focused validation helpers

That produces a cleaner fit for GitHub Actions and a clearer separation between fixture management and test logic.
