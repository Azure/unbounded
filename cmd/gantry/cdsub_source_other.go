// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package main

import (
	"log/slog"

	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/containerdstore"
)

// newCdsubSource on non-linux always returns NoOpSource - the
// containerd Go client only links cleanly on linux, and gantry is
// only meaningful as a kubelet-adjacent DaemonSet anyway. Non-linux
// builds are dev/test only.
func newCdsubSource(_ *config.Config, logger *slog.Logger) cdsub.ImageSource {
	logger.Info("cdsub: containerd integration unavailable on this platform - using NoOpSource")
	return cdsub.NoOpSource{}
}

// containerdBackedStore on non-linux always returns nil. main fails
// loudly when StorageMode == "containerd" but no real containerd
// source is available, rather than silently falling back.
func containerdBackedStore(_ cdsub.ImageSource, _ *config.Config, _ ...containerdstore.Option) *containerdstore.Store {
	return nil
}

// wireDescriptorRecorder is a no-op on non-linux - there is no
// real containerd source to wire.
func wireDescriptorRecorder(_ cdsub.ImageSource, _ *containerdstore.Store) {}
