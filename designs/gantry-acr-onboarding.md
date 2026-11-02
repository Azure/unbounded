# Transparent, operator-led ACR onboarding

## Contract

Applications keep their existing Pod specifications and ServiceAccounts. Private
ACR image pulls work on Linux AKS Flex nodes regardless of whether the machine
is hosted on Azure or in another data center. VM managed identities continue to
serve node bootstrap where applicable; they are not the image-pull identity.

The existing Gantry contract remains requester-delegated registry authorization:
kubelet/containerd obtains credentials and Gantry forwards that request's
credentials to origin. See `docs/content/guides/gantry.md:253` and the delegated
origin tests in `internal/gantry/origin/origin_test.go:585`.

## Architecture

```text
unchanged application Pod
    -> kubelet exec credential-provider adapter
    -> protected node-local Unix socket
    -> operator-managed broker
    -> infrastructure ServiceAccount's projected OIDC token
    -> Microsoft Entra federation -> ACR credential
    -> kubelet/containerd -> Gantry -> ACR or peers
```

All broker replicas use `ServiceAccount/registry-pull` in the operator namespace.
One Azure user-assigned pull identity trusts that subject and the cluster's OIDC
issuer. It has only the required pull role on the configured registry. It does
not need attachment to a VM. The broker uses explicit workload-identity
credentials; it never falls back to IMDS, an application ServiceAccount, CLI
credentials, or an ambient node identity.

The broker and exec adapter are public project code built from the existing
Azure SDK and kubelet v1 protocol. No customer implementation is copied. The
stock upstream ACR provider's Pod-ServiceAccount token mode is not used because
it would require application-ServiceAccount enrollment. No per-application
token attributes, annotations, imagePullSecrets, or workload identity webhook
injection are required by this node-wide design.

## Ownership

### Cluster provisioning script

`hack/scripts/aks-flex-cluster.sh` runs with the cluster creator's Azure
permissions. With `--acr NAME`, it:

1. Resolves an existing shared ACR without adopting or changing the registry.
2. Creates one owned pull identity per cluster, or resolves an explicitly
   supplied `--acr-pull-identity` resource ID.
3. Enables/reads the cluster's OIDC issuer and creates an exact federation
   binding for `system:serviceaccount:unbounded-system:registry-pull` with
   audience `api://AzureADTokenExchange`.
4. Ensures the registry-appropriate scoped pull role. There is no VM ACR role,
   push permission, subscription-wide role, or admin-role fallback.
5. Merges the upstream and explicit identity identifiers into `gantry-config`
   without replacing unrelated administrator settings.
6. Waits for broker reconciliation. An optional private-image probe checks
   access using the broker identity, not application or VM credentials.

ACR onboarding has separately recorded intent so adding it to an already-created
cluster does not change infrastructure ownership hashes. Reruns preserve
existing identities, trust, and unrelated ConfigMap entries. Conflicting trust,
foreign owned-resource names, unsupported authorization modes, and failed
queries stop the script rather than silently taking over resources. The script
is still not an infrastructure teardown tool; shared registry and externally
provided identity lifecycles are never deleted automatically.

### Unbounded operator

The operator consumes explicit `node-auth.yaml` alongside runtime `config.yaml`:

```yaml
mode: workloadIdentity
clientID: <Azure pull identity client UUID>
tenantID: <Azure tenant UUID>
# Optional existing private image used for an authenticated manifest HEAD.
probeImage: example.azurecr.io/private/application:v1
```

`mode: external` (the default when no policy is supplied) preserves existing
credential providers and imagePullSecrets. No identity is inferred from an ACR
hostname. Registries are derived from Gantry's upstream list; the identity
identifiers are configuration, not secrets.

The operator installs the broker DaemonSet only on Linux Flex nodes, projects a
short-lived token for its infrastructure ServiceAccount, and installs the exec
adapter and kubelet configuration. It preserves agent-generated kubelet files
and containerd mirror routes, coordinates restarts with a narrow Lease, and
repairs owned-file drift. The adapter has no Azure token file or credentials of
its own; only the broker receives the projected assertion. The Unix socket is
root-only. Tokens are never stored in ConfigMaps, Kubernetes Secrets, logs, or
host provider configuration.

The operator does not create Azure identities, federation trust, or role
assignments. It needs Kubernetes permissions, not Azure IAM-administrator
permissions. A missing federation or pull permission is a readiness error, not
an excuse to select a different identity.

## Security and lifecycle

- This is shared node-level registry access, not per-application authorization.
  All eligible workloads on target nodes can use the configured pull identity.
  Gantry remains a single cache trust domain.
- Helper images must be available before private ACR authentication is installed;
  the normal public, versioned release image avoids a bootstrap dependency.
- Existing external providers and unmarked files are not overwritten.
- Helper replacement does not remove authentication. Explicit external mode
  removes only owned configuration under the same restart coordination.
- An optional private-image probe verifies registry authorization, not complete
  Gantry/P2P distribution. Status explicitly reports unverified repository
  access when no probe image is provided.
- One identity per test cluster allows a shared ACR without coupling every
  cluster's identity lifecycle. Reusing an explicit identity remains supported,
  with a distinct federation binding for each cluster issuer.

## Validation

Retain and extend the existing protocol, token-handling, ownership, file-drift,
restart recovery, controller plan, and Lease tests. Add Unix-socket broker/exec
round-trip tests and assert that application ServiceAccounts and Pod specs are
not involved. Offline deployment tests cover owned and supplied identities,
OIDC setup, shared ACR non-adoption, scoped roles, exact federation, idempotency,
late onboarding, policy merge, permission failures, and offline plan mode.

Run project formatting, focused and broader relevant Go tests, race tests,
ShellCheck, offline deployment tests, and Linux cross-builds. Include the
previously uncommitted deployment tooling and its CI/runbook changes in this
same PR. Do not modify the live cluster while preparing this revision.
