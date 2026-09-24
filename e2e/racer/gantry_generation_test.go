//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"net/http"
	"testing"

	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

func TestGantryRacerGeneration(t *testing.T) {
	if gantryNamespace(t) {
		return
	}

	data := bytes.Repeat([]byte("generation"), int(sdk.PageSize)/10+123)
	path := gantryPath("public", "blobs", data)
	object := gantryObject{data: data, mediaType: "application/octet-stream", corrupt: true}
	f := newGantryFixture(t, 3, map[string]gantryObject{path: object})
	corrupt := bytes.Clone(data)
	corrupt[len(corrupt)/2] ^= 1
	read := func(node int, want []byte) {
		t.Helper()

		resp, body, err := f.request(node, "GET", path, "", "")
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
			t.Fatalf("node%d: status=%v bytes=%d want=%d err=%v", node, resp, len(body), len(want), err)
		}
	}

	for node := range 3 {
		read(node, corrupt)
	}

	before := f.originRequestCount("GET", path)
	if before == 0 {
		t.Fatal("cold generation never fetched origin payload")
	}

	object.corrupt = false
	if err := f.setOriginObject(path, object); err != nil {
		t.Fatal(err)
	}
	// Origin repair alone cannot change a warm immutable cache entry.
	f.offline.Store(true)

	for node := range 3 {
		read(node, corrupt)
	}

	if f.originRequestCount("GET", path) != before {
		t.Fatal("old-generation warm reads contacted the origin")
	}

	f.offline.Store(false)

	if revision := f.bumpCacheGeneration("gantry"); revision != 2 {
		t.Fatalf("unexpected recovery revision %d", revision)
	}

	for node := range 3 {
		read(node, data)
	}

	after := f.originRequestCount("GET", path)
	if after <= before {
		t.Fatal("activated generation did not refetch repaired origin payload")
	}

	f.offline.Store(true)

	for node := range 3 {
		read(node, data)

		if f.metric(node, "gantry_racer_fallback_total") != 0 {
			t.Fatalf("node%d bypassed Racer", node)
		}
	}

	if f.originRequestCount("GET", path) != after {
		t.Fatal("new-generation warm reads contacted the origin")
	}
}
