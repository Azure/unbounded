// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command racer-ctrl is the node half of the racer control plane.
//
// racer is a dataplane and nothing else: it exports ublk devices, replicates
// page registers through fixed groups, and reads and writes peers through local
// device paths. It has no discovery, no membership protocol, no knowledge of
// NVMe-oF and no knowledge of Kubernetes. Everything it does is a function of
// one config file it is handed and one metrics endpoint it exposes.
//
// racer-ctrl is what hands it that file. It runs as a sidecar in the same pod,
// reads the desired state the unbounded-operator publishes as annotations on
// Nodes, StorageClasses and PersistentVolumes, renders the whole NodeConfig for
// its node, and installs it by rename(2) into the directory racer watches. It
// also manages the NVMe-oF fabric racer refuses to know about, republishes
// racer's metrics as annotations so the operator's sequencers can act on them,
// and serves the CSI Node service so a pod can consume a racer volume.
//
// It takes no flags. Everything is an environment variable, so the DaemonSet
// manifest is the single place any of it is set.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/racerctrl/csi"
	"github.com/Azure/unbounded/internal/racerctrl/node"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	root := &cobra.Command{
		Use:   "racer-ctrl",
		Short: "Node control plane for the racer distributed block device",
		Long: "racer-ctrl renders the racer dataplane's configuration for this node, manages its " +
			"NVMe-oF fabric, republishes its metrics, and serves the CSI node service.",
		SilenceUsage:      true,
		PersistentPreRunE: setupLogging,
	}

	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(preflightCmd(), runCmd(), version.Command())

	if err := root.Execute(); err != nil {
		slog.Error("racer-ctrl failed", "error", err)
		os.Exit(1)
	}
}

// setupLogging installs a JSON handler as the process default. Package code
// logs through the package-level slog functions, so this is the only place the
// handler is chosen.
func setupLogging(*cobra.Command, []string) error {
	level := slog.LevelInfo
	if os.Getenv("RACER_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	return nil
}

// preflightCmd checks the host prerequisites racer needs.
//
// It is a separate subcommand so it can run as an init container: a node that
// cannot satisfy them should say so once, loudly, before racer starts, rather
// than have racer crash-loop with a less legible error at the moment a pod
// tries to use a volume.
func preflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Verify this host can run racer",
		RunE: func(*cobra.Command, []string) error {
			cfg, err := node.LoadConfig()
			if err != nil {
				return err
			}

			if err := node.Preflight(cfg); err != nil {
				return err
			}

			slog.Info("host prerequisites satisfied",
				"store", cfg.StorePath, "fabric", cfg.FabricEnabled())

			return nil
		},
	}
}

// runCmd runs the agent and the CSI node service until the process is asked to
// stop.
func runCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Render racer's config and serve the CSI node service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := node.LoadConfig()
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			return run(ctx, cfg)
		},
	}
}

func run(ctx context.Context, cfg node.Config) error {
	log := slog.Default()

	if !cfg.SkipPreflight {
		if err := node.Preflight(cfg); err != nil {
			return err
		}
	}

	client, err := node.NewClient(cfg.Kubeconfig)
	if err != nil {
		return err
	}

	agent := node.NewAgent(cfg, client, log)
	driver := csi.NewDriver(cfg.NodeName, agent, log)

	// The two halves share a context and a failure: if either stops, the whole
	// process stops. A CSI server without an agent behind it would accept
	// stage calls it can never satisfy, and an agent without a CSI server
	// would render configs for volumes no pod can reach.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 2)

	go func() {
		errs <- describe("agent", agent.Run(ctx))
	}()

	go func() {
		errs <- describe("csi", csi.Serve(ctx, cfg.CSIEndpoint, driver, log))
	}()

	first := <-errs

	cancel()

	// Drain the second so a shutdown that races does not leave a goroutine
	// blocked on an unread channel.
	<-errs

	return first
}

func describe(what string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}

	return fmt.Errorf("%s: %w", what, err)
}
