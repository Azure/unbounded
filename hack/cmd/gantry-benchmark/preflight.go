// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const registryManifestAccept = "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json"

type registryManifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

func (b *benchmark) preflight(ctx context.Context) error {
	state, err := b.loadState(ctx)
	if err != nil {
		return fmt.Errorf("load enabled benchmark: %w", err)
	}

	if err := b.requireLock(ctx, state.RunID); err != nil {
		return err
	}

	if state.Status != "enabled" && state.Status != "preflight-passed" {
		return fmt.Errorf("benchmark state is %q, complete enable or run disable before preflight", state.Status)
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

	if err := b.smokeProxy(ctx, state); err != nil {
		return err
	}

	if err := b.checkNodeProxyReachability(ctx, state); err != nil {
		return err
	}

	if err := b.checkMonitoring(ctx, state); err != nil {
		return err
	}

	state.Status = "preflight-passed"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("preflight passed for %s on %d nodes\n", state.RunID, b.config.NodeCount))

	return nil
}

func (b *benchmark) smokeProxy(ctx context.Context, state benchmarkState) error {
	repository, reference, err := splitImageReference(state.ProxyImage, state.ACRLoginServer)
	if err != nil {
		return fmt.Errorf("use proxy image as ACR smoke image: %w", err)
	}

	base := "http://acr-origin-proxy:5002"
	if _, err := b.proxyHTTP(ctx, "check-url", base+"/v2/", "30s"); err != nil {
		return fmt.Errorf("proxy registry ping failed: %w", err)
	}

	manifestBytes, err := b.proxyHTTP(
		ctx,
		"get-url",
		fmt.Sprintf("%s/v2/%s/manifests/%s", base, repository, reference),
		"30s",
		registryManifestAccept,
	)
	if err != nil {
		return fmt.Errorf("proxy manifest request failed: %w", err)
	}

	var manifest registryManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode smoke manifest: %w", err)
	}

	if len(manifest.Manifests) != 0 {
		childDigest := ""

		for _, child := range manifest.Manifests {
			if child.Platform.OS == "linux" && child.Platform.Architecture == "amd64" {
				childDigest = child.Digest

				break
			}
		}

		if childDigest == "" {
			return fmt.Errorf("smoke image index has no linux/amd64 manifest")
		}

		manifestBytes, err = b.proxyHTTP(
			ctx,
			"get-url",
			fmt.Sprintf("%s/v2/%s/manifests/%s", base, repository, childDigest),
			"30s",
			registryManifestAccept,
		)
		if err != nil {
			return fmt.Errorf("proxy child manifest request failed: %w", err)
		}

		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return fmt.Errorf("decode smoke child manifest: %w", err)
		}
	}

	if manifest.Config.Digest == "" {
		return fmt.Errorf("smoke manifest has no config digest")
	}

	_, err = b.proxyHTTP(
		ctx,
		"check-url",
		fmt.Sprintf("%s/v2/%s/blobs/%s", base, repository, manifest.Config.Digest),
		"30s",
	)
	if err != nil {
		return fmt.Errorf("proxy config blob request failed: %w", err)
	}

	return nil
}

func (b *benchmark) proxyHTTP(ctx context.Context, args ...string) ([]byte, error) {
	commandArgs := []string{
		"-n", b.config.Namespace,
		"exec", "deployment/acr-origin-proxy", "--",
		"/usr/local/bin/acr-origin-proxy",
	}
	commandArgs = append(commandArgs, args...)

	return b.commands.Run(ctx, nil, "kubectl", commandArgs...)
}

func splitImageReference(image, registry string) (string, string, error) {
	prefix := registry + "/"
	if !strings.HasPrefix(image, prefix) {
		return "", "", fmt.Errorf("image %q is not in registry %q", image, registry)
	}

	remainder := strings.TrimPrefix(image, prefix)
	if repository, digest, ok := strings.Cut(remainder, "@"); ok {
		if repository == "" || digest == "" {
			return "", "", fmt.Errorf("invalid digest image reference %q", image)
		}

		return repository, digest, nil
	}

	lastSlash := strings.LastIndexByte(remainder, '/')

	lastColon := strings.LastIndexByte(remainder, ':')
	if lastColon <= lastSlash || lastColon == len(remainder)-1 {
		return "", "", fmt.Errorf("image %q must include a tag or digest", image)
	}

	return remainder[:lastColon], remainder[lastColon+1:], nil
}

