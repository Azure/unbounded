// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

const (
	proxyManifestPath      = "hack/gantry-benchmark/manifests/proxy.yaml.tmpl"
	monitoringManifestPath = "hack/gantry-benchmark/manifests/monitoring.yaml.tmpl"
)

func (b *benchmark) enable(ctx context.Context) (returnErr error) {
	if err := b.config.validateEnable(); err != nil {
		return err
	}

	if err := b.validateContext(ctx); err != nil {
		return err
	}

	if _, err := b.targetNodes(ctx); err != nil {
		return err
	}

	if err := b.validateGantry(ctx); err != nil {
		return err
	}

	runID, err := newRunID()
	if err != nil {
		return err
	}

	var controlToken string

	if b.config.usesProxy() {
		controlToken, err = randomHex(32)
		if err != nil {
			return err
		}
	}

	namespace := map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": b.config.Namespace,
			"labels": map[string]string{
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
	}
	if err := b.applyObject(ctx, namespace); err != nil {
		return err
	}

	if err := b.acquireLock(ctx, runID); err != nil {
		return fmt.Errorf("acquire benchmark lock: %w", err)
	}

	cleanupNamespace := true
	dashboardInstalled := false

	defer func() {
		if returnErr != nil && cleanupNamespace {
			cleanupContext, cancel := context.WithTimeout(context.Background(), b.config.RolloutTimeout)
			defer cancel()

			if dashboardInstalled {
				_ = b.deleteDashboard(cleanupContext) //nolint:errcheck // Preserve the original enable error during best-effort rollback.
			}

			_, namespaceErr := b.commands.Run(
				cleanupContext,
				nil,
				"kubectl", "delete", "namespace", b.config.Namespace,
				"--ignore-not-found=true", "--wait=true",
			)
			if namespaceErr == nil {
				_ = b.releaseLock(cleanupContext, runID) //nolint:errcheck // Preserve the original enable error during best-effort rollback.
			}
		}
	}()

	originalConfig, err := b.readGantryConfig(ctx)
	if err != nil {
		return err
	}

	if !b.config.usesProxy() {
		if err := validateDirectGantryRegistry([]byte(originalConfig), b.config.GantryACRLoginServer); err != nil {
			return fmt.Errorf("validate dedicated Gantry ACR configuration: %w", err)
		}
	}

	state := benchmarkState{
		RunID:                        runID,
		Mode:                         b.config.Mode,
		Status:                       "enabling",
		BenchmarkNamespace:           b.config.Namespace,
		GantryNamespace:              b.config.GantryNamespace,
		GantryDaemonSet:              b.config.GantryDaemonSet,
		GantryConfigMap:              b.config.GantryConfigMap,
		MonitoringNamespace:          b.config.MonitoringNamespace,
		PrometheusService:            b.config.PrometheusService,
		NodeCount:                    b.config.NodeCount,
		ImagePlatform:                b.config.ImagePlatform,
		ImageSizeMiB:                 b.config.ImageSizeMiB,
		ImageLayers:                  b.config.ImageLayers,
		WorkloadRepository:           b.config.WorkloadRepository,
		BaselineACRLoginServer:       b.config.BaselineACRLoginServer,
		GantryACRLoginServer:         b.config.GantryACRLoginServer,
		ACRLoginServer:               b.config.ACRLoginServer,
		ProxyImage:                   b.config.ProxyImage,
		OriginalGantryConfig:         originalConfig,
		OriginalGantryConfigSHA:      gantryConfigSHA(originalConfig),
		GantryRestored:               true,
		AzureTelemetry:               b.config.AzureTelemetry,
		LogAnalyticsWorkspaceID:      b.config.LogAnalyticsWorkspaceID,
		BaselineACRResourceID:        b.config.BaselineACRResourceID,
		BaselinePrivateEndpointID:    b.config.BaselinePrivateEndpointID,
		GantryACRResourceID:          b.config.GantryACRResourceID,
		GantryPrivateEndpointID:      b.config.GantryPrivateEndpointID,
		ACRResourceID:                b.config.ACRResourceID,
		AKSResourceID:                b.config.AKSResourceID,
		ACRPrivateEndpointResourceID: b.config.ACRPrivateEndpointResourceID,
	}
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	manifestData := proxyManifestData{
		Namespace:       b.config.Namespace,
		GantryNamespace: b.config.GantryNamespace,
		MonitoringLabel: b.config.KPSRelease,
		NodeOS:          strings.SplitN(b.config.ImagePlatform, "/", 2)[0],
		NodeArch:        strings.SplitN(b.config.ImagePlatform, "/", 2)[1],
		ProxyImage:      b.config.ProxyImage,
		ACRLoginServer:  b.config.ACRLoginServer,
		RunID:           runID,
	}

	// The Gantry PodMonitor stamps gantry_benchmark="true" onto agent samples and
	// is required in both modes; only the proxy objects are mode-specific.
	monitoring, err := b.renderManifest(monitoringManifestPath, manifestData)
	if err != nil {
		return err
	}

	if _, err := b.commands.Run(ctx, monitoring, "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"rollout", "status", "daemonset/gantry-benchmark-node-observer",
		"--timeout", b.config.RolloutTimeout.String(),
	); err != nil {
		return fmt.Errorf("wait for benchmark node observer: %w", err)
	}

	if b.config.usesProxy() {
		secret := map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      "acr-origin-proxy",
				"namespace": b.config.Namespace,
			},
			"type": "Opaque",
			"stringData": map[string]string{
				"username":      b.config.ACRUsername,
				"password":      b.config.ACRPassword,
				"control-token": controlToken,
			},
		}
		if err := b.applyObject(ctx, secret); err != nil {
			return err
		}

		manifest, err := b.renderManifest(proxyManifestPath, manifestData)
		if err != nil {
			return err
		}

		if _, err := b.commands.Run(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
			return err
		}

		if _, err := b.commands.Run(
			ctx,
			nil,
			"kubectl",
			"-n", b.config.Namespace,
			"rollout", "status", "deployment/acr-origin-proxy",
			"--timeout", b.config.RolloutTimeout.String(),
		); err != nil {
			return err
		}

		proxyIPOutput, err := b.commands.Run(
			ctx,
			nil,
			"kubectl",
			"-n", b.config.Namespace,
			"get", "service", "acr-origin-proxy",
			"-o", "jsonpath={.spec.clusterIP}",
		)
		if err != nil {
			return err
		}

		proxyIP := strings.TrimSpace(string(proxyIPOutput))
		if proxyIP == "" || strings.EqualFold(proxyIP, "None") {
			return fmt.Errorf("proxy service has no ClusterIP")
		}

		state.ProxyClusterIP = proxyIP
	}

	if err := b.installDashboard(ctx); err != nil {
		return err
	}

	dashboardInstalled = true

	// Patch Gantry to route origin pulls through the counting proxy and roll it
	// out now, at enable time. Restarting Gantry clears each agent's in-memory
	// DHT routing table, and a 300-node cluster needs several minutes to
	// re-converge. Doing it here - rather than inside `run`, immediately before
	// the Gantry-cold phase - means `run` never restarts Gantry: the DHT is
	// fully warm by the time the cold phase measures peer distribution.
	// `disable` restores the original Gantry config.
	//
	// Direct mode has no proxy to point at, so Gantry keeps its original ACR
	// endpoint and is never patched or rolled by the benchmark at all.
	if b.config.usesProxy() {
		if err := b.patchGantryForBenchmark(ctx, &state); err != nil {
			return err
		}
	}

	state.Status = "enabled"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	cleanupNamespace = false

	writeAll(b.stdout, fmt.Sprintf("enabled Gantry benchmark %s against context %s\n", runID, b.config.ConfirmedContext))

	return nil
}

