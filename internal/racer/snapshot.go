// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"net/http"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Poll must authenticate the TLS client certificate, parse the optional bounded
// decimal cursor, and bound waiting by both PollWait and certificate expiration.
// A nil publication with no error will mean the normal 204 timeout, never a stub.
func (*Server) Poll(_ context.Context, _ *http.Request) (*CommittedPublication, error) {
	return nil, pending("snapshot.poll")
}

func ParseCursor(_ string) (*wire.Sequence, error) {
	return nil, pending("snapshot.parse_cursor")
}
