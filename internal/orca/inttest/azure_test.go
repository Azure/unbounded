// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

// TestAzureBlobOrigin_ColdGet verifies the azureblob origin driver
// works against Azurite end-to-end on a 3-replica cluster. The
// MediumBlob spans 2 chunks so rendezvous-hashed routing typically
// exercises both fillLocal and FillFromPeer in a single run.
func TestAzureBlobOrigin_ColdGet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	ctr := pkgAzurite.NewContainer(ctx, t, "orca-origin")
	blob := MediumBlob()
	SeedAzure(ctx, t, pkgAzurite, ctr, []SeedBlob{blob})

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:     pkgLocalStack,
		Azurite:        pkgAzurite,
		OriginDriver:   "azureblob",
		AzureContainer: ctr,
	})

	resp := cl.Get(1).HTTP.Get(ctx, t, ctr, blob.Key)
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, string(resp.Body))
	}

	if !bytes.Equal(resp.Body, blob.Data) {
		t.Fatalf("body mismatch: got %d bytes want %d", len(resp.Body), len(blob.Data))
	}
}
