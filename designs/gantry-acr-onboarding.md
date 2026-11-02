# Operator-led ACR onboarding for Gantry

## Problem and verified baseline

A cluster and its Flex nodes can exist before an administrator enables Gantry or
adds a private ACR upstream. Adding the upstream currently configures routing,
not the source of kubelet image credentials. A healthy Gantry DaemonSet therefore
does not establish that private pulls work.

The implementation currently:

- Preserves the administrator's `gantry-config` payload and rolls Gantry when it
  changes (`internal/operator/components/gantry/gantry.go:446`).
- Leaves the default containerd mirror route to node provisioning
  (`pkg/agent/phases/nodestart/cri.go:62`).
- Supports optional kubelet image credential provider paths, but does not
  install the provider (`pkg/agent/phases/nodestart/kubelet.go:250`).
- Passes request-scoped registry authorization through Gantry, rather than
  requiring a second shared registry identity
  (`internal/gantry/mirror/mirror.go:760`).

The AKS Flex nodes inspected for this work have
`kubernetes.azure.com/nodepool-type=FlexNodes` and no kubelet image credential
provider. Native AKS nodes must not be modified by this integration.

## User contract

An administrator can add an Azure public-cloud ACR upstream to `gantry-config`
after the Flex nodes have joined. The operator installs and reconciles the
missing node authentication integration on those nodes, without changing
containerd mirror routing or requiring a node rejoin.

Registry credentials flow from a managed-identity kubelet credential provider
through kubelet/CRI and containerd to Gantry. Tokens are never written to
ConfigMaps, Secrets, logs, or node configuration files by this integration.

Azure role assignment remains an explicit authorization decision. This change
must not silently grant `AcrPull`, enable the ACR admin account, or copy a
credential out of the native AKS nodes. Installation readiness and a successful
private-image probe are separate states.

## Implementation plan

1. Add a small ACR credential-provider implementation using the Azure identity
   SDK already present in the module and Kubernetes' versioned exec JSON wire
   contract. Use managed identity to obtain an Entra ACR-audience token and exchange it for an
   ACR refresh token. Restrict token destinations to validated public-cloud ACR
   login servers; disable redirects and bound response sizes and timeouts.
   Reuse the versioned operator binary so no unpinned runtime download or new
   third-party Go dependency is needed.
2. Add a node-auth reconciler entry point to the operator binary. It copies its
   static binary, installs the provider configuration, and writes a dedicated,
   marked kubelet systemd drop-in. It must not edit the agent's generated unit
   or containerd configuration. Preserve the existing kubelet command and
   environment expansion, reject conflicting externally managed providers or
   drop-ins, write atomically, and restart kubelet only when necessary.
3. Have the Gantry component derive public ACR registry names from the existing
   upstream list. Optional `node-auth.yaml` in the same ConfigMap selects
   `auto` (default) or `external` authentication and an optional managed
   identity client ID and private probe image. Keep unrelated Gantry settings
   and non-ACR upstreams intact. Do not install node authentication for shared
   Gantry credentials. Keep kubelet authentication independent of the Gantry
   storage backend, including when switching to Racer.
4. Reconcile a separately named node-auth DaemonSet, generated configuration,
   and a narrow restart-coordination Lease. Target only Linux
   `kubernetes.azure.com/nodepool-type=FlexNodes` nodes. Serialize kubelet
   reconfiguration across nodes, cover already joined and future nodes, and
   continuously repair owned-file drift. No host files are removed merely
   because the helper pod is replaced.
5. Report installation progress through the existing Gantry component status.
   A node helper is ready only after the configured provider flags are active.
   An optional private-image HEAD preflight verifies repository pull access;
   otherwise status explicitly says that registry permission is unverified.
   Authentication and authorization failures must produce actionable,
   credential-free diagnostics.
6. Document managed-identity setup, explicit ACR role prerequisites, external
   authentication opt-out, existing-node behavior, ownership, removal, and the
   distinction between installation and pull validation.

## Safety and compatibility

- Default scope is public Azure ACR on AKS Flex nodes only. Sovereign clouds,
  workload identity, automatic Azure role assignments, and replacing native
  AKS credential providers are not part of this change.
- Existing `imagePullSecrets` and externally managed providers remain supported
  through explicit external mode. Never take over an unmarked provider file.
- Preserve the original checkout's unrelated changes by implementing in an
  isolated worktree and committing only this feature's files.
- Do not deploy the development operator or restart the user's live kubelets
  while preparing the PR. Live rollout validation requires a separately
  authorized development deployment.

## Validation

- Provider protocol, registry validation, managed-identity errors, OAuth
  exchange errors, bounded responses, redirect refusal, and token handling.
- Node installation, idempotency, permissions, atomic updates, ownership
  conflicts, preserved arguments, failed restart recovery, and file drift.
- Operator plans for late ACR addition, existing/future Flex nodes, non-ACR and
  shared-auth upstreams, native-node exclusion, external mode, configuration
  changes, readiness, and optional probe reporting.
- Narrow Lease access and image/version wiring tests.
- Run the project formatter, focused and broader relevant Go tests, lint, and
  Linux cross-builds. Record any environment-related failures honestly.
- Open a PR containing this plan, implementation, tests, and public docs, with
  explicit notes that Azure RBAC is not changed and live private pulls have not
  been asserted without a real probe.