func (b *benchmark) acquireLock(ctx context.Context, runID string) error {
	lock := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      lockConfigMapName,
			"namespace": b.config.GantryNamespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":    "gantry-benchmark",
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"data": map[string]string{"run-id": runID},
	}

	manifest, err := json.Marshal(lock)
	if err != nil {
		return err
	}

	_, err = b.commands.Run(ctx, manifest, "kubectl", "create", "-f", "-")

	return err
}

func (b *benchmark) readGantryConfig(ctx context.Context) (string, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.GantryNamespace,
		"get", "configmap", b.config.GantryConfigMap,
		"-o", "json",
	)
	if err != nil {
		return "", err
	}

	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(output, &configMap); err != nil {
		return "", fmt.Errorf("decode gantry ConfigMap: %w", err)
	}

	config := configMap.Data["config.yaml"]
	if config == "" {
		return "", fmt.Errorf("gantry ConfigMap %s/%s has no config.yaml", b.config.GantryNamespace, b.config.GantryConfigMap)
	}

	return config, nil
}

func (b *benchmark) applyObject(ctx context.Context, object any) error {
	manifest, err := json.Marshal(object)
	if err != nil {
		return err
	}

	_, err = b.commands.Run(ctx, manifest, "kubectl", "apply", "-f", "-")

	return err
}

type proxyManifestData struct {
	Namespace       string
	GantryNamespace string
	MonitoringLabel string
	NodeOS          string
	NodeArch        string
	ProxyImage      string
	ACRLoginServer  string
	RunID           string
}

func (b *benchmark) renderManifest(manifestPath string, data proxyManifestData) ([]byte, error) {
	path := filepath.Join(b.config.RepoRoot, manifestPath)

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest template %s: %w", manifestPath, err)
	}

	tmpl, err := template.New(filepath.Base(manifestPath)).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse manifest template %s: %w", manifestPath, err)
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return nil, fmt.Errorf("render manifest template %s: %w", manifestPath, err)
	}

	return rendered.Bytes(), nil
}

func newRunID() (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}

	return "run-" + time.Now().UTC().Format("20060102-150405") + "-" + suffix, nil
}

func randomHex(byteCount int) (string, error) {
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}

	return hex.EncodeToString(value), nil
}
