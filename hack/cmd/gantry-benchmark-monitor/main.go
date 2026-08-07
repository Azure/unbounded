// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	defaultBenchmarkNamespace = "gantry-benchmark"
	defaultGantryNamespace    = "gantry-system"
	defaultMonitoringNS       = "monitoring"
	defaultPrometheusService  = "kps-kube-prometheus-stack-prometheus"
	stateConfigMapName        = "gantry-benchmark-state"
	gantryPhaseLabel          = "gantry-cold"
	gridQueryInterval         = 10 * time.Second
)

type monitorConfig struct {
	kubectl             string
	kubeconfig          string
	benchmarkNamespace  string
	gantryNamespace     string
	monitoringNamespace string
	prometheusService   string
	runID               string
	refreshInterval     time.Duration
	nodePage            int
	nodesPerPage        int
	once                bool
	noClear             bool
}

type kubectlRunner struct {
	binary     string
	kubeconfig string
}

func (r kubectlRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	if r.kubeconfig != "" {
		args = append([]string{"--kubeconfig", r.kubeconfig}, args...)
	}

	command := exec.CommandContext(ctx, r.binary, args...)

	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}

	return output, nil
}

type benchmarkState struct {
	RunID               string `json:"run_id"`
	MonitoringNamespace string `json:"monitoring_namespace"`
	PrometheusService   string `json:"prometheus_service"`
	NodeCount           int    `json:"node_count"`
	ImageSizeMiB        int    `json:"image_size_mib"`
}

type monitorSession struct {
	runID               string
	jobName             string
	phaseStart          time.Time
	nodeCount           int
	expectedBytes       float64
	monitoringNamespace string
	prometheusService   string
	revision            string
	image               string
}

type jobStatus struct {
	Succeeded int
	Active    int
	Failed    int
	Complete  bool
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

func parseConfig(args []string) (monitorConfig, error) {
	config := monitorConfig{}
	flags := flag.NewFlagSet("gantry-benchmark-monitor", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: gantry-benchmark-monitor [options]")                           //nolint:errcheck // best-effort help output
		fmt.Fprintln(flags.Output(), "Live peer traffic, layer downloads, image unpacking, and Pod state.") //nolint:errcheck // best-effort help output
		flags.PrintDefaults()
	}

	flags.StringVar(&config.kubectl, "kubectl", envDefault("KUBECTL", "kubectl"), "kubectl executable")
	flags.StringVar(&config.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig path")
	flags.StringVar(&config.benchmarkNamespace, "namespace", envDefault("BENCHMARK_NAMESPACE", defaultBenchmarkNamespace), "benchmark namespace")
	flags.StringVar(&config.gantryNamespace, "gantry-namespace", envDefault("GANTRY_NAMESPACE", defaultGantryNamespace), "Gantry namespace")
	flags.StringVar(&config.monitoringNamespace, "monitoring-namespace", envDefault("MONITORING_NAMESPACE", defaultMonitoringNS), "monitoring namespace")
	flags.StringVar(&config.prometheusService, "prometheus-service", envDefault("PROMETHEUS_SERVICE", defaultPrometheusService), "Prometheus service")
	flags.StringVar(&config.runID, "run-id", "", "run ID (default: active benchmark state)")
	flags.DurationVar(&config.refreshInterval, "refresh", time.Second, "display and query refresh interval")
	flags.IntVar(&config.nodePage, "node-page", 1, "1-based node page shown in progress grids")
	flags.IntVar(&config.nodesPerPage, "nodes-per-page", defaultGridColumns(), "node columns shown per progress-grid page")
	flags.BoolVar(&config.once, "once", false, "print one snapshot and exit")
	flags.BoolVar(&config.noClear, "no-clear", false, "do not redraw the terminal")

	if err := flags.Parse(args); err != nil {
		return monitorConfig{}, err
	}

	if config.refreshInterval <= 0 {
		return monitorConfig{}, fmt.Errorf("refresh must be positive, got %s", config.refreshInterval)
	}

	if config.nodePage < 1 {
		return monitorConfig{}, fmt.Errorf("node-page must be at least 1, got %d", config.nodePage)
	}

	if config.nodesPerPage < 1 {
		return monitorConfig{}, fmt.Errorf("nodes-per-page must be at least 1, got %d", config.nodesPerPage)
	}

	return config, nil
}

func loadBenchmarkState(ctx context.Context, runner kubectlRunner, config monitorConfig) (benchmarkState, error) {
	output, err := runner.run(ctx, "-n", config.benchmarkNamespace, "get", "configmap", stateConfigMapName, "-o", "json")
	if err != nil {
		return benchmarkState{}, err
	}

	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(output, &configMap); err != nil {
		return benchmarkState{}, fmt.Errorf("decode benchmark state ConfigMap: %w", err)
	}

	raw := configMap.Data["state.json"]
	if raw == "" {
		return benchmarkState{}, errors.New("benchmark state ConfigMap has no state.json")
	}

	var state benchmarkState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return benchmarkState{}, fmt.Errorf("decode benchmark state: %w", err)
	}

	return state, nil
}

func loadGantryRevision(ctx context.Context, runner kubectlRunner, namespace string) (string, error) {
	output, err := runner.run(ctx, "-n", namespace, "get", "pods", "-l", "app.kubernetes.io/name=gantry", "-o", "json")
	if err != nil {
		return "", err
	}

	var pods struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &pods); err != nil {
		return "", fmt.Errorf("decode Gantry pods: %w", err)
	}

	for _, pod := range pods.Items {
		ready := false

		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}

		if !ready {
			continue
		}

		if revision := pod.Metadata.Labels["controller-revision-hash"]; revision != "" {
			return revision, nil
		}
	}

	return "", errors.New("no Ready Gantry pod has controller-revision-hash")
}

