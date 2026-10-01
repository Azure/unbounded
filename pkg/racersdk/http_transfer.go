// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "net/http"

// WriteToHTTP delivers a Value through the server's normal response lifecycle.
// The caller sets headers, including the selected range's Content-Length, and
// owns Close. Get Values retain buffered page validation. GetStreaming Values
// validate frame headers and forward bounded payload slices, using the writer's
// ReadFrom when a raw Unix socket is available. The final byte is withheld until
// Complete is validated; an incomplete page prefix can be exposed on failure.
// No connection is hijacked. Empty ranges validate Complete before returning,
// but cannot withhold a byte; callers must avoid committing empty responses early.
// Destination deadlines are used where supported. Arbitrary blocked writers
// cannot be interrupted, but retain a separately bounded copy admission slot.
// A streaming transfer failure is terminal; a handler that has committed headers
// must abort its response rather than emit an error body or report success.
func (v *Value) WriteToHTTP(w http.ResponseWriter) (int64, error) {
	if v.streaming {
		return v.writeStreamingHTTP(w)
	}

	return v.WriteTo(w)
}
