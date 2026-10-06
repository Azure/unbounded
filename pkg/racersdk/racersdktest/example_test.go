// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdktest_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func ExampleNewClient() {
	tag, _ := racersdk.ParseETag(`"v1"`)

	client, cleanup, err := racersdktest.NewClient(func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		m := racersdk.Metadata{Size: 5, ETag: tag, ExpiresAt: time.UnixMilli(0)}
		if err := ctx.Err(); err != nil {
			return m, nil, err
		}

		if pin, ok := request.Pin(); ok && pin != tag {
			return m, nil, racersdk.NewOriginError(racersdk.ErrorVersionUnavailable, nil)
		}

		if request.Operation() == racersdk.OperationHead {
			return m, nil, nil
		}

		page, _ := request.Range()

		first, last, err := page.Resolve(m.Size)
		if err != nil {
			return m, nil, err
		}

		return m, io.NopCloser(strings.NewReader("hello"[first : last+1])), nil
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
