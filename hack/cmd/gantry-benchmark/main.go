// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

type benchmark struct {
	config   benchmarkConfig
	commands commandRunner
	stdout   io.Writer
	stderr   io.Writer
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printUsage(stdout)

		return nil
	}

	config, err := loadBenchmarkConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("configure benchmark: %w", err)
	}

	benchmark := &benchmark{
		config:   config,
		commands: execCommandRunner{directory: config.RepoRoot},
		stdout:   stdout,
		stderr:   stderr,
	}

	switch args[0] {
	case "disable":
		return benchmark.disable(ctx)
	case "enable":
		return benchmark.enable(ctx)
	case "preflight":
		return benchmark.preflight(ctx)
	case "run":
		return benchmark.runBenchmark(ctx)
	case "status":
		return benchmark.status(ctx)
	default:
		printUsage(stderr)

		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func printUsage(writer io.Writer) {
	writeAll(writer, `Usage: gantry-benchmark <subcommand>

Subcommands:
	disable    restore the cluster and remove benchmark instrumentation
	enable     install benchmark instrumentation after safety checks
	preflight  validate proxy, ACR, monitoring, Gantry, and all 300 nodes
	run        execute baseline and Gantry cold phases, then restore routing
	status     print the active benchmark state
	help       print this help
`)
}
