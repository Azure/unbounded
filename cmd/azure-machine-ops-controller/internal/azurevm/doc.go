// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package azurevm is the machine-ops provider for Azure virtual machines. It
// implements the callbacks of pkg/machineops and nothing else: the reconcilers
// that call them live in pkg/machineops/controller.
//
// Provider.Registration in provider.go declares everything the provider
// supports. The callbacks are:
//
//   - HostHardReboot, HostPowerOff and HostPowerOn:
//     Provider.BeginPowerOperation and Provider.PollPowerOperation in
//     host_power.go. Begin sends one request and Poll watches the VM.
//   - HostReplace: Provider.BeginReplace and Provider.PollReplace in
//     host_replace.go. It is the example of an operation with several steps:
//     Begin only records what Poll needs, and each Poll takes one step.
//   - Class resolution: Provider.ResolveClass in capacity_class.go, which
//     turns a MachineClass and its provider config into offerings.
//   - Allocation planning: Provider.PlanAllocation in capacity_allocate.go.
//   - HostAllocate: Provider.BeginAllocate and Provider.PollAllocate in
//     capacity_allocate.go.
//   - HostRelease: Provider.BeginRelease and Provider.PollRelease in
//     capacity_release.go.
//
// Every callback follows the same rule: it sends at most one request, does
// not wait for the provider to finish it, and decides what to do next from
// what the provider reports. No operation state is kept in memory between
// calls, so a controller that restarts continues from the provider's state
// and the handle the engine stored. The Provider keeps only two caches, of
// Azure credentials and of VM SKU listings (see capacity_client.go), and both
// are filled again after a restart.
//
// provider.go holds the registration, the narrow Azure client the host
// operations use and hostTarget, which checks that a handle still names the
// Machine's VM. host_fake_test.go is that client's in-memory fake.
//
// The host operations also use helpers from the capacity files, despite the
// file names:
//
//   - capacity.go: resource naming, tags, the poll interval and the
//     classification of Azure errors into permanent and retryable ones.
//   - capacity_class.go: next to class resolution, the InvalidRequest error
//     and the provider's clock.
//   - capacity_resources.go: the NICs and OS disk of an allocation's VM, how
//     they are observed and proved to belong to it, and the readers of a
//     VM's tags and provisioning state.
//   - capacity_client.go: the narrow Azure client the capacity callbacks
//     use, which the host operations' client wraps, and the two caches.
//   - capacity_config.go: validation of the provider config.
//
// capacity_fake_test.go is the capacity client's in-memory fake.
package azurevm
