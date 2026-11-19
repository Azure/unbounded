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
  `pkg/machineops`.

## Writing a controller for another provider

1. Copy this directory.
2. Replace `internal/azurevm` with a package that returns a
   `*machineops.Provider` for your provider name. Register an immediate
   operation when one provider call completes it, and a long-running operation
   (begin and poll) when the engine has to resume it after a restart.
   `internal/azurevm/provider.go` shows the first, and
   `internal/azurevm/capacity.go` shows the second together with the capacity
   callbacks.
3. Replace `azureProviders` in `main.go` with your registration.
4. Replace the two imports that only exist inside this repository:
   `internal/unbounded` supplies the default namespace of three flags, and
   `internal/version` supplies the version string.
5. Copy the manifests in `deploy/machine-ops` and set `ControllerName` and
   `ControllerImage`.

One controller can serve several providers. `azureProviders` returns a list
for that reason: a second Azure compute mode, such as virtual machine scale
sets, is a second provider with its own name in the same binary, and
`--provider` selects one of them for a deployment.
