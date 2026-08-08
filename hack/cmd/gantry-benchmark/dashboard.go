// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const dashboardConfigMapName = "gantry-benchmark-dashboard"

func (b *benchmark) installDashboard(ctx context.Context) error {
	path := filepath.Join(b.config.RepoRoot, "hack", "gantry-benchmark", "grafana-dashboard.json")

	rawDashboard, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Grafana dashboard: %w", err)
	}

	dashboard, err := renderDashboard(rawDashboard, b.config.Namespace, b.config.GantryNamespace)
	if err != nil {
		return err
	}

	configMap := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      dashboardConfigMapName,
			"namespace": b.config.MonitoringNamespace,
			"labels": map[string]string{
				"grafana_dashboard":         "1",
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"data": map[string]string{"gantry-benchmark.json": string(dashboard)},
	}

	return b.applyObject(ctx, configMap)
}

func renderDashboard(raw []byte, benchmarkNamespace, gantryNamespace string) ([]byte, error) {
	var dashboard map[string]any
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		return nil, fmt.Errorf("decode Grafana dashboard: %w", err)
	}

	templating, ok := dashboard["templating"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("grafana dashboard templating has type %T, want object", dashboard["templating"])
	}

	variables, ok := templating["list"].([]any)
	if !ok {
		return nil, fmt.Errorf("grafana dashboard templating.list has type %T, want array", templating["list"])
	}

	updated := map[string]bool{"namespace": false, "gantry_namespace": false}

	for index, rawVariable := range variables {
		variable, ok := rawVariable.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("grafana dashboard variable %d has type %T, want object", index, rawVariable)
		}

		name, ok := variable["name"].(string)
		if !ok {
			return nil, fmt.Errorf("grafana dashboard variable %d has no string name", index)
		}

		var value string

		switch name {
		case "namespace":
			value = benchmarkNamespace
		case "gantry_namespace":
			value = gantryNamespace
		default:
			continue
		}

		variable["current"] = map[string]any{"text": value, "value": value}
		variable["options"] = []any{map[string]any{"selected": true, "text": value, "value": value}}
		variable["query"] = value
		updated[name] = true
	}

	for name, wasUpdated := range updated {
		if !wasUpdated {
			return nil, fmt.Errorf("grafana dashboard has no %q variable", name)
		}
	}

	encoded, err := json.MarshalIndent(dashboard, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Grafana dashboard: %w", err)
	}

	return append(encoded, '\n'), nil
}

func (b *benchmark) deleteDashboard(ctx context.Context) error {
	_, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.MonitoringNamespace,
		"delete", "configmap", dashboardConfigMapName,
		"--ignore-not-found=true",
	)

	return err
}
