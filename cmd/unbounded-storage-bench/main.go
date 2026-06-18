// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s *stringList) Set(v string) error {
	*s = append(*s, v)

	return nil
}

type options struct {
	nodeSpecs      stringList
	sshOptions     stringList
	sshUser        string
	sshKey         string
	scenarios      string
	remoteBinary   string
	remoteConfig   string
	remoteWorkdir  string
	duration       time.Duration
	warmup         time.Duration
	interval       time.Duration
	metricsTimeout time.Duration
	workers        uint
	objectSize     uint64
	objectCount    uint64
	readBytes      uint64
	stripeSize     uint64
	diskSize       uint64
	memoryBytes    uint64
	servingCores   uint64
	fabricProvider string
	sudo           bool
	noLaunch       bool
	failMissing    bool
	printConfig    bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "unbounded-storage-bench: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var opts options

	flag.Var(&opts.nodeSpecs, "node", "Node spec: id=1,ssh=host,fabric=10.0.0.1:7001,metrics=127.0.0.1:9100[,listen=0.0.0.0:7001][,config=/path][,workdir=/path][,disk=/path][,block=/dev/nvme0n1]")
	flag.Var(&opts.sshOptions, "ssh-option", "Additional ssh -o option. May be repeated.")
	flag.StringVar(&opts.sshUser, "ssh-user", "", "SSH user to prepend when node ssh target omits user@")
	flag.StringVar(&opts.sshKey, "ssh-key", "", "SSH private key path")
	flag.StringVar(&opts.scenarios, "scenario", "block-disk,fabric-rpc,integrated", "Comma-separated scenarios: block-disk,fabric-rpc,integrated")
	flag.StringVar(&opts.remoteBinary, "remote-binary", "unbounded-storage", "Remote unbounded-storage binary path")
	flag.StringVar(&opts.remoteConfig, "remote-config", "", "Remote config path default; per-node config overrides it")
	flag.StringVar(&opts.remoteWorkdir, "remote-workdir", "/tmp", "Remote directory for default config and file-backed disks")
	flag.DurationVar(&opts.duration, "duration", time.Minute, "Measurement duration per scenario")
	flag.DurationVar(&opts.warmup, "warmup", 20*time.Second, "Warmup duration before measurement")
	flag.DurationVar(&opts.interval, "interval", 5*time.Second, "Console reporting interval")
	flag.DurationVar(&opts.metricsTimeout, "metrics-timeout", time.Minute, "Timeout waiting for metrics endpoints")
	flag.UintVar(&opts.workers, "workers", 1, "Loadgen workers per serving shard")
	flag.Uint64Var(&opts.objectSize, "object-size", 64*1024*1024, "Fake backend object size in bytes")
	flag.Uint64Var(&opts.objectCount, "object-count", 1000000, "Loadgen distinct object count")
	flag.Uint64Var(&opts.readBytes, "read-bytes", 4*1024*1024, "Bytes read per loadgen request; 0 reads whole object")
	flag.Uint64Var(&opts.stripeSize, "stripe-size", 4*1024*1024, "Backend stripe size in bytes")
	flag.Uint64Var(&opts.diskSize, "disk-size", 2*1024*1024*1024, "File-backed benchmark disk size in bytes")
	flag.Uint64Var(&opts.memoryBytes, "memory-bytes", 128*1024*1024, "Daemon memory_total_bytes")
	flag.Uint64Var(&opts.servingCores, "serving-cores", 2, "Daemon serving_cores cap")
	flag.StringVar(&opts.fabricProvider, "fabric-provider", "tcp", "Peer fabric provider: tcp or rdma")
	flag.BoolVar(&opts.sudo, "sudo", true, "Use sudo for remote config writes and process launch")
	flag.BoolVar(&opts.noLaunch, "no-launch", false, "Write configs and scrape metrics, but do not launch remote daemons")
	flag.BoolVar(&opts.failMissing, "fail-on-missing-path", true, "Exit non-zero when scenario path counters do not advance")
	flag.BoolVar(&opts.printConfig, "print-config", false, "Print generated configs instead of writing or running")

	flag.Parse()

	nodes, err := parseNodeSpecs(opts)
	if err != nil {
		return err
	}

	plan, err := parseScenarios(opts.scenarios)
	if err != nil {
		return err
	}

	if opts.fabricProvider != "tcp" && opts.fabricProvider != "rdma" {
		return fmt.Errorf("--fabric-provider must be tcp or rdma, got %q", opts.fabricProvider)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := sshRunner{
		keyPath: opts.sshKey,
		options: opts.sshOptions,
	}

	client := &http.Client{Timeout: 3 * time.Second}

	for _, scenario := range plan {
		if err := runScenario(ctx, opts, runner, client, scenario, nodes); err != nil {
			return err
		}
	}

	return nil
}

func runScenario(ctx context.Context, opts options, runner sshRunner, client *http.Client, scenario scenarioKind, allNodes []nodeSpec) error {
	nodes, err := selectScenarioNodes(scenario, allNodes)
	if err != nil {
		return err
	}

	rendered := make([]renderedNodeConfig, 0, len(nodes))
	for _, node := range nodes {
		cfg, err := renderConfig(scenarioConfig{
			Scenario:       scenario,
			Node:           node,
			Nodes:          nodes,
			Workers:        opts.workers,
			ObjectSize:     opts.objectSize,
			ObjectCount:    opts.objectCount,
			ReadBytes:      opts.readBytes,
			StripeSize:     opts.stripeSize,
			DiskSize:       opts.diskSize,
			MemoryBytes:    opts.memoryBytes,
			ServingCores:   opts.servingCores,
			FabricProvider: opts.fabricProvider,
		})
		if err != nil {
			return err
		}

		rendered = append(rendered, renderedNodeConfig{node: node, config: cfg})
	}

	if opts.printConfig {
		for _, cfg := range rendered {
			fmt.Printf("# scenario=%s node=%d config=%s\n%s\n", scenario, cfg.node.ID, cfg.node.ConfigPath, cfg.config)
		}

		return nil
	}

	for _, cfg := range rendered {
		if err := runner.writeFile(ctx, cfg.node.SSHTarget, cfg.node.ConfigPath, cfg.config, opts.sudo); err != nil {
			return fmt.Errorf("write config for node %d: %w", cfg.node.ID, err)
		}
	}

	forwards := make([]*sshProcess, 0, len(nodes))
	processes := make([]*sshProcess, 0, len(nodes))
	defer func() { stopProcesses(forwards) }()
	defer func() { stopProcesses(processes) }()

	for _, node := range nodes {
		forward, err := runner.startForward(ctx, node.SSHTarget, node.MetricsAddr)
		if err != nil {
			return fmt.Errorf("start metrics forward for node %d: %w", node.ID, err)
		}

		node.ForwardURL = forward.url
		forwards = append(forwards, forward.proc)
		updateNode(nodes, node)
	}

	if !opts.noLaunch {
		for _, node := range nodes {
			proc, err := runner.startDaemon(ctx, node.SSHTarget, opts.remoteBinary, node.ConfigPath, opts.sudo)
			if err != nil {
				return fmt.Errorf("launch node %d: %w", node.ID, err)
			}

			processes = append(processes, proc)
		}
	}

	if err := waitForMetrics(ctx, client, nodes, opts.metricsTimeout); err != nil {
		return err
	}

	result, err := scrapeLoop(ctx, client, scenario, nodes, opts.warmup, opts.duration, opts.interval)
	if err != nil {
		return err
	}

	if opts.failMissing {
		if err := validateScenarioResult(scenario, result); err != nil {
			return err
		}
	}

	printSummary(scenario, result)

	return nil
}

func updateNode(nodes []nodeSpec, updated nodeSpec) {
	for i := range nodes {
		if nodes[i].ID == updated.ID {
			nodes[i] = updated

			return
		}
	}
}

func stopProcesses(processes []*sshProcess) {
	for i := len(processes) - 1; i >= 0; i-- {
		processes[i].stop()
	}
}

func parseScenarios(s string) ([]scenarioKind, error) {
	parts := strings.Split(s, ",")
	out := make([]scenarioKind, 0, len(parts))
	seen := map[scenarioKind]bool{}

	for _, part := range parts {
		scenario := scenarioKind(strings.TrimSpace(part))
		if scenario == "" {
			continue
		}

		switch scenario {
		case scenarioBlockDisk, scenarioFabricRPC, scenarioIntegrated:
		default:
			return nil, fmt.Errorf("unknown scenario %q", scenario)
		}

		if !seen[scenario] {
			out = append(out, scenario)
			seen[scenario] = true
		}
	}

	if len(out) == 0 {
		return nil, errors.New("at least one scenario is required")
	}

	return out, nil
}

func selectScenarioNodes(scenario scenarioKind, nodes []nodeSpec) ([]nodeSpec, error) {
	if len(nodes) == 0 {
		return nil, errors.New("at least one --node is required")
	}

	if scenario == scenarioBlockDisk {
		return append([]nodeSpec(nil), nodes[0]), nil
	}

	if len(nodes) < 2 {
		return nil, fmt.Errorf("scenario %s requires at least two nodes", scenario)
	}

	return append([]nodeSpec(nil), nodes...), nil
}
