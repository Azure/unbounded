// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

	stdout := &timestampWriter{target: os.Stdout}
	stderr := &timestampWriter{target: os.Stderr}

	defer func() {
		stdout.Flush()
		stderr.Flush()
	}()

	if err := runCLI(ctx, os.Args[1:], stdout, stderr); err != nil {
		writeAll(stderr, err.Error()+"\n")
		stdout.Flush()
		stderr.Flush()
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
	case "image-pool-status":
		return benchmark.printImagePoolStatus()
	case "prebuild-gantry":
		if len(args) != 2 {
			return fmt.Errorf("usage: gantry-benchmark prebuild-gantry <count>")
		}

		count, err := parsePrebuildCount(args[1])
		if err != nil {
			return err
		}

		return benchmark.prebuildGantryImages(ctx, count)
	case "disable":
		return benchmark.disable(ctx)
	case "enable":
		return benchmark.enable(ctx)
	case "prepare":
		return benchmark.prepareImages(ctx)
	case "prepare-adopt":
		if len(args) != 4 {
			return fmt.Errorf("usage: gantry-benchmark prepare-adopt <baseline-image> <gantry-image> <payload-sha256>")
		}

		return benchmark.prepareAdoptedImages(ctx, args[1], args[2], args[3])
	case "prepare-gantry":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: gantry-benchmark prepare-gantry <baseline-run-id> [prepared-run-id]")
		}

		preparedRunID := ""
		if len(args) == 3 {
			preparedRunID = args[2]
		}

		return benchmark.prepareGantryOnly(ctx, args[1], preparedRunID)
	case "prepare-gantry-fresh":
		if len(args) != 2 {
			return fmt.Errorf("usage: gantry-benchmark prepare-gantry-fresh <baseline-run-id>")
		}

		return benchmark.prepareFreshGantryOnly(ctx, args[1])
	case "prepare-gantry-adopt":
		if len(args) != 4 {
			return fmt.Errorf("usage: gantry-benchmark prepare-gantry-adopt <baseline-run-id> <gantry-image> <payload-sha256>")
		}

		return benchmark.prepareAdoptedFreshGantryOnly(ctx, args[1], args[2], args[3])
	case "prepare-gantry-pool":
		if len(args) != 2 {
			return fmt.Errorf("usage: gantry-benchmark prepare-gantry-pool <baseline-run-id>")
		}

		return benchmark.prepareGantryOnlyFromPool(ctx, args[1])
	case "preflight":
		return benchmark.preflight(ctx)
	case "run":
		return benchmark.runBenchmark(ctx)
	case "run-gantry":
		return benchmark.runGantryOnly(ctx)
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
	image-pool-status
	           list ready and claimed prebuilt Gantry images
	prebuild-gantry <count>
	           build and push reusable Gantry images without enabling a benchmark
	disable    restore the cluster and remove benchmark instrumentation
	enable     install benchmark instrumentation after safety checks
	prepare    build and push both digest-pinned images before ACR goes private
	prepare-adopt <baseline-image> <gantry-image> <payload-sha256>
	           adopt already-pushed direct-mode images with one shared payload
	prepare-gantry <baseline-run-id> [prepared-run-id]
	           rebuild only the Gantry image, or reuse an already-prepared image
	prepare-gantry-fresh <baseline-run-id>
	           generate new random bytes and build only a fresh Gantry image
	prepare-gantry-adopt <baseline-run-id> <gantry-image> <payload-sha256>
	           adopt an already-pushed fresh Gantry image by immutable digest
	prepare-gantry-pool <baseline-run-id>
	           atomically claim and adopt one compatible prebuilt Gantry image
	preflight  validate Azure sources, monitoring, Gantry, and all target nodes
	run        execute baseline and Gantry cold phases, then restore routing
	run-gantry execute only Gantry cold against the retained baseline
	status     print the active benchmark state
	help       print this help
`)
}
