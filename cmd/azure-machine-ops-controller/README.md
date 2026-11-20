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
| `pkg/machineops/controller` | The reconcilers. `AddToManager` runs MachineOperations; `AddCapacityToManager` runs MachineClasses and MachineAllocations. |

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
and `--provider` selects one of them for a deployment. A Machine reaches a
provider other than `AzureVM` through `spec.host.external`.

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
4. Replace the two imports that only exist inside this repository:
   `internal/unbounded` supplies the default namespace of three flags, and
   `internal/version` supplies the version string.
5. Take the manifests from `deploy/machine-ops`:
   - `01` to `03` and `08` belong to one controller. Copy them and set
     `ControllerName` and `ControllerImage`.
   - `04` to `07` are the capacity roles and admission policies, and `09` is
     the admission policy for MachineOperation status. They are cluster-wide
     objects with fixed names that every machine-ops controller shares, so
     install them once and do not copy them.
   - `01` and `03` contain Azure and AKS specifics to replace: the workload
     identity annotation and label, the node affinity and the API server
     annotation.

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
