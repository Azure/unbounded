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
// what the provider reports. Nothing is kept in memory between calls, so a
// controller that restarts continues from the provider's state and the handle
// the engine stored.
//
// provider.go holds the registration and the narrow Azure client the host
// operations use, and host_fake_test.go is its in-memory fake. capacity.go
// holds what the capacity callbacks share: resource naming, tags
// and the classification of Azure errors into permanent and retryable ones.
// capacity_config.go validates the provider config, capacity_client.go is the
// narrow Azure client the callbacks use, and capacity_fake_test.go is its
// in-memory fake.
package azurevm
