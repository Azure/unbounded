// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"time"
)

const nodeDetailHTTPBodyLimit = 1024 * 1024

// Detail commands and retry wakeups share the existing one-in-flight HTTP loop.
// The normal publication ticker remains independent of diagnostic traffic.
func statusPushEvents(ctx context.Context, interval time.Duration, detailWake <-chan struct{}) <-chan bool {
	events := make(chan bool)

	go func() {
		ticker := time.NewTicker(interval)
		retry := time.NewTicker(nodeDetailRetryInterval)

		defer ticker.Stop()
		defer retry.Stop()

		for {
			detailOnly := false

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-detailWake:
				detailOnly = true
			case <-retry.C:
				detailOnly = true
			}

			select {
			case events <- detailOnly:
			case <-ctx.Done():
				return
			}
		}
	}()

	return events
}

func gzipStatusPayload(data []byte) ([]byte, error) {
	var body bytes.Buffer

	writer, err := gzip.NewWriterLevel(&body, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}

	if _, err := writer.Write(data); err != nil {
		return nil, errors.Join(err, writer.Close())
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	return body.Bytes(), nil
}

func (s *nodeDetailState) httpBody(nodeName string, delivery *nodeDetailDelivery, data []byte) ([]byte, error) {
	body, err := gzipStatusPayload(data)
	if err != nil || delivery == nil || len(body) <= nodeDetailHTTPBodyLimit {
		return body, err
	}

	payload := detailErrorPayload(nodeName, delivery.id, "detail response exceeds 1 MiB compressed HTTP body limit")

	s.mu.Lock()
	if reply := s.replies[delivery.id]; reply != nil && !reply.done {
		reply.payload = payload
	}
	s.mu.Unlock()

	delivery.payload = payload

	return gzipStatusPayload(payload)
}
