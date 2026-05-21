// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package controller exposes the reusable MachineOperation provider controller.
package controller

import internalmachineops "github.com/Azure/unbounded/internal/machineops"

// MachineOperationReconciler reconciles MachineOperation objects for external
// provider-controlled Machines.
type MachineOperationReconciler = internalmachineops.MachineOperationReconciler

// ClusterInfo contains bootstrap data used by host replacement operations.
type ClusterInfo = internalmachineops.ClusterInfo
