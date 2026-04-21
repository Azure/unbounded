// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// executor abstracts machine-mutating operations so that tests can mock the
// system calls (nspawn, systemctl, machinectl) without actually affecting a
// host.
type executor interface {
	softRestart(ctx context.Context, log *slog.Logger, machineName string) error
}

// reconciler holds shared state for all action handlers. A single worker
// goroutine calls reconcile sequentially, so no locking is needed.
type reconciler struct {
	client      client.WithWatch
	machineName string // Machine CR name (e.g. "agent")
	exec        executor
	// findActive discovers the currently active nspawn machine. Defaults
	// to findActiveMachine; overridden in tests.
	findActive func(log *slog.Logger) (*ActiveMachine, error)
}

// reconcile dispatches an action to the appropriate handler based on its type.
func (r *reconciler) reconcile(ctx context.Context, log *slog.Logger, action Action) error {
	log = log.With("action", action.Type, "source", action.Source)

	switch action.Type {
	case ActionUpdateMachine:
		return r.reconcileUpdateMachine(ctx, log, action.Source)
	case ActionOperation:
		return r.reconcileOperation(ctx, log, action.Source)
	default:
		log.Warn("unknown action type, ignoring")
		return nil
	}
}
