// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// mibibyte is the payload multiplier used by the image builder.
const mibibyte = 1024 * 1024

type benchmarkMode string

const (
	// benchmarkModeProxy routes both phases through the counting proxy, which is
	// the measured ACR origin. It is the default so existing runs are unchanged.
	benchmarkModeProxy benchmarkMode = "proxy"
	// benchmarkModeDirect removes the counting proxy entirely. Baseline pulls go
	// straight to ACR and Gantry-cold pulls use the peer distribution path.
	benchmarkModeDirect benchmarkMode = "direct"
)

type benchmarkConfig struct {
	RepoRoot                     string
	Mode                         benchmarkMode
	Namespace                    string
	GantryNamespace              string
	GantryDaemonSet              string
	GantryConfigMap              string
	MonitoringNamespace          string
	PrometheusService            string
	KPSRelease                   string
	BaselineACRLoginServer       string
	BaselineACRUsername          string
	BaselineACRPassword          string
	GantryACRLoginServer         string
	GantryACRUsername            string
	GantryACRPassword            string
	ACRLoginServer               string
	ACRUsername                  string
	ACRPassword                  string
	ProxyImage                   string
	WorkloadRepository           string
	ImagePlatform                string
	ContainerEngine              string
	ConfirmedContext             string
	NodeCount                    int
	ImageSizeMiB                 int
	ImageLayers                  int
	JobTimeout                   time.Duration
	RolloutTimeout               time.Duration
	MinimumByteReduction         float64
	MaximumLatencyRatio          float64
	AzureTelemetry               bool
	LogAnalyticsWorkspaceID      string
	BaselineACRResourceID        string
	BaselinePrivateEndpointID    string
	GantryACRResourceID          string
	GantryPrivateEndpointID      string
	ACRResourceID                string
	AKSResourceID                string
	ACRPrivateEndpointResourceID string
	TelemetryTimeout             time.Duration
	TelemetryPollInterval        time.Duration
	JobProgressInterval          time.Duration
	StateRoot                    string
	ImagePoolRoot                string
	ImagePoolBuildRoot           string
}

type phaseRegistry struct {
	LoginServer       string
	ResourceID        string
	PrivateEndpointID string
}

