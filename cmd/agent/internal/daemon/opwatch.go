// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

// watchOperations watches Operation CRs targeting machineName and enqueues
// ActionOperation actions into the shared queue. It retries on watch failure
// with the same interval as the Machine CR watcher.
func watchOperations(ctx context.Context, log *slog.Logger, wc client.WithWatch, machineName string, queue workqueue.TypedRateLimitingInterface[Action]) {
	log = log.With("watcher", "operations")

	for {
		if err := watchOperationCRs(ctx, log, wc, machineName, queue); err != nil {
			if ctx.Err() != nil {
				return
			}

			log.Error("operation watch failed, retrying", "error", err, "retry_in", watchRetryInterval)

			select {
			case <-ctx.Done():
				return
			case <-time.After(watchRetryInterval):
			}
		}
	}
}

// watchOperationCRs establishes a single watch on Operation CRs. It filters
// for Operations targeting machineName that are not yet in a terminal phase,
// and enqueues an ActionOperation for each. Returns when the watch closes or
// the context is cancelled.
func watchOperationCRs(
	ctx context.Context,
	log *slog.Logger,
	wc client.WithWatch,
	machineName string,
	queue workqueue.TypedRateLimitingInterface[Action],
) error {
	opList := &v1alpha3.OperationList{}

	watcher, err := wc.Watch(ctx, opList)
	if err != nil {
		return fmt.Errorf("start watch for Operation CRs (machine=%s): %w", machineName, err)
	}
	defer watcher.Stop()

	log.Info("watching Operation CRs", "machineRef", machineName)

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}

			if event.Type == watch.Error {
				return fmt.Errorf("watch error event: %v", event.Object)
			}

			if event.Type != watch.Modified && event.Type != watch.Added {
				continue
			}

			op, ok := event.Object.(*v1alpha3.Operation)
			if !ok {
				log.Warn("unexpected object type in watch event")
				continue
			}

			// Only process operations targeting this machine.
			if op.Spec.MachineRef != machineName {
				continue
			}

			// Skip operations that have already reached a terminal phase.
			if op.Status.IsTerminal() {
				continue
			}

			log.Debug("enqueuing operation",
				"operation", op.Name,
				"type", op.Spec.Type,
				"phase", op.Status.Phase,
				"event", event.Type,
			)

			queue.Add(Action{Type: ActionOperation, Source: op.Name})
		}
	}
}
