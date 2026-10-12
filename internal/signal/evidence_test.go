// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"strings"
	"testing"
)

func TestKubernetesEvidenceStoreReturnsQueryableRunURIAndDigest(t *testing.T) {
	store, err := NewKubernetesEvidenceStore("project-signal")
	if err != nil {
		t.Fatal(err)
	}

	uri, digest, err := store.Put(context.Background(), "project-signal/example/run-a.json", []byte(`{"verdict":"Pass"}`))
	if err != nil {
		t.Fatal(err)
	}

	if uri != "kubernetes://project-signal/certificationruns/run-a" {
		t.Fatalf("unexpected URI %q", uri)
	}

	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("unexpected digest %q", digest)
	}
}
