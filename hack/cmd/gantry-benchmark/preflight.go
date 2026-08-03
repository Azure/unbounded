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

	if state.Status != "images-prepared" && state.Status != "preflight-passed" {
		return fmt.Errorf("benchmark state is %q, run prepare before preflight or disable the run", state.Status)
	}

	if _, _, err := state.preparedImages(); err != nil {
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

	if state.AzureTelemetry {
		if err := b.checkAzureTelemetry(ctx); err != nil {
			return err
		}
	}

	// The proxy smoke test and the node-to-proxy reachability DaemonSet both
	// exercise objects that direct mode never installs. Everything else
	// (context, node inventory, Gantry convergence, monitoring) still applies.
	if state.usesProxy() {
		if err := b.smokeProxy(ctx, state); err != nil {
			return err
		}

		if err := b.checkNodeProxyReachability(ctx, state); err != nil {
			return err
		}
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

	peerByteCountQuery := fmt.Sprintf(
		`count(gantry_peer_serve_bytes_total{namespace=%q,kind="layer",gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	peerByteCount, err := b.queryPrometheus(ctx, peerByteCountQuery)
	if err != nil {
		return fmt.Errorf("query Gantry peer byte metric count: %w", err)
	}

	if int(peerByteCount) != b.config.NodeCount {
		return fmt.Errorf(
			"prometheus reports peer byte counters for %.0f/%d Gantry pods",
			peerByteCount,
			b.config.NodeCount,
		)
	}

	if !state.usesProxy() {
		originByteCountQuery := fmt.Sprintf(
			`count(gantry_origin_bytes_total{namespace=%q,kind="layer",gantry_benchmark="true",controller_revision_hash=%q})`,
			b.config.GantryNamespace,
			revision,
		)

		originByteCount, err := b.queryPrometheus(ctx, originByteCountQuery)
		if err != nil {
			return fmt.Errorf("query Gantry origin byte metric count: %w", err)
		}

		if int(originByteCount) != b.config.NodeCount {
			return fmt.Errorf(
				"prometheus reports origin byte counters for %.0f/%d Gantry pods; direct mode requires gantry_origin_bytes_total",
				originByteCount,
				b.config.NodeCount,
			)
		}
	}

	// Direct mode has no proxy, so there are no proxy samples to wait for. The
	// Gantry scrape checks above already prove the benchmark PodMonitor is
	// being honoured by Prometheus.
	if !state.usesProxy() {
		return nil
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

func (b *benchmark) checkAzureTelemetry(ctx context.Context) error {
	if _, err := b.commands.Run(ctx, nil, "az", "account", "show", "--output", "none"); err != nil {
		return fmt.Errorf("validate Azure CLI authentication: %w", err)
	}

	registries := []struct {
		name     string
		registry phaseRegistry
	}{}

	if b.config.usesProxy() {
		registry, err := b.config.registryForPhase(proxyPhaseBaseline)
		if err != nil {
			return err
		}

		registries = append(registries, struct {
			name     string
			registry phaseRegistry
		}{name: "proxy", registry: registry})
	} else {
		for _, phase := range []proxyPhase{proxyPhaseBaseline, proxyPhaseGantryCold} {
			registry, err := b.config.registryForPhase(phase)
			if err != nil {
				return err
			}

			registries = append(registries, struct {
				name     string
				registry phaseRegistry
			}{name: string(phase), registry: registry})
		}
	}

	for _, candidate := range registries {
		if err := b.checkAzureRegistry(ctx, candidate.registry); err != nil {
			return fmt.Errorf("validate %s ACR telemetry: %w", candidate.name, err)
		}
	}

	probeWindow := telemetryWindow{StartedAt: time.Now().Add(-time.Hour), FinishedAt: time.Now()}
	if _, err := b.queryLogAnalytics(ctx, false, "ContainerRegistryRepositoryEvents | take 0", probeWindow); err != nil {
		return fmt.Errorf("query ACR repository table: %w", err)
	}

	if err := b.checkAKSAuditDiagnosticSetting(ctx); err != nil {
		return err
	}

	if err := b.checkAKSAuditIngestion(ctx); err != nil {
		return err
	}

	return nil
}

type azureDiagnosticSetting struct {
	LogAnalyticsDestinationType string `json:"logAnalyticsDestinationType"`
	WorkspaceID                 string `json:"workspaceId"`
	Logs                        []struct {
		Category string `json:"category"`
		Enabled  bool   `json:"enabled"`
	} `json:"logs"`
}

func (b *benchmark) checkAKSAuditDiagnosticSetting(ctx context.Context) error {
	output, err := b.commands.Run(
		ctx,
		nil,
		"az", "monitor", "diagnostic-settings", "list",
		"--resource", b.config.AKSResourceID,
		"--output", "json",
	)
	if err != nil {
		return fmt.Errorf("read AKS diagnostic settings: %w", err)
	}

	var settings []azureDiagnosticSetting
	if err := json.Unmarshal(output, &settings); err != nil {
		return fmt.Errorf("decode AKS diagnostic settings: %w", err)
	}

	for _, setting := range settings {
		if !strings.EqualFold(setting.LogAnalyticsDestinationType, "Dedicated") || setting.WorkspaceID == "" {
			continue
		}

		auditEnabled := false
		for _, log := range setting.Logs {
			if log.Enabled && strings.EqualFold(log.Category, "kube-audit-admin") {
				auditEnabled = true

				break
			}
		}
		if !auditEnabled {
			continue
		}

		customerID, err := b.commands.Run(
			ctx,
			nil,
			"az", "monitor", "log-analytics", "workspace", "show",
			"--ids", setting.WorkspaceID,
			"--query", "customerId",
			"--output", "tsv",
		)
		if err != nil {
			return fmt.Errorf("read AKS diagnostic workspace: %w", err)
		}

		if strings.EqualFold(strings.TrimSpace(string(customerID)), b.config.LogAnalyticsWorkspaceID) {
			return nil
		}
	}

	return fmt.Errorf(
		"AKS has no resource-specific kube-audit-admin diagnostic setting targeting Log Analytics workspace %s",
		b.config.LogAnalyticsWorkspaceID,
	)
}

func (b *benchmark) checkAKSAuditIngestion(ctx context.Context) error {
	probeName := fmt.Sprintf("gantry-audit-probe-%x", time.Now().UTC().UnixNano())
	probe := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      probeName,
			"namespace": b.config.Namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":    "gantry-benchmark-audit-probe",
				"app.kubernetes.io/part-of": "gantry-benchmark",
			},
		},
		"data": map[string]string{"created_at": time.Now().UTC().Format(time.RFC3339Nano)},
	}

	windowStart := time.Now().UTC().Add(-time.Minute)
	if err := b.applyObject(ctx, probe); err != nil {
		return fmt.Errorf("create AKS audit preflight probe: %w", err)
	}

	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), b.config.RolloutTimeout)
		defer cancel()

		if _, err := b.commands.Run(
			cleanupContext,
			nil,
			"kubectl", "-n", b.config.Namespace,
			"delete", "configmap/"+probeName,
			"--ignore-not-found=true", "--wait=false",
		); err != nil {
			writeAll(b.stderr, fmt.Sprintf("warning: delete AKS audit preflight probe: %v\n", err))
		}
	}()

	query := fmt.Sprintf(`AKSAuditAdmin
| where _ResourceId =~ %s
| extend ParsedObjectRef=parse_json(ObjectRef)
| extend Resource=tostring(ParsedObjectRef.resource), Namespace=tostring(ParsedObjectRef.namespace), Name=tostring(ParsedObjectRef.name)
| where Verb == "create" and Resource == "configmaps" and Namespace == %s and Name == %s
| project RequestReceivedTime, Name`,
		kustoQuote(b.config.AKSResourceID),
		kustoQuote(b.config.Namespace),
		kustoQuote(probeName),
	)

	pollContext, cancel := context.WithTimeout(ctx, b.config.TelemetryTimeout)
	defer cancel()

	var lastErr error

	for {
		rows, err := b.queryLogAnalytics(
			pollContext,
			true,
			query,
			telemetryWindow{StartedAt: windowStart, FinishedAt: time.Now().UTC()},
		)
		if err == nil && len(rows) > 0 {
			writeAll(b.stdout, fmt.Sprintf("AKS audit preflight probe %s reached Log Analytics\n", probeName))

			return nil
		}
		lastErr = err

		select {
		case <-pollContext.Done():
			if lastErr != nil {
				return fmt.Errorf("AKS audit preflight probe %s was not queryable before %s: %w", probeName, b.config.TelemetryTimeout, lastErr)
			}

			return fmt.Errorf(
				"AKS audit preflight probe %s did not reach AKSAuditAdmin before %s",
				probeName,
				b.config.TelemetryTimeout,
			)
		case <-time.After(b.config.TelemetryPollInterval):
		}
	}
}

func (b *benchmark) checkAzureRegistry(ctx context.Context, registry phaseRegistry) error {
	acrName, found := strings.CutSuffix(strings.ToLower(registry.LoginServer), ".azurecr.io")
	if !found || acrName == "" {
		return fmt.Errorf("ACR login server %q must end in .azurecr.io", registry.LoginServer)
	}

	acrOutput, err := b.commands.Run(
		ctx,
		nil,
		"az", "acr", "show",
		"--name", acrName,
		"--output", "json",
	)
	if err != nil {
		return fmt.Errorf("read ACR resource: %w", err)
	}

	var acr struct {
		ID                  string `json:"id"`
		LoginServer         string `json:"loginServer"`
		PublicNetworkAccess string `json:"publicNetworkAccess"`
	}
	if err := json.Unmarshal(acrOutput, &acr); err != nil {
		return fmt.Errorf("decode ACR resource: %w", err)
	}

	if !strings.EqualFold(acr.ID, registry.ResourceID) || !strings.EqualFold(acr.LoginServer, registry.LoginServer) {
		return fmt.Errorf(
			"ACR telemetry resource mismatch: id=%q loginServer=%q",
			acr.ID,
			acr.LoginServer,
		)
	}

	if !strings.EqualFold(acr.PublicNetworkAccess, "Disabled") {
		return fmt.Errorf(
			"ACR public network access is %q, want Disabled so all measured pulls cross the private endpoint",
			acr.PublicNetworkAccess,
		)
	}

	privateEndpointOutput, err := b.commands.Run(
		ctx,
		nil,
		"az", "network", "private-endpoint", "show",
		"--ids", registry.PrivateEndpointID,
		"--output", "json",
	)
	if err != nil {
		return fmt.Errorf("read ACR private endpoint: %w", err)
	}

	var privateEndpoint struct {
		ProvisioningState string `json:"provisioningState"`
		Connections       []struct {
			PrivateLinkServiceID string `json:"privateLinkServiceId"`
			State                struct {
				Status string `json:"status"`
			} `json:"privateLinkServiceConnectionState"`
		} `json:"privateLinkServiceConnections"`
	}
	if err := json.Unmarshal(privateEndpointOutput, &privateEndpoint); err != nil {
		return fmt.Errorf("decode ACR private endpoint: %w", err)
	}

	approved := false

	for _, connection := range privateEndpoint.Connections {
		if strings.EqualFold(connection.PrivateLinkServiceID, registry.ResourceID) &&
			strings.EqualFold(connection.State.Status, "Approved") {
			approved = true

			break
		}
	}

	if !strings.EqualFold(privateEndpoint.ProvisioningState, "Succeeded") || !approved {
		return fmt.Errorf(
			"ACR private endpoint is not ready: provisioningState=%q approvedACRConnection=%t",
			privateEndpoint.ProvisioningState,
			approved,
		)
	}

	metricDefinitions, err := b.commands.Run(
		ctx,
		nil,
		"az", "monitor", "metrics", "list-definitions",
		"--resource", registry.PrivateEndpointID,
		"--output", "json",
	)
	if err != nil {
		return fmt.Errorf("read private endpoint metric definitions: %w", err)
	}

	var definitions []struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
	}
	if err := json.Unmarshal(metricDefinitions, &definitions); err != nil {
		return fmt.Errorf("decode private endpoint metric definitions: %w", err)
	}

	hasPEBytesIn := false

	for _, definition := range definitions {
		if definition.Name.Value == "PEBytesIn" {
			hasPEBytesIn = true

			break
		}
	}

	if !hasPEBytesIn {
		return fmt.Errorf("private endpoint does not expose PEBytesIn")
	}

	return nil
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
