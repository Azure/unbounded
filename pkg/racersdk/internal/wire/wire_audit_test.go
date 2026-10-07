// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import "testing"

func TestOriginMethodNotAllowed(t *testing.T) {
	for _, tc := range []struct {
		fields string
		want   ErrorKind
	}{
		{"Allow: HEAD, GET\r\n", ErrorInvalidArgument},
		{"Allow: HEAD, POST\r\n", ErrorProtocol},
		{"", ErrorProtocol},
	} {
		_, err := ParseResponseHead(rawResponse(405, "Content-Length: 0\r\n"+tc.fields), Request{Operation: OperationHead}, nil)
		assertKind(t, err, tc.want)
	}
}

func TestClientHeadResponseRejectsOtherOperations(t *testing.T) {
	_, err := ParseClientHeadResponse(rawResponse(405, "Content-Length: 0\r\nAllow: HEAD, POST\r\n"), Request{Operation: OperationBootstrap})
	assertKind(t, err, ErrorProtocol)
}
