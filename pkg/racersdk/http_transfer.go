// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "net/http"

// WriteToHTTP delivers a Value through the server's normal response lifecycle.
// The caller sets headers, including the selected range's Content-Length, and
// owns Close. Subscription frames are validated before their payload is copied;
// their socket is never exposed to the HTTP writer. No connection is hijacked:
// keep-alive, ranges and server shutdown remain intact.
func (v *Value) WriteToHTTP(w http.ResponseWriter) (int64, error) {
	return v.WriteTo(w)
}
