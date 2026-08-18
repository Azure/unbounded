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
	"net"
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
	revertAfter      time.Duration
	interval         time.Duration
	skipDefault      bool
	keepDefault      bool
}

// runNodeConfig keeps the node's containerd configuration in step with whether
// the snapshotter is actually serving on this node.
//
// It runs as its own DaemonSet rather than as part of the snapshotter's pod,
// and the reason is the deadlock the phases exist to avoid. The snapshotter's
// pod runs under a runtime handler that phase one creates, so it cannot be what
// creates it.
//
// The loop is a watchdog, not a one-shot installer. containerd unpacks every
// image through the CRI default snapshotter and the runtime handler a pod names
// has no say in that, so a node left on phase two with no socket cannot pull an
// image at all - not even the snapshotter's own, which is what an upgrade needs
// it to pull. So phase two is only in force while the socket answers, and goes
// away again when it stops.
func runNodeConfig(args []string, stdout, stderr io.Writer) error {
	opts := nodeConfigOptions{}

	fs := flag.NewFlagSet("node-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.config, "config", nodeconfig.DefaultConfigPath, "containerd configuration file to merge into")
	fs.StringVar(&opts.socket, "socket", nodeconfig.DefaultSocket, "snapshotter socket to register and to watch")
	fs.StringVar(&opts.containerdSocket, "containerd-socket", "/run/containerd/containerd.sock",
		"containerd socket to wait for after a restart")
	fs.StringVar(&opts.snapshotter, "snapshotter", nodeconfig.DefaultSnapshotter, "proxy plugin name")
	fs.StringVar(&opts.bootstrapRuntime, "bootstrap-runtime", nodeconfig.DefaultBootstrapRuntime,
		"runtime handler that keeps using overlayfs")
	fs.StringVar(&opts.platform, "platform", "linux/amd64", "platform whose unpack path is pointed at the snapshotter")
	fs.StringVar(&opts.restart, "restart-command",
		"nsenter -t 1 -m -u -i -n -p -- systemctl restart containerd",
		"command that restarts containerd on the host")
	fs.DurationVar(&opts.revertAfter, "revert-after", 90*time.Second,
		"how long the snapshotter socket may stay silent before the node drops back to overlayfs")
	fs.DurationVar(&opts.interval, "interval", 15*time.Second,
		"how often to re-check the node's configuration; zero runs once and exits")
	fs.BoolVar(&opts.skipDefault, "bootstrap-only", false,
		"apply only the inert phase, leaving containerd's default snapshotter alone")
	fs.BoolVar(&opts.keepDefault, "keep-default-on-exit", false,
		"leave the node on the snapshotter when this exits instead of dropping back to overlayfs")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := signalContext()
	state := &nodeConfigState{}

	for {
		if err := nodeConfigPass(ctx, opts, state, stdout); err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(stderr, "node-config: %v\n", err) //nolint:errcheck

				if opts.interval == 0 {
					return err
				}
			}
		}

		if opts.interval == 0 || ctx.Err() != nil {
			break
		}

		select {
		case <-ctx.Done():
		case <-time.After(opts.interval):
		}
	}

	return nodeConfigShutdown(opts, state, stdout, stderr)
}

// nodeConfigShutdown drops the node back to overlayfs on the way out.
//
// A pod that is going away is usually going away because it is being replaced,
// and the replacement has an image to pull. Leaving phase two in force with
// nothing left to watch it is what turns a routine upgrade into a node that
// cannot start anything.
func nodeConfigShutdown(opts nodeConfigOptions, state *nodeConfigState, stdout, stderr io.Writer) error {
	if opts.keepDefault || opts.skipDefault || !state.defaultApplied {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 3*time.Minute)
	defer cancel()

	if err := revertDefault(ctx, opts, state, stdout); err != nil {
		fmt.Fprintf(stderr, "node-config: shutdown revert: %v\n", err) //nolint:errcheck
	}

	return nil
}

// nodeConfigState is what the watchdog remembers between passes.
type nodeConfigState struct {
	// silentSince is when the snapshotter socket last stopped answering, zero
	// while it is answering.
	silentSince time.Time

	// defaultApplied records that this agent has seen phase two in force, so
	// that a pod which only ever ran against a bootstrapped node does not
	// rewrite its configuration on the way out.
	defaultApplied bool
}

// nodeConfigPass brings the node's containerd configuration to where it should
// be, restarting containerd only for the changes that actually happened.
func nodeConfigPass(ctx context.Context, opts nodeConfigOptions, state *nodeConfigState, stdout io.Writer) error {
	changed, err := applyPhase(ctx, opts, nodeconfig.PhaseBootstrap, stdout)
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

	if !socketAnswers(opts.socket) {
		if state.silentSince.IsZero() {
			state.silentSince = time.Now()
		}

		// A snapshotter that is merely restarting is not a reason to rewrite
		// the node. Only a socket that stays silent past the grace period is.
		if time.Since(state.silentSince) < opts.revertAfter {
			return nil
		}

		return revertDefault(ctx, opts, state, stdout)
	}

	state.silentSince = time.Time{}

	changed, err = applyPhase(ctx, opts, nodeconfig.PhaseDefault, stdout)
	if err != nil {
		return err
	}

	state.defaultApplied = true

	if !changed {
		return nil
	}

	return restartContainerd(ctx, opts, stdout)
}

// revertDefault takes the node back to overlayfs, leaving phase one alone.
func revertDefault(ctx context.Context, opts nodeConfigOptions, state *nodeConfigState, stdout io.Writer) error {
	doc, err := nodeconfig.Load(opts.config)
	if err != nil {
		return err
	}

	changed, err := nodeconfig.Revert(doc, mergeOptions(opts))
	if err != nil {
		return fmt.Errorf("revert: %w", err)
	}

	if !changed {
		return nil
	}

	if err := nodeconfig.Save(opts.config, doc); err != nil {
		return err
	}

	state.defaultApplied = false

	fmt.Fprintf(stdout, "node-config: snapshotter is not serving, reverted %s to overlayfs\n", //nolint:errcheck
		opts.config)

	return restartContainerd(ctx, opts, stdout)
}

func mergeOptions(opts nodeConfigOptions) nodeconfig.Options {
	return nodeconfig.Options{
		Snapshotter:      opts.snapshotter,
		BootstrapRuntime: opts.bootstrapRuntime,
		Socket:           opts.socket,
		Platform:         opts.platform,
	}
}

func applyPhase(ctx context.Context, opts nodeConfigOptions, phase nodeconfig.Phase, stdout io.Writer) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	doc, err := nodeconfig.Load(opts.config)
	if err != nil {
		return false, err
	}

	changed, err := nodeconfig.Apply(doc, phase, mergeOptions(opts))
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

// socketAnswers reports whether something is listening on a unix socket.
//
// It dials rather than stats because the file outlives the process that bound
// it: a snapshotter killed without cleaning up leaves a socket that exists and
// refuses every connection, which is the case this most needs to catch.
func socketAnswers(path string) bool {
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return false
	}

	conn.Close() //nolint:errcheck,gosec // nothing was written

	return true
}

// waitForSocket blocks until a unix socket answers.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if socketAnswers(path) {
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
