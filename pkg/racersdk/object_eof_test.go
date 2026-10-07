// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"io"
	"testing"
)

func TestObjectWriteToWriterEOF(t *testing.T) {
	const size = 3

	for _, test := range []struct {
		name     string
		final    bool
		accepted int
		want     int64
	}{
		{name: "early-zero", want: 0},
		{name: "early-partial", accepted: 1, want: 1},
		{name: "final-rejected", final: true, want: size - 1},
		{name: "final-accepted", final: true, accepted: 1, want: size},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := fakeClient(t, offsetOrigin(size))

			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			var written int64

			w := writeFunc(func(p []byte) (int, error) {
				if test.final && written < size-1 {
					written += int64(len(p))
					return len(p), nil
				}

				n := min(len(p), test.accepted)
				written += int64(n)

				return n, io.EOF
			})

			n, err := o.WriteTo(w)
			if n != test.want || written != test.want {
				t.Fatalf("WriteTo count = %d, writer accepted %d; want %d", n, written, test.want)
			}

			assertIs(t, err, io.EOF)
			assertIs(t, err, ErrUnavailable)

			if n, repeatedErr := o.WriteTo(io.Discard); n != 0 || repeatedErr != err {
				t.Fatalf("repeated WriteTo = %d, %v; want 0, %v", n, repeatedErr, err)
			}

			if n, repeatedErr := o.Read(make([]byte, 1)); n != 0 || repeatedErr != err {
				t.Fatalf("Read after writer EOF = %d, %v; want 0, %v", n, repeatedErr, err)
			}
		})
	}
}
