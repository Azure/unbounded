// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/gantry/noderoute"
)

func runNodeConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("gantry node-config: command required (reconcile or check)")
	}

	options := noderoute.DefaultOptions()
	flags := flag.NewFlagSet("node-config "+args[0], flag.ContinueOnError)
	flags.StringVar(&options.DesiredPath, "desired", options.DesiredPath, "path to desired registry routes JSON")
	flags.StringVar(&options.HostCertsDir, "host-certs-dir", options.HostCertsDir, "mounted host containerd certs.d directory")
	flags.StringVar(&options.HostContainerdConfig, "host-containerd-config", options.HostContainerdConfig, "mounted host containerd config.toml")
	flags.StringVar(&options.HostStateDir, "host-state-dir", options.HostStateDir, "mounted host route ownership state directory")
	flags.StringVar(&options.ExpectedContainerdCerts, "expected-certs-dir", options.ExpectedContainerdCerts, "containerd registry config_path expected on the host")

	interval := time.Minute
	if args[0] == "reconcile" {
		flags.DurationVar(&interval, "interval", interval, "desired-state reconcile interval")
	}

	if err := flags.Parse(args[1:]); err != nil {
		return err
	}

	if flags.NArg() != 0 {
		return fmt.Errorf("gantry node-config %s: unexpected arguments: %v", args[0], flags.Args())
	}

	switch args[0] {
	case "reconcile":
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		return noderoute.Run(ctx, options, interval)
	case "check":
		return noderoute.CheckDesired(options)
	default:
		return fmt.Errorf("gantry node-config: unknown command %q", args[0])
	}
}