func (b *benchmark) checkNodeProxyReachability(ctx context.Context, state benchmarkState) error {
	name := "acr-proxy-node-reachability"
	if _, err := b.commands.Run(ctx, nil, "kubectl", "-n", b.config.Namespace, "delete", "daemonset", name, "--ignore-not-found=true", "--wait=true"); err != nil {
		return err
	}

	daemonSet := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": b.config.Namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":    name,
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"spec": map[string]any{
			"selector": map[string]any{
				"matchLabels": map[string]string{"app.kubernetes.io/name": name},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]string{"app.kubernetes.io/name": name},
				},
				"spec": map[string]any{
					"hostNetwork":  true,
					"dnsPolicy":    "ClusterFirstWithHostNet",
					"nodeSelector": b.config.nodeSelector(),
					"tolerations":  []any{map[string]any{"operator": "Exists"}},
					"containers": []any{
						map[string]any{
							"name":            "probe",
							"image":           state.ProxyImage,
							"imagePullPolicy": "Always",
							"args": []string{
								"probe-health",
								fmt.Sprintf("http://%s:5002/healthz", state.ProxyClusterIP),
								"5s",
							},
							"readinessProbe": map[string]any{
								"exec": map[string]any{
									"command": []string{
										"/usr/local/bin/acr-origin-proxy",
										"check-url",
										fmt.Sprintf("http://%s:5002/healthz", state.ProxyClusterIP),
										"5s",
									},
								},
								"periodSeconds": 5,
							},
						},
					},
				},
			},
		},
	}
	if err := b.applyObject(ctx, daemonSet); err != nil {
		return err
	}

	defer func() {
		_, _ = b.commands.Run(context.Background(), nil, "kubectl", "-n", b.config.Namespace, "delete", "daemonset", name, "--ignore-not-found=true", "--wait=false") //nolint:errcheck // Best-effort reachability cleanup.
	}()

	if _, err := b.commands.Run(ctx, nil, "kubectl", "-n", b.config.Namespace, "rollout", "status", "daemonset/"+name, "--timeout", b.config.RolloutTimeout.String()); err != nil {
		return err
	}

	output, err := b.commands.Run(ctx, nil, "kubectl", "-n", b.config.Namespace, "get", "daemonset", name, "-o", "json")
	if err != nil {
		return err
	}

	var status daemonSetStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return fmt.Errorf("decode reachability DaemonSet: %w", err)
	}

	if status.Status.DesiredNumberScheduled != b.config.NodeCount || status.Status.NumberReady != b.config.NodeCount {
		return fmt.Errorf("proxy reachability passed on %d/%d nodes", status.Status.NumberReady, b.config.NodeCount)
	}

	return nil
}

func (b *benchmark) checkMonitoring(ctx context.Context, state benchmarkState) error {
	revision, err := b.gantryRevision(ctx)
	if err != nil {
		return err
	}

	if err := b.waitForGantryRevisionScrape(ctx, revision); err != nil {
		return err
	}

	dhtCountQuery := fmt.Sprintf(
		`count(p2p_dht_health_score{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	dhtCount, err := b.queryPrometheus(ctx, dhtCountQuery)
	if err != nil {
		return fmt.Errorf("query Gantry DHT metric count: %w", err)
	}

	if int(dhtCount) != b.config.NodeCount {
		return fmt.Errorf("prometheus reports DHT health for %.0f/%d Gantry pods", dhtCount, b.config.NodeCount)
	}

	dhtQuery := fmt.Sprintf(
		`min(p2p_dht_health_score{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	dhtHealth, err := b.queryPrometheus(ctx, dhtQuery)
	if err != nil {
		return fmt.Errorf("query Gantry DHT health: %w", err)
	}

	if dhtHealth <= 0 {
		return fmt.Errorf("minimum Gantry DHT health is %.3f, want greater than zero", dhtHealth)
	}

	proxyQuery := fmt.Sprintf(`sum(origin_requests_completed_total{run_id=%q,phase="setup",gantry_benchmark="true"})`, state.RunID)
	deadline := time.Now().Add(2 * time.Minute)

	for {
		proxyRequests, queryErr := b.queryPrometheus(ctx, proxyQuery)
		if queryErr == nil && proxyRequests > 0 {
			return nil
		}

		if time.Now().After(deadline) {
			if queryErr != nil {
				return fmt.Errorf("proxy metrics did not become queryable: %w", queryErr)
			}

			return fmt.Errorf("proxy setup requests were not scraped within 2m")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *benchmark) queryPrometheus(ctx context.Context, query string) (float64, error) {
	return b.queryPrometheusValue(ctx, query, false)
}

func (b *benchmark) queryPrometheusOrZero(ctx context.Context, query string) (float64, error) {
	return b.queryPrometheusValue(ctx, query, true)
}

func (b *benchmark) queryPrometheusValue(ctx context.Context, query string, zeroOnEmpty bool) (float64, error) {
	rawPath := fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:9090/proxy/api/v1/query?query=%s",
		b.config.MonitoringNamespace,
		b.config.PrometheusService,
		url.QueryEscape(query),
	)

	output, err := b.commands.Run(ctx, nil, "kubectl", "get", "--raw", rawPath)
	if err != nil {
		return 0, err
	}

	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return 0, fmt.Errorf("decode Prometheus response: %w", err)
	}

	if response.Status != "success" {
		return 0, fmt.Errorf("prometheus query status is %q", response.Status)
	}

	if len(response.Data.Result) == 0 {
		if zeroOnEmpty {
			return 0, nil
		}

		return 0, fmt.Errorf("prometheus query returned no samples")
	}

	var total float64

	for _, result := range response.Data.Result {
		value, ok := result.Value[1].(string)
		if !ok {
			return 0, fmt.Errorf("prometheus sample has non-string value")
		}

		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("parse Prometheus sample %q: %w", value, err)
		}

		total += parsed
	}

	return total, nil
}
