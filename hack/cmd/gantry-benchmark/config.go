// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type benchmarkConfig struct {
	RepoRoot             string
	Namespace            string
	GantryNamespace      string
	GantryDaemonSet      string
	GantryConfigMap      string
	MonitoringNamespace  string
	PrometheusService    string
	KPSRelease           string
	ACRLoginServer       string
	ACRUsername          string
	ACRPassword          string
	WorkloadRepository   string
	ImagePlatform        string
	ContainerEngine      string
	ConfirmedContext     string
	NodeCount            int
	ImageSizeMiB         int
	ImageLayers          int
	JobTimeout           time.Duration
	RolloutTimeout       time.Duration
	MetricsSettleTime    time.Duration
	MinimumByteReduction float64
	MaximumLatencyRatio  float64
	StateRoot            string
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

	jobTimeout, err := envDuration(getenv, "BENCHMARK_JOB_TIMEOUT", 90*time.Minute)
	if err != nil {
		return benchmarkConfig{}, err
	}

	rolloutTimeout, err := envDuration(getenv, "BENCHMARK_ROLLOUT_TIMEOUT", 20*time.Minute)
	if err != nil {
		return benchmarkConfig{}, err
	}

	metricsSettleTime, err := envDuration(getenv, "BENCHMARK_METRICS_SETTLE_TIME", 2*time.Minute)
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

	config := benchmarkConfig{
		RepoRoot:             repoRoot,
		Namespace:            envDefault(getenv, "BENCHMARK_NAMESPACE", "gantry-benchmark"),
		GantryNamespace:      envDefault(getenv, "GANTRY_NAMESPACE", "gantry-system"),
		GantryDaemonSet:      envDefault(getenv, "GANTRY_DAEMONSET", "gantry"),
		GantryConfigMap:      envDefault(getenv, "GANTRY_CONFIGMAP", "gantry-config"),
		MonitoringNamespace:  envDefault(getenv, "MONITORING_NAMESPACE", "monitoring"),
		PrometheusService:    envDefault(getenv, "PROMETHEUS_SERVICE", "kps-kube-prometheus-stack-prometheus"),
		KPSRelease:           envDefault(getenv, "KPS_RELEASE", "kps"),
		ACRLoginServer:       getenv("ACR_LOGIN_SERVER"),
		ACRUsername:          getenv("ACR_USERNAME"),
		ACRPassword:          getenv("ACR_PASSWORD"),
		WorkloadRepository:   envDefault(getenv, "BENCHMARK_WORKLOAD_REPOSITORY", "gantry-benchmark-pull"),
		ImagePlatform:        envDefault(getenv, "BENCHMARK_IMAGE_PLATFORM", "linux/amd64"),
		ContainerEngine:      envDefault(getenv, "CONTAINER_ENGINE", "podman"),
		ConfirmedContext:     getenv("BENCHMARK_CONFIRM_CONTEXT"),
		NodeCount:            nodeCount,
		ImageSizeMiB:         imageSizeMiB,
		ImageLayers:          imageLayers,
		JobTimeout:           jobTimeout,
		RolloutTimeout:       rolloutTimeout,
		MetricsSettleTime:    metricsSettleTime,
		MinimumByteReduction: minimumByteReduction,
		MaximumLatencyRatio:  maximumLatencyRatio,
		StateRoot:            filepath.Join(repoRoot, "tmp", "gantry-benchmark"),
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

	if config.MetricsSettleTime < 0 {
		return benchmarkConfig{}, errors.New("benchmark metrics settle time cannot be negative")
	}

	return config, nil
}

func (c benchmarkConfig) validateEnable() error {
	var missing []string

	for name, value := range map[string]string{
		"ACR_LOGIN_SERVER": c.ACRLoginServer,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}

	if len(missing) != 0 {
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
