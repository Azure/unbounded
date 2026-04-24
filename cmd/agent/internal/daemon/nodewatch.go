// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// watchNode watches the Kubernetes Node object matching the host's hostname
// and enqueues ActionNodeDeleted when the Node is deleted. This is the
// primary signal for the OnDelete update strategy: an operator cordons,
// drains, and deletes the Node to trigger a repave with the latest
// MachineConfigurationVersion.
//
// The function retries internally on watch failures, similar to
// watchOperations. It blocks until the context is cancelled.
func watchNode(ctx context.Context, log *slog.Logger, wc client.WithWatch, queue workqueue.TypedRateLimitingInterface[Action]) {
	watchNodeWithHostname(ctx, log, wc, queue, os.Hostname)
}

// watchNodeWithHostname is the inner implementation that accepts a hostname
// function so tests can inject a fake hostname.
func watchNodeWithHostname(
	ctx context.Context,
	log *slog.Logger,
	wc client.WithWatch,
	queue workqueue.TypedRateLimitingInterface[Action],
	hostnameFn func() (string, error),
) {
	log = log.With("watcher", "node")

	hostname, err := hostnameFn()
	if err != nil {
		log.Error("failed to get hostname, Node watch disabled", "error", err)
		return
	}

	log.Info("Node watch starting", "node", hostname)

	for {
		if err := watchNodeCR(ctx, log, wc, hostname, queue); err != nil {
			if ctx.Err() != nil {
				return
			}

			log.Error("Node watch failed, retrying", "error", err, "retry_in", watchRetryInterval)

			select {
			case <-ctx.Done():
				return
			case <-time.After(watchRetryInterval):
			}
		}
	}
}

// watchNodeCR establishes a single watch on the named Node object and
// enqueues ActionNodeDeleted when a DELETED event is received. Returns
// when the watch closes or the context is cancelled.
func watchNodeCR(
	ctx context.Context,
	log *slog.Logger,
	wc client.WithWatch,
	nodeName string,
	queue workqueue.TypedRateLimitingInterface[Action],
) error {
	nodeList := &corev1.NodeList{}

	watcher, err := wc.Watch(ctx, nodeList, client.MatchingFields{"metadata.name": nodeName})
	if err != nil {
		return fmt.Errorf("start watch for Node %q: %w", nodeName, err)
	}
	defer watcher.Stop()

	log.Info("watching Node", "name", nodeName)

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

			if event.Type == watch.Deleted {
				log.Info("Node deleted, enqueuing repave", "node", nodeName)
				queue.Add(Action{Type: ActionNodeDeleted, Source: nodeName})
				// Continue watching - the Node may be recreated after
				// repave and we want to detect subsequent deletions.
				continue
			}

			// Log other events at debug level for observability.
			if event.Type == watch.Added || event.Type == watch.Modified {
				log.Debug("Node watch event", "type", event.Type, "node", nodeName)
			}
		}
	}
}
