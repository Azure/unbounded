// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const maxNodeExporterReadinessBody = 2 * 1024 * 1024

type waitNodeExporter struct {
	log        *slog.Logger
	goalState  *goalstates.NodeStart
	httpClient *http.Client
}

// WaitForNodeExporter verifies that the enabled node exporter service is available.
func WaitForNodeExporter(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &waitNodeExporter{log: log, goalState: goalState}
}

func (w *waitNodeExporter) Name() string { return "wait-node-exporter" }

func (w *waitNodeExporter) Do(ctx context.Context) error {
	if !w.goalState.NodeExporter.Enabled {
		return nil
	}

	if w.goalState.NodeExporter.TLS.Enabled {
		if _, err := executil.MachineRun(ctx, w.log, w.goalState.MachineName,
			"systemctl", "is-active", goalstates.NodeExporterServiceUnit); err != nil {
			return fmt.Errorf("node exporter service is not active: %w", err)
		}

		return nil
	}

	client := w.httpClient
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{Proxy: nil},
			Timeout:   3 * time.Second,
		}
	}

	url := "http://" + w.goalState.NodeExporter.ListenAddress + "/metrics"

	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if err := nodeExporterReady(ctx, client, url); err == nil {
			return nil
		} else {
			w.log.Debug("node exporter readiness check failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("node exporter did not become ready")
		case <-ticker.C:
		}
	}
}

func nodeExporterReady(ctx context.Context, client *http.Client, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("create node exporter readiness request: %w", err)
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("query node exporter metrics: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // best effort

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("node exporter metrics returned %s", response.Status)
	}

	limited := io.LimitReader(response.Body, maxNodeExporterReadinessBody+1)
	scanner := bufio.NewScanner(limited)

	read := 0
	for scanner.Scan() {
		read += len(scanner.Bytes()) + 1
		if strings.HasPrefix(scanner.Text(), "node_exporter_build_info{") || scanner.Text() == "node_exporter_build_info 1" {
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read node exporter metrics: %w", err)
	}

	if read > maxNodeExporterReadinessBody {
		return fmt.Errorf("node exporter metrics response exceeds %d bytes", maxNodeExporterReadinessBody)
	}

	return fmt.Errorf("node exporter metrics response is missing node_exporter_build_info")
}
