// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package controller is centerpivot's controller: a scheduler that hands out
// update slots, and a node reconciler that takes each node with a slot
// through its update, one step per pass. See designs/centerpivot.md.
package controller

import (
	"github.com/Azure/unbounded/internal/centerpivot/contract"
)

// Prefix is the prefix of every label, taint, and annotation centerpivot uses,
// including the agent's annotation contract.
const Prefix = "centerpivot.unbounded-cloud.io"

// Node labels, set through a MachineConfiguration's nodeLabels so they survive
// a rebuild, except paused, which an administrator sets.
const (
	// ManagedLabel marks a node centerpivot updates, when "true".
	ManagedLabel = Prefix + "/managed"
	// TrackLabel is the node's Nebraska track, instead of the Site's default.
	TrackLabel = Prefix + "/track"
	// CanaryLabel puts a node first among those due, when "true".
	CanaryLabel = Prefix + "/canary"
	// PausedLabel starts no new update on a node, when "true".
	PausedLabel = Prefix + "/paused"
)

const (
	// UpdatingTaint keeps new pods off a node while it drains and restarts.
	// It is used instead of a cordon so a cordon someone else set is never
	// removed.
	UpdatingTaint = Prefix + "/updating"
	// AgentInfoAnnotation is the Node annotation with the node's Nebraska
	// identity.
	AgentInfoAnnotation = Prefix + "/agent-info"
	// StateAnnotation is the Machine annotation with the node's state. It is
	// on the Machine so it survives the Node being deleted to rebuild it.
	StateAnnotation = Prefix + "/state"
)

// Keys are the agent's annotation contract under Prefix.
var Keys = contract.KeysFor(Prefix)
