// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk_test

import (
	"context"
	"fmt"
	"io"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func ExampleNewFakeClient() {
	snapshot := newExampleSnapshot("hello", `"v1"`)

	client, cleanup, err := racersdk.NewFakeClient(func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		pin, _ := request.Pin()
		page, _ := request.Range()

		return snapshot.open(ctx, request.Operation(), pin, page)
	})
	if err != nil {
		panic(err)
	}
	defer cleanup() // In a test, use t.Cleanup(cleanup).

	value, err := client.Get(context.Background(), racersdk.Request{})
	if err != nil {
		panic(err)
	}
	defer value.Close()

	// io.ReadAll is appropriate for this known five-byte fixture only.
	data, err := io.ReadAll(value)
	if err != nil {
		panic(err)
	}

	fmt.Println(value.Metadata().Size, string(data))
	// Output:
	// 5 hello
}