func loadJob(ctx context.Context, runner kubectlRunner, namespace, runID string) (monitorSession, jobStatus, error) {
	selector := "gantry.unbounded-cloud.io/run-id=" + runID + ",gantry.unbounded-cloud.io/phase=" + gantryPhaseLabel

	output, err := runner.run(ctx, "-n", namespace, "get", "jobs", "-l", selector, "-o", "json")
	if err != nil {
		return monitorSession{}, jobStatus{}, err
	}

	var jobs struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Completions int `json:"completions"`
				Template    struct {
					Spec struct {
						Containers []struct {
							Name  string `json:"name"`
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				StartTime  string `json:"startTime"`
				Succeeded  int    `json:"succeeded"`
				Active     int    `json:"active"`
				Failed     int    `json:"failed"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &jobs); err != nil {
		return monitorSession{}, jobStatus{}, fmt.Errorf("decode Gantry pull Job: %w", err)
	}

	if len(jobs.Items) == 0 {
		return monitorSession{}, jobStatus{}, errors.New("gantry-cold pull Job has not started")
	}

	job := jobs.Items[len(jobs.Items)-1]
	if job.Status.StartTime == "" {
		return monitorSession{}, jobStatus{}, errors.New("gantry-cold pull Job has no startTime")
	}

	startedAt, err := time.Parse(time.RFC3339, job.Status.StartTime)
	if err != nil {
		return monitorSession{}, jobStatus{}, fmt.Errorf("parse Job startTime %q: %w", job.Status.StartTime, err)
	}

	status := jobStatus{Succeeded: job.Status.Succeeded, Active: job.Status.Active, Failed: job.Status.Failed}
	for _, condition := range job.Status.Conditions {
		if condition.Type == "Complete" && condition.Status == "True" {
			status.Complete = true
		}
	}

	image := ""

	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name == "pull" {
			image = container.Image
			break
		}
	}

	return monitorSession{jobName: job.Metadata.Name, phaseStart: startedAt, nodeCount: job.Spec.Completions, image: image}, status, nil
}

func defaultGridColumns() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return defaultNodesPerPage
	}

	return min(96, max(16, width-14))
}

func discoverSession(ctx context.Context, runner kubectlRunner, config monitorConfig) (monitorSession, jobStatus, error) {
	state, err := loadBenchmarkState(ctx, runner, config)
	if err != nil {
		return monitorSession{}, jobStatus{}, err
	}

	runID := config.runID
	if runID == "" {
		runID = state.RunID
	}

	if runID == "" {
		return monitorSession{}, jobStatus{}, errors.New("active benchmark state has no run ID")
	}

	session, status, err := loadJob(ctx, runner, config.benchmarkNamespace, runID)
	if err != nil {
		return monitorSession{}, jobStatus{}, err
	}

	revision, err := loadGantryRevision(ctx, runner, config.gantryNamespace)
	if err != nil {
		return monitorSession{}, jobStatus{}, err
	}

	session.runID = runID
	if session.nodeCount == 0 {
		session.nodeCount = state.NodeCount
	}

	session.expectedBytes = float64(state.NodeCount) * float64(state.ImageSizeMiB) * 1024 * 1024

	session.monitoringNamespace = config.monitoringNamespace
	if state.MonitoringNamespace != "" {
		session.monitoringNamespace = state.MonitoringNamespace
	}

	session.prometheusService = config.prometheusService
	if state.PrometheusService != "" {
		session.prometheusService = state.PrometheusService
	}

	session.revision = revision

	return session, status, nil
}

func refreshJobStatus(ctx context.Context, runner kubectlRunner, config monitorConfig, session monitorSession) (jobStatus, error) {
	_, status, err := loadJob(ctx, runner, config.benchmarkNamespace, session.runID)
	return status, err
}

func prometheusExpression(session monitorSession, gantryNamespace string) string {
	labels := fmt.Sprintf(`namespace=%s,gantry_benchmark="true",controller_revision_hash=%s`, strconv.Quote(gantryNamespace), strconv.Quote(session.revision))
	peer := fmt.Sprintf(`sum by (outcome) (p2p_peer_fetch_total{%s})`, labels)
	bytes := fmt.Sprintf(`sum(gantry_mirror_bytes_served_total{%s,kind="layer"})`, labels)

	return peer + " or " + bytes
}

func queryPrometheusRange(ctx context.Context, runner kubectlRunner, session monitorSession, gantryNamespace string, now time.Time) (rangeResponse, error) {
	start := session.phaseStart.Add(-15 * time.Second)
	rawPath := fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:9090/proxy/api/v1/query_range?query=%s&start=%s&end=%s&step=1",
		session.monitoringNamespace,
		session.prometheusService,
		url.QueryEscape(prometheusExpression(session, gantryNamespace)),
		url.QueryEscape(start.UTC().Format(time.RFC3339Nano)),
		url.QueryEscape(now.UTC().Format(time.RFC3339Nano)),
	)

	output, err := runner.run(ctx, "get", "--raw", rawPath)
	if err != nil {
		return rangeResponse{}, err
	}

	return parseRangeResponse(output)
}

func progressExpression(session monitorSession, config monitorConfig) string {
	downloadLabels := fmt.Sprintf(
		`namespace=%s,gantry_benchmark="true",controller_revision_hash=%s`,
		strconv.Quote(config.gantryNamespace),
		strconv.Quote(session.revision),
	)

	observerLabels := fmt.Sprintf(`namespace=%s,gantry_benchmark="true"`, strconv.Quote(config.benchmarkNamespace))

	if digest := imageDigest(session.image); digest != "" {
		downloadLabels += `,image_digest=` + strconv.Quote(digest)
	}

	if session.image != "" {
		observerLabels += `,image=` + strconv.Quote(session.image)
	}

	return fmt.Sprintf(`gantry_layer_download_completed_timestamp_seconds{%s} or {__name__=~"gantry_benchmark_(image_unpack_started|image_unpacked|layer_unpacked)_timestamp_seconds",%s}`,
		downloadLabels,
		observerLabels,
	)
}

func queryProgress(ctx context.Context, runner kubectlRunner, session monitorSession, config monitorConfig, now time.Time) (instantResponse, error) {
	rawPath := fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:9090/proxy/api/v1/query?query=%s&time=%s",
		session.monitoringNamespace,
		session.prometheusService,
		url.QueryEscape(progressExpression(session, config)),
		url.QueryEscape(now.UTC().Format(time.RFC3339Nano)),
	)

	output, err := runner.run(ctx, "get", "--raw", rawPath)
	if err != nil {
		return instantResponse{}, err
	}

	return parseInstantResponse(output)
}

func renderWaiting(config monitorConfig, err error) {
	if !config.noClear && term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Print("\033[H\033[2J")
	}

	fmt.Printf("Gantry benchmark live monitor\n\ntime: %s\nstatus: waiting\nreason: %v\n", time.Now().UTC().Format(time.RFC3339), err)
}

func runMonitor(ctx context.Context, config monitorConfig) error {
	runner := kubectlRunner{binary: config.kubectl, kubeconfig: config.kubeconfig}

	var (
		session      *monitorSession
		podTracker   *podStateTracker
		lastSnapshot monitorSnapshot
		lastProgress progressGrid
		gridError    string
		nextGridAt   time.Time
	)

	for {
		now := time.Now().UTC()

		var status jobStatus

		if session == nil {
			discovered, job, err := discoverSession(ctx, runner, config)
			if err != nil {
				renderWaiting(config, err)

				if config.once {
					return err
				}

				if err := waitForNext(ctx, config.refreshInterval); err != nil {
					return err
				}

				continue
			}

			session = &discovered
			status = job

			tracker, err := newPodStateTracker(ctx, config.kubeconfig, config.benchmarkNamespace, session.jobName)
			if err != nil {
				session = nil

				renderWaiting(config, err)

				if config.once {
					return err
				}

				if err := waitForNext(ctx, config.refreshInterval); err != nil {
					return err
				}

				continue
			}

			podTracker = tracker
		} else {
			job, err := refreshJobStatus(ctx, runner, config, *session)
			if err != nil {
				renderWaiting(config, err)

				if err := waitForNext(ctx, config.refreshInterval); err != nil {
					return err
				}

				continue
			}

			status = job
		}

		queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		response, err := queryPrometheusRange(queryCtx, runner, *session, config.gantryNamespace, now)

		cancel()

		if err != nil {
			renderWaiting(config, err)

			if config.once {
				return err
			}

			if err := waitForNext(ctx, config.refreshInterval); err != nil {
				return err
			}

			continue
		}

		lastSnapshot = aggregateRange(response, session.phaseStart, now, session.expectedBytes, session.nodeCount)
		lastSnapshot.RunID = session.runID
		lastSnapshot.JobName = session.jobName
		lastSnapshot.PhaseStart = session.phaseStart
		lastSnapshot.Now = now
		lastSnapshot.RefreshInterval = config.refreshInterval
		lastSnapshot.Job = status
		lastSnapshot.Color = term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""
		lastSnapshot.NodePage = config.nodePage
		lastSnapshot.NodesPerPage = config.nodesPerPage

		var nodes []string

		if podTracker != nil {
			counts, podErr := podTracker.snapshot()
			nodes = podTracker.snapshotNodes()

			lastSnapshot.PodStates = counts
			if podErr != nil {
				lastSnapshot.PodStateError = podErr.Error()
			}
		}

		if status.Complete || !now.Before(nextGridAt) {
			gridCtx, gridCancel := context.WithTimeout(ctx, 15*time.Second)
			gridResponse, gridErr := queryProgress(gridCtx, runner, *session, config, now)

			gridCancel()

			if gridErr != nil {
				gridError = gridErr.Error()
			} else {
				lastProgress = aggregateProgressGrid(gridResponse, nodes, session.image)
				gridError = ""
			}

			nextGridAt = now.Add(gridQueryInterval)
		} else if len(nodes) > 0 {
			lastProgress.Nodes = append(lastProgress.Nodes[:0], nodes...)
		}

		lastSnapshot.Progress = lastProgress
		lastSnapshot.GridError = gridError

		if !config.noClear && term.IsTerminal(int(os.Stdout.Fd())) {
			fmt.Print("\033[H\033[2J")
		}

		fmt.Print(renderSnapshot(lastSnapshot))

		if config.once || status.Complete {
			return nil
		}

		if err := waitForNext(ctx, config.refreshInterval); err != nil {
			return err
		}
	}
}

func waitForNext(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func main() {
	config, err := parseConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}

		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	isTTY := term.IsTerminal(int(os.Stdout.Fd())) && !config.noClear
	if isTTY {
		fmt.Print("\033[?1049h")
		defer fmt.Print("\033[?1049l")
	}

	if err := runMonitor(ctx, config); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
