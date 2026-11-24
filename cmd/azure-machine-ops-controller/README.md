# azure-machine-ops-controller

The machine-ops controller for Azure. It executes host operations
(`HostHardReboot`, `HostPowerOff`, `HostPowerOn`, `HostReplace`) on Azure
virtual machines and, with `--enable-capacity`, resolves `MachineClass`
objects and creates and deletes virtual machines for `MachineAllocation`
objects.

It is also the reference for a machine-ops controller for any other provider.
Everything that is not specific to Azure comes from the public SDK:

| Package | What it gives a provider |
|---|---|
| `pkg/machineops` | The provider registration: `NewProvider`, the operation callbacks (`WithImmediateOperation`, `WithLongRunningOperation`) and the capacity callbacks (`WithCapacity`). |
| `pkg/machineops/controller` | The reconcilers. `AddToManager` runs MachineOperations. `AddCapacityToManager` runs MachineClasses and, only with `CapacityOptions.EnableAllocationController`, MachineAllocations. `main.go` sets it whenever `--enable-capacity` is on. |

## Layout

- `main.go` parses the flags, builds a controller-runtime manager and hands
  the providers to the SDK. It contains no provider logic.
- `internal/azurevm` is the provider. It imports only the API types and
  `pkg/machineops`. Its package comment maps each SDK callback to the file
  that implements it.

## Scopes

`--provider` and `--site` each limit a deployment, and either can be used
alone. Every scope gets its own leader-election lease, so the lease does not
stop two deployments with overlapping scopes from acting on the same objects.
Do not run an unscoped deployment next to a scoped one, or two deployments
whose scopes share a provider and a Site.

One controller can serve several providers. `azureProviders` in `main.go`
returns a list for that reason: a second Azure compute mode, such as virtual
machine scale sets, is a second provider with its own name in the same binary,
and `--provider` selects one of them for a deployment. A Machine names its
provider in `spec.host.external.provider`. A Machine with no `spec.host`
owner can still name it in the deprecated `spec.provider`, with the host in
`spec.providerID`. `spec.host.azure` always means `AzureVM`.

## Writing a controller for another provider

1. Copy this directory.
2. Replace `internal/azurevm` with a package that returns a
   `*machineops.Provider` for your provider name. Almost every cloud call
   outlives one reconcile, so register operations with
   `WithLongRunningOperation`: Begin starts the work and returns a small
   handle, and Poll decides the outcome by observing the provider again. The
   engine stores the handle, so the operation resumes after a controller
   restart. Use `WithImmediateOperation` only for a call that completes before
   it returns; the engine does not run it again once it is in progress unless
   it is declared `ReplaySafe`.

   An operation with several steps, such as a replacement that deletes a host
   and creates it again, takes one step per Poll and waits for nothing.
   `host_replace.go` is the example: Begin changes nothing and returns
   everything a later Poll needs in the handle, so the engine has stored it
   before the host is touched. Do not put secrets in the handle. The engine
   builds the replacement bootstrap data again for every Poll of an operation
   registered with `RequiresReplaceUserData`.
3. Replace `azureProviders` in `main.go` with your registration, and change
   `controllerName`. It names the leader-election lease, so two controllers
   that keep the same name in one namespace would exclude each other.
4. Replace what only exists inside this repository:
   - `main.go` imports `internal/systemns` for the default namespace of
     three flags and `internal/version` for the version string.
   - `main_test.go` imports `internal/systemns` too.
   - `envtest_test.go` loads the CRDs from `../../deploy/machina/crd`, a path
     relative to this directory. Point it at your copy of the CRDs.
5. Take the manifests from `deploy/machine-ops`. They are Go templates that
   use sprig functions, and `hack/cmd/render-manifests` renders them
   (`make machine-ops-manifests`). Outside this repository, render them with
   that tool or turn them into plain manifests.
   - `00` creates the namespace. Install it once.
   - `01` to `03` and `08` belong to one controller. Copy them and set
     `ControllerName` and `ControllerImage`.
   - `04` to `07` are the capacity roles and admission policies, and `09` is
     the admission policy for MachineOperation status. They are cluster-wide
     objects with fixed names that every machine-ops controller shares, so
     install them once and do not copy them.
   - `01` and `03` contain Azure and AKS specifics to replace: the workload
     identity annotation and label, the node affinity and the API server
     annotation.
   - `02` names the `aks-cluster-metadata` ConfigMap. Keep that rule on every
     platform: the SDK reads the ConfigMap to detect an AKS control plane
     when it builds bootstrap data, and treats only "not found" as "not AKS".
   - `02` grants no access to provider-owned objects. A provider registered
     with `WithProviderMachineKind` must add `get` on its own cluster-scoped
     Machine kind, because the engine reads that object when an operation
     starts.
   - `EnableCapacity=true` also needs `APIServerEndpoint`. `03` passes
     `--api-server-endpoint` only when the value is set, and the controller
     refuses to start with `--enable-capacity` and no endpoint.

