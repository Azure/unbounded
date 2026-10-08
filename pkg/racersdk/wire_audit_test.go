// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

func TestStatMethodNotAllowed(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         wire.ErrorKind
	}{
		{"client methods", "Allow: HEAD, POST\r\n", wire.ErrorInvalidArgument},
		{"origin methods", "Allow: HEAD, GET\r\n", wire.ErrorProtocol},
		{"missing methods", "", wire.ErrorProtocol},
		{"wrong methods", "Allow: POST\r\n", wire.ErrorProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, head []byte) {
				if !strings.HasPrefix(string(head), "HEAD /v2/objects/") {
					t.Error("Stat did not use the client-v2 HEAD endpoint")
				}

				_, _ = io.WriteString(conn, fmt.Sprintf("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n%s\r\n", tc.fields))
			})
			_, err := c.Stat(t.Context(), Request{})

			var typed *sdkError
			if !errors.As(err, &typed) || typed.kind != tc.want {
				t.Fatalf("Stat = %v; want kind %v", err, tc.want)
			}

			if tc.want == wire.ErrorInvalidArgument {
				assertIs(t, err, ErrInvalidRequest)
			}
		})
	}
}
