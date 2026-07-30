// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	stateConfigMapName = "gantry-benchmark-state"
	lockConfigMapName  = "gantry-benchmark-lock"
)

type benchmarkState struct {
	RunID                        string        `json:"run_id"`
	Mode                         benchmarkMode `json:"mode"`
	Status                       string        `json:"status"`
	BenchmarkNamespace           string        `json:"benchmark_namespace"`
	GantryNamespace              string        `json:"gantry_namespace"`
	GantryDaemonSet              string        `json:"gantry_daemonset"`
	GantryConfigMap              string        `json:"gantry_configmap"`
	MonitoringNamespace          string        `json:"monitoring_namespace"`
	PrometheusService            string        `json:"prometheus_service"`
	NodeCount                    int           `json:"node_count"`
	ImagePlatform                string        `json:"image_platform"`
	ACRLoginServer               string        `json:"acr_login_server"`
	BaselineImage                string        `json:"baseline_image,omitempty"`
	GantryColdImage              string        `json:"gantry_cold_image,omitempty"`
	ProxyImage                   string        `json:"proxy_image,omitempty"`
	ProxyClusterIP               string        `json:"proxy_cluster_ip,omitempty"`
	OriginalGantryConfig         string        `json:"original_gantry_config"`
	OriginalGantryConfigSHA      string        `json:"original_gantry_config_sha256"`
	PatchedGantryConfigSHA       string        `json:"patched_gantry_config_sha256,omitempty"`
	GantryRestored               bool          `json:"gantry_restored"`
	AzureTelemetry               bool          `json:"azure_telemetry"`
	LogAnalyticsWorkspaceID      string        `json:"log_analytics_workspace_id,omitempty"`
	ACRResourceID                string        `json:"acr_resource_id,omitempty"`
	AKSResourceID                string        `json:"aks_resource_id,omitempty"`
	ACRPrivateEndpointResourceID string        `json:"acr_private_endpoint_resource_id,omitempty"`
}

// usesProxy reports whether the enabled run installed the counting proxy.
func (s benchmarkState) usesProxy() bool {
	return s.Mode != benchmarkModeDirect
}

func (s benchmarkState) preparedImages() (string, string, error) {
	if s.BaselineImage == "" || s.GantryColdImage == "" {
		return "", "", fmt.Errorf("benchmark images are not prepared; run prepare before preflight")
	}

	if _, err := imageDigestFromReference(s.BaselineImage); err != nil {
		return "", "", fmt.Errorf("invalid prepared baseline image: %w", err)
	}

	if _, err := imageDigestFromReference(s.GantryColdImage); err != nil {
		return "", "", fmt.Errorf("invalid prepared Gantry-cold image: %w", err)
	}

	return s.BaselineImage, s.GantryColdImage, nil
}

func (b *benchmark) saveState(ctx context.Context, state benchmarkState) error {
	stateForJSON := state
	stateForJSON.OriginalGantryConfig = ""

	encoded, err := json.Marshal(stateForJSON)
	if err != nil {
		return fmt.Errorf("encode benchmark state: %w", err)
	}

	configMap := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      stateConfigMapName,
			"namespace": b.config.Namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":    "gantry-benchmark",
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"data": map[string]string{
			"state.json":         string(encoded),
			"gantry-config.yaml": state.OriginalGantryConfig,
		},
	}

	manifest, err := json.Marshal(configMap)
	if err != nil {
		return fmt.Errorf("encode state ConfigMap: %w", err)
	}

	if _, err := b.commands.Run(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}

	return b.writeLocalState(state)
}

func (b *benchmark) writeLocalState(state benchmarkState) error {
	stateForJSON := state
	stateForJSON.OriginalGantryConfig = ""

	runDirectory := filepath.Join(b.config.StateRoot, state.RunID)
	if err := os.MkdirAll(runDirectory, 0o750); err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}

	localState, err := json.MarshalIndent(stateForJSON, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(runDirectory, "state.json"), append(localState, '\n'), 0o640); err != nil {
		return fmt.Errorf("write local state: %w", err)
	}

	if state.OriginalGantryConfig != "" {
		if err := os.WriteFile(
			filepath.Join(runDirectory, "gantry-config.original.yaml"),
			[]byte(state.OriginalGantryConfig),
			0o640,
		); err != nil {
			return fmt.Errorf("write original Gantry config: %w", err)
		}
	}

	return nil
}

func (b *benchmark) stateExists(ctx context.Context) (bool, error) {
	return b.configMapExists(ctx, b.config.Namespace, stateConfigMapName)
}

func (b *benchmark) lockExists(ctx context.Context) (bool, error) {
	return b.configMapExists(ctx, b.config.GantryNamespace, lockConfigMapName)
}

func (b *benchmark) configMapExists(ctx context.Context, namespace, name string) (bool, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", namespace,
		"get", "configmap", name,
		"--ignore-not-found",
		"-o", "name",
	)
	if err != nil {
		return false, err
	}

	return len(output) != 0, nil
}

