// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

// watchOperations watches MachineOperation CRs targeting machineName using a
// label selector and enqueues ActionOperation actions into the shared queue.
// It retries on watch failure with the same interval as the Machine CR watcher.
func watchOperations(ctx context.Context, log *slog.Logger, wc client.WithWatch, machineName string, queue workqueue.TypedRateLimitingInterface[Action]) {
	log = log.With("watcher", "machineoperations")

	for {
		if err := watchMachineOperationCRs(ctx, log, wc, machineName, queue); err != nil {
			if ctx.Err() != nil {
				return
			}

			log.Error("machine operation watch failed, retrying", "error", err, "retry_in", watchRetryInterval)

			select {
			case <-ctx.Done():
				return
			case <-time.After(watchRetryInterval):
			}
		}
	}
}

// watchMachineOperationCRs establishes a single watch on MachineOperation CRs
// scoped by a label selector on the machine name. It filters for operations
// that are not yet in a terminal phase and enqueues an ActionOperation for
// each. Returns when the watch closes or the context is cancelled.
func watchMachineOperationCRs(
	ctx context.Context,
	log *slog.Logger,
	wc client.WithWatch,
	machineName string,
	queue workqueue.TypedRateLimitingInterface[Action],
) error {
	opList := &v1alpha3.MachineOperationList{}

	// Use a label selector to scope the informer to only operations
	// targeting this machine, as recommended by the design proposal.
	labelSelector := labels.SelectorFromSet(labels.Set{
		v1alpha3.MachineOperationMachineLabelKey: machineName,
	})

	watcher, err := wc.Watch(ctx, opList, &client.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return fmt.Errorf("start watch for MachineOperation CRs (machine=%s): %w", machineName, err)
	}
	defer watcher.Stop()

	log.Info("watching MachineOperation CRs", "machineRef", machineName, "labelSelector", labelSelector.String())

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

			op, ok := event.Object.(*v1alpha3.MachineOperation)
			if !ok {
				log.Warn("unexpected object type in watch event")
				continue
			}

			// Skip operations that have already reached a terminal phase.
			if op.Status.IsTerminal() {
				continue
			}

			log.Debug("enqueuing machine operation",
				"operation", op.Name,
				"operationName", op.Spec.OperationName,
				"phase", op.Status.Phase,
				"event", event.Type,
			)

			queue.Add(Action{Type: ActionOperation, Source: op.Name})
		}
	}
}
