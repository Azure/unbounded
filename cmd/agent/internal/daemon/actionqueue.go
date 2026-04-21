// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import "k8s.io/client-go/util/workqueue"

// ActionType identifies the kind of operation the daemon must perform on the
// nspawn machine.
type ActionType string

const (
	// ActionUpdateMachine triggers a full blue/green repave of the nspawn
	// machine (new rootfs, stop old, start new). Produced by the Machine
	// CR watcher when operation counter drift is detected.
	ActionUpdateMachine ActionType = "UpdateMachine"

	// ActionNodeDeleted triggers a repave because the Kubernetes Node
	// object corresponding to this machine was deleted. This is the
	// primary signal for the OnDelete update strategy: an operator
	// cordons, drains, and deletes the Node to trigger a repave with
	// the latest MachineConfigurationVersion.
	ActionNodeDeleted ActionType = "NodeDeleted"

	// ActionOperation processes an Operation CR (e.g. SoftReboot,
	// HardReboot). Produced by the Operation CR watcher when a
	// non-terminal Operation targeting this machine is observed.
	ActionOperation ActionType = "Operation"
)

// Action is the unit of work processed by the daemon's single worker
// goroutine. Both the Machine CR watcher and the Operation CR watcher
// produce Actions; sequential processing by one worker guarantees that
// machine-mutating operations never overlap.
type Action struct {
	// Type identifies the action to perform.
	Type ActionType

	// Source identifies where this action originated. For Machine CR
	// actions this is the machine name; for Operation actions this is
	// the Operation CR name.
	Source string
}

// newActionQueue creates a rate-limiting workqueue for Action items. The
// default controller rate limiter provides exponential backoff on failure
// (5ms initial, doubling, capped at ~16min) and per-item tracking so that
// a failing action of one type does not slow down retries of another.
func newActionQueue() workqueue.TypedRateLimitingInterface[Action] {
	return workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[Action](),
		workqueue.TypedRateLimitingQueueConfig[Action]{Name: "agent-actions"},
	)
}
