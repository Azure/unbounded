# Gantry ACR onboarding

Status: agreed design, implementation in progress. Live private-image pulls and
end-to-end acceleration have not been validated.

## Contract

Fresh cluster deployment installs Gantry without a central ACR, pull identity,
or authentication policy. The user subsequently attaches an existing ACR in
the same subscription, possibly in another resource group:

Commands and user-facing requirements are documented in the
[ACR attachment guide](../docs/content/guides/gantry.md#acr-attachment-and-explicit-authentication-on-flex-nodes).

Authentication defaults to `external`; no authentication flag is needed to
preserve the customer's current setup. Managed authentication requires explicit
`--auth-mode aks-identity-binding`. Neither Gantry installation, an upgrade, nor
an absent authentication policy implicitly enrolls nodes.

## External authentication

Attachment only merges the selected ACR into Gantry's upstream configuration.
It preserves unrelated configuration and existing authentication policy.
Customers retain their own kubelet providers, imagePullSecrets, managed
identities, shared credentials, or other authentication models. The command
does not create Azure authentication resources or modify kubelets.
Explicit `--auth-mode external` is also accepted. Default attachment does not
disable or replace a managed authentication policy already selected by the user.

## Explicitly managed authentication

The same stock `acr-credential-provider` identity-bindings mode is used on both
Azure and non-Azure Linux Flex nodes. This supersedes the earlier proposal to
assign identities to Azure VMs and VMSS models.

1. The attachment command verifies the selected kubeconfig belongs to the AKS
   cluster and checks current Flex kubelet versions.
2. It creates or reuses one cluster-owned user-assigned managed identity,
   enables AKS workload identity and OIDC prerequisites when necessary, and
   creates the AKS-managed identity binding.
3. It discovers the actual proxy address, SNI name, token audience, and public
   CA through admission of an infrastructure Pod using server-side dry-run.
   No application Pod or ServiceAccount is changed.
4. It grants the identity the registry-appropriate pull-only role. Existing
   conditional grants are not silently broadened.
5. It authorizes `system:serviceaccounts` collectively to use that identity and
   persists validated, nonsecret installation policy.
6. The operator installs a node-configuration DaemonSet targeting Linux Flex
   nodes. It copies the pinned upstream binary and configures its built-in
   identity-bindings mode in the actual kubelet runtime.
7. Kubelet supplies Pod-bound ServiceAccount tokens directly to the stock
   executable. The upstream implementation performs token exchange with the
   AKS-managed endpoint.

The command updates Gantry's upstream configuration and authentication policy
together. The operator validates managed policy against the configured upstream;
it never adds or repairs registry entries. Removing the selected upstream while
retaining managed policy produces a configuration error, not an automatic merge.

There is no custom exec adapter, broker, token-exchange implementation,
VM/VMSS identity assignment, ARM node discovery, or ongoing Azure write
permission for the operator or node installer.

Application image references and ServiceAccounts remain unchanged. No
application annotations or per-application enrollment are required. Collective
authorization also allows ServiceAccounts to acquire Azure tokens as the shared
identity, so that identity must remain pull-only. This is intentionally shared
cluster access, not per-tenant isolation.

## Node lifecycle and ownership

- Existing nodes, VMSS scale-out instances, newly onboarded Azure VMs/VMSS,
  and newly onboarded non-Azure Flex nodes receive the same persisted policy.
- No command rerun or VM model modification is required for future nodes.
- Native AKS nodes remain unaffected.
- The installer preserves the agent-generated kubelet unit, other systemd
  drop-ins, containerd routing, and the node's bootstrap identity.
- Existing foreign credential providers and conflicting CA files cause an
  explicit failure, not replacement.
- Same-named customer Kubernetes resources do not trigger cleanup. Cleanup needs
  operator-owned helper state; all helper resources must have matching ownership
  labels before updates. Existing updates and deletes carry observed API
  preconditions, and absent objects are created without forcing adoption.
- Restart coordination serializes kubelet configuration changes. Owned-file
  drift and Flex-runtime recreation are reconciled automatically.
- Explicit external policy removes only owned node configuration and revokes
  the owned collective identity-use binding. The shared ACR is never adopted,
  retagged, recreated, or deleted.
- Installer images must be accessible before private ACR authentication works.

## Requirements and limitations

The implementation pins upstream cloud-provider-azure v1.36.0. Kubelet must be
1.34 or later and its ServiceAccount-token credential-provider feature must not
be explicitly disabled. Static Pods without ServiceAccounts cannot use this
token-based path.

AKS identity bindings must be available, their admission webhook must expose
the required proxy metadata, and the endpoint must be reachable from every
Flex runtime. API-server VNet integration is unsupported by AKS identity
bindings. Unsupported capability fails explicitly; there is no custom broker,
IMDS fallback, or stored-secret fallback.

One central ACR is selected for managed authentication. Additional upstreams
may use externally managed authentication. A different existing authentication
policy is not silently replaced or migrated.

## Validation

Offline tests cover deployment and upgrades without authentication changes,
default external behavior, explicit managed-auth opt-in, ownership collisions,
configuration preservation, stock-provider request configuration, Linux Flex
selection, ownership conflicts, retry/restart recovery, and failure paths.
Installation readiness does not establish ACR authorization, a successful
private-image pull, or Gantry acceleration. Those require separate live checks
on Azure and non-Azure nodes, including future nodes and runtime recreation.

## References

- [Stock provider identity bindings, v1.36.0](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/v1.36.0/pkg/credentialprovider/identity_bindings_credentials.go)
- [Stock provider flags, v1.36.0](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/v1.36.0/cmd/acr-credential-provider/pkg/config/identity_bindings_config.go)
- [AKS identity bindings](https://learn.microsoft.com/en-us/azure/aks/identity-bindings)
- [AKS identity bindings concepts and limitations](https://learn.microsoft.com/en-us/azure/aks/identity-bindings-concepts)
- [Kubelet ServiceAccount tokens for image credential providers](https://kubernetes.io/docs/tasks/administer-cluster/kubelet-credential-provider/)