func loadBenchmarkConfig(getenv func(string) string) (benchmarkConfig, error) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return benchmarkConfig{}, err
	}

	nodeCount, err := envInt(getenv, "BENCHMARK_NODE_COUNT", 300)
	if err != nil {
		return benchmarkConfig{}, err
	}

	imageSizeMiB, err := envInt(getenv, "BENCHMARK_IMAGE_SIZE_MIB", 1024)
	if err != nil {
		return benchmarkConfig{}, err
	}

	imageLayers, err := envInt(getenv, "BENCHMARK_IMAGE_LAYERS", 1)
	if err != nil {
		return benchmarkConfig{}, err
	}

	rolloutTimeout, err := envDuration(getenv, "BENCHMARK_ROLLOUT_TIMEOUT", 20*time.Minute)
	if err != nil {
		return benchmarkConfig{}, err
	}

	minimumByteReduction, err := envFloat(getenv, "BENCHMARK_MINIMUM_BYTE_REDUCTION", 0.90)
	if err != nil {
		return benchmarkConfig{}, err
	}

	maximumLatencyRatio, err := envFloat(getenv, "BENCHMARK_MAXIMUM_LATENCY_RATIO", 1.0)
	if err != nil {
		return benchmarkConfig{}, err
	}

	azureTelemetry, err := envBool(getenv, "BENCHMARK_AZURE_TELEMETRY", false)
	if err != nil {
		return benchmarkConfig{}, err
	}

	telemetryTimeout, err := envDuration(getenv, "BENCHMARK_TELEMETRY_TIMEOUT", 15*time.Minute)
	if err != nil {
		return benchmarkConfig{}, err
	}

	telemetryPollInterval, err := envDuration(getenv, "BENCHMARK_TELEMETRY_POLL_INTERVAL", 15*time.Second)
	if err != nil {
		return benchmarkConfig{}, err
	}

	jobProgressInterval, err := envDuration(getenv, "BENCHMARK_JOB_PROGRESS_INTERVAL", 15*time.Second)
	if err != nil {
		return benchmarkConfig{}, err
	}

	mode := benchmarkMode(envDefault(getenv, "BENCHMARK_MODE", string(benchmarkModeProxy)))
	if mode != benchmarkModeProxy && mode != benchmarkModeDirect {
		return benchmarkConfig{}, fmt.Errorf(
			"BENCHMARK_MODE must be %q or %q, got %q", //nolint:staticcheck // Environment variable names are intentionally uppercase.
			benchmarkModeProxy,
			benchmarkModeDirect,
			mode,
		)
	}

	stateRoot := filepath.Join(repoRoot, "tmp", "gantry-benchmark")

	config := benchmarkConfig{
		RepoRoot:                     repoRoot,
		Mode:                         mode,
		Namespace:                    envDefault(getenv, "BENCHMARK_NAMESPACE", "gantry-benchmark"),
		GantryNamespace:              envDefault(getenv, "GANTRY_NAMESPACE", "gantry-system"),
		GantryDaemonSet:              envDefault(getenv, "GANTRY_DAEMONSET", "gantry"),
		GantryConfigMap:              envDefault(getenv, "GANTRY_CONFIGMAP", "gantry-config"),
		MonitoringNamespace:          envDefault(getenv, "MONITORING_NAMESPACE", "monitoring"),
		PrometheusService:            envDefault(getenv, "PROMETHEUS_SERVICE", "kps-kube-prometheus-stack-prometheus"),
		KPSRelease:                   envDefault(getenv, "KPS_RELEASE", "kps"),
		BaselineACRLoginServer:       getenv("BASELINE_ACR_LOGIN_SERVER"),
		BaselineACRUsername:          getenv("BASELINE_ACR_USERNAME"),
		BaselineACRPassword:          getenv("BASELINE_ACR_PASSWORD"),
		GantryACRLoginServer:         getenv("GANTRY_ACR_LOGIN_SERVER"),
		GantryACRUsername:            getenv("GANTRY_ACR_USERNAME"),
		GantryACRPassword:            getenv("GANTRY_ACR_PASSWORD"),
		ACRLoginServer:               getenv("ACR_LOGIN_SERVER"),
		ACRUsername:                  getenv("ACR_USERNAME"),
		ACRPassword:                  getenv("ACR_PASSWORD"),
		ProxyImage:                   getenv("BENCHMARK_PROXY_IMAGE"),
		WorkloadRepository:           envDefault(getenv, "BENCHMARK_WORKLOAD_REPOSITORY", "gantry-benchmark-pull"),
		ImagePlatform:                envDefault(getenv, "BENCHMARK_IMAGE_PLATFORM", "linux/amd64"),
		ContainerEngine:              envDefault(getenv, "CONTAINER_ENGINE", "podman"),
		ConfirmedContext:             getenv("BENCHMARK_CONFIRM_CONTEXT"),
		NodeCount:                    nodeCount,
		ImageSizeMiB:                 imageSizeMiB,
		ImageLayers:                  imageLayers,
		JobTimeout:                   4 * time.Hour,
		RolloutTimeout:               rolloutTimeout,
		MinimumByteReduction:         minimumByteReduction,
		MaximumLatencyRatio:          maximumLatencyRatio,
		AzureTelemetry:               azureTelemetry,
		LogAnalyticsWorkspaceID:      getenv("AZURE_LOG_ANALYTICS_WORKSPACE_ID"),
		BaselineACRResourceID:        getenv("AZURE_BASELINE_ACR_RESOURCE_ID"),
		BaselinePrivateEndpointID:    getenv("AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID"),
		GantryACRResourceID:          getenv("AZURE_GANTRY_ACR_RESOURCE_ID"),
		GantryPrivateEndpointID:      getenv("AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID"),
		ACRResourceID:                getenv("AZURE_ACR_RESOURCE_ID"),
		AKSResourceID:                getenv("AZURE_AKS_RESOURCE_ID"),
		ACRPrivateEndpointResourceID: getenv("AZURE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID"),
		TelemetryTimeout:             telemetryTimeout,
		TelemetryPollInterval:        telemetryPollInterval,
		JobProgressInterval:          jobProgressInterval,
		StateRoot:                    stateRoot,
		ImagePoolRoot:                envDefault(getenv, "BENCHMARK_IMAGE_POOL_ROOT", filepath.Join(stateRoot, "image-pool")),
		ImagePoolBuildRoot:           envDefault(getenv, "BENCHMARK_IMAGE_POOL_BUILD_ROOT", filepath.Join(stateRoot, "image-pool-build")),
	}

	if config.NodeCount <= 0 {
		return benchmarkConfig{}, errors.New("benchmark node count must be greater than zero")
	}

	if config.ImageSizeMiB <= 0 {
		return benchmarkConfig{}, errors.New("benchmark image size must be greater than zero")
	}

	if config.ImageLayers <= 0 {
		return benchmarkConfig{}, errors.New("benchmark image layers must be greater than zero")
	}

	if config.ImageLayers > config.ImageSizeMiB {
		return benchmarkConfig{}, errors.New("benchmark image layers cannot exceed image size in MiB")
	}

	if config.MinimumByteReduction < 0 || config.MinimumByteReduction > 1 {
		return benchmarkConfig{}, errors.New("benchmark minimum byte reduction must be between 0 and 1")
	}

	if config.MaximumLatencyRatio <= 0 {
		return benchmarkConfig{}, errors.New("benchmark maximum latency ratio must be greater than zero")
	}

	if config.TelemetryTimeout <= 0 {
		return benchmarkConfig{}, errors.New("benchmark telemetry timeout must be greater than zero")
	}

	if config.TelemetryPollInterval <= 0 {
		return benchmarkConfig{}, errors.New("benchmark telemetry poll interval must be greater than zero")
	}

	return config, nil
}

