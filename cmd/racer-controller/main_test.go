// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Azure/unbounded/internal/version"
)

func TestRejectInitializeCLI(t *testing.T) {
	before := os.Args

	t.Cleanup(func() { os.Args = before })

	for _, args := range [][]string{{"racer-controller", "initialize"}, {"racer-controller", "other"}, {"racer-controller", "initialize", "extra"}} {
		os.Args = args
		if err := run(); err == nil || err.Error() != "usage: racer-controller" {
			t.Fatalf("args %v: %v", args, err)
		}
	}
}

func TestStartupLogsBuildMetadataBeforeConfigurationFailure(t *testing.T) {
	before := os.Args

	t.Cleanup(func() { os.Args = before })

	os.Args = []string{"racer-controller"}

	t.Setenv("RACER_CLUSTER_ID", "invalid")

	var output bytes.Buffer
	ctrl.SetLogger(zap.New(zap.WriteTo(&output)))
	t.Cleanup(func() { ctrl.SetLogger(zap.New()) })
	require.Error(t, run())
	require.Contains(t, output.String(), "starting racer-controller")
	require.Contains(t, output.String(), version.String())
}
