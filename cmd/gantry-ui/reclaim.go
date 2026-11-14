// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/gantryui"
)

func runReclaimer(mode string) error {
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(signals, gantryui.ReclaimTimeout)
	defer cancel()

	if mode == "reclaim-node" {
		node := os.Getenv("GANTRY_RECLAIM_NODE")
		if node == "" {
			return errors.New("GANTRY_RECLAIM_NODE is required")
		}

		result, err := gantryui.ReclaimNode(ctx, node, "/run/containerd/containerd.sock", "/var/lib/gantry", strings.FieldsFunc(os.Getenv("GANTRY_RECLAIM_DIGESTS"), func(r rune) bool { return r == ',' }))
		if err != nil {
			if writeErr := os.WriteFile("/dev/termination-log", []byte(err.Error()), 0o600); writeErr != nil {
				return errors.Join(err, writeErr)
			}

			return err
		}

		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}

		return os.WriteFile("/dev/termination-log", raw, 0o600)
	}

	raw := os.Getenv("GANTRY_RECLAIM")
	if len(raw) == 0 || len(raw) > 4096 {
		return errors.New("GANTRY_RECLAIM must contain a bounded request")
	}

	var spec gantryui.ReclaimSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return err
	}

	if err := spec.Validate(); err != nil {
		return err
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	cfg.Timeout = 10 * time.Second
	cfg.QPS, cfg.Burst = 80, 128

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(os.Stdout)
	emit := func(progress gantryui.ReclaimProgress) {
		if err := encoder.Encode(progress); err != nil {
			fmt.Fprintln(os.Stderr, "reclaim progress write:", err)
		}
	}

	return gantryui.RunReclaim(ctx, client, spec, emit)
}
