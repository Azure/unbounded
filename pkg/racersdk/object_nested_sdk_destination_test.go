// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

func TestObjectWriteToNestedSDKDestinationPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind wire.ErrorKind
		want error
	}{
		{"unavailable", wire.ErrorUnavailable, ErrUnavailable},
		{"not found", wire.ErrorNotFound, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClient(t, offsetOrigin(1000))

			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			cause := errors.New("nested read failed")
			nested := failure(tc.kind, "get", cause)
			writerErr := fmt.Errorf("upstream read: %w", nested)

			n, err := o.WriteTo(writeFunc(func([]byte) (int, error) { return 0, writerErr }))
			if n != 0 {
				t.Fatalf("WriteTo = %d bytes; want 0", n)
			}

			assertIs(t, err, ErrDestination)
			assertIs(t, err, tc.want)
			assertIs(t, err, writerErr)
			assertIs(t, err, nested)
			assertIs(t, err, cause)

			// Check the destination first: the nested read is not this read's failure.
			var classification error

			switch {
			case errors.Is(err, ErrDestination):
				classification = ErrDestination
			case errors.Is(err, tc.want):
				classification = tc.want
			}

			if classification != ErrDestination {
				t.Fatalf("classification = %v; want ErrDestination", classification)
			}
		})
	}
}