## The provider contract

The doc comments in `pkg/machineops` are the reference. These are the rules
they state, with the type to read for each.

- Begin is repeated. The engine calls it again with the same `OperationUID`
  until it has stored the handle: after an ordinary error, after a restart,
  and when the returned `OperationID` is empty. Every call must return a
  handle for the same logical operation (`BeginFunc`).
- The handle never changes. What Begin returns is the only state the engine
  keeps for Poll, and Poll cannot update it. It is stored in the
  MachineOperation status, and the API sets no maximum length. Keep it small
  and put no secrets in it (`ProviderOperation`).
- Poll is repeated after it finished. The engine applies a result before it
  records the terminal phase, and it polls again when either write fails.
  Poll must then give the same terminal answer (`PollFunc`).
- The provider owns deadlines. The engine never times an operation out. It
  retries ordinary Begin and Poll errors without limit. After 75 minutes, by
  default, it sets the `ProviderOperationStalled` condition and polls every
  5 minutes, whatever `RequeueAfter` the provider returns. Return a
  `PermanentError` or a `Failed` `PollResult` to end an operation (`Options`
  in `pkg/machineops/controller` has the three intervals).
- An immediate operation has no retries. Every Execute error ends it with the
  reason `ExecutionFailed`, and the reason of a `PermanentError` is ignored
  (`ExecuteFunc`).
- A host operation's failure reason is free-form, and the engine makes it a
  valid condition reason. A `HostAllocate` reason must be a
  `MachineAllocationFailureReason` value, or the MachineAllocation reports
  another one in its place (`PermanentError`).
- Host operations on one Machine run one at a time, oldest first.
  `request.ProviderID` is read from the Machine on every call and is not
  part of the handle, so Poll must check that the handle still names the same
  host. `hostTarget` in `provider.go` does.
- `HostAllocate` sends its create from Begin, because the user data and the
  provider config arrive only there. The engine records a `PermanentError`
  from a Begin that may have run before as unknown ownership, so
  `BeginAllocate` returns a handle that records Azure's refusal and Poll
  reports it (`WithCapacity`).

Credentials reach a provider as `request.Auth`. The engine takes the one
`MachineOperationCredential` whose `spec.siteName` is the Machine's site label
(`unbounded-cloud.io/site`) and whose `spec.provider` is the Machine's
provider. `Auth.Mode` is its `spec.auth.mode`, and `Auth.SecretData` is the
data of the Secret it references, if it references one. The provider defines
the keys: this one reads `tenantID`, `clientID` and `clientSecret` in
`ExternalPlugin` mode. When no credential matches, more than one does, or the
Secret is missing or outside the credential namespace, a host operation
without a stored handle fails with a reason that starts with `Auth`. One with
a handle stays in progress and is tried again every 5 minutes by default, so
it resumes once the credential is repaired.

## Identity and permissions

- The cloud identity is the real limit on what the controller can touch. Host
  operations act on whatever a Machine's provider ID names, and capacity acts
  on whatever a MachineClass's provider config names, so grant the identity
  only the resource groups the controller should manage.
- A provider config may name a worker identity to attach to new hosts.
  Whoever can write that ConfigMap can therefore attach any identity the
  controller is allowed to assign.
- The controller reads Secrets in two places: its credential namespace
  (`--credential-secret-namespace`), and bootstrap-token Secrets in
  `kube-system`, whose names are not known in advance. Keep credentials in a
  namespace that holds nothing else.
- Capacity needs the roles in `04` and `05`, the bindings in `08` and the
  admission policies in `06` and `07`. The policies are not optional. `06`
  stops an identity that may create Machines from binding one to an
  allocation, and `07` limits the controller's Secret writes in `kube-system`
  to its own bootstrap tokens.
- Every controller needs the admission policy in `09`, with or without
  capacity. The engine trusts `providerOperation` and `input` in a
  MachineOperation's status targets, and the agent on every node may update
  that status. `09` lets only an identity allowed `update` on
  `machineoperations/finalizers` change those two fields. The role in `02`
  grants that, so keep the grant in a copy and never give it to the agents.
