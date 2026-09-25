// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdk provides a streaming Racer client, an origin server, and
// validated client/origin protocol values over HTTP/1.1 Unix sockets.
//
// Keys use canonical lowercase hexadecimal. Opaque fetch credentials are immutable
// and redacted in diagnostic formatting; explicitly extracting their contents is
// intended only for forwarding to an origin. Validation errors never echo input.
//
// Client.Get returns an owned Value: defer its Close and provide a context that
// covers the entire stream. ServeOrigin owns its canonical socket and all callback
// bodies. Callbacks must honor cancellation and provide bodies whose Close can
// interrupt Read. See ClientConfig and OriginConfig for bounded resource defaults.
package racersdk
