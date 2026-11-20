// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package azurevm is the machine-ops provider for Azure virtual machines. It
// implements the callbacks of pkg/machineops and nothing else: the reconcilers
// that call them live in pkg/machineops/controller.
//
// Provider.Registration in provider.go declares everything the provider
// supports. The callbacks are:
//
//   - Host operations (HostHardReboot, HostPowerOff, HostPowerOn,
//     HostReplace): Provider.Execute in provider.go.
//   - Class resolution: Provider.ResolveClass in capacity_class.go, which
//     turns a MachineClass and its provider config into offerings.
//   - Allocation planning: Provider.PlanAllocation in capacity_allocate.go.
//   - HostAllocate: Provider.BeginAllocate and Provider.PollAllocate in
//     capacity_allocate.go.
//   - HostRelease: Provider.BeginRelease and Provider.PollRelease in
//     capacity_release.go.
//
// capacity.go holds what the capacity callbacks share: resource naming, tags
// and the classification of Azure errors into permanent and retryable ones.
// capacity_config.go validates the provider config, capacity_client.go is the
// narrow Azure client the callbacks use, and capacity_fake_test.go is its
// in-memory fake.
package azurevm