func (c benchmarkConfig) usesProxy() bool {
	return c.Mode != benchmarkModeDirect
}

func (c benchmarkConfig) registryForPhase(phase proxyPhase) (phaseRegistry, error) {
	if c.usesProxy() {
		return phaseRegistry{
			LoginServer:       c.ACRLoginServer,
			ResourceID:        c.ACRResourceID,
			PrivateEndpointID: c.ACRPrivateEndpointResourceID,
		}, nil
	}

	switch phase {
	case proxyPhaseBaseline:
		return phaseRegistry{
			LoginServer:       c.BaselineACRLoginServer,
			ResourceID:        c.BaselineACRResourceID,
			PrivateEndpointID: c.BaselinePrivateEndpointID,
		}, nil
	case proxyPhaseGantryCold:
		return phaseRegistry{
			LoginServer:       c.GantryACRLoginServer,
			ResourceID:        c.GantryACRResourceID,
			PrivateEndpointID: c.GantryPrivateEndpointID,
		}, nil
	default:
		return phaseRegistry{}, fmt.Errorf("phase %q has no registry", phase)
	}
}

// imageBytes reports the total generated payload size of one benchmark image.
func (c benchmarkConfig) imageBytes() uint64 {
	return uint64(c.ImageSizeMiB) * mibibyte
}

func (c benchmarkConfig) validateEnable() error {
	required := make(map[string]string)

	// The proxy Secret carries the registry credentials and the proxy image is
	// the workload for both the proxy Deployment and the reachability DaemonSet.
	// Dual-ACR direct mode deploys neither and validates push credentials during
	// prepare, so enable stays usable without exporting either push token.
	if c.usesProxy() {
		required["ACR_LOGIN_SERVER"] = c.ACRLoginServer
		required["ACR_USERNAME"] = c.ACRUsername
		required["ACR_PASSWORD"] = c.ACRPassword
		required["BENCHMARK_PROXY_IMAGE"] = c.ProxyImage
	} else {
		required["BASELINE_ACR_LOGIN_SERVER"] = c.BaselineACRLoginServer
		required["GANTRY_ACR_LOGIN_SERVER"] = c.GantryACRLoginServer

		if c.BaselineACRLoginServer != "" && strings.EqualFold(c.BaselineACRLoginServer, c.GantryACRLoginServer) {
			return errors.New("baseline and Gantry ACR login servers must be different")
		}
	}

	if c.AzureTelemetry {
		required["AZURE_LOG_ANALYTICS_WORKSPACE_ID"] = c.LogAnalyticsWorkspaceID
		required["AZURE_AKS_RESOURCE_ID"] = c.AKSResourceID

		if c.usesProxy() {
			required["AZURE_ACR_RESOURCE_ID"] = c.ACRResourceID
			required["AZURE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID"] = c.ACRPrivateEndpointResourceID
		} else {
			required["AZURE_BASELINE_ACR_RESOURCE_ID"] = c.BaselineACRResourceID
			required["AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID"] = c.BaselinePrivateEndpointID
			required["AZURE_GANTRY_ACR_RESOURCE_ID"] = c.GantryACRResourceID
			required["AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID"] = c.GantryPrivateEndpointID
		}
	}

	var missing []string

	for name, value := range required {
		if value == "" {
			missing = append(missing, name)
		}
	}

	if len(missing) != 0 {
		sort.Strings(missing)

		return fmt.Errorf("required environment variables are missing: %v", missing)
	}

	return nil
}

func (c benchmarkConfig) nodeSelector() map[string]string {
	selector := map[string]string{"kubernetes.io/os": "linux"}

	parts := strings.SplitN(c.ImagePlatform, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		selector["kubernetes.io/os"] = parts[0]
		selector["kubernetes.io/arch"] = parts[1]
	}

	return selector
}

func envDefault(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}

	return fallback
}

func envInt(getenv func(string) string, name string, fallback int) (int, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}

	return parsed, nil
}

func envFloat(getenv func(string) string, name string, fallback float64) (float64, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}

	return parsed, nil
}

func envBool(getenv func(string) string, name string, fallback bool) (bool, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}

	return parsed, nil
}

func envDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}

	return parsed, nil
}

func findRepoRoot() (string, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for directory := workingDirectory; ; directory = filepath.Dir(directory) {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(directory, "deploy", "gantry")); err == nil {
				return directory, nil
			}
		}

		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("could not find the Unbounded repository root")
		}
	}
}
