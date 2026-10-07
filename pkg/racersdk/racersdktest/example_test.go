// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdktest_test //nolint:testableexamples // NewClient needs a *testing.T, so the example shows a test.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

// helloOrigin serves one five-byte object. It shows the whole origin contract:
// honor the ETag pin, return no body for Head, and return exactly the
// requested bytes, clamped to the end of the object.
func helloOrigin(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	const data = "hello"

	m := racersdk.Metadata{Size: int64(len(data)), ETag: `"v1"`, ExpiresAt: time.Now().Add(time.Hour)}
	if r.ETag != "" && r.ETag != m.ETag {
		return m, nil, racersdk.ErrVersionMismatch
	}

	if r.Head {
		return m, nil, nil
	}

	start := min(r.Offset, m.Size)

	length := min(r.Length, m.Size-start)
	if length == 0 {
		return m, nil, nil
	}

	return m, io.NopCloser(strings.NewReader(data[start : start+length])), nil
}

// testGet is an ordinary test. Name it TestGet in your own _test.go file.
func testGet(t *testing.T) {
	client := racersdktest.NewClient(t, helloOrigin)

	object, err := client.Get(t.Context(), racersdk.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()

	data, err := io.ReadAll(object)
	if err != nil || string(data) != "hello" {
		t.Fatalf("Get = %q, %v", data, err)
	}
}

func ExampleNewClient() {
	// Run testGet from a test function:
	//
	//	func TestGet(t *testing.T) { testGet(t) }
	_ = testGet
}
