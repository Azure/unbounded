// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package controller provides a configurable MachineOperation controller pattern.
//
// A controller declares the operation kinds it handles, supplies a target
// resolver that decides whether the controller owns each operation, and receives
// a Store for lifecycle status updates.
package controller
