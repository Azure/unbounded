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

	// ActionSoftRestart stops and restarts the current nspawn machine in
	// place (no rootfs change, no config change). Produced by the
	// operation ConfigMap watcher.
	ActionSoftRestart ActionType = "SoftRestart"
)

// Action is the unit of work processed by the daemon's single worker
// goroutine. Both the Machine CR watcher and the operation ConfigMap
// watcher produce Actions; sequential processing by one worker guarantees
// that machine-mutating operations never overlap.
type Action struct {
	// Type identifies the action to perform.
	Type ActionType

	// Source identifies where this action originated. For Machine CR
	// actions this is the machine name; for operation ConfigMap actions
	// this is "namespace/configmap-name".
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