func (b *benchmark) lockRunID(ctx context.Context) (string, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.GantryNamespace,
		"get", "configmap", lockConfigMapName,
		"--ignore-not-found",
		"-o", "json",
	)
	if err != nil {
		return "", err
	}

	if len(output) == 0 {
		return "", nil
	}

	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(output, &configMap); err != nil {
		return "", fmt.Errorf("decode benchmark lock ConfigMap: %w", err)
	}

	runID := configMap.Data["run-id"]
	if runID == "" {
		return "", fmt.Errorf("benchmark lock ConfigMap has no run-id")
	}

	return runID, nil
}

func (b *benchmark) requireLock(ctx context.Context, runID string) error {
	lockedBy, err := b.lockRunID(ctx)
	if err != nil {
		return err
	}

	if lockedBy != runID {
		return fmt.Errorf("benchmark lock is owned by %q, want %q", lockedBy, runID)
	}

	return nil
}

func (b *benchmark) releaseLock(ctx context.Context, runID string) error {
	lockedBy, err := b.lockRunID(ctx)
	if err != nil {
		return err
	}

	if lockedBy == "" {
		return nil
	}

	if lockedBy != runID {
		return fmt.Errorf("refusing to release benchmark lock owned by %q, want %q", lockedBy, runID)
	}

	_, err = b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.GantryNamespace,
		"delete", "configmap", lockConfigMapName,
		"--wait=true",
	)

	return err
}

func gantryConfigSHA(raw string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}

func (b *benchmark) loadState(ctx context.Context) (benchmarkState, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.Namespace,
		"get", "configmap", stateConfigMapName,
		"-o", "json",
	)
	if err != nil {
		return benchmarkState{}, err
	}

	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(output, &configMap); err != nil {
		return benchmarkState{}, fmt.Errorf("decode benchmark state ConfigMap: %w", err)
	}

	var state benchmarkState
	if err := json.Unmarshal([]byte(configMap.Data["state.json"]), &state); err != nil {
		return benchmarkState{}, fmt.Errorf("decode benchmark state: %w", err)
	}

	state.OriginalGantryConfig = configMap.Data["gantry-config.yaml"]

	// State written before direct mode existed has no mode field and always
	// described a proxy run.
	if state.Mode == "" {
		state.Mode = benchmarkModeProxy
	}

	if state.Mode != benchmarkModeProxy && state.Mode != benchmarkModeDirect {
		return benchmarkState{}, fmt.Errorf("benchmark state has unknown mode %q", state.Mode)
	}

	if state.RunID == "" ||
		state.BenchmarkNamespace == "" ||
		state.GantryNamespace == "" ||
		state.GantryDaemonSet == "" ||
		state.GantryConfigMap == "" ||
		state.MonitoringNamespace == "" ||
		state.PrometheusService == "" ||
		state.NodeCount <= 0 ||
		state.ImagePlatform == "" ||
		state.ACRLoginServer == "" ||
		state.OriginalGantryConfig == "" {
		return benchmarkState{}, fmt.Errorf("benchmark state ConfigMap is incomplete")
	}

	if state.usesProxy() && state.ProxyImage == "" {
		return benchmarkState{}, fmt.Errorf("benchmark state ConfigMap is incomplete: proxy mode requires proxy_image")
	}

	if state.AzureTelemetry && (state.LogAnalyticsWorkspaceID == "" || state.ACRResourceID == "" || state.AKSResourceID == "" || state.ACRPrivateEndpointResourceID == "") {
		return benchmarkState{}, fmt.Errorf("benchmark state ConfigMap is incomplete: Azure telemetry resource IDs are required")
	}

	if state.BenchmarkNamespace != b.config.Namespace {
		return benchmarkState{}, fmt.Errorf(
			"benchmark state namespace is %q, command is configured for %q",
			state.BenchmarkNamespace,
			b.config.Namespace,
		)
	}

	if actualSHA := gantryConfigSHA(state.OriginalGantryConfig); actualSHA != state.OriginalGantryConfigSHA {
		return benchmarkState{}, fmt.Errorf(
			"benchmark state original Gantry config sha256 is %s, computed %s",
			state.OriginalGantryConfigSHA,
			actualSHA,
		)
	}

	b.config.GantryNamespace = state.GantryNamespace
	b.config.GantryDaemonSet = state.GantryDaemonSet
	b.config.GantryConfigMap = state.GantryConfigMap
	b.config.MonitoringNamespace = state.MonitoringNamespace
	b.config.PrometheusService = state.PrometheusService
	b.config.NodeCount = state.NodeCount
	b.config.ImagePlatform = state.ImagePlatform
	// The mode is fixed when the run is enabled. Later commands must not be able
	// to change routing or restoration semantics through the environment.
	b.config.Mode = state.Mode
	b.config.AzureTelemetry = state.AzureTelemetry
	b.config.LogAnalyticsWorkspaceID = state.LogAnalyticsWorkspaceID
	b.config.ACRResourceID = state.ACRResourceID
	b.config.AKSResourceID = state.AKSResourceID
	b.config.ACRPrivateEndpointResourceID = state.ACRPrivateEndpointResourceID

	return state, nil
}

func (b *benchmark) status(ctx context.Context) error {
	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	state.OriginalGantryConfig = ""

	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	writeAll(b.stdout, string(encoded)+"\n")

	return nil
}
