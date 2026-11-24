// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build integrationtest

package inttest

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Package-level container handles shared across tests in this package.
// TestMain brings them up once and tears them down at the end.
var (
	pkgS3      *S3Backend
	pkgAzurite *Azurite
)

// TestMain provisions the S3 backend + Azurite once per `go test` run.
// Per-test buckets / containers are allocated inside individual tests
// to avoid cross-test interference.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	gr, err := StartS3Backend(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: start s3 backend: %v\n", err)
		os.Exit(1)
	}

	pkgS3 = gr

	az, err := StartAzurite(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: start azurite: %v\n", err)

		_ = gr.Terminate(ctx) //nolint:errcheck // best-effort cleanup

		os.Exit(1)
	}

	pkgAzurite = az

	code := m.Run()

	termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer termCancel()

	_ = pkgAzurite.Terminate(termCtx) //nolint:errcheck // best-effort
	_ = pkgS3.Terminate(termCtx)      //nolint:errcheck // best-effort

	os.Exit(code)
}
