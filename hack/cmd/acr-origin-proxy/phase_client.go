// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const localPhaseControlURL = "http://127.0.0.1:9090/control/phase"

func runSetPhase(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: acr-origin-proxy set-phase <setup|baseline|gantry_cold|idle> <timeout>")
	}

	phase := benchmarkPhase(args[0])
	if !phase.valid() {
		return fmt.Errorf("invalid benchmark phase %q", phase)
	}

	timeout, err := time.ParseDuration(args[1])
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid phase timeout %q", args[1])
	}

	token := os.Getenv("BENCHMARK_CONTROL_TOKEN")
	if token == "" {
		return fmt.Errorf("benchmark control token is required in BENCHMARK_CONTROL_TOKEN")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return setRemotePhase(ctx, http.DefaultClient, localPhaseControlURL, token, phase)
}

func setRemotePhase(
	ctx context.Context,
	client *http.Client,
	endpoint,
	token string,
	phase benchmarkPhase,
) error {
	payload, err := json.Marshal(map[string]benchmarkPhase{"phase": phase})
	if err != nil {
		return err
	}

	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}

		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")

		response, err := client.Do(request)
		if err != nil {
			return err
		}

		_, drainErr := io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))

		closeErr := response.Body.Close()
		if drainErr != nil || closeErr != nil {
			return fmt.Errorf("consume phase response: %w", errors.Join(drainErr, closeErr))
		}

		switch response.StatusCode {
		case http.StatusOK:
			return nil
		case http.StatusConflict:
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for proxy phase drain: %w", ctx.Err())
			case <-time.After(2 * time.Second):
			}
		default:
			return fmt.Errorf("phase switch returned HTTP %d", response.StatusCode)
		}
	}
}
