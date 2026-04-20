// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// watchOperations watches ConfigMaps labeled unbounded.io/agent-op=<machineName>
// in the unbounded-system namespace and enqueues ActionSoftRestart actions into
// the shared queue. It retries on watch failure with the same interval as the
// Machine CR watcher.
//
// This function and opshim.go are the ConfigMap-specific layer. When migrating
// to a dedicated Operation CRD, replace these two files.
func watchOperations(ctx context.Context, log *slog.Logger, wc client.WithWatch, machineName string, queue workqueue.TypedRateLimitingInterface[Action]) {
	log = log.With("watcher", "operations")

	for {
		if err := watchConfigMaps(ctx, log, wc, machineName, queue); err != nil {
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

// watchConfigMaps establishes a single watch on ConfigMaps with the
// operation label matching machineName. It enqueues an ActionSoftRestart
// for each ConfigMap that has pending operations. Returns when the watch
// closes or the context is cancelled.
func watchConfigMaps(
	ctx context.Context,
	log *slog.Logger,
	wc client.WithWatch,
	machineName string,
	queue workqueue.TypedRateLimitingInterface[Action],
) error {
	cmList := &corev1.ConfigMapList{}

	watcher, err := wc.Watch(ctx, cmList,
		client.InNamespace(operationNamespace),
		client.MatchingLabels{operationLabelKey: machineName},
	)
	if err != nil {
		return fmt.Errorf("start watch for operation ConfigMaps (machine=%s): %w", machineName, err)
	}
	defer watcher.Stop()

	log.Info("watching operation ConfigMaps",
		"namespace", operationNamespace,
		"label", operationLabelKey+"="+machineName,
	)

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

			cm, ok := event.Object.(*corev1.ConfigMap)
			if !ok {
				log.Warn("unexpected object type in watch event")
				continue
			}

			// Only enqueue if there are pending operations, to avoid
			// unnecessary queue churn for already-completed ConfigMaps.
			ops, err := parseOperations(cm)
			if err != nil {
				log.Warn("failed to parse operations from ConfigMap",
					"configmap", cm.Namespace+"/"+cm.Name,
					"error", err,
				)

				continue
			}

			if !hasPendingOperations(ops) {
				continue
			}

			source := cm.Namespace + "/" + cm.Name
			log.Debug("enqueuing operation",
				"configmap", source,
				"event", event.Type,
			)

			queue.Add(Action{Type: ActionSoftRestart, Source: source})
		}
	}
}
