// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"google.golang.org/grpc"

	gantryoverlaybd "github.com/Azure/unbounded/internal/gantry/overlaybd"
)

type snapshotterOptions struct {
	root       string
	address    string
	endpoint   string
	deviceGlob string
	containerd string
	fallback   string
}

func buildSnapshotterFlagSet() *flag.FlagSet {
	options := snapshotterOptions{}
	fs := flag.NewFlagSet("snapshotter", flag.ContinueOnError)
	bindSnapshotterFlags(fs, &options)

	return fs
}

func bindSnapshotterFlags(fs *flag.FlagSet, options *snapshotterOptions) {
	fs.StringVar(&options.root, "root", "/var/lib/gantry/snapshotter", "snapshotter state directory")
	fs.StringVar(&options.address, "address", "/run/gantry-overlaybd/gantry-overlaybd.sock", "snapshotter Unix socket")
	fs.StringVar(&options.endpoint, "gantry-endpoint", "http://127.0.0.1:5000/blobs", "Gantry exact-range endpoint")
	fs.StringVar(&options.deviceGlob, "nbd-devices", "/dev/nbd*", "Linux NBD device glob")
	fs.StringVar(&options.containerd, "containerd-address", "/run/containerd/containerd.sock", "containerd Unix socket for ordinary OCI layers")
	fs.StringVar(&options.fallback, "fallback-snapshotter", "overlayfs", "containerd snapshotter for ordinary OCI layers")
}

func runSnapshotter(args []string) (returnErr error) {
	options := snapshotterOptions{}
	fs := flag.NewFlagSet("snapshotter", flag.ContinueOnError)
	bindSnapshotterFlags(fs, &options)

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 0 {
		return fmt.Errorf("gantry snapshotter: unexpected arguments %v", fs.Args())
	}

	if options.fallback == "gantry-overlaybd" {
		return errors.New("gantry snapshotter: fallback snapshotter cannot be gantry-overlaybd")
	}

	mounter, err := gantryoverlaybd.NewNBDMounter(options.endpoint, &http.Client{Timeout: 2 * time.Minute}, options.deviceGlob)
	if err != nil {
		return err
	}

	containerdClient, err := containerd.New(options.containerd, containerd.WithTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("gantry snapshotter: connect to containerd: %w", err)
	}

	probeContext, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
	_, probeErr := containerdClient.Version(probeContext)

	cancelProbe()

	if probeErr != nil {
		return errors.Join(fmt.Errorf("gantry snapshotter: probe containerd: %w", probeErr), containerdClient.Close())
	}

	snapshotter, err := gantryoverlaybd.NewSnapshotterWithFallback(
		options.root,
		mounter,
		containerdClient.SnapshotService(options.fallback),
	)
	if err != nil {
		return errors.Join(err, containerdClient.Close())
	}
	defer func() {
		returnErr = errors.Join(returnErr, snapshotter.Close(), containerdClient.Close())
	}()

	if err := snapshotter.Cleanup(context.Background()); err != nil {
		return fmt.Errorf("gantry snapshotter: initial cleanup: %w", err)
	}

	listener, err := listenSnapshotter(options.address)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()           //nolint:errcheck // server shutdown owns listener errors
		_ = os.Remove(options.address) //nolint:errcheck // stale sockets are safely replaced on start
	}()

	server := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(server, snapshotservice.FromSnapshotter(snapshotter))

	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErrors:
		return fmt.Errorf("gantry snapshotter: serve: %w", err)
	case <-ctx.Done():
	}

	stopped := make(chan struct{})

	go func() {
		server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		server.Stop()
		<-stopped
	}

	return nil
}

func listenSnapshotter(address string) (net.Listener, error) {
	if address == "" {
		return nil, errors.New("gantry snapshotter: address is required")
	}

	if err := os.MkdirAll(filepath.Dir(address), 0o755); err != nil {
		return nil, fmt.Errorf("gantry snapshotter: create socket directory: %w", err)
	}

	if info, err := os.Lstat(address); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("gantry snapshotter: refusing to replace non-socket %s", address)
		}

		if err := os.Remove(address); err != nil {
			return nil, fmt.Errorf("gantry snapshotter: remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("gantry snapshotter: inspect socket: %w", err)
	}

	listener, err := net.Listen("unix", address)
	if err != nil {
		return nil, fmt.Errorf("gantry snapshotter: listen: %w", err)
	}

	if err := os.Chmod(address, 0o660); err != nil {
		_ = listener.Close() //nolint:errcheck // chmod error is the actionable failure
		return nil, fmt.Errorf("gantry snapshotter: set socket permissions: %w", err)
	}

	return listener, nil
}
