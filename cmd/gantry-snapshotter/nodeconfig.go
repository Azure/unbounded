// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/nodeconfig"
)

// nodeConfigOptions are the flags of the node-config subcommand.
type nodeConfigOptions struct {
	config           string
	socket           string
	containerdSocket string
	snapshotter      string
	bootstrapRuntime string
	platform         string
	restart          string
	socketTimeout    time.Duration
	interval         time.Duration
	skipDefault      bool
}

// runNodeConfig merges gantry-snapshotter's containerd configuration into the
// node's own, in the order that keeps the node bootable.
//
// It runs as its own DaemonSet rather than as part of the snapshotter's pod,
// and the reason is the deadlock the phases exist to avoid. The snapshotter's
// pod runs under a runtime handler that phase one creates, so it cannot be what
// creates it. This pod runs under the node's default handler, which means that
// after a reboot of a fully configured node it cannot start until the
// snapshotter is already serving. That is fine, and is the whole trick: there
// is nothing for it to do on such a node until then anyway.
func runNodeConfig(args []string, stdout, stderr io.Writer) error {
	opts := nodeConfigOptions{}

	fs := flag.NewFlagSet("node-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.config, "config", nodeconfig.DefaultConfigPath, "containerd configuration file to merge into")
	fs.StringVar(&opts.socket, "socket", nodeconfig.DefaultSocket, "snapshotter socket to register and to wait for")
	fs.StringVar(&opts.containerdSocket, "containerd-socket", "/run/containerd/containerd.sock",
		"containerd socket to wait for after a restart")
	fs.StringVar(&opts.snapshotter, "snapshotter", nodeconfig.DefaultSnapshotter, "proxy plugin name")
	fs.StringVar(&opts.bootstrapRuntime, "bootstrap-runtime", nodeconfig.DefaultBootstrapRuntime,
		"runtime handler that keeps using overlayfs")
	fs.StringVar(&opts.platform, "platform", "linux/amd64", "platform whose unpack path is pointed at the snapshotter")
	fs.StringVar(&opts.restart, "restart-command",
		"nsenter -t 1 -m -u -i -n -p -- systemctl restart containerd",
		"command that restarts containerd on the host")
	fs.DurationVar(&opts.socketTimeout, "socket-timeout", 30*time.Minute,
		"how long to wait for the snapshotter socket before giving up on this pass")
	fs.DurationVar(&opts.interval, "interval", time.Minute,
		"how often to re-check the node's configuration; zero runs once and exits")
	fs.BoolVar(&opts.skipDefault, "bootstrap-only", false,
		"apply only the inert phase, leaving containerd's default snapshotter alone")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := signalContext()

	for {
		if err := nodeConfigPass(ctx, opts, stdout); err != nil {
			if ctx.Err() != nil {
				return nil
			}

			fmt.Fprintf(stderr, "node-config: %v\n", err) //nolint:errcheck

			if opts.interval == 0 {
				return err
			}
		}

		if opts.interval == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.interval):
		}
	}
}

// nodeConfigPass brings the node's containerd configuration to where it should
// be, restarting containerd only for the phases that actually changed it.
func nodeConfigPass(ctx context.Context, opts nodeConfigOptions, stdout io.Writer) error {
	merge := nodeconfig.Options{
		Snapshotter:      opts.snapshotter,
		BootstrapRuntime: opts.bootstrapRuntime,
		Socket:           opts.socket,
		Platform:         opts.platform,
	}

	changed, err := applyPhase(ctx, opts, nodeconfig.PhaseBootstrap, merge, stdout)
	if err != nil {
		return err
	}

	if changed {
		if err := restartContainerd(ctx, opts, stdout); err != nil {
			return err
		}
	}

	if opts.skipDefault {
		return nil
	}

	// Nothing below this line is safe until the socket the configuration is
	// about actually answers. Waiting here rather than checking once is what
	// lets this pod be scheduled at the same time as the snapshotter's.
	if err := waitForSocket(ctx, opts.socket, opts.socketTimeout); err != nil {
		return err
	}

	changed, err = applyPhase(ctx, opts, nodeconfig.PhaseDefault, merge, stdout)
	if err != nil {
		return err
	}

	if !changed {
		return nil
	}

	return restartContainerd(ctx, opts, stdout)
}

func applyPhase(ctx context.Context, opts nodeConfigOptions, phase nodeconfig.Phase,
	merge nodeconfig.Options, stdout io.Writer,
) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	doc, err := nodeconfig.Load(opts.config)
	if err != nil {
		return false, err
	}

	changed, err := nodeconfig.Apply(doc, phase, merge)
	if err != nil {
		return false, fmt.Errorf("phase %d: %w", int(phase), err)
	}

	if !changed {
		return false, nil
	}

	if err := nodeconfig.Save(opts.config, doc); err != nil {
		return false, err
	}

	fmt.Fprintf(stdout, "node-config: applied phase %d to %s\n", int(phase), opts.config) //nolint:errcheck

	return true, nil
}

// restartContainerd restarts containerd and waits for it to answer again.
//
// Restarting containerd does not stop running containers: their shims are
// separate processes that outlive it and reattach. That is what makes this
// safe to do from inside a container containerd itself started.
func restartContainerd(ctx context.Context, opts nodeConfigOptions, stdout io.Writer) error {
	fields := strings.Fields(opts.restart)
	if len(fields) == 0 {
		return errors.New("no restart command configured")
	}

	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...) //nolint:gosec // the command is operator-supplied configuration

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart containerd: %w: %s", err, strings.TrimSpace(string(output)))
	}

	fmt.Fprintln(stdout, "node-config: restarted containerd") //nolint:errcheck

	return waitForSocket(ctx, opts.containerdSocket, 2*time.Minute)
}

// waitForSocket blocks until a unix socket exists.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", path)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
