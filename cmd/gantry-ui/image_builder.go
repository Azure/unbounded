// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/gantryui"
)

func runImageBuilder() error {
	raw := os.Getenv("GANTRY_IMAGE_BUILD")
	if len(raw) == 0 || len(raw) > 4096 {
		return errors.New("GANTRY_IMAGE_BUILD must contain a bounded build request")
	}

	var spec gantryui.BuildSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return err
	}

	if err := spec.Validate(); err != nil {
		return err
	}

	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return err
	}

	registry, err := gantryui.NewRegistry(spec.Registry, spec.Architecture, credential)
	if err != nil {
		return err
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	config.Timeout = 10 * time.Second

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(signals, gantryui.ImageBuildTimeout)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	emit := func(progress gantryui.BuildProgress) {
		logger.Info("Image generation", slog.String("stage", progress.Stage), slog.Int("layer", progress.Layer), slog.Int("layers", progress.Layers), slog.Int64("bytes", progress.Bytes), slog.Int64("layerBytes", progress.LayerBytes), slog.String("message", progress.Message))
	}

	return gantryui.RunImageBuild(ctx, client, registry, spec, "/work", emit)
}
