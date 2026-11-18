// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package centerpivot holds the names centerpivot's parts share: the service,
// the node-info DaemonSet, the operator component, and Unbounded's host agent,
// which configures trident-acl-agent and binds the host's machine ID into the
// node's nspawn machine. It has no dependencies, so the host agent can use it.
// See designs/centerpivot.md.
package centerpivot

// Prefix is the prefix of every label, taint, and annotation centerpivot uses,
// including trident-acl-agent's annotation contract.
const Prefix = "centerpivot.unbounded-cloud.io"

// Node labels, set through a MachineConfiguration's nodeLabels so they survive
// a rebuild, except paused, which an administrator sets.
const (
	// ManagedLabel marks a node centerpivot updates, when "true". On such a
	// node, Unbounded's host agent configures trident-acl-agent.
	ManagedLabel = Prefix + "/managed"
	// AppLabel names the Site app the node runs, when the Site has several.
	AppLabel = Prefix + "/app"
	// TrackLabel is the node's Nebraska track, instead of its app's default.
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

// VersionKey is the os-release key that holds an ACL image's version, such as
// VERSION_ID=3.0.20261106. trident-acl-agent reports it to Nebraska, and
// node-info publishes it, so the two must read the same key.
const VersionKey = "VERSION_ID"

// Where a pod on an ACL node finds the host's files.
const (
	// HostMachineIDPath is where Unbounded's host agent binds the host's
	// /etc/machine-id, read-only, in the node's nspawn machine. The machine
	// has its own /etc/machine-id.
	HostMachineIDPath = "/run/unbounded/host/machine-id"
	// HostOSReleasePath is where systemd-nspawn binds the host's os-release
	// in every machine.
	HostOSReleasePath = "/run/host/os-release"
)
