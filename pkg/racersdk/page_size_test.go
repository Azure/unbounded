// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"fmt"
	"io"
	"testing"
)

func TestFourMiBPageBoundary(t *testing.T) {
	if PageSize != 4<<20 {
		t.Fatalf("PageSize = %d; want 4 MiB", PageSize)
	}

	for _, size := range []int64{(4 << 20) - 1, 4 << 20, (4 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			client := fakeClient(t, offsetOrigin(size))
			for _, small := range []bool{false, true} {
				object, err := client.Get(t.Context(), Request{}, ReadOptions{Offset: size - 1, Length: 1, SmallObject: small})
				if small && size > 4<<20 {
					assertIs(t, err, ErrInvalidRequest)
					continue
				}

				if err != nil {
					t.Fatal(err)
				}

				n, err := io.Copy(&offsetSink{offset: size - 1}, readerOnly{object})
				closeQuietly(object)

				if err != nil || n != 1 {
					t.Fatalf("small=%t: read %d bytes: %v", small, n, err)
				}
			}
		})
	}
}
