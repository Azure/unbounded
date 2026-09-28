// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

type viewerDetailWatch struct {
	cancel context.CancelFunc
}

func (b *WSBroadcaster) rejectAutomaticDetails(client *WSClient, nodeName string) {
	b.sendToClient(client, WSMessage{
		Type: "node_detail_error", NodeName: nodeName,
		Message: "automatic detail subscriptions are retired; request details explicitly",
	})
}

func (b *WSBroadcaster) requestNodeDetails(client *WSClient, nodeName string, refresh bool) {
	client.cancelDetailWatch(nodeName)

	if b.health == nil || !b.health.isLeader.Load() || client.ctx == nil {
		b.sendDetailMetadata(client, detailRequestFailure(nodeName, "", statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable"))

		return
	}

	manager := b.health.getDetailRequests()
	if manager == nil {
		b.sendDetailMetadata(client, detailRequestFailure(nodeName, "", statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable"))

		return
	}

	result := manager.Request(nodeName, refresh)
	b.sendDetailMetadata(client, result)

	if result.State != statusv1alpha1.NodeDetailPending && result.State != statusv1alpha1.NodeDetailComplete {
		return
	}

	ctx, cancel := context.WithCancel(client.ctx)
	watch := &viewerDetailWatch{cancel: cancel}

	client.detailMu.Lock()
	if old := client.detailWatches[nodeName]; old != nil {
		old.cancel()
	}

	if client.detailWatches == nil {
		client.detailWatches = make(map[string]*viewerDetailWatch)
	}

	client.detailWatches[nodeName] = watch
	client.detailMu.Unlock()

	// Pass scalars, not result: it may contain the cache's full payload.
	go b.followNodeDetails(ctx, client, manager, nodeName, result.RequestID, result.State, watch)
}

func (b *WSBroadcaster) followNodeDetails(ctx context.Context, client *WSClient, manager *nodeDetailRequests, nodeName, requestID string, initial statusv1alpha1.NodeDetailState, watch *viewerDetailWatch) {
	defer watch.cancel()
	defer func() {
		client.detailMu.Lock()
		defer client.detailMu.Unlock()

		if client.detailWatches[nodeName] == watch {
			delete(client.detailWatches, nodeName)
		}
	}()

	lastError := ""

	for ctx.Err() == nil {
		result, changed := manager.WatchMetadata(nodeName, requestID)
		if result.State != initial || (result.State == statusv1alpha1.NodeDetailPending && result.Error != lastError) {
			b.sendWatchedDetailMetadata(client, watch, result)
			lastError = result.Error
		}

		if result.State == statusv1alpha1.NodeDetailComplete && result.Details != nil {
			// The displayed snapshot has a fixed expiry, even if later legacy
			// publications renew the controller's cache association.
			timer := time.NewTimer(time.Until(result.Details.ExpiresAt))
			select {
			case <-ctx.Done():
			case <-manager.ctx.Done():
				b.sendWatchedDetailMetadata(client, watch, detailRequestFailure(nodeName, requestID, statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable"))
			case <-timer.C:
				b.sendWatchedDetailMetadata(client, watch, detailRequestFailure(nodeName, requestID, statusv1alpha1.NodeDetailExpired, "displayed details expired; request fresh data explicitly"))
			}

			timer.Stop()

			return
		}

		if result.State != statusv1alpha1.NodeDetailPending {
			return
		}

		select {
		case <-ctx.Done():
		case <-changed:
		}
	}
}

func (c *WSClient) cancelDetailWatch(nodeName string) {
	c.detailMu.Lock()
	defer c.detailMu.Unlock()

	if watch := c.detailWatches[nodeName]; watch != nil {
		watch.cancel()
		delete(c.detailWatches, nodeName)
	}
}

func (b *WSBroadcaster) sendWatchedDetailMetadata(client *WSClient, watch *viewerDetailWatch, result statusv1alpha1.NodeDetailResult) {
	client.detailMu.Lock()
	defer client.detailMu.Unlock()

	if client.detailWatches[result.NodeName] == watch {
		b.sendDetailMetadata(client, result)
	}
}

func (b *WSBroadcaster) sendDetailMetadata(client *WSClient, result statusv1alpha1.NodeDetailResult) {
	if result.Details != nil {
		metadata := *result.Details
		metadata.Status = nil
		result.Details = &metadata
	}

	b.sendToClient(client, WSMessage{Type: "node_detail_response", NodeName: result.NodeName, Data: result})
}

// prepareWrite resolves details only when dequeued. Outbound queues contain
// metadata, not serialized payloads that could survive expiry. A blocked detail
// write is canceled at snapshot expiry or leadership loss.
func (c *WSClient) prepareWrite(data []byte) ([]byte, context.Context, context.CancelFunc, error) {
	noCleanup := func() {}
	if !bytes.HasPrefix(data, []byte(`{"type":"node_detail_response"`)) {
		return data, c.ctx, noCleanup, nil
	}

	var envelope struct {
		Data statusv1alpha1.NodeDetailResult `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, c.ctx, noCleanup, err
	}

	result := envelope.Data
	if result.State != statusv1alpha1.NodeDetailComplete {
		return data, c.ctx, noCleanup, nil
	}

	var manager *nodeDetailRequests
	if c.broadcaster != nil && c.broadcaster.health != nil {
		manager = c.broadcaster.health.getDetailRequests()
	}

	if manager == nil {
		result = detailRequestFailure(result.NodeName, result.RequestID, statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable")
	} else {
		result = manager.Result(result.NodeName, result.RequestID)
	}

	payload, err := json.Marshal(WSMessage{Type: "node_detail_response", NodeName: result.NodeName, Data: result})
	if err != nil || result.Details == nil {
		return payload, c.ctx, noCleanup, err
	}

	ctx, cancel := context.WithDeadline(c.ctx, result.Details.ExpiresAt)
	stop := context.AfterFunc(manager.ctx, cancel)

	return payload, ctx, func() { stop(); cancel() }, nil
}
