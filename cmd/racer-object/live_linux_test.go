// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	racer "github.com/Azure/unbounded/pkg/racer"
)

// Run in an AKS pod with the same projected identity and config as the backend.
func TestLiveWorkloadIdentity(t *testing.T) {
	path := os.Getenv("RACER_OBJECT_AKS_CONFIG")
	if path == "" {
		t.Skip("set RACER_OBJECT_AKS_CONFIG inside a Workload Identity pod")
	}

	c, err := loadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}

	a, err := azureClient(c.Endpoint, "workload-identity", 2)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	o := c.Objects[0]

	m, err := a.stat(ctx, o)
	if err != nil {
		t.Fatal(err)
	}

	if m.size == 0 {
		t.Fatal("live fixture must not be empty")
	}

	data := make([]byte, min(racer.PageSize, m.size))
	if err := a.read(ctx, o, m, data, 0); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256(data)

	want := os.Getenv("RACER_OBJECT_AKS_PAGE_SHA256")
	if want == "" || hex.EncodeToString(digest[:]) != want {
		t.Fatalf("first page digest %x does not match required RACER_OBJECT_AKS_PAGE_SHA256", digest)
	}
}
